package pg

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ParseExplainJSON parses a raw EXPLAIN (FORMAT JSON) payload into an
// [ExplainPlan], without a database.
//
// [ExplainPlan.JSON] carries that payload verbatim, so a plan
// captured in production and stored next to a slow query can be read
// back here and asserted on, or compared with today's. It is also
// what makes the expectations below testable against a fixture
// instead of a live server.
func ParseExplainJSON(body json.RawMessage) (*ExplainPlan, error) {
	return parseExplainPlan(body)
}

// Asserting on a plan, not just fingerprinting it.
//
// [ExplainPlan.Fingerprint] answers "did the plan change?", which is
// the right question for a regression alert and the wrong one for a
// test. A fingerprint is opaque: it tells you something moved, not
// whether the query is doing what you meant, and a test written
// against one has to be updated whenever anything in the plan shifts
// — including the shifts that are fine.
//
// These read the plan instead, so a test can say what it actually
// cares about:
//
//	plan, err := pg.Explain(db, ctx, sql, args...)
//	if err := plan.Expect(
//	    pg.UsesIndex("orders", "orders_customer_created_idx"),
//	    pg.NoSeqScanOn("orders"),
//	); err != nil {
//	    t.Error(err)
//	}
//
// The pairing that makes this worth having is with [WithPlanHints]. A
// hint is silent when it does not apply — the extension may not be
// loaded, the relation name may not match the alias the planner sees,
// the index may have been renamed by a migration — and the query
// simply runs unhinted with nobody the wiser. An assertion on the
// plan is the only thing that notices.

// PlanExpectation is one assertion about a plan. Build them with
// [UsesIndex], [NoSeqScanOn] and the rest, and evaluate with
// [ExplainPlan.Expect].
type PlanExpectation struct {
	describe string
	check    func(*ExplainPlan) error
}

