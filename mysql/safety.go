package mysql

import (
	"regexp"
	"strings"
)

// Pre-flight safety analysis for MySQL migration SQL:
//
//	r, _ := mysql.GenerateMigration(opts)
//	for _, w := range mysql.AnalyzeMigration(r.SQL) {
//	    log.Printf("%s [%s] %s", w.Severity, w.Rule, w.Message)
//	}
//
// The rules are tuned to MySQL's foot-guns rather than PostgreSQL's,
// and two of them have no counterpart there at all.
//
// The first is that nothing rolls back. A PostgreSQL migration that
// fails is a migration that did not happen; a MySQL one that fails is a
// schema in a state nobody wrote down, because every DDL statement
// commits itself. So a destructive statement is not "reviewable" the
// way drops/pg grades one — there is no transaction standing behind it
// — and DROP COLUMN is graded an error here where pg grades it a
// warning.
//
// The second is that the same ALTER TABLE can be instant, online or a
// full copy of the table depending on what it does, and the difference
// between the first and the last is the difference between a
// deployment and an outage. [ClassifyDDL] says which one a statement
// asks for, and the copy-table rule reports it.
//
// False positives are better than the production incident they prevent.

// SafetySeverity ranks a warning's urgency.
type SafetySeverity int

const (
	// SeverityInfo is a heads-up — usually fine, occasionally worth a
	// second look.
	SeverityInfo SafetySeverity = iota
	// SeverityWarn flags a statement that will likely cause downtime or
	// a visible behaviour change on a non-trivial table.
	SeverityWarn
	// SeverityError flags a statement that almost certainly breaks
	// production at any reasonable table size, or that destroys data
	// nothing can bring back.
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

	// Rule is a stable identifier for the check (e.g. "drop-column").
	// Use it to suppress a known-acceptable warning via
	// SafetyOptions.Ignore.
	Rule string

	// Statement is the offending SQL, trimmed of surrounding
	// whitespace and statement breakpoints. It is empty for the one
	// finding that is about the migration rather than a statement in
	// it — see rule "no-transactional-ddl".
	Statement string

	// Message describes the problem in plain language.
	Message string

	// Suggestion is a short hint on how to fix it — usually a safer
	// migration shape.
	Suggestion string
}

// SafetyOptions tunes the analyser.
type SafetyOptions struct {
	// Ignore lists rule IDs to skip, for a migration known to be safe
	// — a small table, a scheduled window.
	Ignore []string
}

// AnalyzeMigration splits a migration script on the
// "--> statement-breakpoint" boundary the generator writes and analyses
// each statement. Pass the SQL field of a GenerateResult.
func AnalyzeMigration(sql string, opts ...SafetyOptions) []SafetyWarning {
	return AnalyzeStatements(splitStatements(sql), opts...)
}

// AnalyzeStatements runs the safety rules against each statement in
// order, preserving statement order in the output.
//
// The first finding may be about the set rather than any one member:
// more than one DDL statement in a migration means the migration has no
// all-or-nothing property, which is worth saying once rather than on
// every line.
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
	emit := func(out []SafetyWarning, w SafetyWarning) []SafetyWarning {
		if ignore[w.Rule] {
			return out
		}
		return append(out, w)
	}

	var out []SafetyWarning
	var ddl []string
	for _, s := range stmts {
		if trim := strings.TrimSpace(s); trim != "" {
			ddl = append(ddl, trim)
		}
	}
	if len(ddl) > 1 {
		out = emit(out, SafetyWarning{
			Severity: SeverityWarn,
			Rule:     "no-transactional-ddl",
			Message: "this migration is " + itoa(len(ddl)) +
				" statements and MySQL commits each one as it runs: a failure part-way leaves the earlier statements applied and no way to undo them.",
			Suggestion: "Order the statements so the schema is usable after any prefix of them, and re-run the migration after fixing the cause — the statements that already applied are the ones a fresh diff no longer emits.",
		})
	}
	for _, s := range ddl {
		for _, rule := range safetyRules {
			if w, ok := rule(s); ok {
				out = emit(out, w)
			}
		}
	}
	return out
}

// splitStatements breaks a migration at the statement-breakpoint
// boundary, stripping trailing semicolons so the matchers see the raw
// statement.
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

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ----------------------------------------------------------------------
// Online DDL classification
// ----------------------------------------------------------------------

// DDLAlgorithm is how a server can carry out an ALTER TABLE.
//
// InnoDB has three, and which one applies is decided by the action,
// not by the operator: an ALTER that cannot be done in place is done by
// building a copy of the table and swapping it in, which takes as long
// as the table is big and, on the copy path, blocks writes throughout.
type DDLAlgorithm int

