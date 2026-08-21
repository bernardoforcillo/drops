package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

func pairRule(ws []mysql.SafetyWarning, rule string) mysql.SafetyWarning {
	for _, w := range ws {
		if w.Rule == rule {
			return w
		}
	}
	return mysql.SafetyWarning{}
}

// DetectRenames only sees the ambiguities it is built to see: a table
// rename is a candidate only while the two tables still share half
// their column names, a column rename only while the two types are one
// family. What falls through comes out as the destructive pair, and
// nothing can tell that from a genuine drop — but the shape has a name.
func TestAnalyzeNamesTheShapeALostRenameMakes(t *testing.T) {
	ws := mysql.AnalyzeStatements([]string{
		"DROP TABLE `players`",
		"CREATE TABLE `athletes` (`ident` bigint NOT NULL AUTO_INCREMENT, PRIMARY KEY (`ident`))",
	})
	w := pairRule(ws, "unstated-table-rename")
	if w.Rule == "" {
		t.Fatalf("a DROP TABLE beside a CREATE TABLE went unnamed: %+v", ws)
	}
	if w.Severity != mysql.SeverityInfo {
		t.Errorf("severity = %v, want info — the DROP TABLE rule already carries the urgency", w.Severity)
	}
	for _, want := range []string{"players", "athletes"} {
		if !strings.Contains(w.Message, want) {
			t.Errorf("message must name both halves, missing %q: %s", want, w.Message)
		}
	}
	// The names are backtick-quoted in the SQL; the message reads as prose.
	if strings.Contains(w.Message, "`") {
		t.Errorf("the message should not carry the quoting: %s", w.Message)
	}

	cols := mysql.AnalyzeStatements([]string{
		"ALTER TABLE `players` DROP COLUMN `joined`",
		"ALTER TABLE `players` ADD COLUMN `joinedOn` date",
	})
	cw := pairRule(cols, "unstated-column-rename")
	if cw.Rule == "" {
		t.Fatalf("a DROP COLUMN beside an ADD COLUMN on one table went unnamed: %+v", cols)
	}
	for _, want := range []string{"players", "joined", "joinedOn"} {
		if !strings.Contains(cw.Message, want) {
			t.Errorf("message must name the table and both columns, missing %q: %s", want, cw.Message)
		}
	}
}

// A warning that fires on every genuine drop is one people learn to
// skip, so the pair rules need both halves.
func TestAnalyzeDoesNotCryRenameOverALoneDrop(t *testing.T) {
	for _, stmts := range [][]string{
		{"DROP TABLE `players`"},
		{"ALTER TABLE `players` DROP COLUMN `joined`"},
		{"ALTER TABLE `players` DROP COLUMN `joined`", "ALTER TABLE `teams` ADD COLUMN `n` int"},
	} {
		ws := mysql.AnalyzeStatements(stmts)
		if pairRule(ws, "unstated-table-rename").Rule != "" || pairRule(ws, "unstated-column-rename").Rule != "" {
			t.Errorf("%v: the pair rules fired without a pair: %+v", stmts, ws)
		}
	}
}

// The hand-written online-change shape — build the replacement, copy,
// drop the old one, rename the replacement in — states where the table
// went, so nothing was lost.
func TestAnalyzeDoesNotCryRenameOverAStatedRename(t *testing.T) {
	ws := mysql.AnalyzeStatements([]string{
		"CREATE TABLE `players_new` (`id` bigint NOT NULL)",
		"INSERT INTO `players_new` (`id`) SELECT `id` FROM `players`",
		"DROP TABLE `players`",
		"ALTER TABLE `players_new` RENAME TO `players`",
	})
	if pairRule(ws, "unstated-table-rename").Rule != "" {
		t.Errorf("the rename states where the table went: %+v", ws)
	}
}

func TestAnalyzePairRulesAreSuppressible(t *testing.T) {
	ws := mysql.AnalyzeStatements([]string{
		"ALTER TABLE `players` DROP COLUMN `joined`",
		"ALTER TABLE `players` ADD COLUMN `joinedOn` date",
	}, mysql.SafetyOptions{Ignore: []string{"unstated-column-rename"}})
	if pairRule(ws, "unstated-column-rename").Rule != "" {
		t.Errorf("Ignore should suppress the pair rule: %+v", ws)
	}
}
