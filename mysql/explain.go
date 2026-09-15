package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

// EXPLAIN capture and analysis.
//
// This is written for MySQL rather than ported. drops/sqlite's twin
// parses EXPLAIN QUERY PLAN — a compact tree of sentences like
// "SCAN users" — and neither the statement nor the shape exists here:
// MySQL's EXPLAIN answers with a row per table, in columns, and the
// facts a caller wants are fields rather than substrings to grep out of
// a description.
//
//	plan, _ := mysql.Explain(db, ctx, `SELECT * FROM users WHERE email = ?`, "a@b.com")
//	fmt.Println(plan.SeqScans())     // ["users"] when nothing indexed the predicate
//	fmt.Println(plan.UsedIndexes())  // the indexes the optimiser chose
//	fmt.Println(plan.Fingerprint())  // stable across runs; store next to the query
//
// # What the fingerprint covers, and what it must not
//
// The plan's SHAPE: per row, the table, the access type and the chosen
// key. Deliberately not the row-count estimate or the filtered
// percentage — both move with the table's statistics, so a fingerprint
// carrying them changes after an ANALYZE TABLE that changed no plan at
// all, and a test asserting it would fail for a reason nobody can act
// on.

// PlanStep is one row of EXPLAIN output.
//
// The fields are MySQL's own columns, named as the server names them,
// so a reader can put this next to the CLI's output. Table, Type and
// Key are the three the analysis reads; the rest are carried because a
// caller debugging a plan wants them and a second round trip to get
// them would be silly.
type PlanStep struct {
	ID           int64
	SelectType   string
	Table        string
	Partitions   string
	Type         string // "ALL", "ref", "range", "const", "eq_ref"…
	PossibleKeys string
	Key          string
	KeyLen       string
	Ref          string
	Rows         int64
	Filtered     float64
	Extra        string
}

// ExplainPlan is the parsed result of EXPLAIN.
type ExplainPlan struct {
	Steps []PlanStep
}

// Explain runs EXPLAIN for sql and returns the parsed plan. The
// supplied sql is not modified — drops prepends the EXPLAIN prefix.
//
// Every column is scanned through a nullable, because most of them are
// NULL for most rows: a query with no usable index has no key, a
// constant-folded one has no table. Scanning those into strings is the
// difference between a plan and a scan error.
func Explain(db *DB, ctx context.Context, sqlText string, args ...any) (*ExplainPlan, error) {
	rows, err := db.Query(ctx, "EXPLAIN "+sqlText, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	plan := &ExplainPlan{}
	for rows.Next() {
		raw := make([]sql.NullString, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		step := PlanStep{}
		for i, name := range cols {
			v := raw[i].String
			switch strings.ToLower(name) {
			case "id":
				step.ID = parseInt(v)
			case "select_type":
				step.SelectType = v
			case "table":
				step.Table = v
			case "partitions":
				step.Partitions = v
			case "type":
				step.Type = v
			case "possible_keys":
				step.PossibleKeys = v
			case "key":
				step.Key = v
			case "key_len":
				step.KeyLen = v
			case "ref":
				step.Ref = v
			case "rows":
				step.Rows = parseInt(v)
			case "filtered":
				step.Filtered = parseFloat(v)
			case "extra":
				step.Extra = v
			}
		}
		plan.Steps = append(plan.Steps, step)
	}
	return plan, rows.Err()
}

// SeqScans returns the tables the optimiser reads with a full scan.
//
// type = "ALL" is MySQL's full table scan, and "index" is a full scan
// of the index rather than of the table — cheaper, still every row, and
// still the thing that stops scaling. Both are reported, because a
// caller asking "what is unindexed here" wants to hear about both.
//
// A derived table has no name of its own and MySQL names it
// "<derived2>". It is reported as the server spells it rather than
// skipped: a full scan of a derived table is still a full scan, and
// hiding it would make an expensive plan look clean.
func (p *ExplainPlan) SeqScans() []string {
	var out []string
	for _, s := range p.Steps {
		switch strings.ToLower(s.Type) {
		case "all", "index":
			if s.Table != "" {
				out = append(out, s.Table)
			}
		}
	}
	return out
}

// UsedIndexes returns the indexes the optimiser chose, in plan order
// and without duplicates.
//
// The key column names one index per row. PRIMARY is included: it is
// the index a lookup by key uses, and a caller checking "did this use
// the index I made" wants to see when it used a different one instead.
func (p *ExplainPlan) UsedIndexes() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range p.Steps {
		if s.Key == "" || seen[s.Key] {
			continue
		}
		seen[s.Key] = true
		out = append(out, s.Key)
	}
	return out
}

// Fingerprint is a stable hash over the plan's shape — the table, the
// access type and the chosen key per row. See the note at the top of
// this file for what it leaves out and why.
func (p *ExplainPlan) Fingerprint() string {
	h := sha256.New()
	for _, s := range p.Steps {
		fmt.Fprintf(h, "%s|%s|%s\n", s.Table, s.Type, s.Key)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// String renders the plan one row per line, in the order the server
// returned them.
func (p *ExplainPlan) String() string {
	var b strings.Builder
	for _, s := range p.Steps {
		fmt.Fprintf(&b, "%s: type=%s key=%s rows=%d", s.Table, s.Type, orNone(s.Key), s.Rows)
		if s.Extra != "" {
			b.WriteString(" (" + s.Extra + ")")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// parseInt and parseFloat read a numeric EXPLAIN column, answering zero
// for the NULL the server writes wherever the number does not apply.
func parseInt(s string) int64 {
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0
	}
	return n
}

func parseFloat(s string) float64 {
	var f float64
	if _, err := fmt.Sscanf(s, "%g", &f); err != nil {
		return 0
	}
	return f
}
