package pg

import (
	"regexp"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// Online schema migrations are the difference between a 10ms
// metadata change and a 30-minute outage. PostgreSQL exposes
// CONCURRENTLY for index DDL and fast-path metadata changes for
// many column ops, but the standard CREATE INDEX, ALTER COLUMN
// TYPE, and ADD COLUMN NOT NULL without default still lock the
// table.
//
// AnalyzeMigrationRisks takes a list of raw SQL statements (the
// output of pg.Diff, a Migrator's Up step, or hand-written SQL)
// and returns a Risk per dangerous operation, with severity and
// a suggested safer rewrite. Wire it into CI to catch the
// "scheduled the schema change for 3am Sunday, locked the
// payments table for 25 minutes" incident before it ships.
//
//	risks := pg.AnalyzeMigrationRisks(stmts)
//	for _, r := range risks {
//	    if r.Level == pg.RiskDanger { fail() }
//	}

// RiskLevel categorises how unsafe a statement is on a table that
// might be large or under read/write load.
type RiskLevel string

const (
	// RiskInfo is for changes that are safe but worth flagging
	// (irreversible, requires app deploys, etc.).
	RiskInfo RiskLevel = "info"
	// RiskWarn covers changes that lock briefly or rewrite a
	// small amount of data. Often safe but worth scheduling.
	RiskWarn RiskLevel = "warn"
	// RiskDanger reserves catastrophic-on-large-tables changes:
	// table rewrites, blocking index builds, dropping tables.
	RiskDanger RiskLevel = "danger"
)

// Risk is one finding from AnalyzeMigrationRisks.
type Risk struct {
	Statement  string
	Level      RiskLevel
	Reason     string
	Suggestion string
}

// AnalyzeMigrationRisks returns a Risk for every statement in
// stmts that needs a second look. Statements considered safe
// produce no entry. The matcher is a pragmatic regex-based scan
// — false positives are preferred to false negatives.
func AnalyzeMigrationRisks(stmts []string) []Risk {
	var out []Risk
	for _, raw := range stmts {
		stmt := strings.TrimSpace(raw)
		if stmt == "" {
			continue
		}
		for _, r := range riskRules {
			if !r.match.MatchString(stmt) {
				continue
			}
			if r.exclude != nil && r.exclude.MatchString(stmt) {
				continue
			}
			out = append(out, Risk{
				Statement:  stmt,
				Level:      r.level,
				Reason:     r.reason,
				Suggestion: r.suggestion,
			})
			break
		}
	}
	return out
}

type riskRule struct {
	match      *regexp.Regexp
	exclude    *regexp.Regexp // when non-nil, statements matching this are skipped (e.g. CONCURRENTLY / NOT VALID)
	level      RiskLevel
	reason     string
	suggestion string
}

// riskRules is matched top-to-bottom; first hit wins. Order from
// most-specific to most-general to keep matches accurate. Go's
// regexp does not support lookahead, so "do not flag when X is
// present" is handled via an exclude regex evaluated after match.
var riskRules = []riskRule{
	{
		match:      regexp.MustCompile(`(?i)^DROP\s+TABLE\b`),
		level:      RiskDanger,
		reason:     "DROP TABLE is irreversible and removes all data.",
		suggestion: "Rename the table first, hold for a release cycle, then drop in a follow-up migration.",
	},
	{
		match:      regexp.MustCompile(`(?i)^TRUNCATE\b`),
		level:      RiskDanger,
		reason:     "TRUNCATE wipes the table and bypasses ON DELETE triggers.",
		suggestion: "Use DELETE FROM ... WHERE for selective wipes; otherwise schedule downtime.",
	},
	{
		match:      regexp.MustCompile(`(?i)^ALTER\s+TABLE.+ALTER\s+COLUMN.+TYPE\b`),
		level:      RiskDanger,
		reason:     "ALTER COLUMN TYPE rewrites every row and holds an ACCESS EXCLUSIVE lock for the duration.",
		suggestion: "Add a new column of the target type, backfill via batched UPDATE, swap, drop old. Use ALTER COLUMN ... TYPE only when the existing type is binary-compatible (e.g. varchar → text).",
	},
	{
		match:      regexp.MustCompile(`(?i)^ALTER\s+TABLE.+ADD\s+COLUMN.+NOT\s+NULL\b`),
		exclude:    regexp.MustCompile(`(?i)\bDEFAULT\b`),
		level:      RiskWarn,
		reason:     "ADD COLUMN NOT NULL with no DEFAULT rewrites every row (or fails on existing rows in PG <11).",
		suggestion: "Add the column nullable, backfill, set NOT NULL in a follow-up. PG >=11 with a non-volatile DEFAULT skips the rewrite.",
	},
	{
		match:      regexp.MustCompile(`(?i)^CREATE\s+(UNIQUE\s+)?INDEX\b`),
		exclude:    regexp.MustCompile(`(?i)\bCONCURRENTLY\b`),
		level:      RiskWarn,
		reason:     "CREATE INDEX without CONCURRENTLY holds a SHARE lock blocking writes for the duration.",
		suggestion: "Use CREATE INDEX CONCURRENTLY; drops emits this via CreateIndexOnline(idx).",
	},
	{
		match:      regexp.MustCompile(`(?i)^DROP\s+INDEX\b`),
		exclude:    regexp.MustCompile(`(?i)\bCONCURRENTLY\b`),
		level:      RiskWarn,
		reason:     "DROP INDEX without CONCURRENTLY takes ACCESS EXCLUSIVE briefly; usually fine but visible under load.",
		suggestion: "Use DROP INDEX CONCURRENTLY in OLTP systems with constant write traffic.",
	},
	{
		match:      regexp.MustCompile(`(?i)^ALTER\s+TABLE.+ADD\s+CONSTRAINT.+FOREIGN\s+KEY\b`),
		exclude:    regexp.MustCompile(`(?i)\bNOT\s+VALID\b`),
		level:      RiskWarn,
		reason:     "Adding a FK validates every existing row, locking the table for the duration.",
		suggestion: "ADD CONSTRAINT ... NOT VALID first; VALIDATE CONSTRAINT in a follow-up statement (no lock).",
	},
	{
		match:      regexp.MustCompile(`(?i)^ALTER\s+TABLE.+DROP\s+COLUMN\b`),
		level:      RiskInfo,
		reason:     "DROP COLUMN is fast (metadata only) but data is unrecoverable.",
		suggestion: "Confirm no application code reads the column. Consider renaming for a release cycle first.",
	},
	{
		match:      regexp.MustCompile(`(?i)^ALTER\s+TABLE.+RENAME\b`),
		level:      RiskInfo,
		reason:     "RENAME is instant but requires the application to handle both names during the rollout.",
		suggestion: "Coordinate with a feature flag or read both names during the transition window.",
	},
	{
		match:      regexp.MustCompile(`(?i)^DROP\s+CONSTRAINT\b`),
		level:      RiskInfo,
		reason:     "DROP CONSTRAINT relaxes a guarantee; downstream consumers may have relied on it.",
		suggestion: "Audit consumers; document the change in the migration message.",
	},
}

// CreateIndexOnline marks idx as CONCURRENTLY and returns the
// CREATE INDEX expression. Convenience for online migrations:
//
//	stmt := pg.CreateIndexOnline(myIdx)
//	_, err := db.ExecExpr(ctx, stmt)
//
// PostgreSQL forbids CONCURRENTLY inside a transaction, so the
// caller must invoke it outside InTx — the analyzer will not
// complain about CONCURRENTLY statements, but a transaction
// containing one will fail at the database.
func CreateIndexOnline(idx *Index) drops.Expression {
	idx.Concurrently()
	return CreateIndex(idx)
}

// HasDangerousMigration reports whether stmts contains at least
// one RiskDanger finding. Suitable as a CI gate.
func HasDangerousMigration(stmts []string) bool {
	for _, r := range AnalyzeMigrationRisks(stmts) {
		if r.Level == RiskDanger {
			return true
		}
	}
	return false
}
