package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestDestructiveNamesWhatItRefuses(t *testing.T) {
	statements := []string{
		`CREATE TABLE "users" ("id" bigserial PRIMARY KEY NOT NULL)`,
		`ALTER TABLE "users" ADD COLUMN "age" integer`,
		`ALTER TABLE "users" DROP COLUMN "nickname"`,
		`DROP TABLE "sessions" CASCADE`,
		`TRUNCATE "audit_log"`,
		`DROP TYPE "user_status"`,
		`CREATE INDEX "users_email_idx" ON "users" ("email")`,
	}
	found := destructive(statements)
	if len(found) != 4 {
		t.Fatalf("destructive found %d statement(s): %+v", len(found), found)
	}
	wantRules := []string{"drop-column", "drop-table", "truncate-table", "drop-object"}
	for i, want := range wantRules {
		if found[i].Rule != want {
			t.Errorf("refusal %d is %q, want %q", i, found[i].Rule, want)
		}
		if found[i].Reason == "" {
			t.Errorf("refusal %d says nothing about what is lost", i)
		}
	}
}

// A statement means the same thing whichever line its words fall on,
// so the gate has to see one that a generator wrapped. drops writes
// its own DROP COLUMN on one line; drizzle-kit, and anyone editing a
// migration by hand, does not have to.
func TestDestructiveSeesAWrappedStatement(t *testing.T) {
	statements := []string{
		"ALTER TABLE \"users\"\n\tDROP COLUMN \"payload\"",
		"ALTER TYPE \"mood\"\n\tDROP VALUE 'ok'",
	}
	found := destructive(statements)
	if len(found) != 2 {
		t.Fatalf("destructive found %d of 2 wrapped statement(s): %+v", len(found), found)
	}
	for i, want := range []string{"drop-column", "alter-type-drop-value"} {
		if found[i].Rule != want {
			t.Errorf("refusal %d is %q, want %q", i, found[i].Rule, want)
		}
	}
}

// A rewrite, a lock, a non-concurrent index: expensive, not
// destructive. Refusing those would train everyone to pass
// --allow-destructive by reflex, which is how the flag stops meaning
// anything.
func TestExpensiveStatementsAreWarningsNotRefusals(t *testing.T) {
	statements := []string{
		`ALTER TABLE "users" ALTER COLUMN "id" SET DATA TYPE bigint`,
		`CREATE INDEX "users_email_idx" ON "users" ("email")`,
		`ALTER TABLE "users" ADD COLUMN "email" text NOT NULL`,
	}
	if found := destructive(statements); len(found) != 0 {
		t.Fatalf("refused %d non-destructive statement(s): %+v", len(found), found)
	}
	if got := warnings(statements, nil); len(got) == 0 {
		t.Fatal("no warnings for a migration that rewrites a table and locks it twice")
	}
}

// The safety analyser sees a DROP COLUMN beside an ADD COLUMN and says
// that is what an unstated rename looks like. It is right about the SQL
// and wrong about this run: --drop-column stated the answer, and
// repeating the question under it is how a tool teaches people to stop
// reading its output.
func TestAnsweredRenameNoticesAreNotPrintedAgain(t *testing.T) {
	statements := []string{
		`ALTER TABLE "users" DROP COLUMN "email"`,
		`ALTER TABLE "users" ADD COLUMN "emailAddress" text`,
		`DROP TABLE "sessions"`,
		`CREATE TABLE "tokens" ("id" bigserial PRIMARY KEY NOT NULL)`,
	}
	if !hasWarning(warnings(statements, nil), "unstated-column-rename") {
		t.Fatal("the notice this test is about is not raised with no answers at all")
	}
	if !hasWarning(warnings(statements, nil), "unstated-table-rename") {
		t.Fatal("the table notice is not raised with no answers at all")
	}

	answered := []bridgeRename{
		{Kind: "column", Table: "users", From: "email"},
		{Kind: "table", From: "sessions"},
	}
	got := warnings(statements, answered)
	if hasWarning(got, "unstated-column-rename") {
		t.Errorf("the column question was asked again after --drop-column answered it: %+v", got)
	}
	if hasWarning(got, "unstated-table-rename") {
		t.Errorf("the table question was asked again after --drop-table answered it: %+v", got)
	}
}

