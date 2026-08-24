package clickhouse

import (
	"strings"
)

// Pre-flight safety analysis for ClickHouse schema statements:
//
//	plan := clickhouse.Diff(live, declared)
//	for _, w := range clickhouse.Analyze(plan) {
//	    log.Printf("%s [%s] %s", w.Cost, w.Rule, w.Message)
//	}
//
// ClickHouse's foot-guns are not PostgreSQL's and not MySQL's, and the
// one that decides everything else is that an ALTER here is one of two
// completely different operations wearing the same syntax.
//
// Most of them are metadata: MODIFY SETTING, COMMENT COLUMN, an ADD
// COLUMN with no default, a MODIFY ORDER BY. They rewrite a file in
// the table's metadata directory and are done, whatever the table
// weighs.
//
// The rest are mutations. A MODIFY COLUMN that changes a type, an
// ALTER … DELETE, a TTL that has to be materialised: each of these
// rewrites every part of the table in the background, asynchronously,
// under the server's own mutation queue. The statement returns long
// before the work does — mutations_sync defaults to 0 — so a migration
// that "succeeded" may have hours of rewriting still ahead of it, and
// a mutation that throws part-way leaves the parts it already
// converted converted. system.mutations is where the truth is.
//
// And the third case is the one that has no statement at all, which is
// why it is not graded here but refused outright: see [Refusal].

// Cost is how expensive a statement is, in the only currency that
// matters for a schema change — whether it touches the data.
type Cost int

const (
	// CostMetadata is a statement that changes the table's definition
	// and nothing else. It costs the same on an empty table and on a
	// petabyte.
	CostMetadata Cost = iota

	// CostRewrite is a statement that queues a mutation: every part of
	// the table is read, converted and written again, in the
	// background, after the statement has already returned.
	CostRewrite

	// CostDestructive is a statement that removes data with no way
	// back — a dropped column, a dropped table, a TTL that deletes
	// rows older than its expression the moment it is materialised.
	CostDestructive
)

// String renders the cost as "metadata" / "rewrite" / "destructive".
func (c Cost) String() string {
	switch c {
	case CostMetadata:
		return "metadata"
	case CostRewrite:
		return "rewrite"
	case CostDestructive:
		return "destructive"
	}
	return "unknown"
}

// SafetyWarning is one finding from the analyser.
type SafetyWarning struct {
	// Cost is what the statement does to the data.
	Cost Cost

	// Rule is a stable identifier for the check — "drop-column",
	// "modify-column-type", "add-ttl". Use it to suppress a
	// known-acceptable finding via [SafetyOptions.Ignore].
	Rule string

	// Statement is the SQL the finding is about. It is empty for the
	// findings [Analyze] raises from a [Refusal], which are about a
	// change that has no statement.
	Statement string

	// Message describes what the statement will do, in plain language.
	Message string

	// Suggestion is a short hint — usually the safer shape of the same
	// change, or where to watch it happen.
	Suggestion string
}

// SafetyOptions tunes the analyser.
type SafetyOptions struct {
	// Ignore lists rule IDs to skip, for a change known to be
	// acceptable — a small table, a scheduled window.
	Ignore []string
}

// Analyze grades a plan's statements and reports its refusals as the
// findings they are: a refusal is the most expensive possible outcome,
// since the only way through it is a new table and a copy of every
// row.
func Analyze(p Plan, opts ...SafetyOptions) []SafetyWarning {
	out := AnalyzeStatements(p.Statements, opts...)
	for _, r := range p.Refusals {
		out = append(out, SafetyWarning{
			Cost:       CostDestructive,
			Rule:       "refused-" + string(r.Kind),
			Message:    r.Detail,
			Suggestion: "There is no ALTER for this. Create the table you want, INSERT … SELECT the data across, and swap the names with RENAME TABLE.",
		})
	}
	return out
}

// AnalyzeStatements runs the safety rules against each statement in
// order, preserving statement order in the output. A statement can
// produce more than one finding.
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
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), ";"))
		if s == "" {
			continue
		}
		for _, rule := range safetyRules {
			w, ok := rule(s)
			if !ok || ignore[w.Rule] {
				continue
			}
			w.Statement = s
			out = append(out, w)
		}
	}
	return out
}

