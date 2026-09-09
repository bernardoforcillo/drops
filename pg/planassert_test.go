package pg_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// planFrom builds an ExplainPlan out of raw EXPLAIN JSON, which is
// how a real plan arrives and therefore the only honest fixture.
func planFrom(t *testing.T, raw string) *pg.ExplainPlan {
	t.Helper()
	p, err := pg.ParseExplainJSON(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

const bitmapPlan = `[{"Plan":{
  "Node Type":"Bitmap Heap Scan",
  "Relation Name":"orders",
  "Total Cost":420.5,
  "Plan Rows":120,
  "Plans":[{
    "Node Type":"Bitmap Index Scan",
    "Index Name":"orders_customer_created_idx",
    "Total Cost":12.0,
    "Plan Rows":120
  }]
}}]`

const seqPlan = `[{"Plan":{
  "Node Type":"Seq Scan",
  "Relation Name":"orders",
  "Total Cost":9800.0,
  "Plan Rows":250000
}}]`

// A Bitmap Index Scan names the index and leaves the relation to the
// heap scan above it. Attributing it correctly is the whole reason
// this helper exists.
func TestIndexesUsedAttributesBitmapScans(t *testing.T) {
	got := planFrom(t, bitmapPlan).IndexesUsed()
	idx, ok := got["orders"]
	if !ok {
		t.Fatalf("no indexes attributed to orders: %v", got)
	}
	if len(idx) != 1 || idx[0] != "orders_customer_created_idx" {
		t.Errorf("indexes = %v", idx)
	}
}

func TestUsesIndexExpectations(t *testing.T) {
	plan := planFrom(t, bitmapPlan)
	if err := plan.Expect(
		pg.UsesIndex("orders", "orders_customer_created_idx"),
		pg.UsesAnyIndex("orders"),
		pg.NoSeqScanOn("orders"),
		pg.MaxCost(1000),
	); err != nil {
		t.Errorf("expectations failed on a plan that satisfies them: %v", err)
	}
}

func TestExpectationsFailUsefully(t *testing.T) {
	plan := planFrom(t, seqPlan)
	err := plan.Expect(
		pg.UsesAnyIndex("orders"),
		pg.NoSeqScanOn("orders"),
		pg.MaxCost(100),
	)
	if err == nil {
		t.Fatal("expected the sequential-scan plan to fail every expectation")
	}
	msg := err.Error()
	// All three are reported, not just the first: a wrong plan is
	// usually wrong in more than one way.
	for _, want := range []string{"index scan", "sequential scan", "exceeds"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message is missing %q:\n%s", want, msg)
		}
	}
	// And it names what it saw, so the fix does not need a second run.
	if !strings.Contains(msg, "250000") {
		t.Errorf("failure message does not report the row estimate:\n%s", msg)
	}
}

func TestUsesIndexNamesTheWrongIndex(t *testing.T) {
	err := planFrom(t, bitmapPlan).Expect(pg.UsesIndex("orders", "orders_pkey"))
	if err == nil {
		t.Fatal("expected a failure")
	}
	if !strings.Contains(err.Error(), "orders_customer_created_idx") {
		t.Errorf("failure does not say which index was used: %v", err)
	}
}

const nestedLoopPlan = `[{"Plan":{
  "Node Type":"Nested Loop",
  "Total Cost":50000.0,
  "Plans":[
    {"Node Type":"Seq Scan","Relation Name":"customers","Plan Rows":1000},
    {"Node Type":"Index Scan","Relation Name":"orders","Index Name":"orders_pkey","Plan Rows":1}
  ]
}}]`

// The shape behind a query that is fast on a fixture and quadratic in
// production.
func TestNoNestedLoopOver(t *testing.T) {
	plan := planFrom(t, nestedLoopPlan)
	if err := plan.Expect(pg.NoNestedLoopOver("orders")); err == nil {
		t.Error("the inner relation of a nested loop was not detected")
	}
	// The driving relation is read once; that is not the problem.
	if err := plan.Expect(pg.NoNestedLoopOver("customers")); err != nil {
		t.Errorf("the driving relation was reported as a nested loop: %v", err)
	}
}

func TestNoNodeType(t *testing.T) {
	plan := planFrom(t, nestedLoopPlan)
	if err := plan.Expect(pg.NoNodeType("Nested Loop")); err == nil {
		t.Error("Nested Loop not detected")
	}
	if err := plan.Expect(pg.NoNodeType("Materialize")); err != nil {
		t.Errorf("Materialize reported in a plan without one: %v", err)
	}
}

func TestScanTypes(t *testing.T) {
	got := planFrom(t, bitmapPlan).ScanTypes("orders")
	if len(got) != 2 || got[0] != "Bitmap Heap Scan" || got[1] != "Bitmap Index Scan" {
		t.Errorf("ScanTypes = %v", got)
	}
	if got := planFrom(t, seqPlan).ScanTypes("orders"); len(got) != 1 || got[0] != "Seq Scan" {
		t.Errorf("ScanTypes = %v", got)
	}
}

func TestExpectOnEmptyPlan(t *testing.T) {
	var p *pg.ExplainPlan
	if got := p.Nodes(); got != nil {
		t.Errorf("Nodes() on a nil plan = %v", got)
	}
}

func TestPlanExpectationString(t *testing.T) {
	if got := pg.UsesIndex("orders", "ix").String(); !strings.Contains(got, "ix") {
		t.Errorf("String() = %q", got)
	}
}