// An answer covers the column it names and no other. A migration that
// drops two columns from one table and has an answer for one of them
// still has a question outstanding, and the notice is how it gets
// asked.
func TestAHalfAnsweredRenameNoticeStays(t *testing.T) {
	statements := []string{
		`ALTER TABLE "users" DROP COLUMN "email"`,
		`ALTER TABLE "users" DROP COLUMN "nickname"`,
		`ALTER TABLE "users" ADD COLUMN "emailAddress" text`,
	}
	answered := []bridgeRename{{Kind: "column", Table: "users", From: "email"}}
	if !hasWarning(warnings(statements, answered), "unstated-column-rename") {
		t.Error("nickname was never asked about and the notice went quiet anyway")
	}
	answered = append(answered, bridgeRename{Kind: "column", Table: "users", From: "nickname"})
	if hasWarning(warnings(statements, answered), "unstated-column-rename") {
		t.Error("both columns are answered and the notice is still asking")
	}
}

// An answer about one table says nothing about another.
func TestAnAnswerDoesNotTravelBetweenTables(t *testing.T) {
	statements := []string{
		`ALTER TABLE "users" DROP COLUMN "email"`,
		`ALTER TABLE "users" ADD COLUMN "emailAddress" text`,
		`ALTER TABLE "orders" DROP COLUMN "note"`,
		`ALTER TABLE "orders" ADD COLUMN "comment" text`,
	}
	answered := []bridgeRename{{Kind: "column", Table: "users", From: "email"}}
	got := warnings(statements, answered)
	if len(got) != 1 || got[0].Rule != "unstated-column-rename" {
		t.Fatalf("warnings = %+v, want the one notice orders still has coming", got)
	}
	if !strings.Contains(got[0].Statement, "orders") {
		t.Errorf("the surviving notice is about the answered table: %s", got[0].Statement)
	}
}

// hasWarning reports whether the rule appears among the findings.
func hasWarning(found []pg.SafetyWarning, rule string) bool {
	for _, w := range found {
		if w.Rule == rule {
			return true
		}
	}
	return false
}

func TestGuardRefusesAndExplains(t *testing.T) {
	var out strings.Builder
	err := guard(&out, []string{`DROP TABLE "sessions"`}, false)
	if err == nil {
		t.Fatal("guard let a DROP TABLE through without the flag")
	}
	var finding findingError
	if !errors.As(err, &finding) {
		t.Errorf("guard returned %T, which does not map to the drift/refusal exit code", err)
	}
	text := out.String()
	if !strings.Contains(text, `DROP TABLE "sessions"`) {
		t.Errorf("the refusal does not name the statement it is holding back:\n%s", text)
	}
	if !strings.Contains(err.Error(), "--allow-destructive") {
		t.Errorf("the refusal does not name the flag that overrides it: %v", err)
	}
}

func TestGuardAllowsWithTheFlag(t *testing.T) {
	if err := guard(io.Discard, []string{`DROP TABLE "sessions"`}, true); err != nil {
		t.Fatalf("guard refused a destructive statement the caller allowed: %v", err)
	}
}

func TestSplitMigration(t *testing.T) {
	sql := "CREATE TABLE a ();\n--> statement-breakpoint\nCREATE TABLE b ();\n--> statement-breakpoint\n\n"
	got := splitMigration(sql)
	if len(got) != 2 || !strings.HasPrefix(got[1], "CREATE TABLE b") {
		t.Fatalf("splitMigration = %#v", got)
	}
}

func TestResolveDSNPrefersTheFlag(t *testing.T) {
	t.Setenv("DROPS_PG_DSN", "postgres://env/db")
	t.Setenv("DATABASE_URL", "postgres://other/db")
	if got, _ := resolveDSN("postgres://flag/db"); got != "postgres://flag/db" {
		t.Errorf("resolveDSN = %q, want the flag", got)
	}
	if got, _ := resolveDSN(""); got != "postgres://env/db" {
		t.Errorf("resolveDSN = %q, want DROPS_PG_DSN", got)
	}
	t.Setenv("DROPS_PG_DSN", "")
	if got, _ := resolveDSN(""); got != "postgres://other/db" {
		t.Errorf("resolveDSN = %q, want DATABASE_URL", got)
	}
	t.Setenv("DATABASE_URL", "")
	if _, err := resolveDSN(""); err == nil {
		t.Error("resolveDSN invented a database to connect to")
	}
}