// ClassifyStatement reports what a single statement costs.
//
// It reads the statement's shape and does not ask the server, so treat
// [CostRewrite] as reliable — those actions have no metadata-only
// implementation — and [CostMetadata] as "at best". The way to turn
// the guess into an answer is to run the statement and then watch
// system.mutations: a metadata change leaves no row there, and a
// rewrite leaves one with is_done = 0 until every part has been
// converted.
func ClassifyStatement(stmt string) Cost {
	s := upperFields(stmt)
	switch {
	case strings.Contains(s, "DROP TABLE"),
		strings.Contains(s, "TRUNCATE TABLE"),
		strings.Contains(s, "DROP COLUMN"),
		strings.Contains(s, "DROP PARTITION"),
		// A table TTL is not a schedule that starts empty: the first
		// merge after it lands deletes every row already past it.
		strings.Contains(s, "MODIFY TTL"):
		return CostDestructive
	case strings.Contains(s, "MODIFY COLUMN") && !strings.Contains(s, " REMOVE "):
		// A restatement whose type is the one the column already has
		// is metadata, and drops cannot tell the two apart from the
		// text alone. The expensive reading is the safe one to report.
		return CostRewrite
	case strings.Contains(s, "MATERIALIZE"),
		strings.Contains(s, "ALTER TABLE") && strings.Contains(s, " UPDATE "),
		strings.Contains(s, "ALTER TABLE") && strings.Contains(s, " DELETE "):
		return CostRewrite
	}
	return CostMetadata
}

// upperFields reduces a statement to the form the matchers below read:
// string literals emptied out, then upper-cased, then whitespace
// collapsed, with a space at each end so a matcher can require one on
// either side of a keyword.
//
// Emptying the literals is what keeps the rules honest. Every keyword
// here is also an ordinary English word, and the one string in a
// schema statement drops did not write is the column comment: a
// MODIFY COLUMN carrying COMMENT 'we remove this in Q3' matches
// " REMOVE " and would be graded a metadata change that "touches no
// data" — which is the opposite of what a type change does, said
// reassuringly. A comment mentioning a drop table would trip the other
// way and cry wolf.
func upperFields(stmt string) string {
	return " " + strings.ToUpper(strings.Join(strings.Fields(maskLiterals(stmt)), " ")) + " "
}

// maskLiterals replaces the contents of every single-quoted literal
// with nothing, leaving the quotes so the statement still reads as a
// statement. It honours both escapes ClickHouse accepts inside one, the
// doubled quote and the backslash, so neither can hide the closing
// quote and spill the rest of the statement into the mask.
func maskLiterals(stmt string) string {
	var b strings.Builder
	b.Grow(len(stmt))
	for i := 0; i < len(stmt); i++ {
		if stmt[i] != '\'' {
			b.WriteByte(stmt[i])
			continue
		}
		b.WriteString("''")
		for i++; i < len(stmt); i++ {
			if stmt[i] == '\\' {
				i++
				continue
			}
			if stmt[i] != '\'' {
				continue
			}
			if i+1 < len(stmt) && stmt[i+1] == '\'' {
				i++
				continue
			}
			break
		}
	}
	return b.String()
}

// safetyRules is the rule set, in declaration order.
var safetyRules = []func(stmt string) (SafetyWarning, bool){
	ruleDropTable,
	ruleTruncate,
	ruleDropColumn,
	ruleModifyColumnType,
	ruleRemoveDefault,
	ruleAddColumn,
	ruleModifyTTL,
	ruleModifySetting,
}

func ruleDropTable(stmt string) (SafetyWarning, bool) {
	if !strings.Contains(upperFields(stmt), " DROP TABLE ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost: CostDestructive,
		Rule: "drop-table",
		Message: "DROP TABLE removes the data with the table. ClickHouse has no transactional DDL, so there is " +
			"nothing to roll back, and unless the server was configured with a delay before deletion the parts " +
			"are gone as soon as the statement returns.",
		Suggestion: "RENAME TABLE it aside, deploy, and drop it in a later change once you are sure nothing reads it.",
	}, true
}

