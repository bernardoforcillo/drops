package pg

import (
	"regexp"
	"strings"
)

// Pre-flight safety analysis for migration SQL. Catches the most
// common foot-guns before they hit production:
//
//	r := pg.GenerateMigration(opts)
//	for _, w := range pg.AnalyzeMigration(r.SQL) {
//	    log.Printf("%s [%s] %s", w.Severity, w.Rule, w.Message)
//	}
//
// The rules are intentionally conservative — most catch correctness
// issues (locks held too long, data loss, plan invalidation), not
// stylistic preferences. False positives are better than the
// production incident they prevent.

// SafetySeverity ranks a warning's urgency.
type SafetySeverity int

const (
	// SeverityInfo is a heads-up — usually fine, occasionally
	// worth a second look.
	SeverityInfo SafetySeverity = iota
	// SeverityWarn flags a statement that will likely cause
	// downtime or visible behaviour change on a non-trivial
	// table. Reviewable.
	SeverityWarn
	// SeverityError flags a statement that almost certainly
	// breaks production at any reasonable table size — full
	// table rewrites, exclusive locks held indefinitely,
	// unrecoverable data loss.
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
	// Severity ranks the urgency — info / warn / error.
	Severity SafetySeverity

	// Rule is a stable identifier for the check (e.g.
	// "add-not-null-column"). Use it to suppress a known-
	// acceptable warning via SafetyOptions.Ignore.
	Rule string

	// Statement is the offending SQL fragment, trimmed of
	// surrounding whitespace and statement breakpoints.
	Statement string

	// Message describes the problem in plain language.
	Message string

	// Suggestion is a short hint on how to fix the issue —
	// typically a safer migration shape.
	Suggestion string
}

// SafetyOptions tunes the analyser — currently used for rule
// suppression. Add to it as the rule set grows.
type SafetyOptions struct {
	// Ignore lists rule IDs to skip. Useful when a particular
	// migration is known-safe (e.g. small table, scheduled
	// downtime).
	Ignore []string
}

// AnalyzeMigration splits a migration script on the drizzle-kit
// "--> statement-breakpoint" boundary and runs the per-statement
// analyser on each piece. Pass the SQL field of a GenerateResult.
func AnalyzeMigration(sql string, opts ...SafetyOptions) []SafetyWarning {
	parts := splitStatements(sql)
	return AnalyzeStatements(parts, opts...)
}

// AnalyzeStatements runs the safety rules against each statement
// in order. The output preserves statement order so callers can
// align warnings with their migration text.
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
// "statement-breakpoint" boundary used by the diff generator.
// Trailing semicolons are stripped so the per-rule matchers see
// the raw statement.
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

// safetyRules is the rule set in declaration order. Each rule is a
// closure that inspects one statement and returns a SafetyWarning
// when the statement matches its pattern.
var safetyRules = []func(stmt string) (SafetyWarning, bool){
	ruleAddColumnNotNullNoDefault,
	ruleAddColumnNotNullVolatileDefault,
	ruleAlterColumnType,
	ruleAlterColumnSetNotNull,
	ruleCreateIndexNotConcurrent,
	ruleDropTable,
	ruleDropColumn,
	ruleAlterTypeDropValue,
	ruleRenameColumn,
	ruleRenameTable,
	ruleTruncate,
}

// ----------------------------------------------------------------------
// Rules
// ----------------------------------------------------------------------

var (
	reAddColumn      = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bADD\s+COLUMN\b`)
	reHasNotNull     = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	reHasDefault     = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	reVolatileDef    = regexp.MustCompile(`(?i)\bDEFAULT\s+(now\(\)|current_timestamp|gen_random_uuid\(\)|uuid_generate_v4\(\)|random\(\))`)
	reAlterColType   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bALTER\s+COLUMN\b.*\b(SET\s+DATA\s+TYPE|TYPE)\b`)
	reAlterColSetNN  = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bALTER\s+COLUMN\b.*\bSET\s+NOT\s+NULL\b`)
	reCreateIndex    = regexp.MustCompile(`(?i)\bCREATE\s+(UNIQUE\s+)?INDEX\b`)
	reConcurrently   = regexp.MustCompile(`(?i)\bCONCURRENTLY\b`)
	reDropTable      = regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)
	reDropColumn     = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bDROP\s+COLUMN\b`)
	reAlterTypeDrop  = regexp.MustCompile(`(?i)\bALTER\s+TYPE\b.*\bDROP\s+VALUE\b`)
	reRenameColumn   = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+COLUMN\b`)
	reRenameTable    = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bRENAME\s+TO\b`)
	reTruncate       = regexp.MustCompile(`(?i)\bTRUNCATE\b`)
	reCreateTableNew = regexp.MustCompile(`(?i)\bCREATE\s+TABLE\b`)
)

func ruleAddColumnNotNullNoDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if !reHasNotNull.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if reHasDefault.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-not-null-column-without-default",
		Statement:  stmt,
		Message:    "ADD COLUMN NOT NULL without DEFAULT requires every existing row to satisfy the constraint and locks the table while PG validates it.",
		Suggestion: "Add the column nullable, backfill in batches, then SET NOT NULL in a follow-up migration — or add it NOT NULL with a constant DEFAULT (PG 11+ skips the rewrite).",
	}, true
}

func ruleAddColumnNotNullVolatileDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if !reHasNotNull.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if !reVolatileDef.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-not-null-column-with-volatile-default",
		Statement:  stmt,
		Message:    "ADD COLUMN NOT NULL with a volatile DEFAULT (now(), gen_random_uuid(), random()) forces PG to rewrite every row — exclusive lock for the duration.",
		Suggestion: "Add the column nullable with the volatile default, backfill existing rows in batches, then SET NOT NULL.",
	}, true
}

func ruleAlterColumnType(stmt string) (SafetyWarning, bool) {
	if !reAlterColType.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "alter-column-type",
		Statement:  stmt,
		Message:    "ALTER COLUMN TYPE often rewrites the whole table (and any dependent indexes / constraints).",
		Suggestion: "Add a new column with the target type, dual-write from the application, backfill, then drop the old column in a follow-up migration.",
	}, true
}

func ruleAlterColumnSetNotNull(stmt string) (SafetyWarning, bool) {
	if !reAlterColSetNN.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "alter-column-set-not-null",
		Statement:  stmt,
		Message:    "SET NOT NULL takes ACCESS EXCLUSIVE while PG scans every row to validate the constraint.",
		Suggestion: "Backfill nulls first, add a CHECK (col IS NOT NULL) NOT VALID, VALIDATE CONSTRAINT, then SET NOT NULL — the constraint stays validated so the SET NOT NULL is metadata-only on PG 12+.",
	}, true
}

func ruleCreateIndexNotConcurrent(stmt string) (SafetyWarning, bool) {
	if !reCreateIndex.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if reConcurrently.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if reCreateTableNew.MatchString(stmt) {
		// CREATE TABLE often carries inline CREATE INDEX-like
		// fragments via reserved keywords — skip when it's a
		// brand-new table (no live traffic yet).
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "create-index-not-concurrent",
		Statement:  stmt,
		Message:    "CREATE INDEX without CONCURRENTLY blocks writes against the table while the index builds.",
		Suggestion: "Append CONCURRENTLY (note: cannot run inside a transaction; emit the index DDL as a standalone migration).",
	}, true
}

func ruleDropTable(stmt string) (SafetyWarning, bool) {
	if !reDropTable.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "drop-table",
		Statement:  stmt,
		Message:    "DROP TABLE destroys data irreversibly.",
		Suggestion: "Rename the table aside (ALTER TABLE ... RENAME TO _archived_xxx) and drop in a follow-up migration after a retention window.",
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
		Message:    "DROP COLUMN is irreversible and breaks any application code that still references the column.",
		Suggestion: "Stop writing to the column first, deploy, wait, then drop — and keep a backup if the data matters.",
	}, true
}

func ruleAlterTypeDropValue(stmt string) (SafetyWarning, bool) {
	if !reAlterTypeDrop.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "alter-type-drop-value",
		Statement:  stmt,
		Message:    "PostgreSQL cannot drop enum values — the statement will fail.",
		Suggestion: "Replace the enum with a text + CHECK constraint, or accept that old values stay listed and stop emitting them from the application.",
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
		Message:    "RENAME COLUMN breaks any application code referring to the old name during a rolling deploy.",
		Suggestion: "Add a new column, dual-write, switch reads, drop the old column — never rename across deploy boundaries.",
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
		Message:    "RENAME TABLE breaks any application code referring to the old name during a rolling deploy.",
		Suggestion: "Create a view with the old name pointing at the new table, deploy, drop the view in a follow-up migration.",
	}, true
}

func ruleTruncate(stmt string) (SafetyWarning, bool) {
	if !reTruncate.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "truncate-table",
		Statement:  stmt,
		Message:    "TRUNCATE removes every row irreversibly and bypasses ON DELETE triggers.",
		Suggestion: "If this is intentional, accept the warning via SafetyOptions.Ignore; otherwise drop the statement.",
	}, true
}
