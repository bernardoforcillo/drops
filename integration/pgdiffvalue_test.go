package integration_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// primaryKeyOf reads the columns of a table's PRIMARY KEY out of the
// catalogue, in key order, or nil when the table has none.
func primaryKeyOf(t *testing.T, db *pg.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT a.attname
		   FROM pg_constraint c
		   JOIN unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		  WHERE c.contype = 'p'
		    AND c.conrelid = ('public.' || quote_ident($1))::regclass
		  ORDER BY k.ord`, table)
	if err != nil {
		t.Fatalf("read primary key of %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// uniqueColumnsOf reads the columns spanned by a named UNIQUE
// constraint, sorted, or nil when there is no such constraint.
func uniqueColumnsOf(t *testing.T, db *pg.DB, table, constraint string) []string {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT a.attname
		   FROM pg_constraint c
		   JOIN unnest(c.conkey) AS k(attnum) ON true
		   JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		  WHERE c.contype = 'u' AND c.conname = $2
		    AND c.conrelid = ('public.' || quote_ident($1))::regclass`, table, constraint)
	if err != nil {
		t.Fatalf("read unique %s on %s: %v", constraint, table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// deleteRuleOf reads the ON DELETE action of a named foreign key.
func deleteRuleOf(t *testing.T, db *pg.DB, table, constraint string) string {
	t.Helper()
	return scalar(t, db,
		`SELECT c.confdeltype::text FROM pg_constraint c
		  WHERE c.contype = 'f' AND c.conname = $2
		    AND c.conrelid = ('public.' || quote_ident($1))::regclass`, table, constraint)
}

// ITEM 1 -------------------------------------------------------------

func TestPGPushNarrowsAKeyToOneColumn(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("memberships")
	pg.Add(before, pg.BigInt("orgId").NotNull())
	pg.Add(before, pg.BigInt("userId").NotNull())
	before.PrimaryKey(before.Col("orgId"), before.Col("userId"))
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("memberships")
	pg.Add(after, pg.BigInt("orgId").NotNull())
	pg.Add(after, pg.BigInt("userId").NotNull())
	after.PrimaryKey(after.Col("userId"))
	pushDeps(t, db, pg.NewSchema(after))

	if got := primaryKeyOf(t, db, "memberships"); len(got) != 1 || got[0] != "userId" {
		t.Fatalf(`primary key of memberships = %v, want ["userId"]`, got)
	}
}

func TestPGPushWidensAKeyFromOneColumn(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("seats")
	pg.Add(before, pg.BigInt("id").NotNull())
	pg.Add(before, pg.BigInt("tenantId").NotNull())
	before.PrimaryKey(before.Col("id"))
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("seats")
	pg.Add(after, pg.BigInt("id").NotNull())
	pg.Add(after, pg.BigInt("tenantId").NotNull())
	after.PrimaryKey(after.Col("tenantId"), after.Col("id"))
	pushDeps(t, db, pg.NewSchema(after))

	got := primaryKeyOf(t, db, "seats")
	if len(got) != 2 || got[0] != "tenantId" || got[1] != "id" {
		t.Fatalf(`primary key of seats = %v, want ["tenantId" "id"]`, got)
	}
}

// The third arity, and the one a bool is least able to describe: a key
// one column wide that moves to a different column. Neither side is
// composite, so before the fix the whole change lived in two bools
// that Diff never read and the key stayed where it was.
func TestPGPushMovesAOneColumnKeySideways(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("badges")
	pg.Add(before, pg.BigInt("legacyId").NotNull())
	pg.Add(before, pg.BigInt("code").NotNull())
	before.PrimaryKey(before.Col("legacyId"))
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("badges")
	pg.Add(after, pg.BigInt("legacyId").NotNull())
	pg.Add(after, pg.BigInt("code").NotNull())
	after.PrimaryKey(after.Col("code"))
	pushDeps(t, db, pg.NewSchema(after))

	if got := primaryKeyOf(t, db, "badges"); len(got) != 1 || got[0] != "code" {
		t.Fatalf(`primary key of badges = %v, want ["code"]`, got)
	}
}

// ITEM 2 -------------------------------------------------------------

func TestPGPushRescopesAUniqueConstraint(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("accounts")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())
	pg.Add(before, pg.BigInt("orgId").NotNull())
	before.AddUnique("accountsEmailUnique", before.Col("email"))
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("accounts")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("email").NotNull())
	pg.Add(after, pg.BigInt("orgId").NotNull())
	after.AddUnique("accountsEmailUnique", after.Col("email"), after.Col("orgId"))
	pushDeps(t, db, pg.NewSchema(after))

	got := uniqueColumnsOf(t, db, "accounts", "accountsEmailUnique")
	if len(got) != 2 || got[0] != "email" || got[1] != "orgId" {
		t.Fatalf(`accountsEmailUnique spans %v, want ["email" "orgId"]`, got)
	}
}

