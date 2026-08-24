package sqlite

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// DiffOptions tunes how Diff renders statements.
type DiffOptions struct {
	// Renames names the objects that changed name rather than being
	// dropped and re-added. Diff cannot work this out — see rename.go —
	// so an unstated rename comes out as a rebuild that copies every
	// column except the one that mattered. Each entry turns its pair
	// into an ALTER TABLE ... RENAME, and the diff that follows is
	// computed as if the rename had already happened.
	//
	// Diff trusts what it is given: a rename naming an object that is
	// not there is emitted anyway and fails at the server.
	// GenerateMigration checks first.
	Renames []Rename
}

// DiffDown returns the SQL that reverses the migration from cur back to
// prev — applying these statements after the corresponding Diff(prev,
// cur) restores the original schema. It is simply Diff(cur, prev), with
// DiffOptions.Renames inverted along with the arguments so a migration
// that renames a column rolls back by renaming it the other way.
func DiffDown(prev, cur *Snapshot, opts ...DiffOptions) []string {
	if len(opts) == 0 {
		return Diff(cur, prev)
	}
	down := opts[0]
	down.Renames = invertRenames(down.Renames)
	return Diff(cur, prev, down)
}

// Diff returns the ordered list of SQL statements (and inline comments)
// that evolve a SQLite database from prev's schema to cur's. Output is
// deterministic for a given (prev, cur): every map is walked in sorted
// key order.
//
// SQLite has no ALTER TABLE ADD CONSTRAINT and no ALTER COLUMN, so the
// rules differ sharply from drops/pg:
//
//   - New table → a full CREATE TABLE with every constraint (composite
//     PK, UNIQUE, CHECK, single- and multi-column FK) rendered INLINE.
//   - Dropped table → DROP TABLE.
//   - Added column that is nullable or has a default (and is not
//     PRIMARY KEY / UNIQUE / a foreign key) → ALTER TABLE ADD COLUMN,
//     which SQLite does support.
//   - Dropped column, a column whose type / NOT NULL / default / PK /
//     autoincrement changed, an added column that ALTER cannot add, or
//     any table-level constraint change → the standard SQLite
//     table-rebuild sequence: create "t_new" with the new shape, copy
//     the shared columns across with INSERT … SELECT, DROP the old
//     table and RENAME "t_new" into place. The sequence is prefixed by
//     a `-- rebuild "t": <reason>` comment.
//
// Operation order:
//  1. ALTER TABLE ... RENAME, for the renames DiffOptions.Renames states
//  2. DROP TABLE   for tables removed entirely
//  3. CREATE TABLE for new tables (all constraints inline)
//  4. per surviving table: ADD COLUMN statements, or a rebuild sequence
//
// Indexes and triggers are preserved, never diffed. Diff emits no
// CREATE INDEX, no DROP INDEX and no CREATE TRIGGER of its own: the Go
// schema DSL has no way to declare either, so every index in a database
// is by definition one drops was not told about, and treating those as
// drift would drop the index the application's hot query depends on.
// They appear in the output only where a rebuild has just destroyed
// them, put back as they were — see replayObjectsSQL.
//
// That replay reaches only as far as prev does. Diff does not read the
// database; it replays what prev recorded, and only Introspect records
// an index or a trigger — BuildSnapshot cannot, having nothing to build
// them from. So Push and DetectDrift, which diff against a live
// introspection, come through a rebuild with their indexes, and
// GenerateMigration, which diffs two snapshot files, does not. It says
// so in the migration it writes; see noteBlindRebuilds.
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
	// The renames go in front of everything, rebuilds included, and
	// everything below is computed against a previous schema in which
	// they have already run.
	renames := renameStatements(opt.Renames)
	prev = applyRenames(prev, opt.Renames)
	var out []string

	// 1. Dropped tables.
	for _, key := range sortedMapKeys(prev.Tables) {
		if _, ok := cur.Tables[key]; !ok {
			out = append(out, fmt.Sprintf("DROP TABLE %s;", quoteIdent(prev.Tables[key].Name)))
		}
	}
	// 2. New tables — full CREATE TABLE with inline constraints.
	for _, key := range sortedMapKeys(cur.Tables) {
		if _, ok := prev.Tables[key]; !ok {
			out = append(out, createTableSQL(cur.Tables[key], cur.Tables[key].Name))
		}
	}
	// 3. Surviving tables — add-column or rebuild.
	for _, key := range sortedMapKeys(cur.Tables) {
		prevT, ok := prev.Tables[key]
		if !ok {
			continue
		}
		out = append(out, diffTable(prevT, cur.Tables[key])...)
	}
	return append(renames, out...)
}

