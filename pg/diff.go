package pg

import (
	"fmt"
	"sort"
	"strings"
)

// DiffOptions tunes how Diff renders statements.
type DiffOptions struct {
	// Safe wraps every destructive or creative DDL in IF [NOT] EXISTS so
	// the migration can be re-run without errors. ALTER COLUMN does not
	// have an IF EXISTS form in PostgreSQL, so it is emitted unchanged.
	Safe bool

	// Renames names the objects that changed name rather than being
	// dropped and re-added. Diff cannot work this out — see rename.go —
	// so an unstated rename comes out as a DROP COLUMN and an ADD
	// COLUMN, which is what the data loss looks like. Each entry turns
	// its pair into an ALTER TABLE ... RENAME, and the diff that follows
	// is computed as if the rename had already happened.
	//
	// Diff trusts what it is given: a rename naming an object that is
	// not there is emitted anyway and fails at the server.
	// GenerateMigration checks first.
	Renames []Rename
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

// Diff returns the ordered list of SQL statements that, applied in
// order, evolve a database from prev's schema to cur's. Output is
// deterministic for a given (prev, cur, opts) — keys are walked in
// sorted order — so re-running against the same input produces
// byte-identical SQL.
//
// Operation order — each step exists where it does because the step
// after it depends on the step before having run:
//
//  1. DROP VIEW / DROP MATERIALIZED VIEW for views removed entirely,
//     ahead of the table drops that would CASCADE them away first
//  2. CREATE TYPE, CREATE SEQUENCE for enums and sequences the schema
//     newly declares, ahead of the CREATE TABLE naming them
//  3. DROP TABLE   for tables removed entirely
//  4. CREATE TABLE for new tables (column defs only — every
//     composite key, UNIQUE, FOREIGN KEY and CHECK constraint is
//     emitted as a separate ALTER TABLE below, never inline)
//  5. ALTER TABLE  for column-level changes on surviving tables
//     (drop, add, type, NOT NULL, DEFAULT)
//  6. UNIQUE       constraint adds/drops on every table
//  7. FOREIGN KEY  adds/drops on every table — emitted after CREATE
//     TABLEs so cross-table references resolve
//  8. CREATE INDEX / DROP INDEX
//  9. DROP SEQUENCE, DROP TYPE, once the last table naming them is
//     gone
//  10. CREATE / CREATE OR REPLACE VIEW, once the tables a view selects
//     from are in their final shape
//  11. ROW LEVEL SECURITY and its policies, table by table
//
// ALTER TABLE ... RENAME comes in front of all of it, when
// DiffOptions.Renames says a rename is what happened; everything below
// is then computed against a previous schema in which the rename has
// already run, so a rename is a RENAME and not a drop and an add.
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
	renames := renameStatements(opt.Renames)
	prev = applyRenames(prev, opt.Renames)
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
			// New table: createTableSQL above emitted the bare
			// column definitions only. Every table-level
			// constraint — composite PK, UNIQUE and CHECK — is
			// emitted here as a separate ALTER TABLE statement so
			// the CREATE TABLE stays constraint-free. (Foreign
			// keys are handled in the dedicated pass below.)
			empty := &TableSnapshot{
				CompositePrimaryKeys: map[string]*CompositePKSnapshot{},
				UniqueConstraints:    map[string]*UniqueSnapshot{},
				CheckConstraints:     map[string]*CheckSnapshot{},
			}
			out = append(out, diffCompositePKs(empty, curT, opt.Safe)...)
			out = append(out, diffUniques(empty, curT, opt.Safe)...)
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

	// Top-level objects. A type or a sequence a column references has
	// to exist before the CREATE TABLE that names it, and cannot be
	// dropped until the last table naming it is gone — so the creates
	// go in front of the table DDL and the drops behind it. Emitting
	// the creates last, as this once did, meant a schema declaring a
	// single enum column produced a migration whose CREATE TABLE
	// named a type three statements before it existed.
	//
	// A view that is going away goes in front of everything: DROP
	// TABLE is emitted CASCADE, which takes every view selecting from
	// the table with it, and the DROP VIEW that followed then named an
	// object PostgreSQL had already removed (SQLSTATE 42P01), stopping
	// the migration halfway. Dropping the view first is always legal —
	// nothing depends on a view being there — and leaves the CASCADE
	// with nothing left to take.
	//
	// Views that survive are built last of all: a view selects from
	// the tables, so it can only be built once they are in their final
	// shape.
	head := diffViewsDrop(prev, cur, opt.Safe)
	head = append(head, diffEnumsCreate(prev, cur, opt.Safe)...)
	head = append(head, diffSequencesCreate(prev, cur, opt.Safe)...)
	out = append(head, out...)
	out = append(out, diffSequencesDrop(prev, cur, opt.Safe)...)
	out = append(out, diffEnumsDrop(prev, cur, opt.Safe)...)
	out = append(out, diffViewsCreate(prev, cur, opt.Safe, len(opt.Renames) > 0)...)

	// RLS + policies, table-scoped.
	for _, key := range sortedKeys(cur.Tables) {
		curT := cur.Tables[key]
		prevT, exists := prev.Tables[key]
		if !exists {
			prevT = &TableSnapshot{Policies: map[string]*PolicySnapshot{}}
		}
		out = append(out, diffRLS(prevT, curT)...)
		out = append(out, diffPolicies(prevT, curT, opt.Safe)...)
	}

	return append(renames, out...)
}

