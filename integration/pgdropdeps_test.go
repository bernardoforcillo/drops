package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// Dropping a column takes everything that names it along.
//
// PostgreSQL removes every constraint and every index that mentions a
// column when the column goes — whether the constraint spans that
// column alone or several of them, and whether the index names it in
// its key, its INCLUDE list, an expression or a partial predicate. A
// migration that drops the column first and names the constraint
// afterwards is therefore naming something that is already gone
// (SQLSTATE 42704), and because Push applies its statements in one
// transaction the whole push rolls back with nothing done.
//
// The other direction is a refusal rather than a stale name: a column
// another table's foreign key points at, or a policy reads, cannot be
// dropped at all while that object stands (SQLSTATE 2BP01). So the
// order that works is one order — everything that depends on a column
// goes before the column does — and it is what these tests pin down.
// Each one is written as the plain two-schema push the bug reproduces
// with, and each asserts the catalogue afterwards rather than the
// statement list, because the statement list was never the thing that
// failed.

// pushDeps applies desired to db and fails the test with the plan that
// broke when the server rejects it.
//
// The plan is computed first, in a dry run, purely so the failure can
// name the statements in the order they were about to run — which is
// the whole subject here and is not in the error the server returns.
func pushDeps(t *testing.T, db *pg.DB, desired *pg.Schema, opts ...pg.PushOptions) {
	t.Helper()
	ctx := context.Background()
	dry := pg.PushOptions{DryRun: true}
	if len(opts) > 0 {
		dry = opts[0]
		dry.DryRun = true
	}
	plan, err := pg.Push(ctx, db, desired, dry)
	if err != nil {
		t.Fatalf("planning the push: %v", err)
	}
	if _, err := pg.Push(ctx, db, desired, opts...); err != nil {
		t.Fatalf("push: %v\nthe plan was:\n  %s", err, strings.Join(plan.Statements, "\n  "))
	}
}

// seedBefore builds the "before" database with a push of its own, so every
// constraint and index wears the name drops gives it rather than the
// one PostgreSQL invents for an inline declaration. The push under
// test then names them the same way.
func seedBefore(t *testing.T, db *pg.DB, before *pg.Schema) {
	t.Helper()
	if _, err := pg.Push(context.Background(), db, before); err != nil {
		t.Fatalf("seeding the before-schema: %v", err)
	}
}

// constraintsOf reads the constraint names on a table.
func constraintsOf(t *testing.T, db *pg.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT conname, contype FROM pg_constraint
		 WHERE conrelid = ('public.' || quote_ident($1))::regclass`, table)
	if err != nil {
		t.Fatalf("read constraints of %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			t.Fatal(err)
		}
		out[name] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// indexNamesOf reads the index names on a table.
func indexNamesOf(t *testing.T, db *pg.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT indexname FROM pg_indexes WHERE schemaname = 'public' AND tablename = $1`, table)
	if err != nil {
		t.Fatalf("read indexes of %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The reported case, in the shape it was reported: a UNIQUE constraint
// on the column that is going.
func TestPGPushDropsAColumnCarryingAUnique(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull().Unique())
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pushDeps(t, db, pg.NewSchema(after))

	if cols := columnsOf(t, db, "users"); cols["email"] != "" {
		t.Fatalf("users after the push = %v", cols)
	}
	if cons := constraintsOf(t, db, "users"); cons["usersEmailUnique"] != "" {
		t.Fatalf("the unique constraint outlived its column: %v", cons)
	}
}

// A constraint spanning several columns of which only one is going.
// PostgreSQL drops such a constraint whole rather than narrowing it,
// so the statement that names it afterwards is as stale as the
// single-column case — and the surviving column keeps no uniqueness.
func TestPGPushDropsOneColumnOfAMultiColumnUnique(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("seats")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("row").NotNull())
	pg.Add(before, pg.Text("number").NotNull())
	before.AddUnique("seatsRowNumberUnique", before.Col("row"), before.Col("number"))
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("seats")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("number").NotNull())
	pushDeps(t, db, pg.NewSchema(after))

	if cols := columnsOf(t, db, "seats"); cols["row"] != "" {
		t.Fatalf("seats after the push = %v", cols)
	}
	if cons := constraintsOf(t, db, "seats"); cons["seatsRowNumberUnique"] != "" {
		t.Fatalf("the unique constraint outlived the column it spanned: %v", cons)
	}
}

