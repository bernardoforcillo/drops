package pg_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// Four of the snapshot's fields were written by both producers and
// read by neither side of the diff: a PRIMARY KEY of one column, a
// UNIQUE's column list, a foreign key's referential actions and a
// sequence's attributes. Each one made a declared change into a change
// the database never received. These tests are the diff-level half;
// the integration suite carries the same cases against a live server,
// where the proof is the catalogue rather than the statement list.
//
// indexOf is shared with diff_droporder_test.go.

func ptr[T any](v T) *T { return &v }

// stmtsMatching returns the statements containing every one of subs.
func stmtsMatching(stmts []string, subs ...string) []string {
	var out []string
	for _, s := range stmts {
		all := true
		for _, sub := range subs {
			if !strings.Contains(s, sub) {
				all = false
				break
			}
		}
		if all {
			out = append(out, s)
		}
	}
	return out
}

// ----------------------------------------------------------------------
// The PRIMARY KEY, at every width
// ----------------------------------------------------------------------

// The same one-column key, declared two ways, has to reach the
// snapshot as the same key. Push diffs a Go schema against a database
// and would otherwise see work to do every time the schema switched
// spelling — and, worse, would compare a key it could describe against
// one it could not.
func TestBuildSnapshotSpellsAOneColumnKeyOneWay(t *testing.T) {
	onColumn := pg.NewTable("users")
	pg.Add(onColumn, pg.BigSerial("id").PrimaryKey())
	pg.Add(onColumn, pg.Text("name").NotNull())

	onTable := pg.NewTable("users")
	pg.Add(onTable, pg.BigSerial("id"))
	pg.Add(onTable, pg.Text("name").NotNull())
	onTable.PrimaryKey(onTable.Col("id"))

	a := pg.BuildSnapshot(pg.NewSchema(onColumn)).Tables["public.users"]
	b := pg.BuildSnapshot(pg.NewSchema(onTable)).Tables["public.users"]

	if !reflect.DeepEqual(a.CompositePrimaryKeys, b.CompositePrimaryKeys) {
		t.Errorf("the two spellings recorded different keys:\n  on the column: %+v\n  on the table:  %+v",
			a.CompositePrimaryKeys["usersIdPk"], b.CompositePrimaryKeys["usersIdPk"])
	}
	if !reflect.DeepEqual(a.Columns["id"], b.Columns["id"]) {
		t.Errorf("the two spellings recorded different columns:\n  %+v\n  %+v", a.Columns["id"], b.Columns["id"])
	}
	if pk := a.CompositePrimaryKeys["usersIdPk"]; pk == nil || !reflect.DeepEqual(pk.Columns, []string{"id"}) {
		t.Errorf("a one-column key did not reach CompositePrimaryKeys: %+v", a.CompositePrimaryKeys)
	}
	if !a.Columns["id"].PrimaryKey {
		t.Error("a one-column key should still be spelled on the column too, for drizzle-kit and drops pull")
	}
}

// Narrowing (orgId, userId) to (userId). The DROP CONSTRAINT was
// emitted on its own and the table lost its key for good.
func TestDiffNarrowsAPrimaryKeyToOneColumn(t *testing.T) {
	before := pg.NewTable("memberships")
	pg.Add(before, pg.BigInt("orgId").NotNull())
	pg.Add(before, pg.BigInt("userId").NotNull())
	before.PrimaryKey(before.Col("orgId"), before.Col("userId"))

	after := pg.NewTable("memberships")
	pg.Add(after, pg.BigInt("orgId").NotNull())
	pg.Add(after, pg.BigInt("userId").NotNull())
	after.PrimaryKey(after.Col("userId"))

	stmts := pg.Diff(pg.BuildSnapshot(pg.NewSchema(before)), pg.BuildSnapshot(pg.NewSchema(after)))
	if got := stmtsMatching(stmts, `DROP CONSTRAINT "membershipsOrgIdUserIdPk"`); len(got) != 1 {
		t.Errorf("the wide key was not dropped: %v", stmts)
	}
	want := `ALTER TABLE "memberships" ADD CONSTRAINT "membershipsUserIdPk" PRIMARY KEY ("userId");`
	if got := stmtsMatching(stmts, want); len(got) != 1 {
		t.Errorf("the narrow key was never added: %v", stmts)
	}
}

