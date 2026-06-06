package pg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// EXPLAIN plan capture and regression detection.
//
// The single biggest visibility gap in real-world ORMs is the moment
// when a query's plan flips — index dropped, statistics stale, new
// data distribution — and P99 latency quietly doubles overnight.
// drops captures EXPLAIN plans as structured nodes, derives a
// stable fingerprint over the plan's shape (operators, relations,
// indexes — without costs or timings that fluctuate per run), and
// surfaces diffs so plan regressions show up as alerts instead of
// pager incidents.
//
//	plan, _ := pg.Explain(db, ctx, "SELECT * FROM players WHERE region = $1", "eu")
//	fmt.Println(plan.Fingerprint())  // store next to the query
//	fmt.Println(plan.SeqScans())     // ["players"]
//	fmt.Println(plan.UsedIndexes())  // []
//
//	// Later — same query, fresh capture
//	now, _ := pg.Explain(db, ctx, sql, args...)
//	diff := pg.DiffPlans(plan, now)
//	if !diff.Same {
//	    alert("plan changed", diff)
//	}

// ExplainOptions tunes the EXPLAIN PG runs.
type ExplainOptions struct {
	// Analyze flips EXPLAIN (ANALYZE) — actually runs the query
	// and reports real timings / row counts. Skip for INSERT /
	// UPDATE / DELETE in production unless you wrap the call in
	// a rolled-back transaction.
	Analyze bool

	// Buffers adds I/O accounting. Requires Analyze.
	Buffers bool

	// Verbose includes per-node target lists. Useful for
	// fingerprinting projection changes but bloats the JSON.
	Verbose bool
}

// ExplainPlan is the parsed result of an EXPLAIN — the raw JSON,
// the root node of the plan tree, and a few derived shortcuts.
type ExplainPlan struct {
	// JSON is the raw EXPLAIN (FORMAT JSON) payload returned by
	// PostgreSQL. Persist this verbatim for debugging — Root and
	// the helpers below are derived from it.
	JSON json.RawMessage

	// Root is the head of the parsed plan tree.
	Root *PlanNode

	// TotalCost mirrors Root.TotalCost — the planner's estimated
	// upper-bound cost in arbitrary units.
	TotalCost float64

	// PlanRows mirrors Root.PlanRows — the planner's row-count
	// estimate.
	PlanRows int64

	// ActualMs is the actual execution time when Analyze was set.
	// Zero otherwise.
	ActualMs float64
}

// PlanNode is one node in the plan tree.
type PlanNode struct {
	// Type is the PostgreSQL node label — "Seq Scan", "Index
	// Scan", "Hash Join", "Sort", "Aggregate", ...
	Type string

	// Relation is the table name when Type touches a relation.
	Relation string

	// Index is the index name when Type is an index scan / search.
	Index string

	// JoinType is the join kind when Type is a join node.
	JoinType string

	// StartupCost / TotalCost are the planner's cost estimates.
	StartupCost float64
	TotalCost   float64

	// PlanRows is the planner's row-count estimate; ActualRows
	// the EXPLAIN ANALYZE measured value (0 without Analyze).
	PlanRows   int64
	ActualRows int64

	// ActualMs is the total time spent in this node when Analyze
	// was set. Zero otherwise.
	ActualMs float64

	// Children are the sub-plans feeding into this node, in
	// declaration order.
	Children []*PlanNode
}

// Explain returns the parsed EXPLAIN plan for sql with default
// options — planner-only, no execution.
func Explain(db *DB, ctx context.Context, sql string, args ...any) (*ExplainPlan, error) {
	return ExplainWith(db, ctx, ExplainOptions{}, sql, args...)
}