const (
	// AlgorithmUnknown is a statement this classifier has no opinion
	// about — anything that is not an ALTER TABLE, or an ALTER whose
	// action it does not recognise.
	AlgorithmUnknown DDLAlgorithm = iota
	// AlgorithmInstant changes the table definition and nothing else.
	// MySQL 8.0.12 and MariaDB 10.3 added it for ADD COLUMN; every
	// server has always had it for a DEFAULT change.
	AlgorithmInstant
	// AlgorithmInplace rebuilds the indexes or the rows without a copy
	// of the table, and can usually run with LOCK=NONE so writes
	// continue.
	AlgorithmInplace
	// AlgorithmCopy builds a new table, copies every row into it and
	// swaps. Concurrent writes are blocked for the duration.
	AlgorithmCopy
)

// String renders the algorithm as MySQL spells it.
func (a DDLAlgorithm) String() string {
	switch a {
	case AlgorithmInstant:
		return "INSTANT"
	case AlgorithmInplace:
		return "INPLACE"
	case AlgorithmCopy:
		return "COPY"
	}
	return "UNKNOWN"
}

// ClassifyDDL reports the cheapest algorithm InnoDB can use for a
// statement.
//
// It is a lower bound read off the statement's shape, not a promise:
// the server decides for itself, and version, row format and the
// column's position in the table all move the answer. Treat a COPY
// verdict as reliable — those actions have no in-place implementation —
// and an INSTANT verdict as "at best".
//
// The way to turn the guess into an answer is to make the server
// commit: append ALGORITHM=INPLACE, LOCK=NONE to the ALTER. Both
// servers then refuse the statement outright — error 1845, "ALGORITHM=
// INPLACE is not supported for this operation", or 1846, which names a
// reason — rather than quietly taking the lock. A migration that fails
// in review beats one that succeeds for forty minutes.
//
// One verdict here is conditional on a server setting rather than on
// the statement. ADD CONSTRAINT … FOREIGN KEY is classified INPLACE,
// which it is only while foreign_key_checks is off; under the default
// the server refuses INPLACE (error 1846, "Adding foreign keys needs
// foreign_key_checks=OFF") and copies the table. The classification is
// the lower bound the doc promises, and ruleAddForeignKey warns about
// the statement in its own right.
func ClassifyDDL(stmt string) DDLAlgorithm {
	s := strings.ToUpper(strings.Join(strings.Fields(stmt), " "))
	if !strings.HasPrefix(s, "ALTER TABLE ") {
		return AlgorithmUnknown
	}
	// The copy cases first: one copying action in a batch makes the
	// whole statement a copy, whatever else it carries.
	switch {
	case reModifyColumn.MatchString(s),
		reChangeColumn.MatchString(s),
		reDropPrimaryKey.MatchString(s),
		reAddPrimaryKey.MatchString(s),
		reEngineChange.MatchString(s),
		reCharsetChange.MatchString(s),
		// Adding a CHECK constraint has no in-place implementation on
		// either server: MariaDB 10.11 answers error 1845 ("try
		// ALGORITHM=COPY") and MySQL documents the operation as
		// rebuilding the table. It reads like metadata and is not.
		reAddCheck.MatchString(s):
		return AlgorithmCopy
	case reDropColumnAct.MatchString(s):
		// DROP COLUMN rewrites every row but does it in place; InnoDB
		// takes ALGORITHM=INPLACE, LOCK=NONE for it on both servers.
		return AlgorithmInplace
	case reAddIndexAct.MatchString(s), reDropIndexAct.MatchString(s),
		reAddForeignKey.MatchString(s), reDropForeignKey.MatchString(s):
		return AlgorithmInplace
	case reAlterColumnDefault.MatchString(s), reAddColumnAct.MatchString(s):
		return AlgorithmInstant
	}
	return AlgorithmUnknown
}

// ----------------------------------------------------------------------
// Rules
// ----------------------------------------------------------------------

// safetyRules is the rule set, in declaration order.
var safetyRules = []func(stmt string) (SafetyWarning, bool){
	ruleDropTable,
	ruleDropColumn,
	ruleTruncate,
	ruleCopyingAlter,
	ruleModifyColumn,
	ruleAddColumnNotNullNoDefault,
	ruleDropPrimaryKey,
	ruleDropIndex,
	ruleAddForeignKey,
	ruleAddCheck,
	ruleRenameColumn,
	ruleRenameTable,
}

