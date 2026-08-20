package mysql

import (
	"fmt"
	"strconv"
	"strings"
)

// DiffOptions tunes how Diff renders statements.
type DiffOptions struct {
	// Safe adds IF [NOT] EXISTS so a migration can be re-run without
	// erroring — as far as MySQL can express that, which is not far.
	//
	// CREATE TABLE IF NOT EXISTS and DROP TABLE IF EXISTS are the only
	// two forms both servers accept. MariaDB also takes IF [NOT]
	// EXISTS on ADD COLUMN, DROP COLUMN, CREATE INDEX and DROP INDEX;
	// MySQL takes none of them and answers a syntax error, so drops
	// targets the intersection and Safe leaves those statements
	// unchanged. A re-run of a migration that adds a column therefore
	// still fails on error 1060 — see Push, which computes the diff
	// from the live schema instead and so never emits one twice.
	Safe bool

	// Server is the server the SQL is destined for. It decides the
	// handful of comparisons that are not a property of the schema —
	// whether JSON and LONGTEXT are the same column, whether DROP
	// CONSTRAINT exists — and defaults to a MySQL of unknown version,
	// which is the conservative reading of both. Push fills it in from
	// the connection.
	Server ServerInfo

	// SplitAlters emits one ALTER TABLE per column change instead of
	// batching a table's changes into a single statement. See Diff for
	// what batching buys and what it costs.
	SplitAlters bool
}

// DiffDown returns the SQL that reverses the migration from cur back to
// prev — applying it after the corresponding Diff(prev, cur) restores
// the original schema, as far as anything can: a dropped column's data
// is gone, and re-adding the column brings back nothing.
func DiffDown(prev, cur *Snapshot, opts ...DiffOptions) []string {
	return Diff(cur, prev, opts...)
}

// Diff returns the ordered list of SQL statements that evolve a
// database from prev's schema to cur's. Output is deterministic for a
// given (prev, cur, opts): keys are walked in sorted order, so
// re-running against the same input produces byte-identical SQL.
//
// Operation order:
//
//  1. DROP FOREIGN KEY on any constraint pointing at a table about to
//     go, because InnoDB refuses to drop a table something references;
//  2. DROP TABLE for tables removed entirely;
//  3. CREATE TABLE for new tables, carrying their columns and PRIMARY
//     KEY only;
//  4. per-table column changes, then index and CHECK changes;
//  5. FOREIGN KEY changes last, once every table and column they name
//     exists.
//
// # Why the PRIMARY KEY is inline and nothing else is
//
// drops/pg emits a bare CREATE TABLE and every constraint after it, so
// each one stays independently diffable. Here the primary key has to
// go inline: an AUTO_INCREMENT column must be part of a key from the
// moment it exists, and a CREATE TABLE that defers the key is rejected
// with error 1075 rather than fixed by the ALTER that follows.
//
// # Batching
//
// A table's column changes are rendered as one ALTER TABLE with
// comma-separated actions unless SplitAlters says otherwise. This is
// not a tidiness preference: each ALTER TABLE that rebuilds a table
// copies every row, so three separate statements copy the table three
// times, and one statement copies it once.
//
// The cost is that the batch is all-or-nothing. One ALTER TABLE is
// atomic in itself — a batch that fails leaves none of its actions
// applied, which is more than can be said for the migration around it,
// since MySQL has no transactional DDL — but the error names the
// statement rather than the action inside it, and a batch of five that
// fails on the fourth tells you less than five statements would.
// SplitAlters trades the rewrites back for that.
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

	dropped := map[string]bool{}
	for _, key := range sortedKeys(prev.Tables) {
		if _, ok := cur.Tables[key]; !ok {
			dropped[key] = true
		}
	}
	// A foreign key pointing at a table that is going has to go first,
	// wherever it lives — including on another table that is also
	// going. InnoDB refuses to drop a referenced table (error 1451 on
	// MariaDB, 3730 on MySQL) and there is no CASCADE to lean on:
	// drops/pg writes DROP TABLE … CASCADE and lets the server work the
	// order out, while MySQL parses CASCADE on a DROP TABLE and ignores
	// it. Ordering the drops by dependency would do instead, until two
	// tables reference each other — which InnoDB permits. Clearing
	// every cross-reference first makes the order of the drops below
	// irrelevant, cycles included. The per-table pass further down
	// would have dropped the ones on surviving tables anyway, so it is
	// told which are already gone.
	preDropped := map[string]bool{}
	for _, key := range sortedKeys(prev.Tables) {
		pt := prev.Tables[key]
		for _, name := range sortedKeys(pt.ForeignKeys) {
			fk := pt.ForeignKeys[name]
			if dropped[fk.TableTo] {
				out = append(out, dropForeignKeySQL(pt.Name, name))
				preDropped[pt.Name+"."+name] = true
			}
		}
	}
	for _, key := range sortedKeys(prev.Tables) {
		if dropped[key] {
			out = append(out, dropTableSQL(prev.Tables[key], opt))
		}
	}
	for _, key := range sortedKeys(cur.Tables) {
		if _, ok := prev.Tables[key]; !ok {
			out = append(out, createTableSQL(cur.Tables[key], opt))
		}
	}

	for _, key := range sortedKeys(cur.Tables) {
		curT := cur.Tables[key]
		prevT, exists := prev.Tables[key]
		if !exists {
			// The CREATE TABLE above already carried this table's
			// columns and its primary key; only the objects it does
			// not carry are left to emit.
			prevT = newTableSnapshot(curT.Name)
		} else {
			out = append(out, diffColumns(prevT, curT, opt)...)
			out = append(out, diffPrimaryKey(prevT, curT)...)
		}
		out = append(out, diffIndexes(prevT, curT, opt)...)
		out = append(out, diffChecks(prevT, curT, opt)...)
	}
	for _, key := range sortedKeys(cur.Tables) {
		curT := cur.Tables[key]
		prevT, exists := prev.Tables[key]
		if !exists {
			prevT = newTableSnapshot(curT.Name)
		}
		out = append(out, diffForeignKeys(prevT, curT, preDropped)...)
	}
	return out
}