// The other direction, which failed at the server rather than
// silently: the ADD arrived with no DROP in front of it and PostgreSQL
// answered "multiple primary keys for table are not allowed".
func TestDiffWidensAPrimaryKeyFromOneColumn(t *testing.T) {
	before := pg.NewTable("seats")
	pg.Add(before, pg.BigInt("id").PrimaryKey())
	pg.Add(before, pg.BigInt("tenantId").NotNull())

	after := pg.NewTable("seats")
	pg.Add(after, pg.BigInt("id").NotNull())
	pg.Add(after, pg.BigInt("tenantId").NotNull())
	after.PrimaryKey(after.Col("tenantId"), after.Col("id"))

	stmts := pg.Diff(pg.BuildSnapshot(pg.NewSchema(before)), pg.BuildSnapshot(pg.NewSchema(after)))
	drop := stmtsMatching(stmts, "DROP CONSTRAINT", "seatsIdPk")
	add := stmtsMatching(stmts, "ADD CONSTRAINT", "PRIMARY KEY")
	if len(drop) != 1 || len(add) != 1 {
		t.Fatalf("widening the key should drop the old one and add the new: %v", stmts)
	}
	if indexOf(stmts, drop[0]) > indexOf(stmts, add[0]) {
		t.Errorf("the DROP has to come first, or PostgreSQL refuses the second key: %v", stmts)
	}
}

// A key PostgreSQL named and the same key drops names are the same
// key. Nothing about widening the map to one-column keys may change
// that: matching on the name would have every push against a
// hand-made table drop and recreate its key.
func TestDiffMatchesAOneColumnKeyByItsColumns(t *testing.T) {
	serverNamed := &pg.Snapshot{Tables: map[string]*pg.TableSnapshot{
		"public.users": {
			Name:    "users",
			Columns: map[string]*pg.ColumnSnapshot{"id": {Name: "id", Type: "bigserial", NotNull: true}},
			CompositePrimaryKeys: map[string]*pg.CompositePKSnapshot{
				"users_pkey": {Name: "users_pkey", Columns: []string{"id"}},
			},
		},
	}}

	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	stmts := pg.Diff(serverNamed, pg.BuildSnapshot(pg.NewSchema(users)))
	if got := stmtsMatching(stmts, "PRIMARY KEY"); len(got) != 0 {
		t.Errorf("the same key under the server's name produced %v", got)
	}
	if got := stmtsMatching(stmts, "DROP CONSTRAINT"); len(got) != 0 {
		t.Errorf("the same key under the server's name produced %v", got)
	}
}

// A snapshot drizzle-kit wrote spells a one-column key only on the
// column. drops reads that back rather than seeing a table with no key
// and stating the declared one afresh.
func TestDiffReadsAKeySpelledOnlyOnTheColumn(t *testing.T) {
	drizzleWritten := &pg.Snapshot{Tables: map[string]*pg.TableSnapshot{
		"public.users": {
			Name:    "users",
			Columns: map[string]*pg.ColumnSnapshot{"id": {Name: "id", Type: "bigserial", PrimaryKey: true, NotNull: true}},
		},
	}}

	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	if got := pg.Diff(drizzleWritten, pg.BuildSnapshot(pg.NewSchema(users))); len(got) != 0 {
		t.Errorf("a key spelled the other way was read as a change: %v", got)
	}
}

// ----------------------------------------------------------------------
// A UNIQUE that was re-scoped under the name it already had
// ----------------------------------------------------------------------

func TestDiffRescopesAUniqueConstraint(t *testing.T) {
	before := pg.NewTable("accounts")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())
	pg.Add(before, pg.BigInt("orgId").NotNull())
	before.AddUnique("accountsEmailUnique", before.Col("email"))

	after := pg.NewTable("accounts")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("email").NotNull())
	pg.Add(after, pg.BigInt("orgId").NotNull())
	after.AddUnique("accountsEmailUnique", after.Col("email"), after.Col("orgId"))

	stmts := pg.Diff(pg.BuildSnapshot(pg.NewSchema(before)), pg.BuildSnapshot(pg.NewSchema(after)))
	drop := stmtsMatching(stmts, `DROP CONSTRAINT "accountsEmailUnique"`)
	add := stmtsMatching(stmts, `ADD CONSTRAINT "accountsEmailUnique" UNIQUE("email", "orgId")`)
	if len(drop) != 1 || len(add) != 1 {
		t.Fatalf("re-scoping a UNIQUE produced %v", stmts)
	}
	if indexOf(stmts, drop[0]) > indexOf(stmts, add[0]) {
		t.Errorf("the DROP has to come first: %v", stmts)
	}
}

func TestDiffLeavesAnUnchangedUniqueAlone(t *testing.T) {
	build := func() *pg.Snapshot {
		t1 := pg.NewTable("accounts")
		pg.Add(t1, pg.BigSerial("id").PrimaryKey())
		pg.Add(t1, pg.Text("email").NotNull())
		pg.Add(t1, pg.BigInt("orgId").NotNull())
		t1.AddUnique("accountsEmailUnique", t1.Col("email"), t1.Col("orgId"))
		return pg.BuildSnapshot(pg.NewSchema(t1))
	}
	if got := pg.Diff(build(), build()); len(got) != 0 {
		t.Errorf("an unchanged UNIQUE produced %v", got)
	}
}

