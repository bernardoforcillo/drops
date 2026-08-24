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

	// Renames names the objects that changed name rather than being
	// dropped and re-added. Diff cannot work this out — see rename.go —
	// so an unstated rename comes out as a DROP COLUMN and an ADD
	// COLUMN, which on a server with no transactional DDL is data gone
	// with nothing to roll back. Each entry turns its pair into a
	// rename, and the diff that follows is computed as if the rename had
	// already happened.
	//
	// Diff trusts what it is given: a rename naming an object that is
	// not there is emitted anyway and fails at the server.
	// GenerateMigration checks first.
	Renames []Rename
}

// DiffDown returns the SQL that reverses the migration from cur back to
// prev — applying it after the corresponding Diff(prev, cur) restores
// the original schema, as far as anything can: a dropped column's data
// is gone, and re-adding the column brings back nothing.
//
// DiffOptions.Renames is inverted along with the arguments, so a
// migration that renames "email" to "emailAddress" rolls back by
// renaming it the other way rather than by dropping it.
func DiffDown(prev, cur *Snapshot, opts ...DiffOptions) []string {
	if len(opts) == 0 {
		return Diff(cur, prev)
	}
	down := opts[0]
	down.Renames = invertRenames(down.Renames)
	return Diff(cur, prev, down)
}

