package integration_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// Plan assertions are parsing PostgreSQL's own EXPLAIN output, so a
// fixture proves the parser against a shape somebody wrote down and
// this proves it against the shape the server actually emits — which
// is where the awkward case lives: a Bitmap Index Scan names the index
// and leaves the relation to the Bitmap Heap Scan above it.

func planFixture(t *testing.T, rows int) (*pg.DB, string, string) {
	t.Helper()
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "plan"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.BigInt("customerId").NotNull())
	pg.Add(tbl, pg.Text("payload").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %q SELECT g, g %% 5000, repeat('x', 50) FROM generate_series(1, $1) g`,
		tbl.Name()), rows); err != nil {
		t.Fatal(err)
	}
	idx := tbl.Name() + "_cust_idx"
	if _, err := db.Exec(ctx, fmt.Sprintf(
		`CREATE INDEX %q ON %q ("customerId")`, idx, tbl.Name())); err != nil {
		t.Fatal(err)
	}
	// Without statistics the planner has no reason to prefer the
	// index, and the assertions below would be about the fixture
	// rather than about the query.
	if _, err := db.Exec(ctx, fmt.Sprintf(`ANALYZE %q`, tbl.Name())); err != nil {
		t.Fatal(err)
	}
	return db, tbl.Name(), idx
}

func TestPGPlanAssertionsAgainstRealExplainOutput(t *testing.T) {
	db, tbl, idx := planFixture(t, 50000)
	ctx := context.Background()

	plan, err := pg.Explain(db, ctx,
		fmt.Sprintf(`SELECT "id" FROM %q WHERE "customerId" = $1`, tbl), 42)
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}

	// The index is attributed to the relation even when the plan is a
	// bitmap pair, which is the case a hand-written fixture is most
	// likely to get wrong.
	used := plan.IndexesUsed()
	names := used[tbl]
	if len(names) == 0 {
		t.Fatalf("no index attributed to %q; the plan reads it with %v\n%s",
			tbl, plan.ScanTypes(tbl), plan.JSON)
	}
	found := false
	for _, n := range names {
		if n == idx {
			found = true
		}
	}
	if !found {
		t.Errorf("IndexesUsed = %v, want %s", names, idx)
	}

	if err := plan.Expect(
		pg.UsesIndex(tbl, idx),
		pg.UsesAnyIndex(tbl),
		pg.NoSeqScanOn(tbl),
	); err != nil {
		t.Errorf("expectations failed on a plan that satisfies them: %v\n%s", err, plan.JSON)
	}
	if len(plan.Nodes()) == 0 {
		t.Error("the plan tree is empty")
	}
	if plan.TotalCost <= 0 || plan.PlanRows <= 0 {
		t.Errorf("cost %v rows %v", plan.TotalCost, plan.PlanRows)
	}
}

// And the expectations have to fail when the plan does not satisfy
// them — a predicate that always passes is not an assertion.
func TestPGPlanAssertionsFailOnASequentialScan(t *testing.T) {
	db, tbl, idx := planFixture(t, 50000)
	ctx := context.Background()

	plan, err := pg.Explain(db, ctx,
		fmt.Sprintf(`SELECT count(*) FROM %q WHERE "payload" LIKE $1`, tbl), "%zzz%")
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	err = plan.Expect(pg.NoSeqScanOn(tbl), pg.UsesIndex(tbl, idx))
	if err == nil {
		t.Fatalf("a full scan satisfied NoSeqScanOn and UsesIndex\n%s", plan.JSON)
	}
	msg := err.Error()
	if !strings.Contains(msg, "sequential scan") {
		t.Errorf("failure does not name the sequential scan: %v", msg)
	}
	if !strings.Contains(msg, "no index at all") {
		t.Errorf("failure does not say the relation is read without an index: %v", msg)
	}
}

// A plan captured today has to be readable back tomorrow, which is the
// point of keeping the raw payload.
func TestPGExplainJSONRoundTrips(t *testing.T) {
	db, tbl, _ := planFixture(t, 1000)
	plan, err := pg.Explain(db, context.Background(),
		fmt.Sprintf(`SELECT "id" FROM %q WHERE "customerId" = $1`, tbl), 1)
	if err != nil {
		t.Fatal(err)
	}
	back, err := pg.ParseExplainJSON(plan.JSON)
	if err != nil {
		t.Fatalf("ParseExplainJSON on the server's own output: %v", err)
	}
	if back.Fingerprint() != plan.Fingerprint() {
		t.Errorf("fingerprints differ after a round trip: %s vs %s",
			back.Fingerprint(), plan.Fingerprint())
	}
	if len(back.Nodes()) != len(plan.Nodes()) {
		t.Errorf("node counts differ: %d vs %d", len(back.Nodes()), len(plan.Nodes()))
	}
}

// pg_hint_plan is not installed here and almost certainly is not
// installed wherever this runs. What matters is that the comment drops
// prepends is a *comment*: the server must parse it, the query must
// return the same rows, and the statement must not become invalid.
// That is the failure mode a hint could realistically introduce, and
// it is the one an installed extension would not change.
func TestPGPlanHintCommentIsHarmless(t *testing.T) {
	db, tbl, idx := planFixture(t, 1000)
	ctx := context.Background()
	sql := fmt.Sprintf(`SELECT count(*) FROM %q WHERE "customerId" = $1`, tbl)

	count := func(ctx context.Context) int64 {
		t.Helper()
		rows, err := db.Query(ctx, sql, 3)
		if err != nil {
			t.Fatalf("the hinted statement was rejected: %v", err)
		}
		defer rows.Close()
		var n int64
		if rows.Next() {
			_ = rows.Scan(&n)
		}
		return n
	}

	plain := count(ctx)
	hinted := count(pg.WithPlanHints(ctx,
		pg.IndexScan(tbl, idx),
		pg.NoSeqScan(tbl),
	))
	if plain != hinted {
		t.Errorf("the hint changed the answer: %d without, %d with", plain, hinted)
	}

	// Every constructor, on one statement, so a directive that the
	// parser would choke on cannot hide behind an untested one.
	all := pg.WithPlanHints(ctx,
		pg.SeqScan(tbl), pg.NoSeqScan(tbl), pg.TidScan(tbl), pg.NoTidScan(tbl),
		pg.IndexScan(tbl, idx), pg.NoIndexScan(tbl),
		pg.IndexOnlyScan(tbl, idx), pg.NoIndexOnlyScan(tbl),
		pg.BitmapScan(tbl, idx), pg.NoBitmapScan(tbl),
		pg.NestLoop(tbl, "other"), pg.NoNestLoop(tbl, "other"),
		pg.HashJoin(tbl, "other"), pg.NoHashJoin(tbl, "other"),
		pg.MergeJoin(tbl, "other"), pg.NoMergeJoin(tbl, "other"),
		pg.Leading(tbl, "other"),
		pg.RowsAbsolute(100, tbl, "other"), pg.RowsMultiply(10, tbl, "other"),
		pg.Parallel(tbl, 4, false), pg.SetGUC("enable_seqscan", "off"),
	)
	if got := count(all); got != plain {
		t.Errorf("the full hint set changed the answer: %d, want %d", got, plain)
	}

	// And EXPLAIN still works through the comment, which is what an
	// assertion on a hinted plan depends on.
	if _, err := pg.Explain(db, all, sql, 3); err != nil {
		t.Errorf("EXPLAIN on a hinted statement: %v", err)
	}
}