func ruleTruncate(stmt string) (SafetyWarning, bool) {
	if !strings.Contains(upperFields(stmt), " TRUNCATE TABLE ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost:       CostDestructive,
		Rule:       "truncate-table",
		Message:    "TRUNCATE drops every part of the table. It is not a DELETE and there is no way back from it.",
		Suggestion: "If it is intentional, accept it via SafetyOptions.Ignore.",
	}, true
}

func ruleDropColumn(stmt string) (SafetyWarning, bool) {
	if !strings.Contains(upperFields(stmt), " DROP COLUMN ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost: CostDestructive,
		Rule: "drop-column",
		Message: "DROP COLUMN deletes the column's files from every part and cannot be undone. Anything still " +
			"writing the column — an INSERT that names it, a materialized view that reads it — starts failing " +
			"at the same instant.",
		Suggestion: "Stop writing the column, deploy, wait out the rollback window, and drop it as a change of its own.",
	}, true
}

func ruleModifyColumnType(stmt string) (SafetyWarning, bool) {
	s := upperFields(stmt)
	if !strings.Contains(s, " MODIFY COLUMN ") || strings.Contains(s, " REMOVE ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost: CostRewrite,
		Rule: "modify-column-type",
		Message: "MODIFY COLUMN with a new type queues a mutation: every part of the table is read, the column " +
			"cast and written again, in the background. The statement returns before any of that has happened " +
			"— mutations_sync defaults to 0 — and a cast that throws on some row leaves the parts already " +
			"converted converted.",
		Suggestion: "Watch it finish in system.mutations (is_done, latest_fail_reason) rather than trusting the statement's return, and check the cast against the extremes of the stored data first.",
	}, true
}

func ruleRemoveDefault(stmt string) (SafetyWarning, bool) {
	s := upperFields(stmt)
	if !strings.Contains(s, " MODIFY COLUMN ") || !strings.Contains(s, " REMOVE ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost: CostMetadata,
		Rule: "remove-column-property",
		Message: "REMOVE takes a clause off the column definition and touches no data. What it changes is what " +
			"happens next: rows already written keep the values the old DEFAULT gave them, and only new rows " +
			"see the difference.",
		Suggestion: "If the point was to change the value existing rows carry, this is not the statement for it — that is an ALTER TABLE … UPDATE.",
	}, true
}

func ruleAddColumn(stmt string) (SafetyWarning, bool) {
	if !strings.Contains(upperFields(stmt), " ADD COLUMN ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost: CostMetadata,
		Rule: "add-column",
		Message: "ADD COLUMN is metadata only and backfills nothing. Rows already in the table read the type's " +
			"default — 0, the empty string, NULL for a Nullable column — for as long as they live, because " +
			"ClickHouse materialises the column lazily as parts merge.",
		Suggestion: "If the rows need real values, follow the ALTER with an ALTER TABLE … UPDATE, or re-ingest them.",
	}, true
}

func ruleModifyTTL(stmt string) (SafetyWarning, bool) {
	s := upperFields(stmt)
	if !strings.Contains(s, " MODIFY TTL ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost: CostDestructive,
		Rule: "add-ttl",
		Message: "MODIFY TTL is metadata, and its consequence is not: every row already older than the new " +
			"expression is deleted by the next merge that touches its part, without further warning and " +
			"without a statement anyone can point at afterwards.",
		Suggestion: "Count the rows the expression already covers (SELECT count() … WHERE <the TTL condition>) before running it.",
	}, true
}

func ruleModifySetting(stmt string) (SafetyWarning, bool) {
	if !strings.Contains(upperFields(stmt), " MODIFY SETTING ") {
		return SafetyWarning{}, false
	}
	return SafetyWarning{
		Cost: CostMetadata,
		Rule: "modify-setting",
		Message: "MODIFY SETTING changes the table's settings for the parts written from now on. Several " +
			"MergeTree settings are fixed when a part is written — index_granularity among them — and the " +
			"server refuses to change those at all rather than applying them to new parts only.",
		Suggestion: "A refusal here means the setting is part of the on-disk format: reaching it needs a new table and a copy.",
	}, true
}