// Diff returns the ordered list of SQL statements that evolve a
// database from prev's schema to cur's. Output is deterministic for a
// given (prev, cur, opts): keys are walked in sorted order, so
// re-running against the same input produces byte-identical SQL.
//
// Operation order:
//
//  1. the renames DiffOptions.Renames states, tables before columns, so
//     everything below is computed against a previous schema in which
//     they have already run;
//  2. DROP FOREIGN KEY for every key that is going — whether because
//     the schema stopped declaring it, or because a table it names is
//     about to go;
//  3. DROP TABLE for tables removed entirely;
//  4. CREATE TABLE for new tables, carrying their columns and PRIMARY
//     KEY only;
//  5. DROP INDEX, DROP CONSTRAINT and DROP PRIMARY KEY for everything
//     else on a surviving table that names a column and is going;
//  6. the column changes — DROP COLUMN, ADD COLUMN, MODIFY COLUMN —
//     once nothing names the columns leaving;
//  7. ADD PRIMARY KEY, CREATE INDEX and ADD CONSTRAINT … CHECK, once
//     the columns they span are there;
//  8. ADD CONSTRAINT … FOREIGN KEY last, once every table, column and
//     key they name exists.
//
// # What MySQL does to a dependent when its column goes
//
// Steps 2, 5 and 6 are one rule, and it is not PostgreSQL's. There, a
// dependent goes whole whenever any column it names goes, so the only
// hazard is a statement that names it afterwards. MySQL answers
// differently for each kind, and the answers were read off a live
// server rather than off the manual:
//
//   - a secondary index over the dropped column alone is removed with
//     the column, so a DROP INDEX afterwards names nothing (1091);
//   - a secondary index over several columns is narrowed to the ones
//     that remain, and stays. Nothing was removed, so the DROP INDEX
//     is not stale — it is the only thing that gets rid of the index.
//     This is why the drops are emitted rather than suppressed on the
//     grounds that the column drop took them: suppressing here would
//     leave a narrowed index standing and the next push asking for the
//     same drop for ever;
//   - a UNIQUE key over the dropped column alone is removed with it,
//     but MariaDB will not narrow one spanning several columns and
//     refuses the column drop outright (1072);
//   - a CHECK naming only the dropped column is removed with it on
//     MariaDB and refused on MySQL 8.0.16+ (3959); one naming a
//     surviving column too is refused on both (1054);
//   - a column on either side of a foreign key cannot be dropped while
//     the key stands, and neither can the index the key needs (1553);
//   - a PRIMARY KEY over the dropped column alone goes with it, and a
//     composite one is refused (1072).
//
// Every one of them points the same way — the dependent goes before
// the column — so that is the order, and the differences show up only
// in which error the wrong order produces. The foreign keys go ahead
// of the rest because they are the one kind that reaches across
// tables: it is dropping the key first that lets the UNIQUE or the
// PRIMARY KEY it pointed at be dropped at all.
//
// The one hazard ordering does not reach is a PRIMARY KEY covering an
// AUTO_INCREMENT column: DROP PRIMARY KEY on its own is 1075 whatever
// order it comes in. See dropPrimaryKey.
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
	// The renames go in front, and everything below is computed against
	// a previous schema in which they have already run — so a rename is
	// a rename and not a drop and an add.
	renames := renameStatements(prev, opt.Renames, opt)
	prev = applyRenames(prev, opt.Renames)
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
	// irrelevant, cycles included. The per-table pass just after would
	// have dropped the ones on surviving tables anyway, so it is told
	// which are already gone.
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
	// The rest of the keys that are going, still ahead of everything
	// else: a key is the one dependent that reaches across tables, and
	// while it stands neither of the two columns it joins can be
	// dropped, nor the index on the referenced side (1553).
	for _, key := range sortedKeys(cur.Tables) {
		out = append(out, dropForeignKeys(prevTable(prev, key), cur.Tables[key], preDropped)...)
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

	// What is left that names a column, before the columns go. A table
	// this migration creates has nothing on its old side, so only the
	// tables both snapshots have can contribute a drop.
	surviving := survivingTables(prev, cur)
	for _, key := range surviving {
		prevT, curT := prev.Tables[key], cur.Tables[key]
		out = append(out, dropIndexes(prevT, curT)...)
		out = append(out, dropChecks(prevT, curT, opt)...)
		out = append(out, dropPrimaryKey(prevT, curT)...)
	}
	// The columns themselves, batched per table: the drops the pass
	// above cleared the way for, and the adds and restatements, which
	// are in the same ALTER TABLE because splitting them would copy
	// the table twice. See Batching.
	for _, key := range surviving {
		out = append(out, diffColumns(prev.Tables[key], cur.Tables[key], opt)...)
	}

	// Everything the columns now support. A table cur creates already
	// carries its columns and its PRIMARY KEY from the CREATE TABLE;
	// its indexes and CHECK constraints are still to come.
	for _, key := range surviving {
		out = append(out, addPrimaryKey(prev.Tables[key], cur.Tables[key])...)
	}
	for _, key := range sortedKeys(cur.Tables) {
		prevT, curT := prevTable(prev, key), cur.Tables[key]
		out = append(out, addIndexes(prevT, curT)...)
		out = append(out, addChecks(prevT, curT)...)
	}
	// Foreign keys after the CREATE TABLEs so cross-table references
	// resolve, and after the indexes because a key can only point at a
	// column an index covers.
	for _, key := range sortedKeys(cur.Tables) {
		out = append(out, addForeignKeys(prevTable(prev, key), cur.Tables[key])...)
	}
	return append(renames, out...)
}

// prevTable returns the table's previous shape, or an empty one when
// this migration creates it. Every pass below is written against two
// sides; handing a new table an empty old side keeps each pass from
// needing its own special case.
func prevTable(prev *Snapshot, name string) *TableSnapshot {
	if t, ok := prev.Tables[name]; ok {
		return t
	}
	return newTableSnapshot(name)
}

