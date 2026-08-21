package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// The order the drops come out in.
//
// PostgreSQL removes a dependent object along with the thing it
// depends on, and refuses to remove the thing while a dependent it
// cannot remove stands — so a migration that drops a column and
// something naming it has exactly one working order. The live proof is
// in integration/pgdropdeps_test.go, where a server parses the
// statements; this is the cheap guard that catches a regrouping of
// Diff's loops before anybody starts a database.

// indexOf reports where the statement containing sub appears, or -1.
func indexOf(stmts []string, sub string) int {
	for i, s := range stmts {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}

// before fails unless the statement containing first is emitted ahead
// of the one containing second, both being present.
func before(t *testing.T, stmts []string, first, second string) {
	t.Helper()
	i, j := indexOf(stmts, first), indexOf(stmts, second)
	if i < 0 {
		t.Fatalf("no statement contains %q:\n  %s", first, strings.Join(stmts, "\n  "))
	}
	if j < 0 {
		t.Fatalf("no statement contains %q:\n  %s", second, strings.Join(stmts, "\n  "))
	}
	if i > j {
		t.Errorf("%q comes after %q:\n  %s", first, second, strings.Join(stmts, "\n  "))
	}
}

func TestDiffDropsDependentsBeforeTheirColumn(t *testing.T) {
	orgs := pg.NewTable("orgs")
	orgCode := pg.Add(orgs, pg.Text("code").NotNull().Unique())
	pg.Add(orgs, pg.Text("name").NotNull())

	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("email").NotNull().Unique())
	age := pg.Add(users, pg.Integer("age"))
	pg.Add(users, pg.Text("orgCode").NotNull().References(orgCode))
	users.AddCheck("usersAgeCheck", `"age" >= 0`)
	users.AddIndex(pg.NewIndex("usersAgeIdx", users, age))
	users.EnableRLS()
	users.AddPolicy(pg.NewPolicy("usersSelf").Using(`"email" = CURRENT_USER`))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	orgsAfter := pg.NewTable("orgs")
	pg.Add(orgsAfter, pg.Text("name").NotNull())
	after.EnableRLS()

	stmts := pg.Diff(
		pg.BuildSnapshot(pg.NewSchema(users, orgs)),
		pg.BuildSnapshot(pg.NewSchema(after, orgsAfter)),
	)

	// Everything that names users.email, users.age or users.orgCode
	// has to be gone before the column is.
	for _, dependent := range []string{
		`DROP POLICY "usersSelf"`,
		`DROP CONSTRAINT "usersEmailUnique"`,
		`DROP CONSTRAINT "usersAgeCheck"`,
		`DROP INDEX "usersAgeIdx"`,
		`DROP CONSTRAINT "usersOrgCodeOrgsCodeFk"`,
	} {
		before(t, stmts, dependent, `ALTER TABLE "users" DROP COLUMN`)
	}
	// And the foreign key has to be gone before the column it points
	// at on the other table, which no per-table pass would order.
	before(t, stmts, `DROP CONSTRAINT "usersOrgCodeOrgsCodeFk"`, `ALTER TABLE "orgs" DROP COLUMN "code"`)
	before(t, stmts, `DROP CONSTRAINT "usersOrgCodeOrgsCodeFk"`, `DROP CONSTRAINT "orgsCodeUnique"`)
}

// A table going away is emitted CASCADE, which takes the foreign keys
// pointing at it from tables that survive — so the DROP CONSTRAINT for
// one of those has to come first or it names something already gone.
func TestDiffDropsAForeignKeyBeforeTheTableItPointsAt(t *testing.T) {
	legacy := pg.NewTable("legacy")
	legacyID := pg.Add(legacy, pg.BigSerial("id").PrimaryKey())
	notes := pg.NewTable("notes")
	pg.Add(notes, pg.BigSerial("id").PrimaryKey())
	pg.Add(notes, pg.BigInt("legacyId").References(legacyID))

	notesAfter := pg.NewTable("notes")
	pg.Add(notesAfter, pg.BigSerial("id").PrimaryKey())
	pg.Add(notesAfter, pg.BigInt("legacyId"))

	stmts := pg.Diff(
		pg.BuildSnapshot(pg.NewSchema(legacy, notes)),
		pg.BuildSnapshot(pg.NewSchema(notesAfter)),
	)
	before(t, stmts, `DROP CONSTRAINT "notesLegacyIdLegacyIdFk"`, `DROP TABLE "legacy"`)
}

// The other half of the question: a constraint whose columns all stay
// is not dropped by anything PostgreSQL does, so the statement has to
// be emitted. A fix that suppressed the drop whenever a column was
// going would still have to keep this one.
func TestDiffStillDropsAConstraintWhoseColumnsStay(t *testing.T) {
	tags := pg.NewTable("tags")
	pg.Add(tags, pg.BigSerial("id").PrimaryKey())
	pg.Add(tags, pg.Text("label").NotNull().Unique())

	after := pg.NewTable("tags")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("label").NotNull())

	stmts := pg.Diff(pg.BuildSnapshot(pg.NewSchema(tags)), pg.BuildSnapshot(pg.NewSchema(after)))
	if indexOf(stmts, `DROP CONSTRAINT "tagsLabelUnique"`) < 0 {
		t.Fatalf("the unique the schema stopped declaring is not dropped:\n  %s", strings.Join(stmts, "\n  "))
	}
	if indexOf(stmts, "DROP COLUMN") >= 0 {
		t.Errorf("nothing asked for a column to go:\n  %s", strings.Join(stmts, "\n  "))
	}
}