// diffEnumsDrop emits DROP TYPE for every enum cur no longer declares.
func diffEnumsDrop(prev, cur *Snapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(prev.Enums) {
		if _, ok := cur.Enums[key]; !ok {
			out = append(out, dropEnumSQL(key, safe))
		}
	}
	return out
}

func diffEnumsCreate(prev, cur *Snapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(cur.Enums) {
		curE := cur.Enums[key]
		prevE, ok := prev.Enums[key]
		if !ok {
			out = append(out, createEnumSQL(curE, safe))
			continue
		}
		// ALTER ADD VALUE for newly-appended labels (PG only
		// supports add, never remove); other shape changes
		// (rename, reorder) need DROP+CREATE which we keep out
		// of automated diffs because data referencing the enum
		// would be lost.
		add := newEnumValues(prevE.Values, curE.Values)
		for _, v := range add {
			out = append(out, fmt.Sprintf(
				`ALTER TYPE %s ADD VALUE IF NOT EXISTS '%s';`, quoteIdent(curE.Name), escapeLit(v)))
		}
	}
	return out
}

// diffSequencesCreate emits CREATE SEQUENCE for every sequence cur
// declares and prev does not.
//
// A sequence present on both sides is left alone whatever its
// attributes say. Introspect reads the attributes back, but only the
// ones the declaration named — see readIntrospectSequences — and
// PostgreSQL's ALTER SEQUENCE ... RESTART is not a migration anybody
// should get by accident, so a moved START WITH is neither applied nor
// reported. Push's doc comment lists it among what Push cannot see.
func diffSequencesCreate(prev, cur *Snapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(cur.Sequences) {
		if _, ok := prev.Sequences[key]; ok {
			continue
		}
		out = append(out, createSequenceSQL(cur.Sequences[key], safe))
	}
	return out
}

// diffSequencesDrop emits DROP SEQUENCE for every sequence cur no
// longer declares.
func diffSequencesDrop(prev, cur *Snapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(prev.Sequences) {
		if _, ok := cur.Sequences[key]; !ok {
			out = append(out, dropSequenceSQL(prev.Sequences[key].Name, safe))
		}
	}
	return out
}