// ----------------------------------------------------------------------
// Table-level rendering
// ----------------------------------------------------------------------

// createTableSQL renders CREATE TABLE with the column definitions and
// the primary key. Indexes, foreign keys and CHECK constraints follow
// as their own statements.
func createTableSQL(t *TableSnapshot, opt DiffOptions) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	if opt.Safe {
		b.WriteString("IF NOT EXISTS ")
	}
	b.WriteString(quoteIdent(t.Name))
	b.WriteString(" (\n")
	for i, k := range sortedKeys(t.Columns) {
		if i > 0 {
			b.WriteString(",\n")
		}
		b.WriteByte('\t')
		b.WriteString(columnDefSQL(t.Columns[k]))
	}
	if t.PrimaryKey != nil && len(t.PrimaryKey.Columns) > 0 {
		b.WriteString(",\n\tPRIMARY KEY (")
		b.WriteString(keyPartList(t.PrimaryKey.Columns, t.PrimaryKey.Prefixes))
		b.WriteByte(')')
	}
	b.WriteString("\n)")
	if t.Engine != "" {
		b.WriteString(" ENGINE=")
		b.WriteString(t.Engine)
	}
	if t.Charset != "" {
		b.WriteString(" DEFAULT CHARSET=")
		b.WriteString(t.Charset)
	}
	if t.Collation != "" {
		b.WriteString(" COLLATE=")
		b.WriteString(t.Collation)
	}
	b.WriteByte(';')
	return b.String()
}

func dropTableSQL(t *TableSnapshot, opt DiffOptions) string {
	if opt.Safe {
		return fmt.Sprintf("DROP TABLE IF EXISTS %s;", quoteIdent(t.Name))
	}
	return fmt.Sprintf("DROP TABLE %s;", quoteIdent(t.Name))
}

// columnDefSQL renders one column definition. The clause order within
// the definition is the one MySQL's grammar requires, not a preference,
// and it matches writeColumnDef in ddl.go so the two render the same
// column.
//
// The columns themselves come out in name order rather than
// declaration order, because a snapshot keys them by name and the
// order is not in the file. A table built by a migration therefore has
// the same columns as one built by CreateTable but not in the same
// positions, which shows up in SELECT * and in an INSERT that names no
// columns. Diff never compares position, so this costs nothing in the
// diff — only in what the two tables look like side by side.
func columnDefSQL(c *ColumnSnapshot) string {
	var b strings.Builder
	b.WriteString(quoteIdent(c.Name))
	b.WriteByte(' ')
	b.WriteString(c.Type)
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	if c.Default != nil {
		b.WriteString(" DEFAULT ")
		b.WriteString(*c.Default)
	}
	if c.AutoIncrement {
		b.WriteString(" AUTO_INCREMENT")
	}
	if c.OnUpdate != "" {
		b.WriteString(" ON UPDATE ")
		b.WriteString(c.OnUpdate)
	}
	if c.Comment != "" {
		b.WriteString(" COMMENT '")
		b.WriteString(quoteLiteral(c.Comment))
		b.WriteByte('\'')
	}
	return b.String()
}