// ----------------------------------------------------------------------
// A foreign key whose referential action moved
// ----------------------------------------------------------------------

func TestDiffChangesAForeignKeyAction(t *testing.T) {
	build := func(action string) *pg.Snapshot {
		orgs := pg.NewTable("orgs")
		orgID := pg.Add(orgs, pg.BigSerial("id").PrimaryKey())
		members := pg.NewTable("members")
		pg.Add(members, pg.BigSerial("id").PrimaryKey())
		pg.Add(members, pg.BigInt("orgId").NotNull().References(orgID, pg.OnDelete(action)))
		return pg.BuildSnapshot(pg.NewSchema(orgs, members))
	}

	stmts := pg.Diff(build("CASCADE"), build("RESTRICT"))
	drop := stmtsMatching(stmts, `DROP CONSTRAINT "membersOrgIdOrgsIdFk"`)
	add := stmtsMatching(stmts, `ADD CONSTRAINT "membersOrgIdOrgsIdFk"`, "ON DELETE restrict")
	if len(drop) != 1 || len(add) != 1 {
		t.Fatalf("changing ON DELETE produced %v", stmts)
	}
	if indexOf(stmts, drop[0]) > indexOf(stmts, add[0]) {
		t.Errorf("the DROP has to come first: %v", stmts)
	}
	if got := pg.Diff(build("CASCADE"), build("CASCADE")); len(got) != 0 {
		t.Errorf("an unchanged foreign key produced %v", got)
	}
}

// SchemaTo is recorded by both producers and spelled differently by
// each: Introspect fills it from the catalogue, BuildSnapshot from the
// target Table, which is empty when the schema never named one.
// Comparing it would churn every key on the default schema.
func TestDiffIgnoresAnUnspelledForeignKeySchema(t *testing.T) {
	introspected := &pg.Snapshot{Tables: map[string]*pg.TableSnapshot{
		"public.members": {
			Name: "members",
			ForeignKeys: map[string]*pg.ForeignKeySnapshot{
				"membersOrgIdOrgsIdFk": {
					Name: "membersOrgIdOrgsIdFk", TableFrom: "members",
					ColumnsFrom: []string{"orgId"}, TableTo: "orgs", SchemaTo: "public",
					ColumnsTo: []string{"id"}, OnDelete: "cascade", OnUpdate: "no action",
				},
			},
		},
	}}
	declared := &pg.Snapshot{Tables: map[string]*pg.TableSnapshot{
		"public.members": {
			Name: "members",
			ForeignKeys: map[string]*pg.ForeignKeySnapshot{
				"membersOrgIdOrgsIdFk": {
					Name: "membersOrgIdOrgsIdFk", TableFrom: "members",
					ColumnsFrom: []string{"orgId"}, TableTo: "orgs", SchemaTo: "",
					ColumnsTo: []string{"id"}, OnDelete: "cascade", OnUpdate: "no action",
				},
			},
		},
	}}
	if got := pg.Diff(introspected, declared); len(got) != 0 {
		t.Errorf("the same key with the schema spelled out produced %v", got)
	}
}

// ----------------------------------------------------------------------
// A sequence whose attributes moved
// ----------------------------------------------------------------------

func TestDiffAltersAChangedSequence(t *testing.T) {
	build := func(opts pg.SequenceOptions) *pg.Snapshot {
		return pg.BuildSnapshot(pg.NewSchema().AddSequence(pg.NewSequence("orderNo", opts)))
	}
	stmts := pg.Diff(
		build(pg.SequenceOptions{Increment: ptr(int64(1))}),
		build(pg.SequenceOptions{Increment: ptr(int64(10)), Cycle: true}),
	)
	if len(stmts) != 1 {
		t.Fatalf("a moved sequence produced %v", stmts)
	}
	want := `ALTER SEQUENCE "orderNo" INCREMENT BY 10 CYCLE;`
	if stmts[0] != want {
		t.Errorf("got  %s\nwant %s", stmts[0], want)
	}
}

