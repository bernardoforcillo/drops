package sqlite_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

func pairRule(ws []sqlite.SafetyWarning, rule string) sqlite.SafetyWarning {
	for _, w := range ws {
		if w.Rule == rule {
			return w
		}
	}
	return sqlite.SafetyWarning{}
}

// DetectRenames only sees the ambiguities it is built to see. A table
// rename where every column is renamed too, and a column rename across
// a type family, fall through and come out as the destructive pair.
// The analyser cannot tell those from a genuine drop — but it can name
// the shape.
func TestAnalyzeNamesTheShapeALostRenameMakes(t *testing.T) {
	ws := sqlite.AnalyzeStatements([]string{
		`DROP TABLE "players"`,
		`CREATE TABLE "athletes" ("ident" INTEGER PRIMARY KEY)`,
	})
	w := pairRule(ws, "unstated-table-rename")
	if w.Rule == "" {
		t.Fatalf("a DROP TABLE beside a CREATE TABLE went unnamed: %+v", ws)
	}
	if w.Severity != sqlite.SeverityInfo {
		t.Errorf("severity = %v, want info", w.Severity)
	}
	for _, want := range []string{"players", "athletes"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("message must name both halves, missing %q: %s", want, w.Message)
		}
	}

	cols := sqlite.AnalyzeStatements([]string{
		`ALTER TABLE "players" DROP COLUMN "joined"`,
		`ALTER TABLE "players" ADD COLUMN "joinedOn" TEXT`,
	})
	if pairRule(cols, "unstated-column-rename").Rule == "" {
		t.Errorf("a DROP COLUMN beside an ADD COLUMN on one table went unnamed: %+v", cols)
	}
}

// SQLite has no ALTER COLUMN, so every column change it cannot express
// becomes a rebuild: CREATE "t_new", copy, DROP "t", RENAME "t_new" TO
// "t". That is a DROP TABLE next to a CREATE TABLE, and if the pair
// rule fired on it the rule would fire on most migrations this dialect
// writes and be in everyone's Ignore list by the end of the week. The
// RENAME states where the table went, so nothing was lost.
func TestAnalyzeDoesNotCryRenameOverATableRebuild(t *testing.T) {
	before := sqlite.NewTable("t")
	sqlite.Add(before, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("a").NotNull())
	after := sqlite.NewTable("t")
	sqlite.Add(after, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("a"))

	stmts := sqlite.Diff(
		sqlite.BuildSnapshot(sqlite.NewSchema(before)),
		sqlite.BuildSnapshot(sqlite.NewSchema(after)),
	)
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, "DROP TABLE") || !strings.Contains(joined, "CREATE TABLE") {
		t.Fatalf("the fixture is meant to be a rebuild:\n%s", joined)
	}
	ws := sqlite.AnalyzeStatements(stmts)
	if pairRule(ws, "unstated-table-rename").Rule != "" {
		t.Errorf("the rebuild is not a lost rename: %+v", ws)
	}
}

// One half is not a pair.
func TestAnalyzeDoesNotCryRenameOverALoneDrop(t *testing.T) {
	for _, stmts := range [][]string{
		{`DROP TABLE "players"`},
		{`ALTER TABLE "players" DROP COLUMN "joined"`},
		{`ALTER TABLE "players" DROP COLUMN "joined"`, `ALTER TABLE "teams" ADD COLUMN "n" INTEGER`},
	} {
		ws := sqlite.AnalyzeStatements(stmts)
		if pairRule(ws, "unstated-table-rename").Rule != "" || pairRule(ws, "unstated-column-rename").Rule != "" {
			t.Errorf("%v: the pair rules fired without a pair: %+v", stmts, ws)
		}
	}
}

func TestAnalyzePairRulesAreSuppressible(t *testing.T) {
	ws := sqlite.AnalyzeStatements([]string{
		`DROP TABLE "players"`,
		`CREATE TABLE "athletes" ("ident" INTEGER PRIMARY KEY)`,
	}, sqlite.SafetyOptions{Ignore: []string{"unstated-table-rename"}})
	if pairRule(ws, "unstated-table-rename").Rule != "" {
		t.Errorf("Ignore should suppress the pair rule: %+v", ws)
	}
}

// The column rule's blind spot, stated as a test so the doc comment
// above pairColumnRename cannot quietly become false. SQLite has no
// ALTER COLUMN and Diff never emits ALTER TABLE ... DROP COLUMN, so a
// column swap it writes is a rebuild: the drop and the add live inside
// a CREATE TABLE and an INSERT … SELECT, where no statement matcher
// can pair them. The rule is for the hand-written SQL AnalyzeStatements
// also accepts.
func TestAnalyzeCannotSeeAColumnPairHiddenInARebuild(t *testing.T) {
	before := sqlite.NewTable("t")
	sqlite.Add(before, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("joined"))
	after := sqlite.NewTable("t")
	sqlite.Add(after, sqlite.Integer("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("joinedOn"))

	stmts := sqlite.Diff(
		sqlite.BuildSnapshot(sqlite.NewSchema(before)),
		sqlite.BuildSnapshot(sqlite.NewSchema(after)),
	)
	joined := strings.Join(stmts, "\n")
	if strings.Contains(joined, "DROP COLUMN") {
		t.Fatalf("this test is stale: SQLite's diff now emits DROP COLUMN, so the pair rule should reach it:\n%s", joined)
	}
	if pairRule(sqlite.AnalyzeStatements(stmts), "unstated-column-rename").Rule != "" {
		t.Errorf("the rule claimed to see a pair it cannot: %+v", stmts)
	}
}
