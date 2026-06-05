package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// explainPlanSeqScan mimics PG's FORMAT JSON shape for a single
// Seq Scan against "players".
const explainPlanSeqScan = `[
  {
    "Plan": {
      "Node Type": "Seq Scan",
      "Relation Name": "players",
      "Startup Cost": 0.00,
      "Total Cost": 25.00,
      "Plan Rows": 1000,
      "Plans": []
    }
  }
]`

// explainPlanIndexScan mimics the same query after an index has been
// added — same shape (Index Scan), different cost.
const explainPlanIndexScan = `[
  {
    "Plan": {
      "Node Type": "Index Scan",
      "Relation Name": "players",
      "Index Name": "playersRegionIdx",
      "Startup Cost": 0.42,
      "Total Cost": 8.44,
      "Plan Rows": 50,
      "Plans": []
    }
  }
]`

// explainPlanNestedJoin is a 2-table hash join, exercising the child
// walk and join-type extraction.
const explainPlanNestedJoin = `[
  {
    "Plan": {
      "Node Type": "Hash Join",
      "Join Type": "Inner",
      "Startup Cost": 1.0,
      "Total Cost": 100.0,
      "Plan Rows": 500,
      "Plans": [
        {
          "Node Type": "Seq Scan",
          "Relation Name": "matches",
          "Total Cost": 50.0,
          "Plan Rows": 1000,
          "Plans": []
        },
        {
          "Node Type": "Hash",
          "Total Cost": 25.0,
          "Plan Rows": 100,
          "Plans": [
            {
              "Node Type": "Index Scan",
              "Relation Name": "players",
              "Index Name": "playersPK",
              "Total Cost": 20.0,
              "Plan Rows": 100,
              "Plans": []
            }
          ]
        }
      ]
    }
  }
]`

func TestExplainParsesSeqScanPlan(t *testing.T) {
	drv := &explainDriver{response: explainPlanSeqScan}
	db := pg.New(drv)

	plan, err := pg.Explain(db, context.Background(), "SELECT * FROM players WHERE region = $1", "eu")
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if plan.Root.Type != "Seq Scan" {
		t.Errorf("type: %q", plan.Root.Type)
	}
	if plan.Root.Relation != "players" {
		t.Errorf("relation: %q", plan.Root.Relation)
	}
	if plan.TotalCost != 25.0 {
		t.Errorf("cost: %v", plan.TotalCost)
	}
	if got := plan.SeqScans(); len(got) != 1 || got[0] != "players" {
		t.Errorf("SeqScans: %v", got)
	}
	if got := plan.UsedIndexes(); len(got) != 0 {
		t.Errorf("UsedIndexes: %v", got)
	}
}

func TestExplainParsesIndexScanPlan(t *testing.T) {
	drv := &explainDriver{response: explainPlanIndexScan}
	db := pg.New(drv)
	plan, err := pg.Explain(db, context.Background(), "...")
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if got := plan.UsedIndexes(); len(got) != 1 || got[0] != "playersRegionIdx" {
		t.Errorf("UsedIndexes: %v", got)
	}
}

func TestExplainWithAnalyzeAddsAnalyzeFlag(t *testing.T) {
	drv := &explainDriver{response: explainPlanSeqScan}
	db := pg.New(drv)
	_, err := pg.ExplainWith(db, context.Background(), pg.ExplainOptions{Analyze: true}, "SELECT 1")
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if !strings.Contains(drv.lastSQL, "ANALYZE") {
		t.Errorf("expected ANALYZE in EXPLAIN call, got: %q", drv.lastSQL)
	}
}

func TestExplainBuffersRequiresAnalyze(t *testing.T) {
	drv := &explainDriver{response: explainPlanSeqScan}
	db := pg.New(drv)
	_, err := pg.ExplainWith(db, context.Background(), pg.ExplainOptions{Buffers: true}, "SELECT 1")
	if err == nil {
		t.Error("Buffers without Analyze must error")
	}
}

