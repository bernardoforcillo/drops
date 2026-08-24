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
//  2. CREATE TYPE and CREATE SEQUENCE for the enums and sequences the
//     schema newly declares, ahead of the CREATE TABLE naming them,
//     plus ALTER TYPE / ALTER SEQUENCE for those that moved
//  3. DROP CONSTRAINT for foreign keys that are going, ahead of the
//     DROP TABLEs that would CASCADE them away first
//  4. DROP TABLE   for tables removed entirely
//  5. CREATE TABLE for new tables (column defs only — every
//     composite key, UNIQUE, FOREIGN KEY and CHECK constraint is
//     emitted as a separate ALTER TABLE below, never inline)
//  6. DROP POLICY, DROP CONSTRAINT and DROP INDEX for everything else
//     that names a column and is going: policies, UNIQUE, the
//     PRIMARY KEY, CHECK, indexes
//  7. DROP COLUMN, once nothing names the column any more
//  8. ADD COLUMN and the ALTER COLUMNs — type, NOT NULL, DEFAULT
//  9. ADD CONSTRAINT for UNIQUE, the PRIMARY KEY and CHECK, once the
//     columns they span are there
//  10. ADD CONSTRAINT ... FOREIGN KEY, after the CREATE TABLEs and the
//     keys they reference, so cross-table references resolve
//  11. CREATE INDEX
//  12. DROP SEQUENCE, DROP TYPE, once the last table naming them is
//     gone
//  13. CREATE / CREATE OR REPLACE VIEW, once the tables a view selects
//     from are in their final shape
//  14. ROW LEVEL SECURITY and CREATE POLICY, table by table
//
// Steps 3, 6 and 7 are one rule seen from three sides: PostgreSQL
// removes a dependent object along with the thing it depends on, and
// refuses to remove the thing while a dependent it cannot remove
// stands. Dropping a column takes every constraint and index that
// names it — whether it spans that column alone or several, and
// whether an index names it in its key, its INCLUDE list, an
// expression or a partial predicate — so a DROP CONSTRAINT emitted
// after the DROP COLUMN names something that is already gone
// (SQLSTATE 42704). A DROP TABLE, emitted CASCADE, does the same to
// the foreign keys pointing at it from tables that survive. In the
// other direction a column another table's foreign key points at, or
// a policy reads, cannot be dropped at all while that object stands
// (SQLSTATE 2BP01) — and neither of those is on the same table, which
// is why the drops are grouped by kind across every table rather than
// run table by table.
//
// Emitting the drop rather than suppressing it is the deliberate half.
// The two are not equivalent: a constraint can be dropped while every
// column it spans stays, which no suppression rule would emit, and
// working out whether an index names a column would mean modelling
// PostgreSQL's dependency graph — including the expressions the
// snapshot deliberately does not record. Ordering needs none of that.
//
// A view that survives the migration is the one dependent ordering
// alone cannot reach. PostgreSQL refuses to drop a column a view reads
// (SQLSTATE 2BP01) and refuses to retype one (SQLSTATE 0A000) whether
// the view's body changed or not, and a body that did not change emits
// no statement to put anywhere. So the view is not reordered, it is
// rebuilt: when the migration drops or retypes any column of any
// table, step 1 takes down every view both sides declare and step 13
// states each of them again — dependents first coming down,
// dependencies first going back up.
//
// It is rebuilt whether or not it was in the way, because which
// columns a view reads is exactly what the snapshot does not record.
// That is the conservative half, and it is cheap: a view holds no rows,
// so the cost of rebuilding one that would have been fine is a DROP
// and a CREATE where nothing would have done. Two things it is not
// free of — a GRANT on the view does not survive the rebuild, and a
// materialised view is repopulated by its CREATE — so a schema whose
// matviews are expensive should push column drops on their own.
//
// A view the Go schema does not declare is not rebuilt, because Push
// will not drop what the schema never claimed. One selecting from a
// view that is being rebuilt would block that rebuild, so Push refuses
// the plan and names it rather than letting the server answer with a
// 2BP01 about the wrong view; see checkUndeclaredViewDependents and
// PushOptions.DropUnmanagedObjects.
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

	// Whether a view could stand in the way of the column work below,
	// which decides whether the views that survive are rebuilt around
	// it or left where they are.
	rebuildViews := columnWorkAViewCanBlock(prev, cur)

	// A view that is going away goes in front of everything: DROP
	// TABLE is emitted CASCADE, which takes every view selecting from
	// the table with it, and the DROP VIEW that followed then named an
	// object PostgreSQL had already removed (SQLSTATE 42P01), stopping
	// the migration halfway. Dropping the view first is always legal —
	// nothing depends on a view being there — and leaves the CASCADE
	// with nothing left to take.
	//
	// A view that survives goes here too when the migration drops or
	// retypes a column; it is built again in step 13. See diffViewsDrop.
	out = append(out, diffViewsDrop(prev, cur, opt.Safe, rebuildViews)...)

	// A type or a sequence a column references has to exist before the
	// CREATE TABLE that names it, and cannot be dropped until the last
	// table naming it is gone — so the creates go in front of the table
	// DDL and the drops (further down) behind it. Emitting the creates
	// last, as this once did, meant a schema declaring a single enum
	// column produced a migration whose CREATE TABLE named a type
	// three statements before it existed.
	out = append(out, diffEnumsCreate(prev, cur, opt.Safe)...)
	out = append(out, diffSequences(prev, cur, opt.Safe)...)

	// Foreign keys that are going, ahead of the DROP TABLEs: a key
	// pointing at a table that is going is removed by that table's
	// CASCADE, and the DROP CONSTRAINT would then name a constraint
	// PostgreSQL had already taken.
	for _, key := range sortedKeys(cur.Tables) {
		out = append(out, dropForeignKeys(prevTable(prev, key), cur.Tables[key], opt.Safe)...)
	}

	for _, key := range sortedKeys(prev.Tables) {
		if _, ok := cur.Tables[key]; !ok {
			out = append(out, dropTableSQL(prev.Tables[key], opt.Safe))
		}
	}
	// New tables carry their column definitions and nothing else.
	// Every table-level constraint — composite PK, UNIQUE and CHECK —
	// is emitted below as a separate ALTER TABLE, so the CREATE TABLE
	// stays constraint-free.
	for _, key := range sortedKeys(cur.Tables) {
		if _, ok := prev.Tables[key]; !ok {
			out = append(out, createTableSQL(cur.Tables[key], opt.Safe))
		}
	}

	// The rest of what names a column, before the columns go. The
	// foreign keys are already out of the way, which is what lets a
	// UNIQUE or a PRIMARY KEY that another table's key referenced be
	// dropped at all.
	for _, key := range sortedKeys(cur.Tables) {
		prevT, curT := prevTable(prev, key), cur.Tables[key]
		out = append(out, dropPolicies(prevT, curT, opt.Safe)...)
		out = append(out, dropUniques(prevT, curT, opt.Safe)...)
		out = append(out, dropPrimaryKey(prevT, curT, opt.Safe)...)
		out = append(out, dropChecks(prevT, curT, opt.Safe)...)
		out = append(out, dropIndexes(prevT, curT, opt.Safe)...)
	}

	// Columns, on the tables both sides have. A table cur creates is
	// already complete from its CREATE TABLE, and one only prev has is
	// gone by now.
	surviving := survivingTables(prev, cur)
	for _, key := range surviving {
		out = append(out, dropColumns(prev.Tables[key], cur.Tables[key], opt.Safe)...)
	}
	for _, key := range surviving {
		out = append(out, addColumns(prev.Tables[key], cur.Tables[key], opt.Safe)...)
		out = append(out, alterColumns(prev.Tables[key], cur.Tables[key])...)
	}

	// Everything the columns now support.
	for _, key := range sortedKeys(cur.Tables) {
		prevT, curT := prevTable(prev, key), cur.Tables[key]
		out = append(out, addUniques(prevT, curT)...)
		out = append(out, addPrimaryKey(prevT, curT)...)
		out = append(out, addChecks(prevT, curT)...)
	}
	// Foreign keys after the CREATE TABLEs so cross-table references
	// resolve, and after the UNIQUE and PRIMARY KEY adds because a key
	// can only point at one of those.
	for _, key := range sortedKeys(cur.Tables) {
		out = append(out, addForeignKeys(prevTable(prev, key), cur.Tables[key])...)
	}
	for _, key := range sortedKeys(cur.Tables) {
		out = append(out, addIndexes(prevTable(prev, key), cur.Tables[key], opt.Safe)...)
	}

	out = append(out, diffSequencesDrop(prev, cur, opt.Safe)...)
	out = append(out, diffEnumsDrop(prev, cur, opt.Safe)...)

	// Views that survive are built last of all: a view selects from
	// the tables, so it can only be built once they are in their final
	// shape.
	out = append(out, diffViewsCreate(prev, cur, opt.Safe, len(opt.Renames) > 0, rebuildViews)...)

	// RLS and the policies it governs, table-scoped. The drops ran
	// with the other dependent objects; only the creates are left, and
	// they wait here because a policy reads columns.
	for _, key := range sortedKeys(cur.Tables) {
		prevT, curT := prevTable(prev, key), cur.Tables[key]
		out = append(out, diffRLS(prevT, curT)...)
		out = append(out, addPolicies(prevT, curT)...)
	}

	return append(renames, out...)
}