// diffViewsDrop emits the DROP for every view cur no longer declares.
// It is emitted ahead of the table DDL; see Diff.
func diffViewsDrop(prev, cur *Snapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(prev.Views) {
		if _, ok := cur.Views[key]; !ok {
			out = append(out, dropViewSQL(prev.Views[key], safe))
		}
	}
	return out
}

// diffViewsCreate builds the views cur declares and brings the ones
// that changed into line.
//
// renamed says a rename is part of this migration, which decides how a
// changed view is replaced: see the comment on the branch that uses it.
func diffViewsCreate(prev, cur *Snapshot, safe, renamed bool) []string {
	var out []string
	for _, key := range sortedKeys(cur.Views) {
		curV := cur.Views[key]
		prevV, ok := prev.Views[key]
		switch {
		case !ok:
			out = append(out, createViewSQL(curV, false))
		case prevV.Materialized != curV.Materialized:
			// A view and a materialised view are different kinds of
			// relation and PostgreSQL has no ALTER between them, so
			// the old one has to go whatever its body says. Comparing
			// only the body left a declaration that had switched from
			// NewView to NewMaterializedView emitting nothing at all:
			// the probe respells both sides identically, the two
			// bodies then agree, and Push reported success against a
			// database still holding the other kind.
			out = append(out, dropViewSQL(prevV, safe))
			out = append(out, createViewSQL(curV, false))
		case prevV.Definition != curV.Definition:
			// CREATE OR REPLACE if the shape didn't change
			// (non-materialised views support REPLACE);
			// materialised views require drop + recreate.
			//
			// A rename changes the shape by definition. CREATE OR
			// REPLACE VIEW may add columns at the end and nothing
			// else — it will not rename one — so a view selecting a
			// column that has just been renamed is rejected with
			// "cannot change name of view column", and the migration
			// stops there with the rename already applied. Whether
			// this particular view named the renamed column is not
			// something drops can tell without parsing the SQL it was
			// handed, so any view whose body moved in a migration that
			// renames anything is rebuilt rather than replaced. A view
			// holds no rows, so the cost of being wrong is a DROP and
			// a CREATE where a REPLACE would have done.
			if curV.Materialized || renamed {
				out = append(out, dropViewSQL(prevV, safe))
				out = append(out, createViewSQL(curV, false))
			} else {
				out = append(out, createViewSQL(curV, true))
			}
		}
	}
	return out
}

// diffRLS emits ENABLE / DISABLE and FORCE / NO FORCE ROW LEVEL
// SECURITY when either flag flips between prev and cur.
//
// The two are independent in PostgreSQL and independent here: FORCE
// says whether the table's owner is subject to its own policies, and
// a table can carry it while RLS is switched off, so a diff that
// inferred one from the other would either leave a declared FORCE
// unapplied or emit it again on every push.
func diffRLS(prev, cur *TableSnapshot) []string {
	var out []string
	if prev.IsRLSEnabled != cur.IsRLSEnabled {
		out = append(out, fmt.Sprintf(`ALTER TABLE %s %s ROW LEVEL SECURITY;`,
			quoteIdent(cur.Name), rlsVerb(cur.IsRLSEnabled, "ENABLE", "DISABLE")))
	}
	if prev.IsRLSForced != cur.IsRLSForced {
		out = append(out, fmt.Sprintf(`ALTER TABLE %s %s ROW LEVEL SECURITY;`,
			quoteIdent(cur.Name), rlsVerb(cur.IsRLSForced, "FORCE", "NO FORCE")))
	}
	return out
}

// rlsVerb picks the ALTER TABLE keyword for a row-level security flag.
func rlsVerb(on bool, yes, no string) string {
	if on {
		return yes
	}
	return no
}