// diffTable produces the statements for a table present in both prev and
// cur — either a set of ALTER TABLE ADD COLUMN statements, or the full
// table-rebuild sequence when SQLite cannot apply the change in place.
func diffTable(prev, cur *TableSnapshot) []string {
	var dropped, changed, addedAddable, addedNotAddable []string

	for _, k := range sortedMapKeys(prev.Columns) {
		if _, ok := cur.Columns[k]; !ok {
			dropped = append(dropped, k)
		}
	}
	// Which columns in cur are targets of a single-column FK (they can't
	// be added via ALTER, forcing a rebuild).
	fkCols := map[string]bool{}
	for _, fk := range cur.ForeignKeys {
		if len(fk.ColumnsFrom) == 1 {
			fkCols[fk.ColumnsFrom[0]] = true
		}
	}
	for _, k := range sortedMapKeys(cur.Columns) {
		curC := cur.Columns[k]
		prevC, ok := prev.Columns[k]
		if !ok {
			if columnAddable(curC, fkCols[k]) {
				addedAddable = append(addedAddable, k)
			} else {
				addedNotAddable = append(addedNotAddable, k)
			}
			continue
		}
		if !columnEqual(prevC, curC) {
			changed = append(changed, k)
		}
	}

	constraintsDiffer := !sameConstraintMap(prev.CompositePrimaryKeys, cur.CompositePrimaryKeys) ||
		!sameUniqueKeys(prev, cur) ||
		!sameConstraintMap(prev.CheckConstraints, cur.CheckConstraints) ||
		!sameConstraintMap(prev.ForeignKeys, cur.ForeignKeys)

	needsRebuild := len(dropped) > 0 || len(changed) > 0 ||
		len(addedNotAddable) > 0 || constraintsDiffer

	if !needsRebuild {
		var out []string
		for _, k := range addedAddable {
			out = append(out, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s;",
				quoteIdent(cur.Name), addColumnDefSQL(cur.Columns[k])))
		}
		return out
	}
	return rebuildTable(prev, cur, rebuildReason(prev, cur, dropped, changed, addedNotAddable, constraintsDiffer))
}

// rebuildReason builds a short, deterministic explanation for the
// rebuild comment.
func rebuildReason(prev, cur *TableSnapshot, dropped, changed, addedNotAddable []string, constraintsDiffer bool) string {
	var parts []string
	for _, k := range dropped {
		parts = append(parts, fmt.Sprintf("drop column %s", quoteIdent(k)))
	}
	for _, k := range changed {
		p, c := prev.Columns[k], cur.Columns[k]
		if p.Type != c.Type {
			parts = append(parts, fmt.Sprintf("change column %s type %s -> %s", quoteIdent(k), p.Type, c.Type))
		} else {
			parts = append(parts, fmt.Sprintf("change column %s", quoteIdent(k)))
		}
	}
	for _, k := range addedNotAddable {
		parts = append(parts, fmt.Sprintf("add column %s (not ALTER-addable)", quoteIdent(k)))
	}
	if constraintsDiffer {
		parts = append(parts, "constraint change")
	}
	return strings.Join(parts, "; ")
}

// rebuildTable emits the SQLite table rebuild preceded by a comment:
// create "t_new" with cur's shape, copy the shared columns (present in
// both prev and cur, sorted) across, DROP the old table, RENAME the new
// one into place — and then put back the indexes and triggers the DROP
// destroyed. See replayObjectsSQL for what survives that and what does
// not.
func rebuildTable(prev, cur *TableSnapshot, reason string) []string {
	newName := cur.Name + "_new"
	var shared []string
	for _, k := range sortedMapKeys(cur.Columns) {
		if _, ok := prev.Columns[k]; ok {
			shared = append(shared, k)
		}
	}
	cols := strings.Join(quoteIdentList(shared), ", ")
	insert := fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s;",
		quoteIdent(newName), cols, cols, quoteIdent(cur.Name))
	out := []string{
		fmt.Sprintf("-- rebuild %s: %s", quoteIdent(cur.Name), reason),
		createTableSQL(cur, newName),
		insert,
		fmt.Sprintf("DROP TABLE %s;", quoteIdent(cur.Name)),
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", quoteIdent(newName), quoteIdent(cur.Name)),
	}
	return append(out, replayObjectsSQL(prev, cur)...)
}