// A CHECK constraint on the column that is going.
func TestPGPushDropsAColumnCarryingACheck(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("people")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Integer("age").NotNull())
	before.AddCheck("peopleAgeCheck", `"age" >= 0`)
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("people")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pushDeps(t, db, pg.NewSchema(after))

	if cols := columnsOf(t, db, "people"); cols["age"] != "" {
		t.Fatalf("people after the push = %v", cols)
	}
	if cons := constraintsOf(t, db, "people"); cons["peopleAgeCheck"] != "" {
		t.Fatalf("the check outlived its column: %v", cons)
	}
}

// An index on the column that is going. DropUnmanagedIndexes because
// the new schema declares no index at all, and Push withholds the drop
// of an index nothing declares unless told otherwise.
func TestPGPushDropsAColumnCarryingAnIndex(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("events")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("kind").NotNull())
	before.AddIndex(pg.NewIndex("eventsKindIdx", before, before.Col("kind")))
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("events")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pushDeps(t, db, pg.NewSchema(after), pg.PushOptions{DropUnmanagedIndexes: true})

	if cols := columnsOf(t, db, "events"); cols["kind"] != "" {
		t.Fatalf("events after the push = %v", cols)
	}
	if idx := indexNamesOf(t, db, "events"); idx["eventsKindIdx"] {
		t.Fatalf("the index outlived its column: %v", idx)
	}
}