// ITEM 3 -------------------------------------------------------------

func TestPGPushChangesAForeignKeyAction(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	orgs := pg.NewTable("orgs")
	orgID := pg.Add(orgs, pg.BigSerial("id").PrimaryKey())
	members := pg.NewTable("members")
	pg.Add(members, pg.BigSerial("id").PrimaryKey())
	pg.Add(members, pg.BigInt("orgId").NotNull().References(orgID, pg.OnDelete("CASCADE")))
	seedBefore(t, db, pg.NewSchema(orgs, members))

	orgs2 := pg.NewTable("orgs")
	orgID2 := pg.Add(orgs2, pg.BigSerial("id").PrimaryKey())
	members2 := pg.NewTable("members")
	pg.Add(members2, pg.BigSerial("id").PrimaryKey())
	pg.Add(members2, pg.BigInt("orgId").NotNull().References(orgID2, pg.OnDelete("RESTRICT")))
	pushDeps(t, db, pg.NewSchema(orgs2, members2))

	if got := deleteRuleOf(t, db, "members", "membersOrgIdOrgsIdFk"); got != "r" {
		t.Fatalf("ON DELETE action = %q, want %q (RESTRICT)", got, "r")
	}
}

// foreignKeyEqual compares both referential actions, so both need a
// test. ON UPDATE is the one nothing else here exercises.
func TestPGPushChangesAForeignKeyUpdateAction(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	teams := pg.NewTable("teams")
	teamID := pg.Add(teams, pg.BigInt("id").PrimaryKey())
	seats := pg.NewTable("teamSeats")
	pg.Add(seats, pg.BigSerial("id").PrimaryKey())
	pg.Add(seats, pg.BigInt("teamId").NotNull().References(teamID, pg.OnUpdate("CASCADE")))
	seedBefore(t, db, pg.NewSchema(teams, seats))

	teams2 := pg.NewTable("teams")
	teamID2 := pg.Add(teams2, pg.BigInt("id").PrimaryKey())
	seats2 := pg.NewTable("teamSeats")
	pg.Add(seats2, pg.BigSerial("id").PrimaryKey())
	pg.Add(seats2, pg.BigInt("teamId").NotNull().References(teamID2, pg.OnUpdate("RESTRICT")))
	pushDeps(t, db, pg.NewSchema(teams2, seats2))

	got := scalar(t, db, `SELECT c.confupdtype::text FROM pg_constraint c
	  WHERE c.contype = 'f' AND c.conname = $1
	    AND c.conrelid = 'public."teamSeats"'::regclass`, "teamSeatsTeamIdTeamsIdFk")
	if got != "r" {
		t.Fatalf("ON UPDATE action = %q, want %q (RESTRICT)", got, "r")
	}
}

// ITEM 4 -------------------------------------------------------------

// A view whose body moves in the same migration that drops the column
// it reads. The CREATE OR REPLACE that brings the view into line is
// emitted after the table DDL — it has to be, the view selects from the
// table — so the DROP COLUMN ran first and PostgreSQL refused it.
func TestPGPushDropsAColumnAChangedViewSelects(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("docs")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("title").NotNull())
	pg.Add(before, pg.Text("legacy"))
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewView("docSummary", `SELECT "id", "title", "legacy" FROM "docs"`))
	seedBefore(t, db, beforeSchema)

	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("title").NotNull())
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewView("docSummary", `SELECT "id", "title" FROM "docs"`))
	pushDeps(t, db, afterSchema)

	if cols := columnsOf(t, db, "docs"); cols["legacy"] != "" {
		t.Fatalf("docs after the push = %v", cols)
	}
	if got := viewColumnsOf(t, db, "docSummary"); len(got) != 2 || got[0] != "id" || got[1] != "title" {
		t.Fatalf(`docSummary selects %v, want ["id" "title"]`, got)
	}
}