// keyPartList renders a key's columns with their prefix lengths, which
// a TEXT or BLOB column must carry or the server refuses the key with
// error 1170.
func keyPartList(columns []string, prefixes []int) string {
	parts := make([]string, len(columns))
	for i, c := range columns {
		parts[i] = quoteIdent(c)
		if n := intAt(prefixes, i); n > 0 {
			parts[i] += "(" + strconv.Itoa(n) + ")"
		}
	}
	return strings.Join(parts, ", ")
}

// ----------------------------------------------------------------------
// Columns
// ----------------------------------------------------------------------

// diffColumns emits the ALTER TABLE actions that bring prev's columns
// up to cur's, batched into one statement unless SplitAlters is set.
//
// A change confined to the DEFAULT is emitted as ALTER COLUMN … SET
// DEFAULT rather than MODIFY COLUMN, and the difference is not
// cosmetic: SET DEFAULT touches only the table definition, while
// MODIFY COLUMN restates the whole column and can rebuild the table to
// do it. MySQL has no narrower form for anything else — there is no
// "SET NOT NULL", no "SET DATA TYPE" — so every other change restates
// the column in full, which is why the snapshot has to carry the
// column's comment and ON UPDATE clause: leaving either out of the
// restatement would silently drop it.
func diffColumns(prev, cur *TableSnapshot, opt DiffOptions) []string {
	var actions []string
	for _, k := range sortedKeys(prev.Columns) {
		if _, ok := cur.Columns[k]; !ok {
			actions = append(actions, "DROP COLUMN "+quoteIdent(k))
		}
	}
	for _, k := range sortedKeys(cur.Columns) {
		if _, ok := prev.Columns[k]; ok {
			continue
		}
		actions = append(actions, "ADD COLUMN "+columnDefSQL(cur.Columns[k]))
	}
	for _, k := range sortedKeys(cur.Columns) {
		prevC, ok := prev.Columns[k]
		if !ok {
			continue
		}
		curC := cur.Columns[k]
		switch {
		case columnEqual(prevC, curC, opt.Server):
		case onlyDefaultDiffers(prevC, curC, opt.Server):
			if curC.Default == nil {
				actions = append(actions, "ALTER COLUMN "+quoteIdent(k)+" DROP DEFAULT")
			} else {
				actions = append(actions, "ALTER COLUMN "+quoteIdent(k)+" SET DEFAULT "+*curC.Default)
			}
		default:
			actions = append(actions, "MODIFY COLUMN "+columnDefSQL(curC))
		}
	}
	return alterStatements(cur.Name, actions, opt)
}

// alterStatements folds a table's actions into one ALTER TABLE, or one
// statement each when SplitAlters is set.
func alterStatements(table string, actions []string, opt DiffOptions) []string {
	if len(actions) == 0 {
		return nil
	}
	if opt.SplitAlters {
		out := make([]string, len(actions))
		for i, a := range actions {
			out[i] = fmt.Sprintf("ALTER TABLE %s %s;", quoteIdent(table), a)
		}
		return out
	}
	if len(actions) == 1 {
		return []string{fmt.Sprintf("ALTER TABLE %s %s;", quoteIdent(table), actions[0])}
	}
	return []string{fmt.Sprintf("ALTER TABLE %s\n\t%s;", quoteIdent(table), strings.Join(actions, ",\n\t"))}
}

func columnEqual(a, b *ColumnSnapshot, server ServerInfo) bool {
	return typeEqual(a.Type, b.Type, server) &&
		a.NotNull == b.NotNull &&
		a.AutoIncrement == b.AutoIncrement &&
		a.OnUpdate == b.OnUpdate &&
		a.Comment == b.Comment &&
		sameStringPtr(a.Default, b.Default)
}

func onlyDefaultDiffers(a, b *ColumnSnapshot, server ServerInfo) bool {
	return typeEqual(a.Type, b.Type, server) &&
		a.NotNull == b.NotNull &&
		a.AutoIncrement == b.AutoIncrement &&
		a.OnUpdate == b.OnUpdate &&
		a.Comment == b.Comment
}

// ----------------------------------------------------------------------
// Keys, indexes, checks and foreign keys
// ----------------------------------------------------------------------