// replayObjectsSQL re-creates the indexes and triggers that the
// rebuild's DROP TABLE destroys, from the DDL SQLite stored for them.
// Everything it emits runs after the RENAME, when the names are free
// again.
//
// A rebuild that only widens a table gets all of them back untouched.
// A rebuild that removes a column cannot, and the three ways that goes
// are deliberately different:
//
//   - An index that keys a removed column is NOT re-created, and a
//     comment in its place records that. Replaying it would abort the
//     migration on "no such column", which would make dropping a column
//     impossible while any index mentions it; PostgreSQL drops the
//     dependent index along with the column, and so does this.
//
//   - An index that reaches the removed column some other way — a
//     partial index's WHERE, an expression key — is replayed as stored,
//     because PRAGMA index_info does not report what those name and
//     drops has no way to know. SQLite resolves the names when the
//     CREATE INDEX runs and rejects it, so the migration fails loudly
//     and rolls back. That is the intended outcome: drops will not
//     silently discard an index it cannot reason about.
//
//   - A trigger is always replayed as stored. Its body is arbitrary SQL
//     and SQLite does not resolve the names in it at CREATE TRIGGER
//     time, so a trigger naming a removed column is accepted here and
//     fails the first time it fires. drops cannot prevent that without
//     parsing SQL, so it warns instead: a trigger whose text mentions a
//     removed column gets a comment above it.
func replayObjectsSQL(prev, cur *TableSnapshot) []string {
	var dropped []string
	for _, k := range sortedMapKeys(prev.Columns) {
		if _, ok := cur.Columns[k]; !ok {
			dropped = append(dropped, k)
		}
	}
	goneCol := map[string]bool{}
	for _, k := range dropped {
		goneCol[k] = true
	}

	var out []string
	for _, name := range sortedMapKeys(prev.Indexes) {
		idx := prev.Indexes[name]
		if idx.SQL == "" {
			continue
		}
		var lost []string
		for _, c := range idx.Columns {
			if goneCol[c] {
				lost = append(lost, quoteIdent(c))
			}
		}
		if len(lost) > 0 {
			out = append(out, fmt.Sprintf("-- index %s dropped with column %s",
				quoteIdent(name), strings.Join(lost, ", ")))
			continue
		}
		out = append(out, idx.SQL+";")
	}
	for _, name := range sortedMapKeys(prev.Triggers) {
		trg := prev.Triggers[name]
		if trg.SQL == "" {
			continue
		}
		if named := identsMentioned(trg.SQL, dropped); len(named) > 0 {
			out = append(out, fmt.Sprintf("-- trigger %s names dropped column %s; it will fail when it fires",
				quoteIdent(name), strings.Join(quoteIdentList(named), ", ")))
		}
		out = append(out, trg.SQL+";")
	}
	return out
}

// identsMentioned returns the entries of names that appear in sql as
// whole identifiers.
//
// This is a text scan, not a parse: it sees a column name inside a
// string literal or a comment, and it cannot tell one table's "id" from
// another's. That is acceptable because the only thing it drives is the
// wording of a warning comment — a false positive costs a line of
// noise, where missing a real one costs a trigger that fails at 3am.
func identsMentioned(sql string, names []string) []string {
	lower := strings.ToLower(sql)
	var out []string
	for _, n := range names {
		if n == "" {
			continue
		}
		target := strings.ToLower(n)
		for i := 0; ; {
			j := strings.Index(lower[i:], target)
			if j < 0 {
				break
			}
			at := i + j
			end := at + len(target)
			if !isIdentByte(byteAt(lower, at-1)) && !isIdentByte(byteAt(lower, end)) {
				out = append(out, n)
				break
			}
			i = at + 1
		}
	}
	return out
}

func byteAt(s string, i int) byte {
	if i < 0 || i >= len(s) {
		return ' '
	}
	return s[i]
}