var (
	reDropTable          = regexp.MustCompile(`(?i)\bDROP\s+TABLE\b`)
	reTruncate           = regexp.MustCompile(`(?i)\bTRUNCATE\b`)
	reDropColumnAct      = regexp.MustCompile(`(?i)\bDROP\s+COLUMN\b`)
	reModifyColumn       = regexp.MustCompile(`(?i)\bMODIFY\s+COLUMN\b`)
	reChangeColumn       = regexp.MustCompile(`(?i)\bCHANGE\s+COLUMN\b`)
	reAddColumnAct       = regexp.MustCompile(`(?i)\bADD\s+COLUMN\b`)
	reAlterColumnDefault = regexp.MustCompile(`(?i)\bALTER\s+COLUMN\b.*\b(SET|DROP)\s+DEFAULT\b`)
	reAddIndexAct        = regexp.MustCompile(`(?i)\bADD\s+(UNIQUE\s+)?(INDEX|KEY)\b`)
	reDropIndexAct       = regexp.MustCompile(`(?i)\bDROP\s+(INDEX|KEY)\b`)
	reDropIndexStmt      = regexp.MustCompile(`(?i)^\s*DROP\s+INDEX\b`)
	reAddPrimaryKey      = regexp.MustCompile(`(?i)\bADD\s+PRIMARY\s+KEY\b`)
	reDropPrimaryKey     = regexp.MustCompile(`(?i)\bDROP\s+PRIMARY\s+KEY\b`)
	reAddForeignKey      = regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\b.*\bFOREIGN\s+KEY\b`)
	reDropForeignKey     = regexp.MustCompile(`(?i)\bDROP\s+FOREIGN\s+KEY\b`)
	reAddCheck           = regexp.MustCompile(`(?i)\bADD\s+CONSTRAINT\b.*\bCHECK\b`)
	reEngineChange       = regexp.MustCompile(`(?i)\bALTER\s+TABLE\b.*\bENGINE\s*=`)
	reCharsetChange      = regexp.MustCompile(`(?i)\bCONVERT\s+TO\s+CHARACTER\s+SET\b`)
	reRenameColumn       = regexp.MustCompile(`(?i)\bRENAME\s+COLUMN\b`)
	reRenameTable        = regexp.MustCompile(`(?i)\bRENAME\s+TO\b`)
	reNotNull            = regexp.MustCompile(`(?i)\bNOT\s+NULL\b`)
	reHasDefault         = regexp.MustCompile(`(?i)\bDEFAULT\b`)
	reAutoIncrement      = regexp.MustCompile(`(?i)\bAUTO_INCREMENT\b`)
	reAlgorithmGiven     = regexp.MustCompile(`(?i)\bALGORITHM\s*=`)
)

func ruleDropTable(stmt string) (SafetyWarning, bool) {
	if !reDropTable.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "drop-table",
		Statement:  stmt,
		Message:    "DROP TABLE destroys the data irreversibly, and it commits the moment it runs — there is no transaction to abandon and no undo.",
		Suggestion: "RENAME the table aside (ALTER TABLE t RENAME TO _archived_t), deploy, and drop it in a later migration once you are sure.",
	}, true
}

// ruleDropColumn grades DROP COLUMN an error rather than the warning
// drops/pg gives it.
//
// The statement is the same; what differs is what stands behind it.
// PostgreSQL runs the drop inside the migration's transaction, so a
// migration that turns out to be wrong is abandoned and the column is
// still there. MySQL commits it as it runs. By the time anyone reads
// the error from the statement after it, the column and everything in
// it are gone, and the only route back is a backup.
func ruleDropColumn(stmt string) (SafetyWarning, bool) {
	if !reDropColumnAct.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "drop-column",
		Statement:  stmt,
		Message:    "DROP COLUMN destroys the column's data, commits immediately, and cannot be rolled back by the migration that contains it. Any application still reading the column starts erroring at the same instant.",
		Suggestion: "Stop writing the column, deploy, wait out the rollback window, and drop it in a separate migration — with a backup if the data mattered.",
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
		Message:    "TRUNCATE removes every row irreversibly, bypasses ON DELETE triggers, and is DDL — it commits itself, so a surrounding transaction will not save you.",
		Suggestion: "If it is intentional, accept it via SafetyOptions.Ignore; otherwise use DELETE with a WHERE clause.",
	}, true
}

// ruleCopyingAlter reports the statements that rebuild the table.
func ruleCopyingAlter(stmt string) (SafetyWarning, bool) {
	if ClassifyDDL(stmt) != AlgorithmCopy {
		return SafetyWarning{}, false
	}
	if reAlgorithmGiven.MatchString(stmt) {
		// The author already told the server which algorithm to use,
		// so the server, not this rule, gets to object.
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "copy-table-alter",
		Statement:  stmt,
		Message:    "this ALTER cannot be done in place: InnoDB builds a copy of the table, copies every row into it and swaps, blocking writes for as long as that takes.",
		Suggestion: "Append ALGORITHM=INPLACE, LOCK=NONE to make the server refuse the statement (error 1845 or 1846) instead of taking the lock, then reshape the change — a new column plus a backfill, or an online schema-change tool — until it is accepted.",
	}, true
}

// ruleModifyColumn explains why an unrelated-looking change restates
// the whole column.
func ruleModifyColumn(stmt string) (SafetyWarning, bool) {
	if !reModifyColumn.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "modify-column",
		Statement:  stmt,
		Message:    "MODIFY COLUMN restates the column in full: every attribute the statement leaves out — NOT NULL, DEFAULT, ON UPDATE, COMMENT, AUTO_INCREMENT — is dropped from the column, not preserved.",
		Suggestion: "Check the restatement carries everything the column had. If only the default is changing, ALTER COLUMN … SET DEFAULT touches the definition alone and never rebuilds the table.",
	}, true
}

func ruleAddColumnNotNullNoDefault(stmt string) (SafetyWarning, bool) {
	if !reAddColumnAct.MatchString(stmt) || !reNotNull.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	if reHasDefault.MatchString(stmt) || reAutoIncrement.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "add-not-null-column-without-default",
		Statement:  stmt,
		Message:    "ADD COLUMN NOT NULL with no DEFAULT has to give every existing row a value. Under a strict sql_mode the statement is rejected outright; under a lax one it silently backfills zeros and empty strings, which is worse.",
		Suggestion: "Add the column nullable, backfill in batches, then MODIFY it NOT NULL — or give it a constant DEFAULT so the server has something to write.",
	}, true
}

// ruleDropPrimaryKey flags the one statement in this file that usually
// cannot run at all.
func ruleDropPrimaryKey(stmt string) (SafetyWarning, bool) {
	if !reDropPrimaryKey.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityError,
		Rule:       "drop-primary-key",
		Statement:  stmt,
		Message:    "DROP PRIMARY KEY copies the whole table, and on a table with an AUTO_INCREMENT column it fails instead: error 1075, since that column must be part of a key at all times. The ADD PRIMARY KEY meant to follow it will not run.",
		Suggestion: "Change the key by building the new table beside the old one and moving the rows, or drop the AUTO_INCREMENT attribute in the same ALTER statement as the key.",
	}, true
}

// ruleDropIndex flags DROP INDEX.
//
// The statement is metadata-only and quick, so the severity is not
// about the drop; it is about the rebuild that follows once someone
// notices the query plan that regressed, on a table large enough to
// have wanted the index in the first place.
func ruleDropIndex(stmt string) (SafetyWarning, bool) {
	if !reDropIndexStmt.MatchString(stmt) && !reDropIndexAct.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "drop-index",
		Statement:  stmt,
		Message:    "DROP INDEX is not reversible in any useful sense: rebuilding it means a full index build on a table that was big enough to need it. If a foreign key depends on the index the statement fails instead, with error 1553.",
		Suggestion: "Confirm the index is unused (performance_schema.table_io_waits_summary_by_index_usage) before dropping it, and keep the CREATE INDEX that restores it to hand.",
	}, true
}

func ruleAddForeignKey(stmt string) (SafetyWarning, bool) {
	if !reAddForeignKey.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityWarn,
		Rule:       "add-foreign-key",
		Statement:  stmt,
		Message:    "ADD FOREIGN KEY validates every existing row against the parent table and creates an index on the child column if none exists. It fails on the first orphan, after doing all the work — and under the default foreign_key_checks=ON the server refuses to do it in place at all, so it copies the table.",
		Suggestion: "Delete or repair the orphans first (SELECT … LEFT JOIN parent WHERE parent.id IS NULL), and create the child index in advance so the constraint has one to adopt.",
	}, true
}

func ruleAddCheck(stmt string) (SafetyWarning, bool) {
	if !reAddCheck.MatchString(stmt) {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Severity:   SeverityInfo,
		Rule:       "add-check-constraint",
		Statement:  stmt,
		Message:    "ADD CHECK reads every row to validate the constraint and has no in-place implementation, so InnoDB copies the table to do it. It is enforced only from MySQL 8.0.16 and MariaDB 10.2 — MySQL 5.7 parses the clause and discards it without a word.",
		Suggestion: "Confirm the server enforces checks (mysql.ServerVersion reports it) before treating the constraint as a guarantee, and fix the violating rows before running this on a large table.",
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
		Message:    "RENAME COLUMN breaks every query still using the old name the instant it commits, which during a rolling deploy is most of them.",
		Suggestion: "Add a new column, dual-write, switch reads, drop the old one — never rename across a deploy boundary.",
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
		Message:    "RENAME TO breaks every query still using the old name, and takes any foreign key pointing at the table with it.",
		Suggestion: "Create a view under the old name pointing at the new table, deploy, then drop the view in a later migration.",
	}, true
}