// diffPrimaryKey emits DROP PRIMARY KEY / ADD PRIMARY KEY.
//
// The two sides are matched by their column list, never by name: MySQL
// calls every primary key PRIMARY, so the name carries no information
// and comparing it would always agree.
//
// Dropping one is the sharpest edge in this file. A table whose primary
// key covers an AUTO_INCREMENT column cannot have it dropped — error
// 1075, "there can be only one auto column and it must be defined as a
// key" — and the ADD that would have restored it never runs, because
// the DROP failed and there is no transaction. The safety analyser
// flags it.
func diffPrimaryKey(prev, cur *TableSnapshot) []string {
	prevPK, curPK := prev.PrimaryKey, cur.PrimaryKey
	switch {
	case prevPK == nil && curPK == nil:
		return nil
	case curPK == nil:
		return []string{fmt.Sprintf("ALTER TABLE %s DROP PRIMARY KEY;", quoteIdent(cur.Name))}
	case prevPK == nil:
		return []string{addPrimaryKeySQL(cur.Name, curPK)}
	case sameStrings(prevPK.Columns, curPK.Columns) && sameInts(prevPK.Prefixes, curPK.Prefixes):
		return nil
	}
	return []string{
		fmt.Sprintf("ALTER TABLE %s DROP PRIMARY KEY;", quoteIdent(cur.Name)),
		addPrimaryKeySQL(cur.Name, curPK),
	}
}

func addPrimaryKeySQL(table string, pk *PrimaryKeySnapshot) string {
	return fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s);",
		quoteIdent(table), keyPartList(pk.Columns, pk.Prefixes))
}

// diffIndexes emits CREATE INDEX / DROP INDEX. An index is never
// altered in place: any structural change drops and recreates it.
func diffIndexes(prev, cur *TableSnapshot, opt DiffOptions) []string {
	var out []string
	for _, k := range sortedKeys(prev.Indexes) {
		curIdx, present := cur.Indexes[k]
		if !present || !indexEqual(prev.Indexes[k], curIdx) {
			out = append(out, dropIndexSQL(cur.Name, k))
		}
	}
	for _, k := range sortedKeys(cur.Indexes) {
		curIdx := cur.Indexes[k]
		if len(curIdx.Columns) == 0 {
			// Every key part was one the snapshot cannot describe, so
			// there is nothing to put between the parentheses.
			// Rendering "ON `t` ()" is not a migration, it is a syntax
			// error that takes the rest of the file with it. Push
			// names the index it skipped in its notices.
			continue
		}
		if prevIdx, present := prev.Indexes[k]; present && indexEqual(prevIdx, curIdx) {
			continue
		}
		out = append(out, createIndexSQL(cur.Name, curIdx))
	}
	return out
}

func indexEqual(a, b *IndexSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.IsUnique == b.IsUnique &&
		indexMethodEqual(a.Method, b.Method) &&
		a.Comment == b.Comment &&
		sameStrings(a.Columns, b.Columns) &&
		sameInts(a.Prefixes, b.Prefixes)
}

// indexMethodEqual compares two access methods, treating an empty one
// as matching anything.
//
// An empty method is what normaliseIndexMethod folds BTREE to, and
// BTREE is what the catalogue reports for every index InnoDB builds —
// including one created with USING HASH, which InnoDB accepts without
// complaint and then ignores. Comparing the two as text would make a
// declared HASH index differ from the live BTREE one on every push,
// and since dropping and recreating it produces the same BTREE again,
// the difference would never close: an index rebuild on every push,
// for ever. So the engine's answer wins, and Push reports the
// discarded request as an "index-method-ignored" notice rather than
// acting on it.
func indexMethodEqual(a, b string) bool {
	return a == b || a == "" || b == ""
}

func createIndexSQL(table string, idx *IndexSnapshot) string {
	var b strings.Builder
	b.WriteString("CREATE ")
	if idx.IsUnique {
		b.WriteString("UNIQUE ")
	}
	b.WriteString("INDEX ")
	b.WriteString(quoteIdent(idx.Name))
	b.WriteString(" ON ")
	b.WriteString(quoteIdent(table))
	b.WriteString(" (")
	b.WriteString(keyPartList(idx.Columns, idx.Prefixes))
	b.WriteByte(')')
	if idx.Method != "" {
		b.WriteString(" USING ")
		b.WriteString(idx.Method)
	}
	if idx.Comment != "" {
		b.WriteString(" COMMENT '")
		b.WriteString(quoteLiteral(idx.Comment))
		b.WriteByte('\'')
	}
	b.WriteByte(';')
	return b.String()
}

// dropIndexSQL renders DROP INDEX … ON …. MySQL scopes an index name
// to its table rather than to the schema, so the table is part of the
// statement — unlike PostgreSQL, where the name is enough.
func dropIndexSQL(table, name string) string {
	return fmt.Sprintf("DROP INDEX %s ON %s;", quoteIdent(name), quoteIdent(table))
}