func diffPolicies(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(prev.Policies) {
		if _, ok := cur.Policies[key]; !ok {
			out = append(out, dropPolicySQL(cur.Name, key, safe))
		}
	}
	for _, key := range sortedKeys(cur.Policies) {
		curP := cur.Policies[key]
		prevP, ok := prev.Policies[key]
		if !ok {
			out = append(out, createPolicySQL(cur.Name, curP))
			continue
		}
		if !policyEqual(prevP, curP) {
			out = append(out, dropPolicySQL(cur.Name, key, safe))
			out = append(out, createPolicySQL(cur.Name, curP))
		}
	}
	return out
}

// ----------------------------------------------------------------------
// SQL renderers for the new object types
// ----------------------------------------------------------------------

func createEnumSQL(e *EnumSnapshot, safe bool) string {
	var b strings.Builder
	if safe {
		b.WriteString("DO $$ BEGIN ")
	}
	fmt.Fprintf(&b, `CREATE TYPE "%s" AS ENUM (`, e.Name)
	for i, v := range e.Values {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "'%s'", escapeLit(v))
	}
	b.WriteByte(')')
	if safe {
		b.WriteString("; EXCEPTION WHEN duplicate_object THEN null; END $$;")
	} else {
		b.WriteByte(';')
	}
	return b.String()
}

func dropEnumSQL(name string, safe bool) string {
	if safe {
		return fmt.Sprintf(`DROP TYPE IF EXISTS %s;`, quoteIdent(name))
	}
	return fmt.Sprintf(`DROP TYPE %s;`, quoteIdent(name))
}

// newEnumValues returns the labels present in cur but not prev,
// preserving cur's order. Drops do NOT generate value removals
// because PG cannot drop an enum value while rows depend on it.
func newEnumValues(prev, cur []string) []string {
	seen := map[string]bool{}
	for _, v := range prev {
		seen[v] = true
	}
	var add []string
	for _, v := range cur {
		if !seen[v] {
			add = append(add, v)
		}
	}
	return add
}

func createSequenceSQL(s *SequenceSnapshot, safe bool) string {
	var b strings.Builder
	if safe {
		b.WriteString(`CREATE SEQUENCE IF NOT EXISTS "`)
	} else {
		b.WriteString(`CREATE SEQUENCE "`)
	}
	b.WriteString(s.Name)
	b.WriteByte('"')
	if s.Increment != nil {
		fmt.Fprintf(&b, " INCREMENT BY %d", *s.Increment)
	}
	if s.MinValue != nil {
		fmt.Fprintf(&b, " MINVALUE %d", *s.MinValue)
	}
	if s.MaxValue != nil {
		fmt.Fprintf(&b, " MAXVALUE %d", *s.MaxValue)
	}
	if s.Start != nil {
		fmt.Fprintf(&b, " START WITH %d", *s.Start)
	}
	if s.Cache != nil {
		fmt.Fprintf(&b, " CACHE %d", *s.Cache)
	}
	if s.Cycle {
		b.WriteString(" CYCLE")
	}
	b.WriteByte(';')
	return b.String()
}

func dropSequenceSQL(name string, safe bool) string {
	if safe {
		return fmt.Sprintf(`DROP SEQUENCE IF EXISTS %s;`, quoteIdent(name))
	}
	return fmt.Sprintf(`DROP SEQUENCE %s;`, quoteIdent(name))
}

func createViewSQL(v *ViewSnapshot, replace bool) string {
	var b strings.Builder
	switch {
	case v.Materialized:
		fmt.Fprintf(&b, `CREATE MATERIALIZED VIEW "%s" AS %s;`, v.Name, v.Definition)
	case replace:
		fmt.Fprintf(&b, `CREATE OR REPLACE VIEW "%s" AS %s;`, v.Name, v.Definition)
	default:
		fmt.Fprintf(&b, `CREATE VIEW "%s" AS %s;`, v.Name, v.Definition)
	}
	return b.String()
}

func dropViewSQL(v *ViewSnapshot, safe bool) string {
	kind := "VIEW"
	if v.Materialized {
		kind = "MATERIALIZED VIEW"
	}
	if safe {
		return fmt.Sprintf(`DROP %s IF EXISTS %s;`, kind, quoteIdent(v.Name))
	}
	return fmt.Sprintf(`DROP %s %s;`, kind, quoteIdent(v.Name))
}

