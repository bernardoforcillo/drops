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
func warnings(statements []string) []pg.SafetyWarning {
	flat := make([]string, 0, len(statements))
	for _, stmt := range statements {
		flat = append(flat, collapse(stmt))
	}
	var out []pg.SafetyWarning
	for _, w := range pg.AnalyzeStatements(flat) {
		if _, isRefusal := destructiveRules[w.Rule]; isRefusal {
			continue
		}
		out = append(out, w)
	}
	return out
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

// printWarnings writes the non-destructive safety findings.
func printWarnings(w io.Writer, statements []string) {
	found := warnings(statements)
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