// A column of a composite primary key. The key is dropped whole, and
// the migration also states the narrower key that replaces it.
func TestPGPushDropsAColumnOfACompositePrimaryKey(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("memberships")
	pg.Add(before, pg.BigInt("orgId").NotNull())
	pg.Add(before, pg.BigInt("userId").NotNull())
	pg.Add(before, pg.Text("role").NotNull())
	before.PrimaryKey(before.Col("orgId"), before.Col("userId"))
	seedBefore(t, db, pg.NewSchema(before))
	if _, err := db.Exec(ctx, `INSERT INTO memberships ("orgId", "userId", "role") VALUES (1, 2, 'owner')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	after := pg.NewTable("memberships")
	pg.Add(after, pg.BigInt("userId").NotNull())
	pg.Add(after, pg.Text("role").NotNull())
	after.PrimaryKey(after.Col("userId"), after.Col("role"))
	pushDeps(t, db, pg.NewSchema(after))

	if cols := columnsOf(t, db, "memberships"); cols["orgId"] != "" {
		t.Fatalf("memberships after the push = %v", cols)
	}
	cons := constraintsOf(t, db, "memberships")
	if cons["membershipsOrgIdUserIdPk"] != "" {
		t.Fatalf("the old key outlived the column it spanned: %v", cons)
	}
	if cons["membershipsUserIdRolePk"] != "p" {
		t.Fatalf("the new key was never added: %v", cons)
	}
}

// The column another table's foreign key points at. PostgreSQL refuses
// the drop outright while the key stands, so the child's DROP
// CONSTRAINT has to come first — across tables, which is the ordering
// no per-table pass can reach.
func TestPGPushDropsAColumnAForeignKeyPointsAt(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	orgs := pg.NewTable("orgs")
	pg.Add(orgs, pg.BigSerial("id").PrimaryKey())
	pg.Add(orgs, pg.Text("code").NotNull().Unique())
	members := pg.NewTable("members")
	pg.Add(members, pg.BigSerial("id").PrimaryKey())
	pg.Add(members, pg.Text("orgCode").NotNull())
	members.ForeignKey(members.Col("orgCode"), orgs.Col("code"))
	seedBefore(t, db, pg.NewSchema(orgs, members))

	orgsAfter := pg.NewTable("orgs")
	pg.Add(orgsAfter, pg.BigSerial("id").PrimaryKey())
	membersAfter := pg.NewTable("members")
	pg.Add(membersAfter, pg.BigSerial("id").PrimaryKey())
	pg.Add(membersAfter, pg.Text("orgCode").NotNull())
	pushDeps(t, db, pg.NewSchema(orgsAfter, membersAfter))

	if cols := columnsOf(t, db, "orgs"); cols["code"] != "" {
		t.Fatalf("orgs after the push = %v", cols)
	}
	if cons := constraintsOf(t, db, "members"); cons["membersOrgCodeOrgsCodeFk"] != "" {
		t.Fatalf("the foreign key outlived the column it pointed at: %v", cons)
	}
}

// A table going away takes the surviving table's foreign key with it:
// DROP TABLE is emitted CASCADE, so the DROP CONSTRAINT that follows
// names a constraint PostgreSQL has already removed.
func TestPGPushDropsATableAForeignKeyPointsAt(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	legacy := pg.NewTable("legacy")
	pg.Add(legacy, pg.BigSerial("id").PrimaryKey())
	notes := pg.NewTable("notes")
	pg.Add(notes, pg.BigSerial("id").PrimaryKey())
	pg.Add(notes, pg.BigInt("legacyId"))
	notes.ForeignKey(notes.Col("legacyId"), legacy.Col("id"))
	seedBefore(t, db, pg.NewSchema(legacy, notes))

	notesAfter := pg.NewTable("notes")
	pg.Add(notesAfter, pg.BigSerial("id").PrimaryKey())
	pg.Add(notesAfter, pg.BigInt("legacyId"))
	pushDeps(t, db, pg.NewSchema(notesAfter))

	if tableExists(t, db, "legacy") {
		t.Fatal("the table the schema stopped naming is still there")
	}
	if cons := constraintsOf(t, db, "notes"); cons["notesLegacyIdLegacyIdFk"] != "" {
		t.Fatalf("the foreign key outlived the table it pointed at: %v", cons)
	}
}

// A policy reading the column that is going. PostgreSQL refuses the
// drop while the policy stands, and policies were built last of all.
func TestPGPushDropsAColumnAPolicyReads(t *testing.T) {
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("documents")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("owner").NotNull())
	before.EnableRLS()
	before.AddPolicy(pg.NewPolicy("documentsOwner").Using(`"owner" = CURRENT_USER`))
	seedBefore(t, db, pg.NewSchema(before))

	after := pg.NewTable("documents")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	after.EnableRLS()
	pushDeps(t, db, pg.NewSchema(after), pg.PushOptions{DropUnmanagedObjects: true})

	if cols := columnsOf(t, db, "documents"); cols["owner"] != "" {
		t.Fatalf("documents after the push = %v", cols)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM pg_policy WHERE polname = 'documentsOwner'`); got != "0" {
		t.Fatalf("the policy outlived the column it read: count = %s", got)
	}
}

// The other half of the question: a constraint dropped while every
// column it spans stays. Nothing in PostgreSQL removes it, so the
// statement has to be emitted — a fix that suppressed the drop
// whenever a column was going would still have to keep this one.
func TestPGPushDropsAUniqueAndKeepsItsColumn(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("tags")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("label").NotNull().Unique())
	pg.Add(before, pg.Text("note"))
	seedBefore(t, db, pg.NewSchema(before))

	// The unique goes, "note" goes, "label" stays.
	after := pg.NewTable("tags")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("label").NotNull())
	pushDeps(t, db, pg.NewSchema(after))

	cols := columnsOf(t, db, "tags")
	if cols["label"] == "" || cols["note"] != "" {
		t.Fatalf("tags after the push = %v", cols)
	}
	if cons := constraintsOf(t, db, "tags"); cons["tagsLabelUnique"] != "" {
		t.Fatalf("the unique the schema stopped declaring is still there: %v", cons)
	}
	if _, err := db.Exec(ctx, `INSERT INTO tags ("label") VALUES ('x'), ('x')`); err != nil {
		t.Fatalf("the dropped unique is still being enforced: %v", err)
	}
}
