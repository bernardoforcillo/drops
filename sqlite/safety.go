package sqlite

import (
	"regexp"
	"strings"
)

// Pre-flight safety analysis for SQLite migration SQL. The rules are
// tuned to SQLite's specific foot-guns, which differ from PostgreSQL's:
// SQLite's ALTER TABLE only supports RENAME, ADD COLUMN and DROP COLUMN,
// and it rejects some ADD COLUMN shapes outright.
//
//	for _, w := range sqlite.AnalyzeMigration(sql) {
//	    log.Printf("%s [%s] %s", w.Severity, w.Rule, w.Message)
//	}

// SafetySeverity ranks a warning's urgency.
type SafetySeverity int

const (
	// SeverityInfo is a heads-up — usually fine.
	SeverityInfo SafetySeverity = iota
	// SeverityWarn flags a likely behaviour change or a rewrite.
	SeverityWarn
	// SeverityError flags a statement that destroys data or that SQLite
	// will reject at execution time.
	SeverityError
)

// String renders the level as "info" / "warn" / "error".
func (s SafetySeverity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarn:
		return "warn"
	case SeverityError:
		return "error"
	}
	return "unknown"
}

// SafetyWarning is one finding from the migration analyser.
type SafetyWarning struct {
	Severity   SafetySeverity
	Rule       string
	Statement  string
	Message    string
	Suggestion string
}

// SafetyOptions tunes the analyser — currently rule suppression.
type SafetyOptions struct {
	// Ignore lists rule IDs to skip (known-safe migrations).
	Ignore []string
}

// AnalyzeMigration splits a migration script on the drizzle-kit
// "--> statement-breakpoint" boundary and analyses each statement.
func AnalyzeMigration(sql string, opts ...SafetyOptions) []SafetyWarning {
	return AnalyzeStatements(splitStatements(sql), opts...)
}

// AnalyzeStatements runs the safety rules against each statement in
// order, preserving statement order in the output.
func AnalyzeStatements(stmts []string, opts ...SafetyOptions) []SafetyWarning {
	var ignore map[string]bool
	for _, o := range opts {
		for _, r := range o.Ignore {
			if ignore == nil {
				ignore = map[string]bool{}
			}
			ignore[r] = true
		}
	}
	var out []SafetyWarning
	for _, s := range stmts {
		trim := strings.TrimSpace(s)
		if trim == "" {
			continue
		}
		for _, rule := range safetyRules {
			if w, ok := rule(trim); ok {
				if ignore[w.Rule] {
					continue
				}
				out = append(out, w)
			}
		}
	}
	return out
}

// splitStatements breaks a migration up at the drizzle-kit
// "statement-breakpoint" boundary, stripping trailing semicolons.
func splitStatements(sql string) []string {
	parts := strings.Split(sql, "--> statement-breakpoint")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.TrimSuffix(p, ";")
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// safetyRules is the rule set in declaration order.
var safetyRules = []func(stmt string) (SafetyWarning, bool){
	ruleDropTable,
	ruleDropColumn,
	ruleRenameTable,
	ruleRenameColumn,
	ruleAddColumnNotNullNoDefault,
	ruleAddColumnDynamicDefault,
	ruleDeleteWithoutWhere,
	ruleUpdateWithoutWhere,
}

var (
	reDropTable      = regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)
	reDropColumn     = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bDROP\s+COLUMN\b`)
	reRenameTable    = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+TO\b`)
	reRenameColumn   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+COLUMN\b`)
	reAddColumn      = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bADD\s+COLUMN\b`)
	reHasNotNull     = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	reHasDefault     = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	reDynamicDefault = regexp.MustCompile(`(?i)\bDEFAULT\s+(CURRENT_TIME|CURRENT_DATE|CURRENT_TIMESTAMP|\()`)
	reDelete         = regexp.MustCompile(`(?i)^\s*DELETE\s+FROM\b`)
	reUpdate         = regexp.MustCompile(`(?i)^\s*UPDATE\b`)
	reHasWhere       = regexp.MustCompile(`(?i)\bWHERE\b`)
)

func ruleDropTable(stmt string) (SafetyWarning, bool) {
	if !reDropTable.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "drop-table",
		Statement:  stmt,
		Message:    "DROP TABLE destroys data irreversibly.",
		Suggestion: "Rename the table aside (ALTER TABLE ... RENAME TO _archived_x) and drop it in a later migration after a retention window.",
	}, true
}

func ruleDropColumn(stmt string) (SafetyWarning, bool) {
	if !reDropColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "drop-column",
		Statement:  stmt,
		Message:    "DROP COLUMN is irreversible and breaks any code still referencing the column (and requires SQLite 3.35+).",
		Suggestion: "Stop writing to the column, deploy, then drop it in a follow-up migration — and back up the data if it matters.",
	}, true
}

func ruleRenameTable(stmt string) (SafetyWarning, bool) {
	if !reRenameTable.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "rename-table",
		Statement:  stmt,
		Message:    "RENAME TABLE breaks code referring to the old name during a rolling deploy.",
		Suggestion: "Create a view with the old name over the new table, deploy, then drop the view later.",
	}, true
}

func ruleRenameColumn(stmt string) (SafetyWarning, bool) {
	if !reRenameColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "rename-column",
		Statement:  stmt,
		Message:    "RENAME COLUMN breaks code referring to the old name during a rolling deploy.",
		Suggestion: "Add a new column, dual-write, switch reads, then drop the old column — avoid renames across deploy boundaries.",
	}, true
}

func ruleAddColumnNotNullNoDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) || !reHasNotNull.MatchString(stmt) || reHasDefault.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-not-null-column-without-default",
		Statement:  stmt,
		Message:    "SQLite rejects ADD COLUMN ... NOT NULL without a DEFAULT unless the table is empty — the migration will fail on a populated table.",
		Suggestion: "Add the column nullable, backfill it, then rebuild the table to enforce NOT NULL — or add it NOT NULL with a constant DEFAULT.",
	}, true
}

func ruleAddColumnDynamicDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) || !reDynamicDefault.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-column-dynamic-default",
		Statement:  stmt,
		Message:    "SQLite forbids ADD COLUMN with a non-constant DEFAULT (CURRENT_TIMESTAMP, a parenthesised expression, etc.) — the statement will be rejected.",
		Suggestion: "Add the column with a constant DEFAULT (or nullable), then backfill the dynamic value with an UPDATE.",
	}, true
}

func ruleDeleteWithoutWhere(stmt string) (SafetyWarning, bool) {
	if !reDelete.MatchString(stmt) || reHasWhere.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "delete-without-where",
		Statement:  stmt,
		Message:    "DELETE without WHERE removes every row in the table.",
		Suggestion: "Add a WHERE clause, or if a full wipe is intended, accept this warning via SafetyOptions.Ignore.",
	}, true
}

func ruleUpdateWithoutWhere(stmt string) (SafetyWarning, bool) {
	if !reUpdate.MatchString(stmt) || reHasWhere.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "update-without-where",
		Statement:  stmt,
		Message:    "UPDATE without WHERE rewrites every row in the table.",
		Suggestion: "Add a WHERE clause, or accept this warning via SafetyOptions.Ignore if a table-wide update is intended.",
	}, true
}
