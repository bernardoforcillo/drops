package pg

import (
	"fmt"
	"strings"
)

// DiffOptions tunes how Diff renders statements.
type DiffOptions struct {
	// Safe wraps every destructive or creative DDL in IF [NOT] EXISTS so
	// the migration can be re-run without errors. ALTER COLUMN does not
	// have an IF EXISTS form in PostgreSQL, so it is emitted unchanged.
	Safe bool
}

// DiffDown returns the SQL that reverses the migration from cur
// back to prev — applying these statements after the corresponding
// Diff(prev, cur) would restore the original schema. Provided as
// a distinct entry point so generated migration sets can carry the
// rollback alongside the forward direction without the caller
// having to swap arguments.
//
//	up := pg.Diff(prev, cur, opts)
//	down := pg.DiffDown(prev, cur, opts) // = Diff(cur, prev, opts)
func DiffDown(prev, cur *Snapshot, opts ...DiffOptions) []string {
	return Diff(cur, prev, opts...)
}

// Diff returns the ordered list of SQL statements that, applied in
// order, evolve a database from prev's schema to cur's. Output is
// deterministic for a given (prev, cur, opts) — keys are walked in
// sorted order — so re-running against the same input produces
// byte-identical SQL.
//
// Operation order:
//  1. DROP TABLE   for tables removed entirely
//  2. CREATE TABLE for new tables (column defs + inline UNIQUE only)
//  3. ALTER TABLE  for column-level changes on surviving tables
//     (drop, add, type, NOT NULL, DEFAULT)
//  4. UNIQUE       constraint adds/drops on surviving tables
//  5. FOREIGN KEY  adds/drops on every table — emitted after CREATE
//     TABLEs so cross-table references resolve.
func Diff(prev, cur *Snapshot, opts ...DiffOptions) []string {
	var opt DiffOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	if prev == nil {
		prev = EmptySnapshot()
	}
	if cur == nil {
		cur = EmptySnapshot()
	}
	var out []string

	for _, key := range sortedKeys(prev.Tables) {
		if _, ok := cur.Tables[key]; !ok {
			out = append(out, dropTableSQL(prev.Tables[key], opt.Safe))
		}
	}
	for _, key := range sortedKeys(cur.Tables) {
		if _, ok := prev.Tables[key]; !ok {
			out = append(out, createTableSQL(cur.Tables[key], opt.Safe))
		}
	}
	for _, key := range sortedKeys(cur.Tables) {
		curT := cur.Tables[key]
		prevT, exists := prev.Tables[key]
		if !exists {
			// New table: only emit composite PKs and CHECK
			// constraints here (columns + inline UNIQUE were
			// rendered by createTableSQL above).
			empty := &TableSnapshot{
				CompositePrimaryKeys: map[string]*CompositePKSnapshot{},
				CheckConstraints:     map[string]*CheckSnapshot{},
			}
			out = append(out, diffCompositePKs(empty, curT, opt.Safe)...)
			out = append(out, diffChecks(empty, curT, opt.Safe)...)
			continue
		}
		out = append(out, diffColumns(prevT, curT, opt.Safe)...)
		out = append(out, diffUniques(prevT, curT, opt.Safe)...)
		out = append(out, diffCompositePKs(prevT, curT, opt.Safe)...)
		out = append(out, diffChecks(prevT, curT, opt.Safe)...)
	}
	// Foreign keys: emitted after CREATE TABLE / column changes
	// so target columns exist; emitted before indexes so the
	// supporting unique constraint is in place if a FK depends on
	// it.
	for _, key := range sortedKeys(cur.Tables) {
		curT := cur.Tables[key]
		prevT, exists := prev.Tables[key]
		if !exists {
			prevT = &TableSnapshot{ForeignKeys: map[string]*ForeignKeySnapshot{}}
		}
		out = append(out, diffForeignKeys(prevT, curT, opt.Safe)...)
	}
	// Indexes after FKs so dependency order is consistent.
	for _, key := range sortedKeys(cur.Tables) {
		curT := cur.Tables[key]
		prevT, exists := prev.Tables[key]
		if !exists {
			prevT = &TableSnapshot{Indexes: map[string]*IndexSnapshot{}}
		}
		out = append(out, diffIndexes(prevT, curT, opt.Safe)...)
	}

	return out
}

