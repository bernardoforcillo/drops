package integration_test

import (
	"context"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// Every dependent shape in one migration.
//
// The tests above take the hazards one at a time, and a fix that
// grouped the drops per shape rather than per kind would still pass
// all of them: each one has a single table with a single dependent on
// it. This is the case that separates them — six kinds of dependent
// across four tables, all going in the same push, with a foreign key
// pointing from a table that survives at a column that does not and a
// table dropped out from under another key. The order has to hold
// across the whole statement list at once, not within a table.
func TestPGPushDropsEveryDependentShapeInOneMigration(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	orgs := pg.NewTable("orgs")
	pg.Add(orgs, pg.BigSerial("id").PrimaryKey())
	pg.Add(orgs, pg.Text("code").NotNull().Unique())
	pg.Add(orgs, pg.Text("name").NotNull())

	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("email").NotNull().Unique())
	age := pg.Add(users, pg.Integer("age"))
	pg.Add(users, pg.Text("a"))
	pg.Add(users, pg.Text("b"))
	pg.Add(users, pg.Text("owner").NotNull())
	pg.Add(users, pg.Text("orgCode").NotNull())
	users.AddUnique("usersAbUnique", users.Col("a"), users.Col("b"))
	users.AddCheck("usersAgeCheck", `"age" >= 0`)
	users.AddIndex(pg.NewIndex("usersAgeIdx", users, age))
	users.ForeignKey(users.Col("orgCode"), orgs.Col("code"))
	users.EnableRLS()
	users.AddPolicy(pg.NewPolicy("usersOwner").Using(`"owner" = CURRENT_USER`))

	memberships := pg.NewTable("memberships")
	pg.Add(memberships, pg.BigInt("orgId").NotNull())
	pg.Add(memberships, pg.BigInt("userId").NotNull())
	pg.Add(memberships, pg.Text("role").NotNull())
	memberships.PrimaryKey(memberships.Col("orgId"), memberships.Col("userId"), memberships.Col("role"))

	legacy := pg.NewTable("legacy")
	pg.Add(legacy, pg.BigSerial("id").PrimaryKey())
	notes := pg.NewTable("notes")
	pg.Add(notes, pg.BigSerial("id").PrimaryKey())
	pg.Add(notes, pg.BigInt("legacyId"))
	notes.ForeignKey(notes.Col("legacyId"), legacy.Col("id"))

	seedBefore(t, db, pg.NewSchema(orgs, users, memberships, legacy, notes))
	if _, err := db.Exec(ctx, `INSERT INTO memberships ("orgId", "userId", "role") VALUES (1, 2, 'owner')`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// orgs loses the column a surviving table's key points at; users
	// loses a column carrying a single-column UNIQUE, one carrying a
	// CHECK and an index, one column of a two-column UNIQUE, the
	// column a policy reads and the column the key is on; memberships
	// loses a column of its composite PRIMARY KEY; legacy goes whole.
	orgsAfter := pg.NewTable("orgs")
	pg.Add(orgsAfter, pg.BigSerial("id").PrimaryKey())
	pg.Add(orgsAfter, pg.Text("name").NotNull())

	usersAfter := pg.NewTable("users")
	pg.Add(usersAfter, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersAfter, pg.Text("b"))
	usersAfter.EnableRLS()

	membershipsAfter := pg.NewTable("memberships")
	pg.Add(membershipsAfter, pg.BigInt("userId").NotNull())
	pg.Add(membershipsAfter, pg.Text("role").NotNull())
	membershipsAfter.PrimaryKey(membershipsAfter.Col("userId"), membershipsAfter.Col("role"))

	notesAfter := pg.NewTable("notes")
	pg.Add(notesAfter, pg.BigSerial("id").PrimaryKey())
	pg.Add(notesAfter, pg.BigInt("legacyId"))

	pushDeps(t, db, pg.NewSchema(orgsAfter, usersAfter, membershipsAfter, notesAfter),
		pg.PushOptions{DropUnmanagedObjects: true})

	if cols := columnsOf(t, db, "users"); cols["email"] != "" || cols["age"] != "" ||
		cols["a"] != "" || cols["owner"] != "" || cols["orgCode"] != "" || cols["b"] == "" {
		t.Fatalf("users after the push = %v", cols)
	}
	if cols := columnsOf(t, db, "orgs"); cols["code"] != "" {
		t.Fatalf("orgs after the push = %v", cols)
	}
	// The key wears drops's name, not PostgreSQL's: seedBefore built
	// this database with a push, and a push states the PRIMARY KEY as
	// its own ALTER TABLE ADD CONSTRAINT whatever its width, so nothing
	// is left for the server to invent a name for.
	if cons := constraintsOf(t, db, "users"); len(cons) != 1 || cons["usersIdPk"] != "p" {
		t.Fatalf("users kept a constraint it stopped declaring: %v", cons)
	}
	if idx := indexNamesOf(t, db, "users"); idx["usersAgeIdx"] {
		t.Fatalf("the index outlived its column: %v", idx)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM pg_policy WHERE polname = 'usersOwner'`); got != "0" {
		t.Fatalf("the policy outlived the column it read: count = %s", got)
	}
	if cons := constraintsOf(t, db, "memberships"); cons["membershipsOrgIdUserIdRolePk"] != "" ||
		cons["membershipsUserIdRolePk"] != "p" {
		t.Fatalf("memberships after the push = %v", cons)
	}
	if tableExists(t, db, "legacy") {
		t.Fatal("the table the schema stopped naming is still there")
	}
	if cons := constraintsOf(t, db, "notes"); cons["notesLegacyIdLegacyIdFk"] != "" {
		t.Fatalf("the foreign key outlived the table it pointed at: %v", cons)
	}
}
