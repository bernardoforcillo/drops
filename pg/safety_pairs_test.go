package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// DetectRenames answers the question when it can see it. Two shapes
// slip past it: a table rename where every column is also renamed
// (tablesLookAlike sees nothing in common) and a column rename that
// also crosses a type family. Both come out of the generator as the
// destructive pair, silently. The analyser cannot tell those apart from
// a genuine drop either — but it can name the shape.
func TestAnalyzeNamesTheShapeALostTableRenameMakes(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{
		`DROP TABLE "players" CASCADE`,
		`CREATE TABLE "athletes" ("ident" bigserial PRIMARY KEY, "handle" text NOT NULL)`,
	})
	w := findRule(ws, "unstated-table-rename")
	if w.Rule == "" {
		t.Fatalf("a DROP TABLE beside a CREATE TABLE went unnamed: %+v", ws)
	}
	if w.Severity != pg.SeverityInfo {
		t.Errorf("severity = %v, want info — the DROP TABLE rule already carries the urgency", w.Severity)
	}
	for _, want := range []string{"players", "athletes"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("message must name both halves, missing %q: %s", want, w.Message)
		}
	}
	if !strings.Contains(w.Statement, "DROP TABLE") {
		t.Errorf("the finding should anchor to the destructive half: %q", w.Statement)
	}
}

func TestAnalyzeNamesTheShapeALostColumnRenameMakes(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{
		`ALTER TABLE "players" DROP COLUMN "joined"`,
		`ALTER TABLE "players" ADD COLUMN "joinedOn" date`,
	})
	w := findRule(ws, "unstated-column-rename")
	if w.Rule == "" {
		t.Fatalf("a DROP COLUMN beside an ADD COLUMN on one table went unnamed: %+v", ws)
	}
	if w.Severity != pg.SeverityInfo {
		t.Errorf("severity = %v, want info", w.Severity)
	}
	for _, want := range []string{"players", "joined", "joinedOn"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("message must name the table and both columns, missing %q: %s", want, w.Message)
		}
	}
}

// A warning that fires on every genuine drop is a warning people learn
// to skip, so the pair rules earn their place by needing both halves.
// One drop on its own is just a drop.
func TestAnalyzeDoesNotCryRenameOverALoneDrop(t *testing.T) {
	cases := [][]string{
		{`DROP TABLE "players" CASCADE`},
		{`DROP TABLE "players" CASCADE`, `ALTER TABLE "teams" ADD COLUMN "n" integer`},
		{`ALTER TABLE "players" DROP COLUMN "joined"`},
		{`ALTER TABLE "players" DROP COLUMN "joined"`, `ALTER TABLE "teams" ADD COLUMN "n" integer`},
		{`CREATE TABLE "athletes" ("id" bigserial PRIMARY KEY)`},
	}
	for _, stmts := range cases {
		ws := pg.AnalyzeStatements(stmts)
		if hasRule(ws, "unstated-table-rename") || hasRule(ws, "unstated-column-rename") {
			t.Errorf("%v: the pair rules fired without a pair: %+v", stmts, ws)
		}
	}
}

// Dropping a table and creating one of the same name is a rebuild, not
// a rename. Nothing was renamed to anything.
func TestAnalyzeIgnoresADropAndRecreateOfTheSameTable(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{
		`DROP TABLE "players" CASCADE`,
		`CREATE TABLE "players" ("id" bigserial PRIMARY KEY)`,
	})
	if hasRule(ws, "unstated-table-rename") {
		t.Errorf("a drop-and-recreate of one name is not a rename: %+v", ws)
	}
}

// The column pair is per table: a drop on one table and an add on
// another are two unrelated changes.
func TestAnalyzeColumnPairIsScopedToOneTable(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{
		`ALTER TABLE "players" DROP COLUMN "joined"`,
		`ALTER TABLE "teams" ADD COLUMN "joinedOn" date`,
	})
	if hasRule(ws, "unstated-column-rename") {
		t.Errorf("columns on different tables are not a rename pair: %+v", ws)
	}
}

// The rule IDs are suppression handles like every other.
func TestAnalyzePairRulesAreSuppressible(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{
		`ALTER TABLE "players" DROP COLUMN "joined"`,
		`ALTER TABLE "players" ADD COLUMN "joinedOn" date`,
	}, pg.SafetyOptions{Ignore: []string{"unstated-column-rename"}})
	if hasRule(ws, "unstated-column-rename") {
		t.Errorf("Ignore should suppress the pair rule: %+v", ws)
	}
}

// A migration that renames explicitly has already answered the
// question, so there is nothing to raise.
func TestAnalyzeSaysNothingWhenTheRenameIsStated(t *testing.T) {
	ws := pg.AnalyzeStatements([]string{
		`ALTER TABLE "players" RENAME COLUMN "joined" TO "joinedOn"`,
	})
	if hasRule(ws, "unstated-column-rename") {
		t.Errorf("a stated rename is not an unstated one: %+v", ws)
	}
}
