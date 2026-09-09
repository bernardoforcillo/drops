package integration_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// The sketch arithmetic is unit-tested. What needs a server is the
// statement that fills it: a GROUP BY over a quoted column, a
// TABLESAMPLE clause, and the NULL exclusion — none of which a
// rendered string can be held to.

func sketchFixture(t *testing.T, rows int) (*pg.DB, string) {
	t.Helper()
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "sk"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("status").Nullable())
	pg.Add(tbl, pg.BigInt("customerId").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	// A quarter 'pending', the rest spread over many values, plus a
	// handful of NULLs.
	if _, err := db.Exec(context.Background(), fmt.Sprintf(`
		INSERT INTO %q
		SELECT g,
		       CASE WHEN g %% 4 = 0 THEN 'pending'
		            WHEN g %% 997 = 0 THEN NULL
		            ELSE 'v' || (g %% 500) END,
		       g %% 1000
		FROM generate_series(1, $1) g`, tbl.Name()), rows); err != nil {
		t.Fatal(err)
	}
	return db, tbl.Name()
}

func TestPGSketchColumnEstimatesSelectivity(t *testing.T) {
	db, tbl := sketchFixture(t, 40000)
	ctx := context.Background()

	s, err := pg.SketchColumn(ctx, db, tbl, "status", pg.SketchOptions{})
	if err != nil {
		t.Fatalf("SketchColumn: %v", err)
	}

	// NULLs are skipped, so the total is the non-null count rather
	// than the row count.
	rows, err := db.Query(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE "status" IS NOT NULL`, tbl))
	if err != nil {
		t.Fatal(err)
	}
	var nonNull int64
	if rows.Next() {
		_ = rows.Scan(&nonNull)
	}
	_ = rows.Close()
	if int64(s.Total()) != nonNull {
		t.Errorf("Total() = %d, want the non-null count %d", s.Total(), nonNull)
	}

	// A quarter of the table, and the answer a caller acts on.
	sel := s.Selectivity("pending")
	if sel < 0.23 || sel > 0.28 {
		t.Errorf("selectivity of 'pending' = %v, want ~0.25", sel)
	}
	if sel <= 0.2 {
		t.Errorf("a quarter of the table reported as selective (%v)", sel)
	}

	// A value in a fraction of a percent, and the opposite answer.
	if got := s.Selectivity("v7"); got > 0.02 {
		t.Errorf("selectivity of a rare value = %v", got)
	}
	// An absent value must be near zero, not merely small.
	if got := s.Estimate("no-such-status"); float64(got) > s.ExpectedError()+1 {
		t.Errorf("an absent value estimated at %d, above the sketch's own error bound %v",
			got, s.ExpectedError())
	}

	// The estimate is an upper bound. Check it against the truth for
	// the value that matters most.
	rows, err = db.Query(ctx, fmt.Sprintf(`SELECT count(*) FROM %q WHERE "status" = 'pending'`, tbl))
	if err != nil {
		t.Fatal(err)
	}
	var truth int64
	if rows.Next() {
		_ = rows.Scan(&truth)
	}
	_ = rows.Close()
	if int64(s.Estimate("pending")) < truth {
		t.Errorf("estimate %d is below the true count %d — the sketch must never undercount",
			s.Estimate("pending"), truth)
	}

	if s.DistinctEstimate() < 100 {
		t.Errorf("DistinctEstimate() = %d for a column with ~500 values", s.DistinctEstimate())
	}
}

// TABLESAMPLE is a separate clause the server has to accept, and the
// scaling has to bring the total back to roughly the table's size.
func TestPGSketchColumnSamples(t *testing.T) {
	db, tbl := sketchFixture(t, 40000)
	ctx := context.Background()

	s, err := pg.SketchColumn(ctx, db, tbl, "customerId", pg.SketchOptions{Sample: 0.25})
	if err != nil {
		t.Fatalf("SketchColumn with a sample: %v", err)
	}
	if s.Total() == 0 {
		t.Fatal("the sampled sketch is empty")
	}
	// TABLESAMPLE SYSTEM picks whole pages, so the fraction is
	// approximate. A generous band still catches a scaling that is
	// out by a factor rather than by a page.
	if s.Total() < 15000 || s.Total() > 90000 {
		t.Errorf("sampled total scaled to %d, want roughly 40000", s.Total())
	}
}

func TestPGSketchColumnRejectsBadIdentifiers(t *testing.T) {
	db, tbl := sketchFixture(t, 10)
	ctx := context.Background()

	for _, tc := range []struct{ table, column string }{
		{"", "status"},
		{tbl, ""},
		{"a\"b", "status"},
		{tbl, "a\x00b"},
	} {
		if _, err := pg.SketchColumn(ctx, db, tc.table, tc.column, pg.SketchOptions{}); err == nil {
			t.Errorf("SketchColumn(%q, %q) was accepted", tc.table, tc.column)
		}
	}
}

// A sketch built per shard and merged has to equal one built in a
// single pass, or "build it in parallel" is not a real offer.
func TestPGSketchMergesAcrossPartitions(t *testing.T) {
	db, tbl := sketchFixture(t, 20000)
	ctx := context.Background()

	whole, err := pg.SketchColumn(ctx, db, tbl, "status", pg.SketchOptions{Width: 512, Depth: 4})
	if err != nil {
		t.Fatal(err)
	}

	// Two halves, sketched separately through the same statement
	// against views over the ranges.
	halves := make([]*pg.Sketch, 2)
	for i, pred := range []string{`"id" <= 10000`, `"id" > 10000`} {
		view := fmt.Sprintf("%s_half%d", tbl, i)
		if _, err := db.Exec(ctx, fmt.Sprintf(
			`CREATE VIEW %q AS SELECT * FROM %q WHERE %s`, view, tbl, pred)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP VIEW IF EXISTS %q`, view))
		})
		halves[i], err = pg.SketchColumn(ctx, db, view, "status", pg.SketchOptions{Width: 512, Depth: 4})
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := halves[0].Merge(halves[1]); err != nil {
		t.Fatal(err)
	}
	if halves[0].Total() != whole.Total() {
		t.Fatalf("merged total %d, single-pass total %d", halves[0].Total(), whole.Total())
	}
	for _, v := range []string{"pending", "v1", "v250", "absent"} {
		if halves[0].Estimate(v) != whole.Estimate(v) {
			t.Errorf("%s: merged %d, single-pass %d", v, halves[0].Estimate(v), whole.Estimate(v))
		}
	}
}