// survivingTables lists, in sorted order, the tables both snapshots
// have — the ones a column diff is about.
func survivingTables(prev, cur *Snapshot) []string {
	var out []string
	for _, key := range sortedKeys(cur.Tables) {
		if _, ok := prev.Tables[key]; ok {
			out = append(out, key)
		}
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

// dropPrimaryKey emits DROP PRIMARY KEY when the key is going or is
// about to be restated, and addPrimaryKey is its other half. The two
// are emitted a long way apart — the drop with the other dependents,
// the add once the columns are in their final shape — because a
// primary key over a column that is leaving refuses to be narrowed:
// MariaDB answers 1072 to a DROP COLUMN naming one column of a
// composite key, and takes a single-column key away with its column,
// which makes the DROP PRIMARY KEY that came afterwards a 1091.
//
// The two sides are matched by their column list, never by name: MySQL
// calls every primary key PRIMARY, so the name carries no information
// and comparing it would always agree.
//
// Dropping one is the sharpest edge in this file. A table whose primary
// key covers an AUTO_INCREMENT column cannot have it dropped — error
// 1075, "there can be only one auto column and it must be defined as a
// key" — and the ADD that would have restored it never runs, because
// the DROP failed and there is no transaction. Ordering does not reach
// that one: the statement fails wherever it is put, and it is what the
// safety analyser flags.
func dropPrimaryKey(prev, cur *TableSnapshot) []string {
	if prev.PrimaryKey == nil || primaryKeyEqual(prev.PrimaryKey, cur.PrimaryKey) {
		return nil
	}
	return []string{fmt.Sprintf("ALTER TABLE %s DROP PRIMARY KEY;", quoteIdent(cur.Name))}
}

// addPrimaryKey emits ADD PRIMARY KEY for a key cur declares and prev
// did not have in that shape.
func addPrimaryKey(prev, cur *TableSnapshot) []string {
	if cur.PrimaryKey == nil || primaryKeyEqual(prev.PrimaryKey, cur.PrimaryKey) {
		return nil
	}
	return []string{addPrimaryKeySQL(cur.Name, cur.PrimaryKey)}
}

func primaryKeyEqual(a, b *PrimaryKeySnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return sameStrings(a.Columns, b.Columns) && sameInts(a.Prefixes, b.Prefixes)
}

func addPrimaryKeySQL(table string, pk *PrimaryKeySnapshot) string {
	return fmt.Sprintf("ALTER TABLE %s ADD PRIMARY KEY (%s);",
		quoteIdent(table), keyPartList(pk.Columns, pk.Prefixes))
}

// dropIndexes emits DROP INDEX for every index cur no longer declares
// and every one whose shape changed — an index is never altered in
// place, so any structural change drops and recreates it.
//
// The drop is emitted even for an index whose every column is about to
// go. On MariaDB such an index is sometimes removed with the column
// and sometimes only narrowed, and the two cases are not distinguished
// here: a drop naming an index the server already took is a 1091, but
// it never comes to that, because the drop is emitted first. See Diff.
func dropIndexes(prev, cur *TableSnapshot) []string {
	var out []string
	for _, k := range sortedKeys(prev.Indexes) {
		curIdx, present := cur.Indexes[k]
		if !present || !indexEqual(prev.Indexes[k], curIdx) {
			out = append(out, dropIndexSQL(cur.Name, k))
		}
	}
	return out
}

// addIndexes emits CREATE INDEX for every index cur newly declares or
// restates.
func addIndexes(prev, cur *TableSnapshot) []string {
	var out []string
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

// dropChecks emits DROP CONSTRAINT for every CHECK cur no longer
// declares, and for every one whose expression changed under an
// unchanged name: neither server can alter one in place, so a changed
// constraint is dropped and re-added.
//
// The comparison is textual, so both sides have to be spelled alike.
// Against a live server that is Push's job — see probeCheckExpressions
// — not Diff's.
func dropChecks(prev, cur *TableSnapshot, opt DiffOptions) []string {
	var out []string
	for _, k := range sortedKeys(prev.CheckConstraints) {
		curC, ok := cur.CheckConstraints[k]
		if !ok || curC.Value != prev.CheckConstraints[k].Value {
			out = append(out, dropCheckSQL(cur.Name, k, opt.Server))
		}
	}
	return out
}

// addChecks is dropChecks's other half, emitted once the columns the
// expressions name are in place.
func addChecks(prev, cur *TableSnapshot) []string {
	var out []string
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

// dropForeignKeys emits DROP FOREIGN KEY for one table's keys that are
// going or being restated. alreadyDropped names the constraints Diff
// has already emitted a drop for, because they pointed at a table that
// is going.
func dropForeignKeys(prev, cur *TableSnapshot, alreadyDropped map[string]bool) []string {
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
	return out
}

// addForeignKeys is dropForeignKeys's other half, emitted last of all
// so every table, column and key a constraint names already exists.
func addForeignKeys(prev, cur *TableSnapshot) []string {
	var out []string
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