func createPolicySQL(table string, p *PolicySnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, `CREATE POLICY "%s" ON "%s"`, p.Name, table)
	if p.As != "" && p.As != "PERMISSIVE" {
		b.WriteString(" AS ")
		b.WriteString(p.As)
	}
	if p.For != "" && p.For != "ALL" {
		b.WriteString(" FOR ")
		b.WriteString(p.For)
	}
	if len(p.To) > 0 {
		b.WriteString(" TO ")
		b.WriteString(strings.Join(p.To, ", "))
	}
	if p.Using != "" {
		fmt.Fprintf(&b, " USING (%s)", p.Using)
	}
	if p.WithCheck != "" {
		fmt.Fprintf(&b, " WITH CHECK (%s)", p.WithCheck)
	}
	b.WriteByte(';')
	return b.String()
}

func dropPolicySQL(table, name string, safe bool) string {
	if safe {
		return fmt.Sprintf(`DROP POLICY IF EXISTS %s ON %s;`, quoteIdent(name), quoteIdent(table))
	}
	return fmt.Sprintf(`DROP POLICY %s ON %s;`, quoteIdent(name), quoteIdent(table))
}

func policyEqual(a, b *PolicySnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.As != b.As || a.For != b.For || a.Using != b.Using || a.WithCheck != b.WithCheck {
		return false
	}
	// TO is a set, not a list. PostgreSQL 16 happens to store
	// polroles in the order CREATE POLICY named the roles, but the
	// column is an oid array with no ordering contract — a policy is
	// the same policy whichever way its roles were typed, and a drop
	// and recreate on every push would be the cost of assuming
	// otherwise.
	return sameStringSets(a.To, b.To)
}

// sameStringSets reports whether two identifier lists hold the same
// names, in any order.
func sameStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return sameStrings(x, y)
}

func escapeLit(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// createTableSQL renders the bare CREATE TABLE — column definitions
// only. Composite keys, UNIQUE, FOREIGN KEY and CHECK constraints are
// never inlined here; Diff emits each of them as a separate raw SQL
// ALTER TABLE statement so constraint changes stay independently
// diffable and re-orderable across migrations.
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
	b.WriteString("\n);")
	return b.String()
}

func dropTableSQL(t *TableSnapshot, safe bool) string {
	if safe {
		return fmt.Sprintf(`DROP TABLE IF EXISTS %s CASCADE;`, quoteIdent(t.Name))
	}
	return fmt.Sprintf(`DROP TABLE %s CASCADE;`, quoteIdent(t.Name))
}

// snapshotTypeSQL renders a snapshot's column type for DDL.
//
// The snapshot holds the type as the catalogue reports it in udt_name,
// which is what the two sides of a diff are compared on. Written
// straight into a CREATE TABLE that is right for every built-in type
// and wrong for a user-defined one whose name carries an uppercase
// letter: unquoted, PostgreSQL folds docKind to dockind and reports
// the type missing (SQLSTATE 42704), so a schema declaring an enum in
// the camelCase the rest of drops uses could not be pushed at all.
//
// Only a bare identifier carrying an uppercase letter is quoted. No
// built-in type name has one, and the parametrised and multi-word
// spellings — varchar(255), double precision, timestamp with time
// zone — are not bare identifiers, so none of them is caught.
func snapshotTypeSQL(t string) string {
	if strings.ToLower(t) == t || !isBareIdent(t) {
		return t
	}
	return quoteIdent(t)
}