// The worse half: the view's body did not change at all.
//
// Widening "code" from varchar(20) to text is refused while any view
// reads the column (SQLSTATE 0A000), and this view's declaration is
// word for word the one the database already holds — so the migration
// carries no statement for it and there is nothing an ordering rule
// could move. The view has to come down around the ALTER and go back
// up afterwards.
func TestPGPushRetypesAColumnAnUnchangedViewSelects(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	const body = `SELECT "id", "code" FROM "parts"`

	before := pg.NewTable("parts")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Varchar("code", 20).NotNull())
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewView("partCodes", body))
	seedBefore(t, db, beforeSchema)

	after := pg.NewTable("parts")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("code").NotNull())
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewView("partCodes", body))
	pushDeps(t, db, afterSchema)

	if cols := columnsOf(t, db, "parts"); cols["code"] != "text" {
		t.Fatalf("parts after the push = %v", cols)
	}
	if got := viewColumnsOf(t, db, "partCodes"); len(got) != 2 || got[0] != "id" || got[1] != "code" {
		t.Fatalf(`partCodes selects %v, want ["id" "code"]`, got)
	}
}

// A view built on another view. Dropping the pair takes them in
// whichever order CASCADE settles, but building them again has one
// order and only one: the view that is selected from goes first.
// Sorting by name would have got this backwards — "aSummary" reads
// "partCodes" and sorts ahead of it.
func TestPGPushRebuildsViewsOnViewsInDependencyOrder(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	base := `SELECT "id", "code" FROM "parts"`
	stacked := `SELECT "id" FROM "partCodes"`

	before := pg.NewTable("parts")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Varchar("code", 20).NotNull())
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewView("partCodes", base))
	beforeSchema.AddView(pg.NewView("aSummary", stacked))
	seedBefore(t, db, beforeSchema)

	after := pg.NewTable("parts")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("code").NotNull())
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewView("partCodes", base))
	afterSchema.AddView(pg.NewView("aSummary", stacked))
	pushDeps(t, db, afterSchema)

	if cols := columnsOf(t, db, "parts"); cols["code"] != "text" {
		t.Fatalf("parts after the push = %v", cols)
	}
	for _, name := range []string{"partCodes", "aSummary"} {
		if got := scalar(t, db,
			`SELECT count(*)::text FROM pg_views WHERE schemaname='public' AND viewname=$1`, name); got != "1" {
			t.Fatalf("view %q did not survive the push (count %s)", name, got)
		}
	}
}

// A materialised view blocks a column drop exactly as a plain one
// does, and is rebuilt the same way — with its rows recomputed, which
// is the cost the Diff doc comment names.
//
// The view has to read the column that is going, or it blocks
// nothing: an earlier spelling of this test selected only the columns
// that survive, so the DROP COLUMN went through with or without the
// rebuild and the only thing the assertion could still catch was the
// repopulation. It reads "legacy" now, which is what makes the first
// assertion mean what its name says.
func TestPGPushDropsAColumnAMaterializedViewSelects(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("docs")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("title").NotNull())
	pg.Add(before, pg.Text("legacy"))
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewMaterializedView("docTitlesMat",
		`SELECT "id", "title", "legacy" FROM "docs"`))
	seedBefore(t, db, beforeSchema)
	if _, err := db.Exec(ctx, `INSERT INTO docs ("title", "legacy") VALUES ('a', 'x')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("title").NotNull())
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewMaterializedView("docTitlesMat", `SELECT "id", "title" FROM "docs"`))
	pushDeps(t, db, afterSchema)

	if cols := columnsOf(t, db, "docs"); cols["legacy"] != "" {
		t.Fatalf("docs after the push = %v", cols)
	}
	if got := viewColumnsOf(t, db, "docTitlesMat"); len(got) != 2 || got[0] != "id" || got[1] != "title" {
		t.Fatalf(`docTitlesMat selects %v, want ["id" "title"]`, got)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM "docTitlesMat"`); got != "1" {
		t.Fatalf("the matview was not repopulated by its rebuild: count = %s", got)
	}
}