// ExplainWith runs EXPLAIN with the supplied options and returns
// the parsed plan. The supplied sql is not modified — drops
// prepends "EXPLAIN (...) " with the chosen flags.
func ExplainWith(db *DB, ctx context.Context, opts ExplainOptions, sql string, args ...any) (*ExplainPlan, error) {
	flags := []string{"FORMAT JSON"}
	if opts.Analyze {
		flags = append(flags, "ANALYZE")
	}
	if opts.Buffers {
		if !opts.Analyze {
			return nil, fmt.Errorf("drops/pg: Explain Buffers requires Analyze")
		}
		flags = append(flags, "BUFFERS")
	}
	if opts.Verbose {
		flags = append(flags, "VERBOSE")
	}
	prefix := "EXPLAIN (" + strings.Join(flags, ", ") + ") "
	rows, err := db.Query(ctx, prefix+sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// EXPLAIN (FORMAT JSON) returns one row with a single column
	// holding the JSON array. The driver may surface it as a
	// string, []byte, or json.RawMessage depending on its quirks
	// — accept all three.
	if !rows.Next() {
		return nil, fmt.Errorf("drops/pg: Explain returned no rows")
	}
	var raw any
	if err := rows.Scan(&raw); err != nil {
		return nil, err
	}
	body, err := explainBytes(raw)
	if err != nil {
		return nil, err
	}
	return parseExplainPlan(body)
}

// explainBytes coerces a scanned value into the raw JSON bytes,
// tolerating the three shapes drivers commonly produce.
func explainBytes(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, fmt.Errorf("drops/pg: Explain returned NULL")
	case []byte:
		return x, nil
	case string:
		return []byte(x), nil
	case json.RawMessage:
		return []byte(x), nil
	default:
		// Last resort — round-trip through encoding/json so
		// drivers that hand back a map[string]any work too.
		return json.Marshal(x)
	}
}

// parseExplainPlan decodes PG's FORMAT JSON payload into an
// ExplainPlan. The payload is an array of one element holding
// {"Plan": {...}}.
func parseExplainPlan(body []byte) (*ExplainPlan, error) {
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(body, &arr); err != nil {
		return nil, fmt.Errorf("drops/pg: Explain JSON: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("drops/pg: Explain empty array")
	}
	planRaw, ok := arr[0]["Plan"]
	if !ok {
		return nil, fmt.Errorf("drops/pg: Explain missing Plan")
	}
	root, err := parsePlanNode(planRaw)
	if err != nil {
		return nil, err
	}
	p := &ExplainPlan{JSON: body, Root: root}
	if root != nil {
		p.TotalCost = root.TotalCost
		p.PlanRows = root.PlanRows
		p.ActualMs = root.ActualMs
	}
	return p, nil
}

// rawPlanNode is the JSON shape PG emits per node — only the
// fields we read into PlanNode are declared.
type rawPlanNode struct {
	NodeType      string            `json:"Node Type"`
	RelationName  string            `json:"Relation Name"`
	IndexName     string            `json:"Index Name"`
	JoinType      string            `json:"Join Type"`
	StartupCost   float64           `json:"Startup Cost"`
	TotalCost     float64           `json:"Total Cost"`
	PlanRows      int64             `json:"Plan Rows"`
	ActualRows    int64             `json:"Actual Rows"`
	ActualTotalMs float64           `json:"Actual Total Time"`
	Plans         []json.RawMessage `json:"Plans"`
}

func parsePlanNode(body json.RawMessage) (*PlanNode, error) {
	var raw rawPlanNode
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("drops/pg: plan node: %w", err)
	}
	node := &PlanNode{
		Type:        raw.NodeType,
		Relation:    raw.RelationName,
		Index:       raw.IndexName,
		JoinType:    raw.JoinType,
		StartupCost: raw.StartupCost,
		TotalCost:   raw.TotalCost,
		PlanRows:    raw.PlanRows,
		ActualRows:  raw.ActualRows,
		ActualMs:    raw.ActualTotalMs,
	}
	for _, sub := range raw.Plans {
		child, err := parsePlanNode(sub)
		if err != nil {
			return nil, err
		}
		node.Children = append(node.Children, child)
	}
	return node, nil
}

// Fingerprint returns a stable hash of the plan's structural
// shape — node types, relations, indexes, join types — without
// the cost / row estimates that fluctuate between runs. Two
// fingerprints comparing equal indicate the planner picked the
// same shape; a mismatch flags a regression candidate.
func (p *ExplainPlan) Fingerprint() string {
	if p == nil || p.Root == nil {
		return ""
	}
	h := sha256.New()
	fingerprintNode(h, p.Root)
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:12])
}

