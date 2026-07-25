package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// EXPLAIN QUERY PLAN capture and analysis. Unlike PostgreSQL's rich
// JSON EXPLAIN, SQLite reports a compact tree of steps — each a short
// human-readable description like "SCAN users" or
// "SEARCH users USING INDEX ix_users_email (email=?)". drops parses those
// into structured steps, derives a stable fingerprint over the plan
// shape, and surfaces full-table scans and used indexes so plan
// regressions can be caught in tests / CI.
//
//	plan, _ := sqlite.Explain(db, ctx, `SELECT * FROM users WHERE email = ?`, "a@b.com")
//	fmt.Println(plan.SeqScans())     // ["users"] if no index is used
//	fmt.Println(plan.UsedIndexes())  // index names the planner picked
//	fmt.Println(plan.Fingerprint())  // stable across runs; store next to the query

// PlanStep is one row of EXPLAIN QUERY PLAN output.
type PlanStep struct {
	ID     int64
	Parent int64
	Detail string
}

// ExplainPlan is the parsed result of EXPLAIN QUERY PLAN.
type ExplainPlan struct {
	Steps []PlanStep
}

// Explain runs EXPLAIN QUERY PLAN for sql and returns the parsed plan.
// The supplied sql is not modified — drops prepends the EXPLAIN prefix.
func Explain(db *DB, ctx context.Context, sql string, args ...any) (*ExplainPlan, error) {
	rows, err := db.Query(ctx, "EXPLAIN QUERY PLAN "+sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	plan := &ExplainPlan{}
	for rows.Next() {
		var (
			id, parent, notused int64
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			return nil, err
		}
		plan.Steps = append(plan.Steps, PlanStep{ID: id, Parent: parent, Detail: detail})
	}
	return plan, rows.Err()
}

// SeqScans returns the tables the planner reads with a full table scan —
// a bare "SCAN <table>" with no "USING INDEX". These are the usual
// suspects behind a slow query on a large table.
func (p *ExplainPlan) SeqScans() []string {
	var out []string
	for _, s := range p.Steps {
		d := s.Detail
		if !strings.HasPrefix(d, "SCAN ") {
			continue
		}
		if strings.Contains(d, "USING INDEX") || strings.Contains(d, "USING COVERING INDEX") {
			continue
		}
		out = append(out, scanTarget(d[len("SCAN "):]))
	}
	return out
}

// UsedIndexes returns the index names the planner chose, parsed from
// "USING [COVERING] INDEX <name>" fragments.
func (p *ExplainPlan) UsedIndexes() []string {
	var out []string
	for _, s := range p.Steps {
		if idx := indexAfter(s.Detail, "USING COVERING INDEX "); idx != "" {
			out = append(out, idx)
			continue
		}
		if idx := indexAfter(s.Detail, "USING INDEX "); idx != "" {
			out = append(out, idx)
		}
	}
	return out
}

// Fingerprint is a stable hash over the plan's step details — the plan's
// shape without any run-specific values. Persist it next to a query and
// diff future captures to catch plan regressions.
func (p *ExplainPlan) Fingerprint() string {
	h := sha256.New()
	for _, s := range p.Steps {
		h.Write([]byte(s.Detail))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// String renders the plan as an indented tree, matching how the SQLite
// CLI prints it.
func (p *ExplainPlan) String() string {
	var b strings.Builder
	for _, s := range p.Steps {
		b.WriteString(s.Detail)
		b.WriteByte('\n')
	}
	return b.String()
}

// scanTarget extracts the relation name from the text following "SCAN ",
// stopping at the first " USING" / " (" / space-delimited qualifier.
func scanTarget(rest string) string {
	rest = strings.TrimSpace(rest)
	for _, cut := range []string{" USING", " (", " AS "} {
		if i := strings.Index(rest, cut); i >= 0 {
			rest = rest[:i]
		}
	}
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// indexAfter returns the index name following marker in detail, or "".
func indexAfter(detail, marker string) string {
	i := strings.Index(detail, marker)
	if i < 0 {
		return ""
	}
	rest := detail[i+len(marker):]
	if j := strings.IndexAny(rest, " ("); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