// Expect evaluates every expectation and returns the failures joined
// into one error, or nil when all of them hold.
//
// All of them are evaluated rather than stopping at the first: when a
// plan is wrong it is usually wrong in more than one way, and a test
// that reports one failure per run turns a five-minute fix into five
// runs.
func (p *ExplainPlan) Expect(expectations ...PlanExpectation) error {
	var failures []string
	for _, e := range expectations {
		if err := e.check(p); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("drops/pg: plan expectations failed:\n  %s", strings.Join(failures, "\n  "))
}

// Nodes returns every node of the plan tree, parents before children.
func (p *ExplainPlan) Nodes() []*PlanNode {
	if p == nil || p.Root == nil {
		return nil
	}
	var out []*PlanNode
	var walk func(*PlanNode)
	walk = func(n *PlanNode) {
		if n == nil {
			return
		}
		out = append(out, n)
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(p.Root)
	return out
}

// IndexesUsed reports which indexes the plan reads, per relation.
//
// It is the question a test about an index actually wants answered,
// and the one EXPLAIN makes surprisingly awkward: the index name
// lives on the scan node, the relation on the same node for a plain
// index scan but on a sibling for a bitmap heap scan, and neither is
// anywhere near the top of the tree.
//
// A relation reached without an index does not appear. Names are
// sorted and deduplicated so the result is comparable.
func (p *ExplainPlan) IndexesUsed() map[string][]string {
	out := map[string]map[string]struct{}{}
	for _, n := range p.Nodes() {
		if n.Index == "" {
			continue
		}
		rel := n.Relation
		if rel == "" {
			// A Bitmap Index Scan names the index and leaves the
			// relation to the Bitmap Heap Scan above it. The tree
			// walk is parents-first, so the nearest named relation
			// is the enclosing one.
			rel = enclosingRelation(p.Root, n)
		}
		if rel == "" {
			continue
		}
		if out[rel] == nil {
			out[rel] = map[string]struct{}{}
		}
		out[rel][n.Index] = struct{}{}
	}
	flat := make(map[string][]string, len(out))
	for rel, set := range out {
		names := make([]string, 0, len(set))
		for n := range set {
			names = append(names, n)
		}
		sort.Strings(names)
		flat[rel] = names
	}
	return flat
}

// ScanTypes reports the node types that read relation, e.g.
// ["Bitmap Heap Scan", "Bitmap Index Scan"] or ["Seq Scan"].
func (p *ExplainPlan) ScanTypes(relation string) []string {
	seen := map[string]struct{}{}
	for _, n := range p.Nodes() {
		if n.Relation != relation {
			continue
		}
		seen[n.Type] = struct{}{}
		for _, c := range n.Children {
			if c.Index != "" {
				seen[c.Type] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// enclosingRelation finds the nearest ancestor of target that names a
// relation.
func enclosingRelation(root, target *PlanNode) string {
	var found string
	var walk func(n *PlanNode, inherited string) bool
	walk = func(n *PlanNode, inherited string) bool {
		if n == nil {
			return false
		}
		rel := inherited
		if n.Relation != "" {
			rel = n.Relation
		}
		if n == target {
			found = rel
			return true
		}
		for _, c := range n.Children {
			if walk(c, rel) {
				return true
			}
		}
		return false
	}
	walk(root, "")
	return found
}

// UsesIndex expects the plan to read relation through index.
func UsesIndex(relation, index string) PlanExpectation {
	return PlanExpectation{
		describe: fmt.Sprintf("uses index %s on %s", index, relation),
		check: func(p *ExplainPlan) error {
			used := p.IndexesUsed()[relation]
			for _, name := range used {
				if name == index {
					return nil
				}
			}
			if len(used) == 0 {
				return fmt.Errorf("expected index %q on %q, but the plan reads %q with no index at all (%s)",
					index, relation, relation, strings.Join(p.ScanTypes(relation), ", "))
			}
			return fmt.Errorf("expected index %q on %q, got %s",
				index, relation, strings.Join(used, ", "))
		},
	}
}

// UsesAnyIndex expects the plan to read relation through some index,
// without saying which.
//
// It is the assertion to reach for most of the time. Naming the index
// pins the test to an object a migration can rename, and the thing
// worth defending is almost always "this does not scan the table"
// rather than "this uses precisely that index".
func UsesAnyIndex(relation string) PlanExpectation {
	return PlanExpectation{
		describe: fmt.Sprintf("uses some index on %s", relation),
		check: func(p *ExplainPlan) error {
			if len(p.IndexesUsed()[relation]) > 0 {
				return nil
			}
			return fmt.Errorf("expected an index scan on %q, got %s",
				relation, strings.Join(p.ScanTypes(relation), ", "))
		},
	}
}

// NoSeqScanOn expects the plan not to sequentially scan relation.
//
// PostgreSQL is right to choose a sequential scan on a small table,
// so this is an assertion about a table you expect to be large. On a
// test fixture of twenty rows it will fail for a query that is
// perfectly correct — populate the table, or assert on the plan of a
// query against a table the fixture fills properly.
func NoSeqScanOn(relation string) PlanExpectation {
	return PlanExpectation{
		describe: fmt.Sprintf("no sequential scan on %s", relation),
		check: func(p *ExplainPlan) error {
			for _, n := range p.Nodes() {
				if n.Relation == relation && strings.HasPrefix(n.Type, "Seq Scan") {
					return fmt.Errorf("sequential scan on %q (estimated %d rows)", relation, n.PlanRows)
				}
			}
			return nil
		},
	}
}

// NoNestedLoopOver expects relation not to be read on the inner side
// of a nested loop — the shape behind a query that is fast on a test
// fixture and quadratic in production.
func NoNestedLoopOver(relation string) PlanExpectation {
	return PlanExpectation{
		describe: fmt.Sprintf("no nested loop over %s", relation),
		check: func(p *ExplainPlan) error {
			var bad bool
			var walk func(*PlanNode, bool)
			walk = func(n *PlanNode, insideLoop bool) {
				if n == nil {
					return
				}
				if insideLoop && n.Relation == relation {
					bad = true
				}
				loop := strings.HasPrefix(n.Type, "Nested Loop")
				for i, c := range n.Children {
					// The inner side is the second child; the first
					// is the driving relation and reading it once is
					// not the problem.
					walk(c, insideLoop || (loop && i > 0))
				}
			}
			walk(p.Root, false)
			if bad {
				return fmt.Errorf("%q is read on the inner side of a nested loop", relation)
			}
			return nil
		},
	}
}

// NoNodeType expects no node of the given type, e.g. "Materialize" or
// "Sort". Matching is by prefix, so "Seq Scan" also catches
// "Parallel Seq Scan".
func NoNodeType(nodeType string) PlanExpectation {
	return PlanExpectation{
		describe: fmt.Sprintf("no %s node", nodeType),
		check: func(p *ExplainPlan) error {
			for _, n := range p.Nodes() {
				if strings.Contains(n.Type, nodeType) {
					return fmt.Errorf("plan contains a %q node", n.Type)
				}
			}
			return nil
		},
	}
}

// MaxCost expects the planner's total cost estimate to stay under
// limit.
//
// Cost units are arbitrary and comparable only against themselves, so
// this is a ratchet rather than a threshold: record what the query
// costs today, set the limit somewhat above it, and let the test
// catch the change that makes it an order of magnitude worse.
func MaxCost(limit float64) PlanExpectation {
	return PlanExpectation{
		describe: fmt.Sprintf("total cost under %.0f", limit),
		check: func(p *ExplainPlan) error {
			if p.TotalCost <= limit {
				return nil
			}
			return fmt.Errorf("total cost %.0f exceeds %.0f", p.TotalCost, limit)
		},
	}
}

// String returns the expectation's description, so a failing test can
// print what was being asserted.
func (e PlanExpectation) String() string { return e.describe }