// A migration that touches no column leaves the views alone. The
// rebuild is the price of a column drop, not of every push.
func TestPGPushLeavesViewsAloneWhenNoColumnMoves(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	const body = `SELECT "id", "title" FROM "docs"`

	before := pg.NewTable("docs")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("title").NotNull())
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewView("docTitles", body))
	seedBefore(t, db, beforeSchema)

	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("title").NotNull())
	pg.Add(after, pg.Text("subtitle"))
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewView("docTitles", body))

	plan, err := pg.Push(context.Background(), db, afterSchema, pg.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("planning the push: %v", err)
	}
	for _, s := range plan.Statements {
		if strings.Contains(s, "VIEW") {
			t.Fatalf("adding a column rebuilt a view it could not have disturbed:\n  %s",
				strings.Join(plan.Statements, "\n  "))
		}
	}
}

// viewColumnsOf reads the column names a view exposes, in order.
func viewColumnsOf(t *testing.T, db *pg.DB, view string) []string {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT a.attname FROM pg_attribute a
		  WHERE a.attrelid = ('public.' || quote_ident($1))::regclass
		    AND a.attnum > 0 AND NOT a.attisdropped
		  ORDER BY a.attnum`, view)
	if err != nil {
		t.Fatalf("read columns of view %s: %v", view, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// ITEM 2 AND 3, THE SAME SHAPE ELSEWHERE ------------------------------

// A sequence's attributes were the third thing the snapshot recorded
// on both sides and the diff read on neither.
func TestPGPushAltersAChangedSequence(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	one, ten := int64(1), int64(10)
	before := pg.NewSchema().AddSequence(pg.NewSequence("orderNo", pg.SequenceOptions{Increment: &one}))
	seedBefore(t, db, before)

	after := pg.NewSchema().AddSequence(pg.NewSequence("orderNo",
		pg.SequenceOptions{Increment: &ten, Cycle: true}))
	pushDeps(t, db, after)

	if got := scalar(t, db,
		`SELECT seqincrement::text FROM pg_sequence WHERE seqrelid = 'public."orderNo"'::regclass`); got != "10" {
		t.Fatalf("INCREMENT BY = %s, want 10", got)
	}
	if got := scalar(t, db,
		`SELECT seqcycle::text FROM pg_sequence WHERE seqrelid = 'public."orderNo"'::regclass`); got != "true" {
		t.Fatalf("CYCLE = %s, want true", got)
	}

	// And a second push of the same declaration is a no-op: what the
	// catalogue reports for an attribute nobody named has to compare
	// equal to the declaration that left it unnamed.
	plan, err := pg.Push(context.Background(), db, after, pg.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("planning the second push: %v", err)
	}
	if len(plan.Statements) != 0 {
		t.Fatalf("the sequence churns on every push: %v", plan.Statements)
	}
}

// The other side of "no push restarts a live sequence": a declaration
// that raises MINVALUE above the value the sequence is sitting on
// cannot be applied without moving it, so PostgreSQL refuses the ALTER
// (22023) and the push rolls back whole rather than resetting the
// sequence behind the caller.
//
// It is worth pinning because the alternative is not a no-op. Before
// the attributes were compared at all this push reported success and
// changed nothing, which is the failure mode the slice existed to
// close; a refusal that names the sequence and both numbers is the
// honest answer, and the sequence going on handing out the values it
// was handing out is the guarantee.
func TestPGPushRefusesToRestartALiveSequence(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	low, high := int64(5), int64(100)
	before := pg.NewSchema().AddSequence(pg.NewSequence("ticketNo",
		pg.SequenceOptions{Start: &low, MinValue: &low, MaxValue: &high}))
	seedBefore(t, db, before)
	if got := scalar(t, db, `SELECT nextval('"ticketNo"')::text`); got != "5" {
		t.Fatalf("the seeded sequence starts at %s, want 5", got)
	}

	newLow, newHigh := int64(1000), int64(20000)
	after := pg.NewSchema().AddSequence(pg.NewSequence("ticketNo",
		pg.SequenceOptions{Start: &newLow, MinValue: &newLow, MaxValue: &newHigh}))

	_, err := pg.Push(ctx, db, after)
	if err == nil {
		t.Fatal("the push went through; raising MINVALUE past the sequence cannot be done without a RESTART")
	}
	if !strings.Contains(err.Error(), "ticketNo") {
		t.Errorf("the refusal does not name the sequence: %v", err)
	}
	if got := scalar(t, db,
		`SELECT seqmin::text FROM pg_sequence WHERE seqrelid = 'public."ticketNo"'::regclass`); got != "5" {
		t.Fatalf("the refused push left the sequence changed: seqmin = %s", got)
	}
	if got := scalar(t, db, `SELECT nextval('"ticketNo"')::text`); got != "6" {
		t.Fatalf("the sequence was restarted anyway: next value = %s, want 6", got)
	}
}

// A view selecting from another view has one creation order. Nothing
// in the schema says which comes first — it has to be read out of the
// bodies — and sorted by name this pair is the wrong way round.
func TestPGPushCreatesViewsOnViewsInOrder(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	docs := pg.NewTable("docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	schema := pg.NewSchema(docs)
	schema.AddView(pg.NewView("docsAll", `SELECT "id" FROM "docs"`))
	schema.AddView(pg.NewView("aSummary", `SELECT "id" FROM "docsAll"`))

	pushDeps(t, db, schema)

	for _, name := range []string{"docsAll", "aSummary"} {
		if got := scalar(t, db,
			`SELECT count(*)::text FROM pg_views WHERE schemaname='public' AND viewname=$1`, name); got != "1" {
			t.Fatalf("view %q was not created (count %s)", name, got)
		}
	}
}

// A table PostgreSQL named the key on — an inline PRIMARY KEY, so
// "handmade_pkey" — is not work. Recording a one-column key in
// CompositePrimaryKeys widened what Diff compares; matching on the
// name would have every push against such a table drop and rebuild
// its key.
func TestPGPushLeavesAServerNamedKeyAlone(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	if _, err := db.Exec(ctx, `CREATE TABLE "handmade" ("id" bigint PRIMARY KEY, "name" text NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	declared := pg.NewTable("handmade")
	pg.Add(declared, pg.BigInt("id").PrimaryKey())
	pg.Add(declared, pg.Text("name").NotNull())

	plan, err := pg.Push(ctx, db, pg.NewSchema(declared), pg.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("planning the push: %v", err)
	}
	if len(plan.Statements) != 0 {
		t.Fatalf("a key the server named was read as a change: %v", plan.Statements)
	}
	if got := primaryKeyOf(t, db, "handmade"); len(got) != 1 || got[0] != "id" {
		t.Fatalf("primary key of handmade = %v", got)
	}
}

// A view the Go schema never declared, selecting from one it did.
//
// The rebuild takes down the declared view around the column drop, and
// the undeclared one stands on it. Dropping with CASCADE would take it
// too — silently, for good, and against the rule that Push does not
// drop what the schema never claimed — so the drop is plain and
// PostgreSQL refuses it by name. The push rolls back whole: the column
// is still there and so is the view nobody declared.
func TestPGPushRefusesToCascadeAwayAnUndeclaredView(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("docs")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("title").NotNull())
	pg.Add(before, pg.Text("legacy"))
	beforeSchema := pg.NewSchema(before)
	beforeSchema.AddView(pg.NewView("docTitles", `SELECT "id", "title", "legacy" FROM "docs"`))
	seedBefore(t, db, beforeSchema)

	// Built by hand, as a DBA would, on top of the declared view.
	if _, err := db.Exec(ctx, `CREATE VIEW "dbaReport" AS SELECT "id" FROM "docTitles"`); err != nil {
		t.Fatalf("create the undeclared view: %v", err)
	}

	after := pg.NewTable("docs")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("title").NotNull())
	afterSchema := pg.NewSchema(after)
	afterSchema.AddView(pg.NewView("docTitles", `SELECT "id", "title" FROM "docs"`))

	_, err := pg.Push(ctx, db, afterSchema)
	if err == nil {
		t.Fatal("the push went through; the undeclared view should have stopped it")
	}
	if !strings.Contains(err.Error(), "dbaReport") {
		t.Errorf("the refusal does not name the view standing in the way: %v", err)
	}
	if got := scalar(t, db,
		`SELECT count(*)::text FROM pg_views WHERE schemaname='public' AND viewname='dbaReport'`); got != "1" {
		t.Fatalf("the undeclared view was destroyed anyway (count %s)", got)
	}
	if cols := columnsOf(t, db, "docs"); cols["legacy"] == "" {
		t.Fatal("the push was not rolled back whole")
	}
}