// The stored form has to survive a round trip, or "compute it nightly
// and load it at start-up" does not work.
func TestPGSketchRoundTripsThroughBytes(t *testing.T) {
	db, tbl := sketchFixture(t, 5000)
	ctx := context.Background()

	s, err := pg.SketchColumn(ctx, db, tbl, "status", pg.SketchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := s.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}

	// Through the database, since that is where it would be kept.
	name := integration.UniqueName(t, "sketches")
	if _, err := db.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE %q (id text PRIMARY KEY, data bytea NOT NULL)`, name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %q`, name))
	})
	if _, err := db.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %q VALUES ($1, $2)`, name), "status", blob); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(ctx, fmt.Sprintf(`SELECT data FROM %q WHERE id = $1`, name), "status")
	if err != nil {
		t.Fatal(err)
	}
	var back []byte
	if rows.Next() {
		_ = rows.Scan(&back)
	}
	_ = rows.Close()

	var loaded pg.Sketch
	if err := loaded.UnmarshalBinary(back); err != nil {
		t.Fatalf("UnmarshalBinary after a round trip through bytea: %v", err)
	}
	if loaded.Total() != s.Total() {
		t.Errorf("total = %d, want %d", loaded.Total(), s.Total())
	}
	if loaded.Selectivity("pending") != s.Selectivity("pending") {
		t.Errorf("selectivity changed across the round trip")
	}
}
