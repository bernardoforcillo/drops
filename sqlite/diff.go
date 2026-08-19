package sqlite

import (
	"fmt"
	"reflect"
	"strings"
)

// DiffDown returns the SQL that reverses the migration from cur back to
// prev — applying these statements after the corresponding Diff(prev,
// cur) restores the original schema. It is simply Diff(cur, prev).
func DiffDown(prev, cur *Snapshot) []string {
	return Diff(cur, prev)
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
//  1. DROP TABLE   for tables removed entirely
//  2. CREATE TABLE for new tables (all constraints inline)
//  3. per surviving table: ADD COLUMN statements, or a rebuild sequence
func Diff(prev, cur *Snapshot) []string {
	if prev == nil {
		prev = EmptySnapshot()
	}
	if cur == nil {
		cur = EmptySnapshot()
	}
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
	return out
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
		!sameConstraintMap(prev.UniqueConstraints, cur.UniqueConstraints) ||
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

// rebuildTable emits the four-step SQLite table rebuild preceded by a
// comment. The new table is built with cur's full shape; the shared
// columns (present in both prev and cur, sorted) are copied across.
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
	return []string{
		fmt.Sprintf("-- rebuild %s: %s", quoteIdent(cur.Name), reason),
		createTableSQL(cur, newName),
		insert,
		fmt.Sprintf("DROP TABLE %s;", quoteIdent(cur.Name)),
		fmt.Sprintf("ALTER TABLE %s RENAME TO %s;", quoteIdent(newName), quoteIdent(cur.Name)),
	}
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
func columnEqual(a, b *ColumnSnapshot) bool {
	if a.Type != b.Type || a.PrimaryKey != b.PrimaryKey || a.NotNull != b.NotNull ||
		a.Unique != b.Unique || a.AutoIncrement != b.AutoIncrement {
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