// diffChecks emits ADD / DROP CONSTRAINT for CHECK constraints. A
// constraint whose expression changed under an unchanged name is
// dropped and re-added: neither server can alter one in place.
//
// The comparison is textual, so both sides have to be spelled alike.
// Against a live server that is Push's job — see probeCheckExpressions
// — not Diff's.
func diffChecks(prev, cur *TableSnapshot, opt DiffOptions) []string {
	var out []string
	for _, k := range sortedKeys(prev.CheckConstraints) {
		curC, ok := cur.CheckConstraints[k]
		if !ok || curC.Value != prev.CheckConstraints[k].Value {
			out = append(out, dropCheckSQL(cur.Name, k, opt.Server))
		}
	}
	for _, k := range sortedKeys(cur.CheckConstraints) {
		if prevC, ok := prev.CheckConstraints[k]; ok && prevC.Value == cur.CheckConstraints[k].Value {
			continue
		}
		c := cur.CheckConstraints[k]
		out = append(out, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s);",
			quoteIdent(cur.Name), quoteIdent(c.Name), c.Value))
	}
	return out
}

// dropCheckSQL renders the portable spelling, DROP CONSTRAINT, and
// falls back to MySQL's older DROP CHECK on a server too old for it.
// MariaDB has never accepted DROP CHECK; MySQL only accepted it
// between 8.0.16 and 8.0.18.
func dropCheckSQL(table, name string, server ServerInfo) string {
	verb := "DROP CONSTRAINT"
	if !server.MariaDB && server.Major != 0 && !server.SupportsDropConstraint() {
		verb = "DROP CHECK"
	}
	return fmt.Sprintf("ALTER TABLE %s %s %s;", quoteIdent(table), verb, quoteIdent(name))
}

// diffForeignKeys emits the foreign-key changes for one table.
// alreadyDropped names the constraints Diff has already emitted a drop
// for, because they pointed at a table that is going.
func diffForeignKeys(prev, cur *TableSnapshot, alreadyDropped map[string]bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.ForeignKeys) {
		if alreadyDropped[cur.Name+"."+k] {
			continue
		}
		curFK, ok := cur.ForeignKeys[k]
		if !ok || !foreignKeyEqual(prev.ForeignKeys[k], curFK) {
			out = append(out, dropForeignKeySQL(cur.Name, k))
		}
	}
	for _, k := range sortedKeys(cur.ForeignKeys) {
		if prevFK, ok := prev.ForeignKeys[k]; ok && foreignKeyEqual(prevFK, cur.ForeignKeys[k]) {
			continue
		}
		out = append(out, addForeignKeySQL(cur.Name, cur.ForeignKeys[k]))
	}
	return out
}

func foreignKeyEqual(a, b *ForeignKeySnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.TableTo == b.TableTo &&
		a.OnDelete == b.OnDelete &&
		a.OnUpdate == b.OnUpdate &&
		sameStrings(a.ColumnsFrom, b.ColumnsFrom) &&
		sameStrings(a.ColumnsTo, b.ColumnsTo)
}

// addForeignKeySQL renders ADD CONSTRAINT … FOREIGN KEY, naming the
// referential actions only when they are not the default. Spelling out
// "ON DELETE NO ACTION" would be legal and would round-trip badly:
// MariaDB reports it back as RESTRICT, which is the same behaviour
// under a different name.
func addForeignKeySQL(table string, fk *ForeignKeySnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s (%s)",
		quoteIdent(table), quoteIdent(fk.Name),
		strings.Join(quoteIdents(fk.ColumnsFrom), ", "),
		quoteIdent(fk.TableTo),
		strings.Join(quoteIdents(fk.ColumnsTo), ", "))
	if a := normaliseAction(fk.OnDelete); a != "no action" {
		b.WriteString(" ON DELETE ")
		b.WriteString(strings.ToUpper(a))
	}
	if a := normaliseAction(fk.OnUpdate); a != "no action" {
		b.WriteString(" ON UPDATE ")
		b.WriteString(strings.ToUpper(a))
	}
	b.WriteByte(';')
	return b.String()
}

// dropForeignKeySQL renders DROP FOREIGN KEY, which both servers have
// accepted for ever — unlike DROP CONSTRAINT, which MySQL only learned
// in 8.0.19.
func dropForeignKeySQL(table, name string) string {
	return fmt.Sprintf("ALTER TABLE %s DROP FOREIGN KEY %s;", quoteIdent(table), quoteIdent(name))
}