func createTableSQL(t *TableSnapshot, safe bool) string {
	var b strings.Builder
	if safe {
		b.WriteString(`CREATE TABLE IF NOT EXISTS "`)
	} else {
		b.WriteString(`CREATE TABLE "`)
	}
	b.WriteString(t.Name)
	b.WriteString("\" (\n")
	first := true
	for _, k := range sortedKeys(t.Columns) {
		if !first {
			b.WriteString(",\n")
		}
		first = false
		b.WriteByte('\t')
		b.WriteString(columnDefSQL(t.Columns[k]))
	}
	for _, k := range sortedKeys(t.UniqueConstraints) {
		b.WriteString(",\n\t")
		b.WriteString(uniqueInlineSQL(t.UniqueConstraints[k]))
	}
	b.WriteString("\n);")
	return b.String()
}

func dropTableSQL(t *TableSnapshot, safe bool) string {
	if safe {
		return fmt.Sprintf(`DROP TABLE IF EXISTS "%s" CASCADE;`, t.Name)
	}
	return fmt.Sprintf(`DROP TABLE "%s" CASCADE;`, t.Name)
}

func columnDefSQL(c *ColumnSnapshot) string {
	var b strings.Builder
	b.WriteByte('"')
	b.WriteString(c.Name)
	b.WriteString(`" `)
	b.WriteString(c.Type)
	if c.PrimaryKey {
		b.WriteString(" PRIMARY KEY")
	}
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	if c.Default != nil {
		b.WriteString(" DEFAULT ")
		b.WriteString(*c.Default)
	}
	return b.String()
}

func uniqueInlineSQL(u *UniqueSnapshot) string {
	return fmt.Sprintf(`CONSTRAINT "%s" UNIQUE(%s)`, u.Name, strings.Join(quoteIdents(u.Columns), ", "))
}