// isBareIdent reports whether s is an unquoted SQL identifier and
// nothing else — no parentheses, spaces or brackets.
func isBareIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case (c >= '0' && c <= '9') || c == '$':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func columnDefSQL(c *ColumnSnapshot) string {
	var b strings.Builder
	b.WriteByte('"')
	b.WriteString(c.Name)
	b.WriteString(`" `)
	b.WriteString(snapshotTypeSQL(c.Type))
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

func diffColumns(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.Columns) {
		if _, ok := cur.Columns[k]; !ok {
			if safe {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s;`, quoteIdent(cur.Name), quoteIdent(k)))
			} else {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s;`, quoteIdent(cur.Name), quoteIdent(k)))
			}
		}
	}
	for _, k := range sortedKeys(cur.Columns) {
		if _, ok := prev.Columns[k]; ok {
			continue
		}
		if safe {
			out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s;`, quoteIdent(cur.Name), columnDefSQL(cur.Columns[k])))
		} else {
			out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s;`, quoteIdent(cur.Name), columnDefSQL(cur.Columns[k])))
		}
	}
	for _, k := range sortedKeys(cur.Columns) {
		prevC, ok := prev.Columns[k]
		if !ok {
			continue
		}
		curC := cur.Columns[k]
		if prevC.Type != curC.Type {
			out = append(out, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET DATA TYPE %s;`,
				quoteIdent(cur.Name), quoteIdent(k), snapshotTypeSQL(curC.Type)))
		}
		if prevC.NotNull != curC.NotNull {
			if curC.NotNull {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET NOT NULL;`, quoteIdent(cur.Name), quoteIdent(k)))
			} else {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s DROP NOT NULL;`, quoteIdent(cur.Name), quoteIdent(k)))
			}
		}
		if !sameStringPtr(prevC.Default, curC.Default) {
			if curC.Default == nil {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s DROP DEFAULT;`, quoteIdent(cur.Name), quoteIdent(k)))
			} else {
				out = append(out, fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s;`,
					quoteIdent(cur.Name), quoteIdent(k), *curC.Default))
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
		out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s UNIQUE(%s);`, quoteIdent(cur.Name), quoteIdent(u.Name), cols))
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
		return fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s;`, quoteIdent(table), quoteIdent(name))
	}
	return fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT %s;`, quoteIdent(table), quoteIdent(name))
}

func fkAddSQL(tableFrom string, fk *ForeignKeySnapshot) string {
	cols := strings.Join(quoteIdents(fk.ColumnsFrom), ", ")
	targetCols := strings.Join(quoteIdents(fk.ColumnsTo), ", ")
	return fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s FOREIGN KEY (%s) REFERENCES %s(%s) ON DELETE %s ON UPDATE %s;`,
		quoteIdent(tableFrom), quoteIdent(fk.Name), cols, quoteIdent(fk.TableTo), targetCols, fk.OnDelete, fk.OnUpdate)
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
		if len(curIdx.Columns) == 0 {
			// At least one element was an expression, so the
			// snapshot kept none of them and there is nothing to put
			// between the parentheses. Rendering `ON "t" ()` is not a
			// migration,
			// it is a syntax error that takes the rest of the file
			// down with it. Push names the index it skipped here in
			// its notices; declare it with pg.CreateIndex instead.
			continue
		}
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
//
// The two sides are matched by their column list, not by their name: a
// table has at most one PRIMARY KEY, and the name it wears is whatever
// created it — "members_pkey" when PostgreSQL chose it,
// compositePKName's camelCase when drops did. Matching by name would
// have a push against a server-named key drop and recreate it, every
// time, for no change at all.
func diffCompositePKs(prev, cur *TableSnapshot, safe bool) []string {
	prevPK := tableCompositePK(prev)
	curPK := tableCompositePK(cur)
	switch {
	case prevPK == nil && curPK == nil:
		return nil
	case curPK == nil:
		return []string{dropConstraintSQL(cur.Name, prevPK.Name, safe)}
	case prevPK == nil:
		return []string{addPrimaryKeySQL(cur.Name, curPK)}
	case sameStrings(prevPK.Columns, curPK.Columns):
		return nil
	}
	return []string{
		dropConstraintSQL(cur.Name, prevPK.Name, safe),
		addPrimaryKeySQL(cur.Name, curPK),
	}
}

// tableCompositePK returns the table's multi-column PRIMARY KEY, or
// nil when it has none. The snapshot format keys them by name for
// drizzle-kit compatibility, but PostgreSQL permits only one; the
// lowest name wins so a hand-written snapshot carrying more than one
// still diffs deterministically.
func tableCompositePK(t *TableSnapshot) *CompositePKSnapshot {
	for _, k := range sortedKeys(t.CompositePrimaryKeys) {
		return t.CompositePrimaryKeys[k]
	}
	return nil
}

func addPrimaryKeySQL(table string, pk *CompositePKSnapshot) string {
	return fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY (%s);`,
		quoteIdent(table), quoteIdent(pk.Name), strings.Join(quoteIdents(pk.Columns), ", "))
}