// An attribute spelled out on one side and left to the server on the
// other is the same sequence. Introspect records nothing for a value
// PostgreSQL would have picked anyway, so comparing the pointers would
// have restated every sequence declared START WITH 1 on every push.
func TestDiffLeavesADefaultedSequenceAlone(t *testing.T) {
	spelledOut := pg.BuildSnapshot(pg.NewSchema().AddSequence(pg.NewSequence("orderNo",
		pg.SequenceOptions{Start: ptr(int64(1)), Increment: ptr(int64(1)), Cache: ptr(int64(1))})))
	introspected := pg.BuildSnapshot(pg.NewSchema().AddSequence(pg.NewSequence("orderNo")))

	if got := pg.Diff(introspected, spelledOut); len(got) != 0 {
		t.Errorf("a sequence carrying the server's own defaults produced %v", got)
	}
	if got := pg.Diff(spelledOut, introspected); len(got) != 0 {
		t.Errorf("a sequence carrying the server's own defaults produced %v", got)
	}
}

// ----------------------------------------------------------------------
// Views around the column work
// ----------------------------------------------------------------------

func TestDiffRebuildsASurvivingViewAroundAColumnDrop(t *testing.T) {
	before := pg.NewTable("docs")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("legacy"))
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewView("docsAll", `SELECT "id" FROM "docs"`))

	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewView("docsAll", `SELECT "id" FROM "docs"`))

	stmts := pg.Diff(pg.BuildSnapshot(beforeSchema), pg.BuildSnapshot(afterSchema))
	drop := stmtsMatching(stmts, `DROP VIEW "docsAll"`)
	create := stmtsMatching(stmts, `CREATE VIEW "docsAll"`)
	dropCol := stmtsMatching(stmts, "DROP COLUMN")
	if len(drop) != 1 || len(create) != 1 || len(dropCol) != 1 {
		t.Fatalf("a view over a table losing a column produced %v", stmts)
	}
	if indexOf(stmts, drop[0]) > indexOf(stmts, dropCol[0]) {
		t.Errorf("the view has to come down before the column does: %v", stmts)
	}
	if indexOf(stmts, create[0]) < indexOf(stmts, dropCol[0]) {
		t.Errorf("the view has to go back up after the column has gone: %v", stmts)
	}
}

// The rebuild is the price of a column drop, not of every migration.
func TestDiffLeavesAViewAloneWhenNoColumnMoves(t *testing.T) {
	before := pg.NewTable("docs")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewView("docsAll", `SELECT "id" FROM "docs"`))

	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("title"))
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewView("docsAll", `SELECT "id" FROM "docs"`))

	stmts := pg.Diff(pg.BuildSnapshot(beforeSchema), pg.BuildSnapshot(afterSchema))
	if got := stmtsMatching(stmts, "VIEW"); len(got) != 0 {
		t.Errorf("adding a column disturbed a view: %v", got)
	}
}

// A view is created after the view it selects from, whatever the two
// are called. Sorted by name, "aSummary" would have been created
// before the "docsAll" it reads and PostgreSQL would have refused it.
func TestDiffCreatesViewsInDependencyOrder(t *testing.T) {
	docs := pg.NewTable("docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	schema := pg.NewSchema(docs)
	schema.AddView(pg.NewView("docsAll", `SELECT "id" FROM "docs"`))
	schema.AddView(pg.NewView("aSummary", `SELECT "id" FROM "docsAll"`))

	stmts := pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(schema))
	base := stmtsMatching(stmts, `CREATE VIEW "docsAll"`)
	stacked := stmtsMatching(stmts, `CREATE VIEW "aSummary"`)
	if len(base) != 1 || len(stacked) != 1 {
		t.Fatalf("both views should be created: %v", stmts)
	}
	if indexOf(stmts, base[0]) > indexOf(stmts, stacked[0]) {
		t.Errorf("a view was created before the one it selects from: %v", stmts)
	}
}

// ----------------------------------------------------------------------
// What DetectDrift could not say
// ----------------------------------------------------------------------

// DetectDrift is two Diffs, so a change Diff could not see was one it
// could not report either: a database enforcing UNIQUE(email) under a
// schema declaring UNIQUE(email, orgId) came back in sync.
func TestDetectDriftSeesARescopedUnique(t *testing.T) {
	build := func(cols ...string) *pg.Snapshot {
		t1 := pg.NewTable("accounts")
		pg.Add(t1, pg.BigSerial("id").PrimaryKey())
		pg.Add(t1, pg.Text("email").NotNull())
		pg.Add(t1, pg.BigInt("orgId").NotNull())
		refs := make([]pg.ColRef, len(cols))
		for i, c := range cols {
			refs[i] = t1.Col(c)
		}
		t1.AddUnique("accountsEmailUnique", refs...)
		return pg.BuildSnapshot(pg.NewSchema(t1))
	}

	report := pg.DetectDrift(build("email", "orgId"), build("email"))
	if report.InSync {
		t.Error("a database enforcing a narrower rule than the schema declares was reported in sync")
	}
	if !report.HasPendingMigrations() {
		t.Errorf("no statement would bring the database up to the schema: %+v", report)
	}
}