func diffColumns(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.Columns) {
		if _, ok := cur.Columns[k]; !ok {
			if safe {
				out = append(out, fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN IF EXISTS "%s";`, cur.Name, k))
			} else {
				out = append(out, fmt.Sprintf(`ALTER TABLE "%s" DROP COLUMN "%s";`, cur.Name, k))
			}
		}
	}
	for _, k := range sortedKeys(cur.Columns) {
		if _, ok := prev.Columns[k]; ok {
			continue
		}
		if safe {
			out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN IF NOT EXISTS %s;`, cur.Name, columnDefSQL(cur.Columns[k])))
		} else {
			out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ADD COLUMN %s;`, cur.Name, columnDefSQL(cur.Columns[k])))
		}
	}
	for _, k := range sortedKeys(cur.Columns) {
		prevC, ok := prev.Columns[k]
		if !ok {
			continue
		}
		curC := cur.Columns[k]
		if prevC.Type != curC.Type {
			out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" SET DATA TYPE %s;`,
				cur.Name, k, curC.Type))
		}
		if prevC.NotNull != curC.NotNull {
			if curC.NotNull {
				out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" SET NOT NULL;`, cur.Name, k))
			} else {
				out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" DROP NOT NULL;`, cur.Name, k))
			}
		}
		if !sameStringPtr(prevC.Default, curC.Default) {
			if curC.Default == nil {
				out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" DROP DEFAULT;`, cur.Name, k))
			} else {
				out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ALTER COLUMN "%s" SET DEFAULT %s;`,
					cur.Name, k, *curC.Default))
			}
		}
	}
	return out
}

func diffUniques(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.UniqueConstraints) {
		if _, ok := cur.UniqueConstraints[k]; !ok {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	for _, k := range sortedKeys(cur.UniqueConstraints) {
		if _, ok := prev.UniqueConstraints[k]; ok {
			continue
		}
		u := cur.UniqueConstraints[k]
		cols := strings.Join(quoteIdents(u.Columns), ", ")
		out = append(out, fmt.Sprintf(`ALTER TABLE "%s" ADD CONSTRAINT "%s" UNIQUE(%s);`, cur.Name, u.Name, cols))
	}
	return out
}

func diffForeignKeys(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.ForeignKeys) {
		if _, ok := cur.ForeignKeys[k]; !ok {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	for _, k := range sortedKeys(cur.ForeignKeys) {
		if _, ok := prev.ForeignKeys[k]; ok {
			continue
		}
		out = append(out, fkAddSQL(cur.Name, cur.ForeignKeys[k]))
	}
	return out
}

// dropConstraintSQL emits DROP CONSTRAINT [IF EXISTS] "name".
func dropConstraintSQL(table, name string, safe bool) string {
	if safe {
		return fmt.Sprintf(`ALTER TABLE "%s" DROP CONSTRAINT IF EXISTS "%s";`, table, name)
	}
	return fmt.Sprintf(`ALTER TABLE "%s" DROP CONSTRAINT "%s";`, table, name)
}

func fkAddSQL(tableFrom string, fk *ForeignKeySnapshot) string {
	cols := strings.Join(quoteIdents(fk.ColumnsFrom), ", ")
	targetCols := strings.Join(quoteIdents(fk.ColumnsTo), ", ")
	return fmt.Sprintf(`ALTER TABLE "%s" ADD CONSTRAINT "%s" FOREIGN KEY (%s) REFERENCES "%s"(%s) ON DELETE %s ON UPDATE %s;`,
		tableFrom, fk.Name, cols, fk.TableTo, targetCols, fk.OnDelete, fk.OnUpdate)
}

func quoteIdents(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = `"` + n + `"`
	}
	return out
}

// diffIndexes emits CREATE INDEX / DROP INDEX for indexes that
// were added or removed between prev and cur. Index changes are
// not "modified in place" — they are dropped and recreated when
// any structural field differs.
func diffIndexes(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.Indexes) {
		curIdx, present := cur.Indexes[k]
		prevIdx := prev.Indexes[k]
		if !present {
			out = append(out, dropIndexSQL(k, safe))
			continue
		}
		// Drop-and-recreate when shape changed.
		if !indexEqual(prevIdx, curIdx) {
			out = append(out, dropIndexSQL(k, safe))
		}
	}
	for _, k := range sortedKeys(cur.Indexes) {
		curIdx := cur.Indexes[k]
		prevIdx, present := prev.Indexes[k]
		if !present {
			out = append(out, createIndexSQL(cur.Name, curIdx, safe))
			continue
		}
		if !indexEqual(prevIdx, curIdx) {
			out = append(out, createIndexSQL(cur.Name, curIdx, safe))
		}
	}
	return out
}

// diffCompositePKs emits ALTER TABLE ADD/DROP PRIMARY KEY.
// Single-column PKs continue to live on the column definition
// and are handled by the column diff.
func diffCompositePKs(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.CompositePrimaryKeys) {
		if _, ok := cur.CompositePrimaryKeys[k]; !ok {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	for _, k := range sortedKeys(cur.CompositePrimaryKeys) {
		if _, ok := prev.CompositePrimaryKeys[k]; ok {
			continue
		}
		pk := cur.CompositePrimaryKeys[k]
		cols := strings.Join(quoteIdents(pk.Columns), ", ")
		out = append(out, fmt.Sprintf(
			`ALTER TABLE "%s" ADD CONSTRAINT "%s" PRIMARY KEY (%s);`,
			cur.Name, pk.Name, cols))
	}
	return out
}

// diffChecks emits ALTER TABLE ADD/DROP CONSTRAINT for CHECK
// constraints.
func diffChecks(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.CheckConstraints) {
		if _, ok := cur.CheckConstraints[k]; !ok {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	for _, k := range sortedKeys(cur.CheckConstraints) {
		if _, ok := prev.CheckConstraints[k]; ok {
			continue
		}
		c := cur.CheckConstraints[k]
		out = append(out, fmt.Sprintf(
			`ALTER TABLE "%s" ADD CONSTRAINT "%s" CHECK (%s);`,
			cur.Name, c.Name, c.Value))
	}
	return out
}

// createIndexSQL renders a CREATE INDEX statement from a snapshot.
func createIndexSQL(table string, idx *IndexSnapshot, safe bool) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if idx.IsUnique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	if idx.Concurrently {
		b.WriteString("CONCURRENTLY ")
	}
	if safe {
		b.WriteString("IF NOT EXISTS ")
	}
	fmt.Fprintf(&b, `"%s" ON "%s"`, idx.Name, table)
	if idx.Method != "" && idx.Method != "btree" {
		fmt.Fprintf(&b, " USING %s", idx.Method)
	}
	b.WriteString(" (")
	b.WriteString(strings.Join(quoteIdents(idx.Columns), ", "))
	b.WriteByte(')')
	if idx.Where != "" {
		fmt.Fprintf(&b, " WHERE %s", idx.Where)
	}
	b.WriteByte(';')
	return b.String()
}

// dropIndexSQL renders DROP INDEX [IF EXISTS] "name".
func dropIndexSQL(name string, safe bool) string {
	if safe {
		return fmt.Sprintf(`DROP INDEX IF EXISTS "%s";`, name)
	}
	return fmt.Sprintf(`DROP INDEX "%s";`, name)
}

// indexEqual reports whether two index snapshots describe the
// same logical index.
func indexEqual(a, b *IndexSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.IsUnique != b.IsUnique || a.Method != b.Method || a.Where != b.Where || a.Concurrently != b.Concurrently {
		return false
	}
	if len(a.Columns) != len(b.Columns) {
		return false
	}
	for i := range a.Columns {
		if a.Columns[i] != b.Columns[i] {
			return false
		}
	}
	return true
}

func sameStringPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