// sameStrings reports whether two ordered identifier lists match.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// diffChecks emits ALTER TABLE ADD/DROP CONSTRAINT for CHECK
// constraints.
//
// A constraint whose expression changed under an unchanged name is
// dropped and re-added: PostgreSQL has no ALTER CONSTRAINT for a
// CHECK, and matching on the name alone — which is all this did —
// meant tightening `age >= 0` to `age >= 18` produced no migration at
// all. The same caveat as indexEqual applies to the comparison: the
// two Values have to be spelled alike, which against a live server is
// Push's job, not Diff's.
func diffChecks(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.CheckConstraints) {
		curC, ok := cur.CheckConstraints[k]
		if !ok || curC.Value != prev.CheckConstraints[k].Value {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	for _, k := range sortedKeys(cur.CheckConstraints) {
		if prevC, ok := prev.CheckConstraints[k]; ok && prevC.Value == cur.CheckConstraints[k].Value {
			continue
		}
		c := cur.CheckConstraints[k]
		out = append(out, fmt.Sprintf(
			`ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s);`,
			quoteIdent(cur.Name), quoteIdent(c.Name), c.Value))
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
	if len(idx.Include) > 0 {
		fmt.Fprintf(&b, " INCLUDE (%s)", strings.Join(quoteIdents(idx.Include), ", "))
	}
	if idx.Where != "" {
		fmt.Fprintf(&b, " WHERE %s", idx.Where)
	}
	b.WriteByte(';')
	return b.String()
}

// dropIndexSQL renders DROP INDEX [IF EXISTS] "name".
func dropIndexSQL(name string, safe bool) string {
	if safe {
		return fmt.Sprintf(`DROP INDEX IF EXISTS %s;`, quoteIdent(name))
	}
	return fmt.Sprintf(`DROP INDEX %s;`, quoteIdent(name))
}

// indexEqual reports whether two index snapshots describe the
// same logical index.
//
// Concurrently is not part of the comparison: it says how an index was
// built, not what it is, and the catalogue cannot report it — comparing
// it would have every push drop and rebuild an index declared
// Concurrently. Everything else is compared literally, the predicate
// included.
//
// Comparing the predicate as text is only honest because both sides
// arrive spelled the same way. Two snapshots built from Go schemas are
// spelled by drops; a snapshot read back from a server is spelled by
// pg_get_expr, which renormalises ("age" >= 18) to (age >= 18) and
// would churn forever against a declared index. Push closes that gap
// by asking the server to respell the declared side before it diffs —
// see renormaliseExpressions. A caller diffing Introspect against
// BuildSnapshot directly does not get that and should expect the
// predicate to compare unequal.
func indexEqual(a, b *IndexSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.IsUnique != b.IsUnique || a.Method != b.Method || a.Where != b.Where {
		return false
	}
	return sameStrings(a.Columns, b.Columns) && sameStrings(a.Include, b.Include)
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
