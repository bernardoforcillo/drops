package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// The order the drops come out in.
//
// MySQL answers a different way for each kind of dependent when the
// column under it goes — some are taken with the column, some narrowed
// and left standing, some refuse the drop outright — and every one of
// those answers is only reachable if the dependent is still there when
// the column goes. The live proof is in
// integration/mysqldropdeps_test.go, where a server parses the
// statements; this is the cheap guard that catches a regrouping of
// Diff's loops before anybody starts a database.

// stmtIndexOf reports where the statement containing sub appears, or -1.
func stmtIndexOf(stmts []string, sub string) int {
	for i, s := range stmts {
		if strings.Contains(s, sub) {
			return i
		}
	}
	return -1
}

// emittedBefore fails unless the statement containing first is emitted
// ahead of the one containing second, both being present.
func emittedBefore(t *testing.T, stmts []string, first, second string) {
	t.Helper()
	i, j := stmtIndexOf(stmts, first), stmtIndexOf(stmts, second)
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
	orgs := mysql.NewTable("orgs")
	mysql.Add(orgs, mysql.BigSerial("id").PrimaryKey())
	orgCode := mysql.Add(orgs, mysql.Varchar("code", 64).NotNull().Unique())
	mysql.Add(orgs, mysql.Varchar("name", 64).NotNull())

	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(users, mysql.Varchar("email", 190).NotNull().Unique())
	age := mysql.Add(users, mysql.Integer("age"))
	a := mysql.Add(users, mysql.Integer("a"))
	b := mysql.Add(users, mysql.Integer("b"))
	mysql.Add(users, mysql.Varchar("orgCode", 64).References(orgCode))
	users.AddIndex(mysql.NewIndex("idx_age", users, age))
	users.AddIndex(mysql.NewIndex("idx_ab", users, a, b))
	users.AddCheck("ck_age", "`age` >= 0")

	members := mysql.NewTable("members")
	mysql.Add(members, mysql.BigInt("orgId").PrimaryKey())
	mysql.Add(members, mysql.BigInt("userId").PrimaryKey())

	orgsAfter := mysql.NewTable("orgs")
	mysql.Add(orgsAfter, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(orgsAfter, mysql.Varchar("name", 64).NotNull())

	usersAfter := mysql.NewTable("users")
	mysql.Add(usersAfter, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(usersAfter, mysql.Integer("b"))

	membersAfter := mysql.NewTable("members")
	mysql.Add(membersAfter, mysql.BigInt("userId").PrimaryKey())

	stmts := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(orgs, users, members)),
		mysql.BuildSnapshot(mysql.NewSchema(orgsAfter, usersAfter, membersAfter)))

	// The key on the referenced side goes before either column it
	// joins — the column it is on, and the one it points at on the
	// other table.
	emittedBefore(t, stmts, "DROP FOREIGN KEY `fk_users_orgCode`", "`users`\n\tDROP COLUMN")
	emittedBefore(t, stmts, "DROP FOREIGN KEY `fk_users_orgCode`", "ALTER TABLE `orgs` DROP COLUMN")
	// And before the unique key it pointed at, which InnoDB will not
	// let go while the key needs it.
	emittedBefore(t, stmts, "DROP FOREIGN KEY `fk_users_orgCode`", "DROP INDEX `uq_orgs_code`")

	for _, dependent := range []string{
		"DROP INDEX `uq_users_email`",
		"DROP INDEX `idx_age`",
		"DROP INDEX `idx_ab`",
		"DROP CONSTRAINT `ck_age`",
	} {
		emittedBefore(t, stmts, dependent, "`users`\n\tDROP COLUMN")
	}
	emittedBefore(t, stmts, "DROP INDEX `uq_orgs_code`", "ALTER TABLE `orgs` DROP COLUMN")
	emittedBefore(t, stmts, "ALTER TABLE `members` DROP PRIMARY KEY", "ALTER TABLE `members` DROP COLUMN")
	// The other half of the primary key waits until the column it no
	// longer spans has gone.
	emittedBefore(t, stmts, "ALTER TABLE `members` DROP COLUMN", "ALTER TABLE `members` ADD PRIMARY KEY")
}

// A dependent that is being restated rather than removed splits the
// same way: the drop with the other drops, the create once the columns
// are in their final shape.
func TestDiffSplitsARestatedDependentAroundTheColumns(t *testing.T) {
	before := mysql.NewTable("t")
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	slug := mysql.Add(before, mysql.Varchar("slug", 190))
	mysql.Add(before, mysql.Integer("gone"))
	before.AddIndex(mysql.NewIndex("idx_slug", before, slug))

	after := mysql.NewTable("t")
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	afterSlug := mysql.Add(after, mysql.Varchar("slug", 190))
	after.AddIndex(mysql.NewIndex("idx_slug", after, afterSlug).Unique())

	stmts := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(before)),
		mysql.BuildSnapshot(mysql.NewSchema(after)))

	emittedBefore(t, stmts, "DROP INDEX `idx_slug`", "DROP COLUMN `gone`")
	emittedBefore(t, stmts, "DROP COLUMN `gone`", "CREATE UNIQUE INDEX `idx_slug`")
}

// A column added in the same migration is there before anything spans
// it — the adds are the half of the split that waits.
func TestDiffAddsColumnsBeforeTheIndexesOverThem(t *testing.T) {
	before := mysql.NewTable("t")
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())

	after := mysql.NewTable("t")
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	slug := mysql.Add(after, mysql.Varchar("slug", 190))
	after.AddIndex(mysql.NewIndex("idx_slug", after, slug))
	after.AddCheck("ck_slug", "`slug` <> ''")

	stmts := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(before)),
		mysql.BuildSnapshot(mysql.NewSchema(after)))

	emittedBefore(t, stmts, "ADD COLUMN `slug`", "CREATE INDEX `idx_slug`")
	emittedBefore(t, stmts, "ADD COLUMN `slug`", "ADD CONSTRAINT `ck_slug`")
}