func isIdentByte(b byte) bool {
	return b == '_' || b == '$' ||
		(b >= '0' && b <= '9') ||
		(b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z')
}

// columnAddable reports whether an added column can be introduced with
// ALTER TABLE ADD COLUMN. SQLite forbids adding a PRIMARY KEY or UNIQUE
// column, a NOT NULL column without a default, a column whose default is
// not a constant, or (here, conservatively) a column that is the source
// of a foreign key.
func columnAddable(c *ColumnSnapshot, hasFK bool) bool {
	if c.PrimaryKey || c.Unique || hasFK {
		return false
	}
	if c.NotNull && c.Default == nil {
		return false
	}
	if c.Default != nil && !constantDefault(*c.Default) {
		return false
	}
	return true
}

// constantDefault reports whether d is a default SQLite will accept on
// ALTER TABLE ADD COLUMN.
//
// The value has to be a constant there: CURRENT_TIME, CURRENT_DATE,
// CURRENT_TIMESTAMP and any parenthesised expression are rejected with
// "Cannot add a column with non-constant default". A CREATE TABLE takes
// all of them, so a column carrying one reaches the table through a
// rebuild instead. This is the same shape AnalyzeMigration's
// add-column-dynamic-default rule warns about after the fact.
func constantDefault(d string) bool {
	t := strings.TrimSpace(d)
	if strings.HasPrefix(t, "(") {
		return false
	}
	switch strings.ToUpper(t) {
	case "CURRENT_TIME", "CURRENT_DATE", "CURRENT_TIMESTAMP":
		return false
	}
	return true
}

// columnEqual reports whether two column snapshots describe the same
// column shape (any difference forces a table rebuild).
//
// Unique is not compared here — sameUniqueKeys compares it at the table
// level, for the reason spelled out there.
func columnEqual(a, b *ColumnSnapshot) bool {
	if a.Type != b.Type || a.PrimaryKey != b.PrimaryKey || a.NotNull != b.NotNull ||
		a.AutoIncrement != b.AutoIncrement {
		return false
	}
	switch {
	case a.Default == nil && b.Default == nil:
		return true
	case a.Default == nil || b.Default == nil:
		return false
	default:
		return *a.Default == *b.Default
	}
}

// createTableSQL renders a full CREATE TABLE for t under the given name,
// with every constraint inline — the SQLite form (there is no ALTER
// TABLE ADD CONSTRAINT). name lets a rebuild render the same shape under
// "t_new". Output mirrors ddl.go's createTable layout.
func createTableSQL(t *TableSnapshot, name string) string {
	var b strings.Builder
	b.WriteString("CREATE TABLE ")
	b.WriteString(quoteIdent(name))
	b.WriteString(" (\n  ")
	first := true
	sep := func() {
		if !first {
			b.WriteString(",\n  ")
		}
		first = false
	}
	singlePK := len(t.CompositePrimaryKeys) == 0
	// Single-column FKs render inline on their column.
	inlineFK := map[string]*ForeignKeySnapshot{}
	for _, fk := range t.ForeignKeys {
		if len(fk.ColumnsFrom) == 1 {
			inlineFK[fk.ColumnsFrom[0]] = fk
		}
	}
	for _, cn := range sortedMapKeys(t.Columns) {
		sep()
		writeColumnDefSQL(&b, t.Columns[cn], singlePK, inlineFK[cn])
	}
	// Composite PRIMARY KEY.
	for _, k := range sortedMapKeys(t.CompositePrimaryKeys) {
		sep()
		pk := t.CompositePrimaryKeys[k]
		b.WriteString("PRIMARY KEY (")
		b.WriteString(strings.Join(quoteIdentList(pk.Columns), ", "))
		b.WriteByte(')')
	}
	// Named composite UNIQUE constraints.
	for _, k := range sortedMapKeys(t.UniqueConstraints) {
		sep()
		u := t.UniqueConstraints[k]
		b.WriteString("CONSTRAINT ")
		b.WriteString(quoteIdent(u.Name))
		b.WriteString(" UNIQUE (")
		b.WriteString(strings.Join(quoteIdentList(u.Columns), ", "))
		b.WriteByte(')')
	}
	// CHECK constraints.
	for _, k := range sortedMapKeys(t.CheckConstraints) {
		sep()
		c := t.CheckConstraints[k]
		b.WriteString("CONSTRAINT ")
		b.WriteString(quoteIdent(c.Name))
		b.WriteString(" CHECK (")
		b.WriteString(c.Value)
		b.WriteByte(')')
	}
	// Composite (multi-column) FOREIGN KEY constraints.
	for _, k := range sortedMapKeys(t.ForeignKeys) {
		fk := t.ForeignKeys[k]
		if len(fk.ColumnsFrom) <= 1 {
			continue
		}
		sep()
		b.WriteString("CONSTRAINT ")
		b.WriteString(quoteIdent(fk.Name))
		b.WriteString(" FOREIGN KEY (")
		b.WriteString(strings.Join(quoteIdentList(fk.ColumnsFrom), ", "))
		b.WriteString(") REFERENCES ")
		b.WriteString(quoteIdent(fk.TableTo))
		b.WriteString(" (")
		b.WriteString(strings.Join(quoteIdentList(fk.ColumnsTo), ", "))
		b.WriteByte(')')
		writeRefActionsSQL(&b, fk.OnDelete, fk.OnUpdate)
	}
	b.WriteString("\n);")
	return b.String()
}

// writeColumnDefSQL renders one column definition inside CREATE TABLE.
// allowInlinePK attaches a single-column PRIMARY KEY to the column (it
// is false when the table has a composite PK). fk, if non-nil, is the
// single-column foreign key whose source is this column.
func writeColumnDefSQL(b *strings.Builder, c *ColumnSnapshot, allowInlinePK bool, fk *ForeignKeySnapshot) {
	b.WriteString(quoteIdent(c.Name))
	b.WriteByte(' ')
	b.WriteString(c.Type)
	if allowInlinePK && c.PrimaryKey {
		b.WriteString(" PRIMARY KEY")
		if c.AutoIncrement {
			b.WriteString(" AUTOINCREMENT")
		}
	}
	// A key column keeps its NOT NULL here for the same reason
	// CreateTable emits one: SQLite does not imply it, and a rebuild
	// that dropped it would both weaken the constraint and leave the
	// table diffing against its declaration on the next run — so the
	// migration would never converge.
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	if c.Unique && !c.PrimaryKey {
		b.WriteString(" UNIQUE")
	}
	if c.Default != nil {
		b.WriteString(" DEFAULT ")
		b.WriteString(*c.Default)
	}
	if fk != nil {
		b.WriteString(" REFERENCES ")
		b.WriteString(quoteIdent(fk.TableTo))
		b.WriteString(" (")
		b.WriteString(quoteIdent(fk.ColumnsTo[0]))
		b.WriteByte(')')
		writeRefActionsSQL(b, fk.OnDelete, fk.OnUpdate)
	}
}

// addColumnDefSQL renders the column definition used in ALTER TABLE ADD
// COLUMN. It never includes PRIMARY KEY / UNIQUE / FK clauses (SQLite
// rejects those on ADD COLUMN); columnAddable has already guaranteed the
// column is compatible.
func addColumnDefSQL(c *ColumnSnapshot) string {
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
	return b.String()
}

func writeRefActionsSQL(b *strings.Builder, onDelete, onUpdate string) {
	if onDelete != "" {
		b.WriteString(" ON DELETE ")
		b.WriteString(onDelete)
	}
	if onUpdate != "" {
		b.WriteString(" ON UPDATE ")
		b.WriteString(onUpdate)
	}
}

// quoteIdentList quotes each identifier in names.
func quoteIdentList(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return out
}

// sameUniqueKeys compares two tables' UNIQUE constraints as a set of
// column tuples, ignoring both the constraint names and which syntax
// declared them.
//
// SQLite stores neither. `email TEXT UNIQUE`, `UNIQUE (email)` and
// `CONSTRAINT c UNIQUE (email)` all leave exactly one anonymous unique
// index behind, which PRAGMA index_list reports under a generated name
// like "sqlite_autoindex_users_1" — so an introspected snapshot can
// never carry the declared name, and comparing names made every table
// with a UNIQUE constraint report a constraint change against its own
// declaration. On SQLite that means a full table rebuild, so such a
// table copied itself, and lost every index and trigger on it, on every
// single push. The column tuple is the only part of a UNIQUE constraint
// the engine actually remembers, so it is the only part worth comparing.
func sameUniqueKeys(a, b *TableSnapshot) bool {
	ak, bk := uniqueKeyTuples(a), uniqueKeyTuples(b)
	if len(ak) != len(bk) {
		return false
	}
	for i := range ak {
		if ak[i] != bk[i] {
			return false
		}
	}
	return true
}

// uniqueKeyTuples returns the table's unique column tuples in a
// canonical sorted form, drawn from both the inline column flag and the
// table-level constraints.
func uniqueKeyTuples(t *TableSnapshot) []string {
	var out []string
	for _, n := range sortedMapKeys(t.Columns) {
		if c := t.Columns[n]; c.Unique && !c.PrimaryKey {
			out = append(out, c.Name)
		}
	}
	for _, n := range sortedMapKeys(t.UniqueConstraints) {
		out = append(out, strings.Join(t.UniqueConstraints[n].Columns, "\x00"))
	}
	sort.Strings(out)
	return out
}

// sameConstraintMap compares two constraint sets, treating a nil map
// and an empty one as the same thing.
//
// reflect.DeepEqual does not: it reports a nil map and an empty map as
// different. Snapshots reach this function from two producers that
// disagree — BuildSnapshot initialises every map, Introspect leaves
// one nil when the table has no such constraint — so comparing them
// directly reported a constraint change for every table with no CHECK
// constraints. On SQLite a constraint change means a full table
// rebuild, so a schema that already matched its declaration would copy
// itself on every deploy.
func sameConstraintMap[V any](a, b map[string]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !reflect.DeepEqual(av, bv) {
			return false
		}
	}
	return true
}