// prevTable returns the table's previous shape, or an empty one when
// this migration creates it. Every pass below is written against two
// sides; a table that is new simply has nothing on its old side, and
// handing it one keeps each pass from needing its own special case.
func prevTable(prev *Snapshot, name string) *TableSnapshot {
	if t, ok := prev.Tables[name]; ok {
		return t
	}
	return &TableSnapshot{}
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

// diffSequences emits CREATE SEQUENCE for every sequence cur
// declares and prev does not, and ALTER SEQUENCE for every one both
// sides carry whose attributes moved.
//
// The attributes were the third place the snapshot recorded a value
// the diff never read, alongside a UNIQUE's column list and a foreign
// key's referential actions: Introspect reads seqincrement, seqmin,
// seqmax, seqstart, seqcache and seqcycle back from pg_sequence,
// BuildSnapshot records whatever SequenceOptions named, and matching
// on the name alone meant a declaration that moved INCREMENT BY 1 to
// INCREMENT BY 10 produced no statement and no notice.
//
// One ALTER SEQUENCE carries every clause that moved, rather than one
// statement each: MINVALUE and MAXVALUE have to widen together or the
// first of them can be rejected for crossing the other.
//
// START WITH is stated with the rest and is not RESTART: it sets the
// value a future ALTER SEQUENCE ... RESTART would return to, and moves
// nothing the sequence is handing out today. Restarting a live
// sequence is not a migration anybody should get from editing a
// declaration, and drops does not emit one.
//
// That choice has a cost, and it is a refusal rather than a silence:
// PostgreSQL will not accept a MINVALUE above the value the sequence
// is currently sitting on, or a MAXVALUE below it, without a RESTART
// to go with it — it answers 22023 and names both numbers. Diff has
// no server to ask where the sequence has got to, so it states the
// declaration and lets the server refuse; the push rolls back whole
// and the operator restarts the sequence by hand. See Push's "What
// Push cannot see".
func diffSequences(prev, cur *Snapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(cur.Sequences) {
		prevS, ok := prev.Sequences[key]
		if !ok {
			out = append(out, createSequenceSQL(cur.Sequences[key], safe))
			continue
		}
		if alter := alterSequenceSQL(prevS, cur.Sequences[key]); alter != "" {
			out = append(out, alter)
		}
	}
	return out
}

// alterSequenceSQL renders the ALTER SEQUENCE that brings prev into
// line with cur, or "" when the two describe the same sequence.
//
// The two sides are compared on the values the server would end up
// with, not on the pointers: Introspect leaves an attribute nil when
// it holds the default PostgreSQL would have chosen anyway, while a
// declaration that spells that same default out records it. Comparing
// the pointers would have every push restate a sequence declared
// START WITH 1.
func alterSequenceSQL(prev, cur *SequenceSnapshot) string {
	p, c := effectiveSequence(prev), effectiveSequence(cur)
	var b strings.Builder
	if p.increment != c.increment {
		fmt.Fprintf(&b, " INCREMENT BY %d", c.increment)
	}
	if p.minValue != c.minValue {
		fmt.Fprintf(&b, " MINVALUE %d", c.minValue)
	}
	if p.maxValue != c.maxValue {
		fmt.Fprintf(&b, " MAXVALUE %d", c.maxValue)
	}
	if p.start != c.start {
		fmt.Fprintf(&b, " START WITH %d", c.start)
	}
	if p.cache != c.cache {
		fmt.Fprintf(&b, " CACHE %d", c.cache)
	}
	if prev.Cycle != cur.Cycle {
		if cur.Cycle {
			b.WriteString(" CYCLE")
		} else {
			b.WriteString(" NO CYCLE")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return fmt.Sprintf(`ALTER SEQUENCE %s%s;`, quoteIdent(cur.Name), b.String())
}

// effectiveSequence resolves a snapshot's attributes to the numbers
// the sequence actually carries, filling each unstated one with the
// default CREATE SEQUENCE would have picked for that direction.
func effectiveSequence(s *SequenceSnapshot) seqDefaults {
	out := sequenceDefaults(1)
	if s.Increment != nil {
		out = sequenceDefaults(*s.Increment)
		out.increment = *s.Increment
	}
	if s.Start != nil {
		out.start = *s.Start
	}
	if s.MinValue != nil {
		out.minValue = *s.MinValue
	}
	if s.MaxValue != nil {
		out.maxValue = *s.MaxValue
	}
	if s.Cache != nil {
		out.cache = *s.Cache
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

// diffViewsDrop emits the DROP for every view cur no longer declares,
// and — when rebuild says so — for the ones it still declares too, the
// ones diffViewsCreate builds again once the table DDL has run. Both
// go ahead of that DDL; see Diff.
//
// They are emitted dependents first, which is viewOrder read backwards:
// PostgreSQL will not drop a view another view selects from, so the
// order that creates them is the reverse of the order that removes
// them. That holds for the views the schema stopped declaring as much
// as for the ones coming back — two stacked views removed together
// were already an SQLSTATE 2BP01 in sorted order.
//
// No CASCADE. It would make the order unnecessary and is the wrong
// trade: a CASCADE from a declared view takes an undeclared one
// selecting from it along, silently and for good, and Push does not
// drop what the Go schema never claimed. Without it PostgreSQL refuses
// and names the view, which is an answer an operator can act on —
// declare it with Schema.AddView so it is rebuilt too, or take it down
// by hand.
func diffViewsDrop(prev, cur *Snapshot, safe, rebuild bool) []string {
	var out []string
	order := viewOrder(prev.Views)
	for i := len(order) - 1; i >= 0; i-- {
		key := order[i]
		if _, declared := cur.Views[key]; !declared || rebuild {
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
// rebuild says diffViewsDrop has already taken down every view both
// sides carry, so each of them is stated afresh whether its body moved
// or not.
//
// The views are emitted in dependency order rather than by name, so a
// view selecting from another is created after the one it reads.
func diffViewsCreate(prev, cur *Snapshot, safe, renamed, rebuild bool) []string {
	var out []string
	for _, key := range viewOrder(cur.Views) {
		curV := cur.Views[key]
		prevV, ok := prev.Views[key]
		switch {
		case !ok, rebuild:
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

// dropPolicies emits DROP POLICY for the policies that are going, and
// for those whose definition changed — PostgreSQL's ALTER POLICY
// cannot restate one, so a changed policy is dropped and recreated.
//
// It runs with the constraint and index drops rather than beside
// addPolicies, because a policy's USING and WITH CHECK expressions
// read columns: PostgreSQL refuses to drop a column a policy names
// (SQLSTATE 2BP01), and a policy that is going was blocking the very
// column drop the same migration asked for.
func dropPolicies(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, key := range sortedKeys(prev.Policies) {
		curP, ok := cur.Policies[key]
		if !ok || !policyEqual(prev.Policies[key], curP) {
			out = append(out, dropPolicySQL(cur.Name, key, safe))
		}
	}
	return out
}

// addPolicies is dropPolicies's other half, emitted last of all
// because a policy reads the columns it is written against.
func addPolicies(prev, cur *TableSnapshot) []string {
	var out []string
	for _, key := range sortedKeys(cur.Policies) {
		curP := cur.Policies[key]
		prevP, ok := prev.Policies[key]
		if !ok || !policyEqual(prevP, curP) {
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

// columnWorkAViewCanBlock reports whether this migration changes a
// column in one of the two ways PostgreSQL refuses while a view reads
// it: dropping the column, which comes back as SQLSTATE 2BP01
// ("cannot drop column ... because other objects depend on it"), and
// changing its type, which comes back as SQLSTATE 0A000 ("cannot alter
// type of a column used by a view or rule"). Neither has a way around
// it that leaves the view standing.
//
// A rename is not one of them — a view follows a renamed column,
// because it holds the column's number and not its name — and neither
// is a NOT NULL or a DEFAULT.
//
// It is deliberately a question about the whole migration rather than
// about one view: which columns a view reads is exactly what the
// snapshot does not record, so the only honest answers are "no view
// can be in the way" and "one might be".
func columnWorkAViewCanBlock(prev, cur *Snapshot) bool {
	for _, key := range survivingTables(prev, cur) {
		prevT, curT := prev.Tables[key], cur.Tables[key]
		for name, prevC := range prevT.Columns {
			curC, ok := curT.Columns[name]
			if !ok || prevC.Type != curC.Type {
				return true
			}
		}
	}
	return false
}

// viewOrder returns the keys of views in an order it is safe to CREATE
// them in: a view another one selects from comes first.
//
// PostgreSQL records what a view reads, but Diff has no server to ask
// and the snapshot holds only the body, so the dependency is read out
// of that text — a view whose body names another view's identifier,
// quoted or bare, is taken to select from it. A false positive, say a
// column sharing a view's name, only moves a CREATE earlier than it
// needed to be. A genuine cycle cannot exist in the catalogue, so
// anything the sort cannot place is appended in name order rather than
// dropped on the floor.
func viewOrder(views map[string]*ViewSnapshot) []string {
	keys := sortedKeys(views)
	reads := make(map[string][]string, len(keys))
	pending := make(map[string]bool, len(keys))
	for _, k := range keys {
		pending[k] = true
		for _, other := range keys {
			if other != k && mentionsIdent(views[k].Definition, views[other].Name) {
				reads[k] = append(reads[k], other)
			}
		}
	}
	out := make([]string, 0, len(keys))
	for len(pending) > 0 {
		placed := false
		for _, k := range keys {
			if !pending[k] {
				continue
			}
			ready := true
			for _, dep := range reads[k] {
				if pending[dep] {
					ready = false
					break
				}
			}
			if ready {
				out = append(out, k)
				delete(pending, k)
				placed = true
			}
		}
		if !placed {
			for _, k := range keys {
				if pending[k] {
					out = append(out, k)
					delete(pending, k)
				}
			}
		}
	}
	return out
}

// mentionsIdent reports whether sql names ident as an identifier of
// its own rather than as part of a longer word. The double quotes
// around a quoted identifier are not identifier bytes, so one spelling
// of the test answers for both.
func mentionsIdent(sql, ident string) bool {
	if ident == "" {
		return false
	}
	for i := 0; i+len(ident) <= len(sql); {
		j := strings.Index(sql[i:], ident)
		if j < 0 {
			return false
		}
		start := i + j
		if !identByteAt(sql, start-1) && !identByteAt(sql, start+len(ident)) {
			return true
		}
		i = start + 1
	}
	return false
}

// identByteAt reports whether sql[i] is a byte an SQL identifier may
// carry. An index outside the string is not.
func identByteAt(sql string, i int) bool {
	if i < 0 || i >= len(sql) {
		return false
	}
	c := sql[i]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$'
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
	// ColumnSnapshot.PrimaryKey is deliberately not rendered. The key
	// is stated once, by addPrimaryKey, whether it spans one column or
	// five: inlining the one-column case here meant a new table got its
	// key from the CREATE TABLE and a widened key got it from an ALTER,
	// which is two code paths for one constraint and the reason
	// narrowing a key emitted a DROP with no ADD behind it.
	if c.NotNull {
		b.WriteString(" NOT NULL")
	}
	if c.Default != nil {
		b.WriteString(" DEFAULT ")
		b.WriteString(*c.Default)
	}
	return b.String()
}

// dropColumns emits DROP COLUMN for every column cur no longer has.
// It runs after the constraints, indexes and policies that name those
// columns are already gone; see Diff.
func dropColumns(prev, cur *TableSnapshot, safe bool) []string {
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
	return out
}

// addColumns emits ADD COLUMN for every column cur newly declares.
func addColumns(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
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
	return out
}

// alterColumns brings a column both sides have into line: its type,
// its nullability and its default.
func alterColumns(prev, cur *TableSnapshot) []string {
	var out []string
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

// dropUniques emits DROP CONSTRAINT for every UNIQUE cur no longer
// declares, and for every one whose columns changed under a name that
// did not.
//
// PostgreSQL has no ALTER for a UNIQUE constraint's column list, so a
// re-scoped one is dropped and re-added — the same treatment dropChecks
// gives an expression that moved, for the same reason. Comparing the
// names alone, which is all this did, meant widening
// UNIQUE(email) to UNIQUE(email, orgId) under the name it already had
// produced no statement at all: pg.Diff returned the empty string, and
// the rule the schema declared and the rule the database enforced
// disagreed for ever with nothing to say so.
func dropUniques(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.UniqueConstraints) {
		curU, ok := cur.UniqueConstraints[k]
		if !ok || !uniqueEqual(prev.UniqueConstraints[k], curU) {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	return out
}

// addUniques is dropUniques's other half: the ADD CONSTRAINT for every
// UNIQUE cur newly declares and every one dropUniques took down
// because its columns moved.
func addUniques(prev, cur *TableSnapshot) []string {
	var out []string
	for _, k := range sortedKeys(cur.UniqueConstraints) {
		if prevU, ok := prev.UniqueConstraints[k]; ok && uniqueEqual(prevU, cur.UniqueConstraints[k]) {
			continue
		}
		u := cur.UniqueConstraints[k]
		cols := strings.Join(quoteIdents(u.Columns), ", ")
		out = append(out, fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s UNIQUE(%s);`, quoteIdent(cur.Name), quoteIdent(u.Name), cols))
	}
	return out
}

// uniqueEqual reports whether two UNIQUE constraints span the same
// columns in the same order.
//
// Order is compared, though uniqueness does not depend on it: the
// constraint is backed by an index, and (email, orgId) and
// (orgId, email) are the same rule enforced by two indexes that serve
// different lookups. Introspect reads the columns back in key order,
// so the two sides are comparable and a reorder is a real change.
//
// NullsNotDistinct is not compared. Neither producer records it —
// BuildSnapshot has no way to declare it and Introspect writes false —
// and addUniques cannot render it, so comparing it would drop and
// re-add such a constraint on every push without ever making it true.
func uniqueEqual(a, b *UniqueSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return sameStrings(a.Columns, b.Columns)
}

// dropForeignKeys emits DROP CONSTRAINT for every foreign key cur no
// longer declares, and for every one whose definition changed under a
// name that did not. It runs ahead of the DROP TABLEs; see Diff.
//
// The referential actions are the half that moves without the name
// moving: fkName is built from the columns and the target, so a key
// that starts pointing somewhere else arrives under a new name and is
// an add and a drop already — but ON DELETE CASCADE becoming ON DELETE
// RESTRICT keeps every part of that name. Compared by name alone, as
// this was, the change produced no statement, and the database went on
// deleting the children of a deleted parent that the schema said it
// should refuse to delete. PostgreSQL has no ALTER for the action of a
// non-deferrable constraint, so the key is dropped and re-added.
func dropForeignKeys(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.ForeignKeys) {
		curFK, ok := cur.ForeignKeys[k]
		if !ok || !foreignKeyEqual(prev.ForeignKeys[k], curFK) {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	return out
}

// addForeignKeys is dropForeignKeys's other half: the ADD CONSTRAINT
// for every key cur newly declares and every one dropForeignKeys took
// down because its definition moved.
func addForeignKeys(prev, cur *TableSnapshot) []string {
	var out []string
	for _, k := range sortedKeys(cur.ForeignKeys) {
		if prevFK, ok := prev.ForeignKeys[k]; ok && foreignKeyEqual(prevFK, cur.ForeignKeys[k]) {
			continue
		}
		out = append(out, fkAddSQL(cur.Name, cur.ForeignKeys[k]))
	}
	return out
}

// foreignKeyEqual reports whether two foreign keys describe the same
// constraint: the same columns pointing at the same columns of the
// same table, under the same referential actions.
//
// SchemaTo is left out. Introspect fills it from the catalogue and
// BuildSnapshot from the target Table's declared schema, which is
// empty for a table that never named one — the common case — so
// comparing it would have every push drop and re-add every key on the
// default schema. fkAddSQL does not render it either.
func foreignKeyEqual(a, b *ForeignKeySnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.TableTo != b.TableTo || a.OnDelete != b.OnDelete || a.OnUpdate != b.OnUpdate {
		return false
	}
	return sameStrings(a.ColumnsFrom, b.ColumnsFrom) && sameStrings(a.ColumnsTo, b.ColumnsTo)
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

// dropIndexes emits DROP INDEX for every index cur no longer declares,
// and for every one whose shape changed — an index is never modified
// in place, it is dropped and recreated by addIndexes.
func dropIndexes(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.Indexes) {
		curIdx, present := cur.Indexes[k]
		if !present || !indexEqual(prev.Indexes[k], curIdx) {
			out = append(out, dropIndexSQL(k, safe))
		}
	}
	return out
}

// addIndexes emits CREATE INDEX for every index cur newly declares and
// for every one dropIndexes took down because its shape changed.
func addIndexes(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
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

// dropPrimaryKey emits ALTER TABLE DROP CONSTRAINT for a PRIMARY KEY
// that is going or whose columns changed — at any arity, a key of one
// column included.
//
// The two sides are matched by their column list, not by their name: a
// table has at most one PRIMARY KEY, and the name it wears is whatever
// created it — "members_pkey" when PostgreSQL chose it,
// compositePKName's camelCase when drops did. Matching by name would
// have a push against a server-named key drop and recreate it, every
// time, for no change at all.
func dropPrimaryKey(prev, cur *TableSnapshot, safe bool) []string {
	prevPK := tablePrimaryKey(prev)
	if prevPK == nil {
		return nil
	}
	if curPK := tablePrimaryKey(cur); curPK != nil && sameStrings(prevPK.Columns, curPK.Columns) {
		return nil
	}
	return []string{dropConstraintSQL(cur.Name, prevPK.Name, safe)}
}

// addPrimaryKey is dropPrimaryKey's other half: the ADD CONSTRAINT for
// the key cur declares, matched the same way.
func addPrimaryKey(prev, cur *TableSnapshot) []string {
	curPK := tablePrimaryKey(cur)
	if curPK == nil {
		return nil
	}
	if prevPK := tablePrimaryKey(prev); prevPK != nil && sameStrings(prevPK.Columns, curPK.Columns) {
		return nil
	}
	return []string{addPrimaryKeySQL(cur.Name, curPK)}
}

// tablePrimaryKey returns the table's PRIMARY KEY, or nil when it has
// none. The snapshot format keys them by name for drizzle-kit
// compatibility, but PostgreSQL permits only one; the lowest name wins
// so a hand-written snapshot carrying more than one still diffs
// deterministically.
//
// A single-column key is read off the column when the map is empty.
// BuildSnapshot and Introspect both fill the map at every arity now,
// so that fallback is for the snapshots they did not write: the ones
// drizzle-kit wrote, which spell a one-column key only on the column,
// and the ones drops itself wrote before the map covered every width.
// Without it a migration generated against such a file would read the
// table as having no key at all and state the declared one afresh.
func tablePrimaryKey(t *TableSnapshot) *CompositePKSnapshot {
	for _, k := range sortedKeys(t.CompositePrimaryKeys) {
		return t.CompositePrimaryKeys[k]
	}
	for _, k := range sortedKeys(t.Columns) {
		if t.Columns[k].PrimaryKey {
			return &CompositePKSnapshot{
				Name:    compositePKName(t.Name, []string{k}),
				Columns: []string{k},
			}
		}
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

// dropChecks emits ALTER TABLE DROP CONSTRAINT for the CHECK
// constraints that are going, and for those whose expression changed.
//
// A constraint whose expression changed under an unchanged name is
// dropped and re-added: PostgreSQL has no ALTER CONSTRAINT for a
// CHECK, and matching on the name alone — which is all this did —
// meant tightening `age >= 0` to `age >= 18` produced no migration at
// all. The same caveat as indexEqual applies to the comparison: the
// two Values have to be spelled alike, which against a live server is
// Push's job, not Diff's.
func dropChecks(prev, cur *TableSnapshot, safe bool) []string {
	var out []string
	for _, k := range sortedKeys(prev.CheckConstraints) {
		curC, ok := cur.CheckConstraints[k]
		if !ok || curC.Value != prev.CheckConstraints[k].Value {
			out = append(out, dropConstraintSQL(cur.Name, k, safe))
		}
	}
	return out
}

// addChecks is dropChecks's other half.
func addChecks(prev, cur *TableSnapshot) []string {
	var out []string
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
// Concurrently. NullsNotDistinct and With are not part of it either,
// for the opposite reason: the two fields are in IndexSnapshot for
// drizzle-kit's format and neither BuildSnapshot nor Introspect ever
// puts a value in one, so there is nothing to compare and
// createIndexSQL could not render the difference if there were. See
// Push's "What Push cannot see". Everything else is compared
// literally, the predicate included.
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
