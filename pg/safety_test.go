package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestAnalyzeFlagsAddNotNullWithoutDefault(t *testing.T) {
	stmt := `ALTER TABLE "players" ADD COLUMN "region" text NOT NULL`
	ws := pg.AnalyzeStatements([]string{stmt})
	if !hasRule(ws, "add-not-null-column-without-default") {
		t.Errorf("missing rule, got: %+v", ws)
	}
	w := findRule(ws, "add-not-null-column-without-default")
	if w.Severity != pg.SeverityError {
		t.Errorf("severity: %v", w.Severity)
	}
}

func TestAnalyzeAllowsAddNotNullWithStaticDefault(t *testing.T) {
	stmt := `ALTER TABLE "players" ADD COLUMN "region" text NOT NULL DEFAULT 'eu'`
	ws := pg.AnalyzeStatements([]string{stmt})
	if hasRule(ws, "add-not-null-column-without-default") {
		t.Errorf("static default should be allowed, got: %+v", ws)
	}
}

func TestAnalyzeFlagsAddNotNullWithVolatileDefault(t *testing.T) {
	stmt := `ALTER TABLE "players" ADD COLUMN "created" timestamptz NOT NULL DEFAULT now()`
	ws := pg.AnalyzeStatements([]string{stmt})
	if !hasRule(ws, "add-not-null-column-with-volatile-default") {
		t.Errorf("missing volatile-default rule: %+v", ws)
	}
}

func TestAnalyzeFlagsAlterColumnType(t *testing.T) {
	stmt := `ALTER TABLE "players" ALTER COLUMN "level" TYPE bigint`
	ws := pg.AnalyzeStatements([]string{stmt})
	if !hasRule(ws, "alter-column-type") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeFlagsAlterColumnSetNotNull(t *testing.T) {
	stmt := `ALTER TABLE "players" ALTER COLUMN "email" SET NOT NULL`
	ws := pg.AnalyzeStatements([]string{stmt})
	if !hasRule(ws, "alter-column-set-not-null") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeFlagsCreateIndexNotConcurrent(t *testing.T) {
	stmt := `CREATE INDEX "players_email_idx" ON "players" ("email")`
	ws := pg.AnalyzeStatements([]string{stmt})
	if !hasRule(ws, "create-index-not-concurrent") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeAllowsConcurrentIndex(t *testing.T) {
	stmt := `CREATE INDEX CONCURRENTLY "players_email_idx" ON "players" ("email")`
	ws := pg.AnalyzeStatements([]string{stmt})
	if hasRule(ws, "create-index-not-concurrent") {
		t.Errorf("CONCURRENTLY should suppress rule: %+v", ws)
	}
}

func TestAnalyzeSkipsIndexInsideCreateTable(t *testing.T) {
	// drops doesn't emit inline CREATE INDEX inside CREATE TABLE, but
	// ensure brand-new tables never trip the rule.
	stmt := `CREATE TABLE "players" ("id" bigserial PRIMARY KEY)`
	ws := pg.AnalyzeStatements([]string{stmt})
	if hasRule(ws, "create-index-not-concurrent") {
		t.Errorf("new table should be exempt: %+v", ws)
	}
}

func TestAnalyzeFlagsDropTable(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{`DROP TABLE "players"`})
	if !hasRule(ws, "drop-table") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeFlagsDropColumn(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{`ALTER TABLE "players" DROP COLUMN "old"`})
	if !hasRule(ws, "drop-column") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeFlagsRenameColumn(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{`ALTER TABLE "players" RENAME COLUMN "old" TO "new"`})
	if !hasRule(ws, "rename-column") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeFlagsRenameTable(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{`ALTER TABLE "players" RENAME TO "users"`})
	if !hasRule(ws, "rename-table") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeFlagsTruncate(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{`TRUNCATE "players"`})
	if !hasRule(ws, "truncate-table") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeFlagsAlterTypeDropValue(t *testing.T) {
	// PG actually doesn't support this; drops shouldn't emit it,
	// but the analyser catches it anyway.
	ws := pg.AnalyzeStatements([]string{`ALTER TYPE "status" DROP VALUE 'archived'`})
	if !hasRule(ws, "alter-type-drop-value") {
		t.Errorf("missing rule: %+v", ws)
	}
}

func TestAnalyzeIgnoreSuppressesRule(t *testing.T) {
	stmt := `DROP TABLE "players"`
	ws := pg.AnalyzeStatements([]string{stmt}, pg.SafetyOptions{Ignore: []string{"drop-table"}})
	if hasRule(ws, "drop-table") {
		t.Errorf("Ignore should suppress drop-table: %+v", ws)
	}
}

func TestAnalyzeMigrationSplitsOnStatementBreakpoint(t *testing.T) {
	migration := strings.Join([]string{
		`ALTER TABLE "a" DROP COLUMN "x";`,
		`--> statement-breakpoint`,
		`CREATE INDEX "i" ON "a" ("y")`,
	}, "\n")
	ws := pg.AnalyzeMigration(migration)
	if !hasRule(ws, "drop-column") || !hasRule(ws, "create-index-not-concurrent") {
		t.Errorf("expected both rules to fire, got: %+v", ws)
	}
}

func TestAnalyzeBenignStatementsHaveNoWarnings(t *testing.T) {
	stmts := []string{
		`CREATE TABLE "players" ("id" bigserial PRIMARY KEY, "name" text NOT NULL)`,
		`ALTER TABLE "players" ADD COLUMN "level" integer DEFAULT 1`,
		`CREATE INDEX CONCURRENTLY "players_name_idx" ON "players" ("name")`,
	}
	ws := pg.AnalyzeStatements(stmts)
	if len(ws) != 0 {
		t.Errorf("expected no warnings, got: %+v", ws)
	}
}

func TestSafetySeverityString(t *testing.T) {
	cases := map[pg.SafetySeverity]string{
		pg.SeverityInfo:  "info",
		pg.SeverityWarn:  "warn",
		pg.SeverityError: "error",
	}
	for s, want := range cases {
		if got := s.String(); got != want {
			t.Errorf("severity %d: got %q want %q", s, got, want)
		}
	}
}

func hasRule(ws []pg.SafetyWarning, rule string) bool {
	for _, w := range ws {
		if w.Rule == rule {
			return true
		}
	}
	return false
}

func findRule(ws []pg.SafetyWarning, rule string) pg.SafetyWarning {
	for _, w := range ws {
		if w.Rule == rule {
			return w
		}
	}
	return pg.SafetyWarning{}
}