func TestExplainExtractsJoinTypes(t *testing.T) {
	drv := &explainDriver{response: explainPlanNestedJoin}
	db := pg.New(drv)
	plan, err := pg.Explain(db, context.Background(), "...")
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	joins := plan.JoinTypes()
	if len(joins) != 1 || joins[0] != "Hash Join" {
		t.Errorf("JoinTypes: %v", joins)
	}
	if got := plan.SeqScans(); len(got) != 1 || got[0] != "matches" {
		t.Errorf("SeqScans: %v", got)
	}
	if got := plan.UsedIndexes(); len(got) != 1 || got[0] != "playersPK" {
		t.Errorf("UsedIndexes: %v", got)
	}
}

func TestExplainFingerprintStableAcrossCostChanges(t *testing.T) {
	// Same shape, different costs — fingerprints must match.
	a, _ := pg.Explain(pg.New(&explainDriver{response: explainPlanSeqScan}), context.Background(), "q")
	b, _ := pg.Explain(pg.New(&explainDriver{response: strings.Replace(explainPlanSeqScan, "25.00", "999.99", 1)}), context.Background(), "q")
	if a.Fingerprint() != b.Fingerprint() {
		t.Errorf("fingerprints diverged on cost change: %q vs %q", a.Fingerprint(), b.Fingerprint())
	}
}

func TestExplainFingerprintDiffersAcrossNodeTypeChange(t *testing.T) {
	a, _ := pg.Explain(pg.New(&explainDriver{response: explainPlanSeqScan}), context.Background(), "q")
	b, _ := pg.Explain(pg.New(&explainDriver{response: explainPlanIndexScan}), context.Background(), "q")
	if a.Fingerprint() == b.Fingerprint() {
		t.Errorf("Seq vs Index Scan should hash differently: both = %q", a.Fingerprint())
	}
}

func TestDiffPlansSpotsSeqToIndex(t *testing.T) {
	before, _ := pg.Explain(pg.New(&explainDriver{response: explainPlanSeqScan}), context.Background(), "q")
	after, _ := pg.Explain(pg.New(&explainDriver{response: explainPlanIndexScan}), context.Background(), "q")
	diff := pg.DiffPlans(before, after)
	if diff.Same {
		t.Error("Same must be false on shape change")
	}
	if len(diff.SeqScansRemoved) != 1 || diff.SeqScansRemoved[0] != "players" {
		t.Errorf("SeqScansRemoved: %v", diff.SeqScansRemoved)
	}
	if len(diff.IndexesAdded) != 1 || diff.IndexesAdded[0] != "playersRegionIdx" {
		t.Errorf("IndexesAdded: %v", diff.IndexesAdded)
	}
	if diff.CostDelta >= 0 {
		t.Errorf("cost should drop seq→index, got delta %v", diff.CostDelta)
	}
}

func TestDiffPlansSameWhenIdentical(t *testing.T) {
	a, _ := pg.Explain(pg.New(&explainDriver{response: explainPlanSeqScan}), context.Background(), "q")
	b, _ := pg.Explain(pg.New(&explainDriver{response: explainPlanSeqScan}), context.Background(), "q")
	d := pg.DiffPlans(a, b)
	if !d.Same {
		t.Errorf("identical plans must report Same: %+v", d)
	}
}

func TestExplainEmptyResultErrors(t *testing.T) {
	drv := &explainDriver{response: ""} // Query returns no rows
	db := pg.New(drv)
	_, err := pg.Explain(db, context.Background(), "...")
	if err == nil {
		t.Error("expected error on no rows")
	}
}

// explainDriver returns canned EXPLAIN JSON on Query — enough to
// exercise the parsing path without a real database.
type explainDriver struct {
	response string
	lastSQL  string
}

func (d *explainDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, errors.New("explainDriver: no Exec")
}

func (d *explainDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.lastSQL = sql
	if d.response == "" {
		return &fakeRows{cols: []string{"QUERY PLAN"}}, nil
	}
	return &fakeRows{
		cols: []string{"QUERY PLAN"},
		data: [][]any{{json.RawMessage(d.response)}},
	}, nil
}
func (d *explainDriver) Begin(context.Context) (drops.Tx, error) { return nil, errors.New("no tx") }