// fingerprintNode walks the plan tree in pre-order, writing each
// node's structural signature into h. Children are visited in
// declaration order so equivalent plans always hash the same way.
func fingerprintNode(h interface{ Write([]byte) (int, error) }, n *PlanNode) {
	fmt.Fprintf(h, "%s|%s|%s|%s\n", n.Type, n.Relation, n.Index, n.JoinType)
	for _, c := range n.Children {
		fingerprintNode(h, c)
	}
}

// SeqScans returns the relations scanned with a Seq Scan node —
// often the headline finding (a missing or unused index).
func (p *ExplainPlan) SeqScans() []string {
	out := map[string]bool{}
	walkPlan(p.Root, func(n *PlanNode) {
		if n.Type == "Seq Scan" && n.Relation != "" {
			out[n.Relation] = true
		}
	})
	return sortedBoolKeys(out)
}

// UsedIndexes returns the index names referenced by Index Scan /
// Bitmap Index Scan / Index Only Scan nodes — useful for asserting
// that the planner is honouring an expected index.
func (p *ExplainPlan) UsedIndexes() []string {
	out := map[string]bool{}
	walkPlan(p.Root, func(n *PlanNode) {
		if n.Index == "" {
			return
		}
		switch n.Type {
		case "Index Scan", "Index Only Scan", "Bitmap Index Scan":
			out[n.Index] = true
		}
	})
	return sortedBoolKeys(out)
}

// JoinTypes returns the join methods used across the plan in
// pre-order — "Hash Join", "Merge Join", "Nested Loop". Useful
// when a regression manifests as a nested-loop replacing what
// used to be a hash join.
func (p *ExplainPlan) JoinTypes() []string {
	var out []string
	walkPlan(p.Root, func(n *PlanNode) {
		switch n.Type {
		case "Hash Join", "Merge Join", "Nested Loop":
			out = append(out, n.Type)
		}
	})
	return out
}

func walkPlan(n *PlanNode, fn func(*PlanNode)) {
	if n == nil {
		return
	}
	fn(n)
	for _, c := range n.Children {
		walkPlan(c, fn)
	}
}

func sortedBoolKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PlanDiff describes the structural change between two plans for
// the same query. Same is true when fingerprints match — every
// other field is then zero / empty.
type PlanDiff struct {
	// BeforeFingerprint / AfterFingerprint are the structural
	// hashes used to detect change. Identical values mean the
	// planner produced equivalent shapes.
	BeforeFingerprint string
	AfterFingerprint  string

	// Same is shorthand for BeforeFingerprint == AfterFingerprint.
	Same bool

	// SeqScansAdded / SeqScansRemoved name the relations that
	// gained / lost a Seq Scan between the two plans. A regression
	// commonly shows up as a relation moving from index to seq.
	SeqScansAdded   []string
	SeqScansRemoved []string

	// IndexesAdded / IndexesRemoved name the indexes the planner
	// started / stopped using.
	IndexesAdded   []string
	IndexesRemoved []string

	// CostDelta is the difference in TotalCost (after - before).
	// Positive means the new plan is more expensive.
	CostDelta float64

	// RowsDelta is the difference in estimated rows
	// (after - before).
	RowsDelta int64
}

// DiffPlans compares two captured plans of the same query. Returns
// a structured diff highlighting plan shape changes that typically
// matter for tail latency.
func DiffPlans(before, after *ExplainPlan) PlanDiff {
	d := PlanDiff{
		BeforeFingerprint: before.Fingerprint(),
		AfterFingerprint:  after.Fingerprint(),
	}
	d.Same = d.BeforeFingerprint == d.AfterFingerprint
	beforeSeq := stringSet(before.SeqScans())
	afterSeq := stringSet(after.SeqScans())
	d.SeqScansAdded = setDiff(afterSeq, beforeSeq)
	d.SeqScansRemoved = setDiff(beforeSeq, afterSeq)
	beforeIdx := stringSet(before.UsedIndexes())
	afterIdx := stringSet(after.UsedIndexes())
	d.IndexesAdded = setDiff(afterIdx, beforeIdx)
	d.IndexesRemoved = setDiff(beforeIdx, afterIdx)
	d.CostDelta = after.TotalCost - before.TotalCost
	d.RowsDelta = after.PlanRows - before.PlanRows
	return d
}

func stringSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func setDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
