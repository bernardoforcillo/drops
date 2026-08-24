package main

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/bernardoforcillo/drops/pg"
)

// A destructive statement is one whose damage survives the migration:
// data or an object is gone afterwards and no rollback brings it back.
// Everything else the safety analyser reports — a lock held too long,
// a rewrite, an index built without CONCURRENTLY — is a statement that
// hurts while it runs and is correct once it has. Those are printed as
// warnings; these are refused unless the caller says otherwise.
var destructiveRules = map[string]string{
	"drop-table":            "the table and every row in it are gone",
	"drop-column":           "the column's data is gone",
	"truncate-table":        "every row is gone",
	"alter-type-drop-value": "rows still holding the value become unreadable",
}

// reDropObject catches the drops the per-statement rule set does not
// have a rule for. Diff emits them for enums, sequences and views, and
// a migration that quietly drops a view is exactly as surprising as
// one that quietly drops a table.
var reDropObject = regexp.MustCompile(`(?is)\bDROP\s+(TYPE|VIEW|MATERIALIZED\s+VIEW|SEQUENCE|SCHEMA|DATABASE)\b`)

// refusal is one statement the CLI will not run unattended.
type refusal struct {
	Statement string
	Rule      string
	Reason    string
}

// destructive returns the statements that destroy something, in the
// order they would run.
func destructive(statements []string) []refusal {
	var out []refusal
	for _, stmt := range statements {
		trimmed := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(stmt), ";"))
		if trimmed == "" {
			continue
		}
		matched := false
		for _, w := range pg.AnalyzeStatements([]string{collapse(trimmed)}) {
			reason, ok := destructiveRules[w.Rule]
			if !ok {
				continue
			}
			out = append(out, refusal{Statement: trimmed, Rule: w.Rule, Reason: reason})
			matched = true
			break
		}
		if matched {
			continue
		}
		if m := reDropObject.FindStringSubmatch(trimmed); m != nil {
			kind := strings.ToLower(strings.Join(strings.Fields(m[1]), " "))
			out = append(out, refusal{
				Statement: trimmed,
				Rule:      "drop-object",
				Reason:    "the " + kind + " and anything depending on it are gone",
			})
		}
	}
	return out
}

// warnings returns the safety findings that are not refusals, so a
// migration that is merely expensive still says so.
//
// decisions are the rename answers this run carries — the flags, plus
// whatever --interactive collected. The two rename notices they have
// already settled are left out; see answeredRenames.
func warnings(statements []string, decisions []bridgeRename) []pg.SafetyWarning {
	flat := make([]string, 0, len(statements))
	for _, stmt := range statements {
		flat = append(flat, collapse(stmt))
	}
	answered := newAnsweredRenames(flat, decisions)
	var out []pg.SafetyWarning
	for _, w := range pg.AnalyzeStatements(flat) {
		if _, isRefusal := destructiveRules[w.Rule]; isRefusal {
			continue
		}
		if answered.settles(w) {
			continue
		}
		out = append(out, w)
	}
	return out
}

// answeredRenames matches a rename notice against the answers this run
// was given.
//
// pg.AnalyzeStatements reads statements and only statements. That is
// deliberate and worth keeping: it is a library, and most of what it is
// handed — a drizzle-kit migration, a file somebody edited by hand — has
// no decision behind it to be told about. So it goes on reporting that a
// migration drops one column and adds another, which is all the SQL
// says, and cannot know that --drop-column named that very column.
//
// Printing "state the rename" directly underneath the answer is how a
// tool teaches people to stop reading its output, so the notice is
// dropped here, at the one layer that holds both the plan and the
// answers, rather than by giving the analyser a second kind of input.
type answeredRenames struct {
	// tables and columns are the objects a decision spoke for, by the
	// name that is going. A rename and a refusal both count: either
	// way the question has an answer on record.
	tables  map[string]bool
	columns map[string]map[string]bool
	// dropped is every column the plan drops, by table. A notice
	// covers all of them, not only the one its Statement happens to
	// name, so a half-answered question has to stay on the screen.
	dropped map[string][]string
}

func newAnsweredRenames(statements []string, decisions []bridgeRename) answeredRenames {
	a := answeredRenames{
		tables:  map[string]bool{},
		columns: map[string]map[string]bool{},
		dropped: map[string][]string{},
	}
	for _, d := range decisions {
		switch d.Kind {
		case "table":
			a.tables[d.From] = true
		case "column":
			if a.columns[d.Table] == nil {
				a.columns[d.Table] = map[string]bool{}
			}
			a.columns[d.Table][d.From] = true
		}
	}
	for _, stmt := range statements {
		if m := reDropColumnStmt.FindStringSubmatch(stmt); len(m) > 2 {
			table := unquoteIdent(m[1])
			a.dropped[table] = append(a.dropped[table], unquoteIdent(m[2]))
		}
	}
	return a
}

// settles reports whether w is one of the two rename notices and every
// object it is about has a decision on record.
func (a answeredRenames) settles(w pg.SafetyWarning) bool {
	switch w.Rule {
	case "unstated-table-rename":
		m := reDropTableStmt.FindStringSubmatch(w.Statement)
		if len(m) < 2 {
			return false
		}
		return a.tables[unquoteIdent(m[1])]
	case "unstated-column-rename":
		m := reDropColumnStmt.FindStringSubmatch(w.Statement)
		if len(m) < 3 {
			return false
		}
		table := unquoteIdent(m[1])
		for _, col := range a.dropped[table] {
			if !a.columns[table][col] {
				return false
			}
		}
		return true
	}
	return false
}

// identPat matches one SQL identifier, quoted or bare, optionally
// schema-qualified — the same shape pg's analyser reads names with. The
// CLI needs its own readers because a notice arrives as text: matching
// one against the answers on the command line means recovering the
// names out of the statement it names.
const identPat = `(?:"[^"]*"|[A-Za-z_][\w$]*)(?:\.(?:"[^"]*"|[A-Za-z_][\w$]*))?`

var (
	reDropTableStmt  = regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?(` + identPat + `)`)
	reDropColumnStmt = regexp.MustCompile(
		`(?i)\bALTER\s+TABLE\s+(?:ONLY\s+)?(` + identPat + `)\s+DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?(` + identPat + `)`)
)

// unquoteIdent strips the quoting from an identifier so the two
// spellings of one name compare equal.
func unquoteIdent(s string) string {
	return strings.ReplaceAll(s, `"`, "")
}

// guard reports the destructive statements in a plan and, unless
// allowed, refuses them.
//
// The refusal names every statement it is holding back: a tool that
// says "this migration is destructive" and stops has told the operator
// nothing they can act on, and the next thing they will do is pass the
// flag blind.
func guard(w io.Writer, statements []string, allowed bool) error {
	found := destructive(statements)
	if len(found) == 0 {
		return nil
	}
	verb := "refusing"
	if allowed {
		verb = "running"
	}
	fmt.Fprintf(w, "\n%s %d destructive statement(s):\n", verb, len(found))
	for _, r := range found {
		fmt.Fprintf(w, "  %-22s %s\n", r.Rule, oneLine(r.Statement))
		fmt.Fprintf(w, "  %-22s %s\n", "", r.Reason)
	}
	if allowed {
		return nil
	}
	return findingError{errors.New("stopped before applying anything; re-run with --allow-destructive if that is what you meant")}
}

// printWarnings writes the non-destructive safety findings, less the
// rename notices the run's own answers have already settled.
func printWarnings(w io.Writer, statements []string, decisions []bridgeRename) {
	found := warnings(statements, decisions)
	if len(found) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d safety warning(s):\n", len(found))
	for _, warn := range found {
		fmt.Fprintf(w, "  %-7s %-38s %s\n", warn.Severity, warn.Rule, oneLine(warn.Statement))
		fmt.Fprintf(w, "  %-7s %-38s %s\n", "", "", warn.Suggestion)
	}
}

// collapse puts a statement on one line.
//
// The safety analyser matches patterns whose two halves — ALTER TABLE
// and DROP COLUMN, ALTER TYPE and DROP VALUE — are separated by `.*`,
// and `.` does not cross a newline. drops writes those statements on
// one line, so its own migrations classify either way; a set from
// drizzle-kit or from an editor need not, and a statement means the
// same thing whichever line its words fall on. Everything the gate
// judges is collapsed first so the verdict is about the SQL rather
// than about its formatting.
func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// oneLine collapses a statement to a single line for tabular output.
func oneLine(s string) string {
	s = collapse(s)
	if len(s) > 90 {
		return s[:87] + "..."
	}
	return s
}
