package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// recordingDriver keeps the exact statement text it was handed, which
// is the only thing worth asserting about a comment. It answers every
// query with an empty cursor rather than a nil one, so callers that
// iterate the result — everything in pg/logical.go does — reach their
// "no rows" branch instead of a nil dereference.
type recordingDriver struct {
	sql string
}

func (d *recordingDriver) Exec(_ context.Context, sql string, _ ...any) (drops.Result, error) {
	d.sql = sql
	return nil, nil
}

func (d *recordingDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.sql = sql
	return &stubRows{}, nil
}

func (d *recordingDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }

func TestHintRendering(t *testing.T) {
	cases := []struct {
		hint pg.Hint
		want string
	}{
		{pg.SeqScan("orders"), "SeqScan(orders)"},
		{pg.NoSeqScan("orders"), "NoSeqScan(orders)"},
		{pg.IndexScan("orders"), "IndexScan(orders)"},
		{pg.IndexScan("orders", "orders_pkey"), "IndexScan(orders orders_pkey)"},
		{pg.IndexOnlyScan("o", "i1", "i2"), "IndexOnlyScan(o i1 i2)"},
		{pg.BitmapScan("t", "ix"), "BitmapScan(t ix)"},
		{pg.TidScan("t"), "TidScan(t)"},
		{pg.NestLoop("a", "b"), "NestLoop(a b)"},
		{pg.NoHashJoin("a", "b", "c"), "NoHashJoin(a b c)"},
		{pg.MergeJoin("a", "b"), "MergeJoin(a b)"},
		{pg.Leading("a", "b", "c"), "Leading(a b c)"},
		{pg.RowsAbsolute(100, "a", "b"), "Rows(a b #100)"},
		{pg.RowsMultiply(10, "a", "b"), "Rows(a b *10)"},
		{pg.Parallel("t", 4, false), "Parallel(t 4 soft)"},
		{pg.Parallel("t", 0, true), "Parallel(t 0 hard)"},
		{pg.SetGUC("enable_seqscan", "off"), "Set(enable_seqscan off)"},
		// A name that is not a bare lowercase identifier is quoted,
		// which is also how a case-sensitive relation reaches
		// pg_hint_plan intact.
		{pg.SeqScan("Orders"), `SeqScan("Orders")`},
		{pg.SeqScan("my table"), `SeqScan("my table")`},
		{pg.SeqScan(`we"ird`), `SeqScan("we""ird")`},
	}
	for _, tc := range cases {
		if got := tc.hint.String(); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
		if !tc.hint.Valid() {
			t.Errorf("%q reported invalid", tc.want)
		}
	}
}

// A name that could end the comment early cannot be quoted out of the
// problem, so the hint is refused instead.
func TestHintRefusesCommentBreakers(t *testing.T) {
	bad := []pg.Hint{
		pg.SeqScan("orders */ DROP TABLE users; /*"),
		pg.SeqScan("a*/b"),
		pg.SeqScan("a/*b"),
		pg.SeqScan("line\nbreak"),
		pg.SeqScan("nul\x00byte"),
		pg.SeqScan(""),
		pg.IndexScan("orders", "idx*/x"),
		pg.SetGUC("work_mem", "*/"),
		// A join hint needs two relations; one does nothing.
		pg.NestLoop("solo"),
		pg.Leading(),
		pg.RowsAbsolute(5, "only"),
		pg.Parallel("t", -1, false),
	}
	for _, h := range bad {
		if h.Valid() {
			t.Errorf("hint %q reported valid", h.String())
		}
		if got := h.String(); got != "" {
			t.Errorf("invalid hint rendered as %q, want empty", got)
		}
	}
	if _, ok := pg.HintsValid(bad); ok {
		t.Error("HintsValid accepted a set containing invalid hints")
	}
	if _, ok := pg.HintsValid([]pg.Hint{pg.SeqScan("t")}); !ok {
		t.Error("HintsValid rejected a valid set")
	}
}

func TestRenderPlanHints(t *testing.T) {
	got := pg.RenderPlanHints([]pg.Hint{
		pg.IndexScan("orders", "orders_customer_idx"),
		pg.NestLoop("orders", "customers"),
	})
	want := "/*+ IndexScan(orders orders_customer_idx) NestLoop(orders customers) */"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got := pg.RenderPlanHints(nil); got != "" {
		t.Errorf("nil hints rendered %q", got)
	}
	// A set of only-invalid hints produces no comment at all rather
	// than an empty one the extension would have to parse.
	if got := pg.RenderPlanHints([]pg.Hint{pg.SeqScan("a*/b")}); got != "" {
		t.Errorf("all-invalid set rendered %q", got)
	}
}

func TestPlanHintsReachTheStatement(t *testing.T) {
	drv := &recordingDriver{}
	db := pg.New(drv)
	ctx := pg.WithPlanHints(context.Background(), pg.NoSeqScan("orders"))

	if _, err := db.Query(ctx, "SELECT * FROM orders"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(drv.sql, "/*+ NoSeqScan(orders) */") {
		t.Errorf("hint is not at the front of the statement: %q", drv.sql)
	}
	if !strings.Contains(drv.sql, "SELECT * FROM orders") {
		t.Errorf("statement text was lost: %q", drv.sql)
	}
}

// The hint goes at the front, the SQLCommenter tag at the back, and
// both survive on the same statement.
func TestPlanHintCoexistsWithQueryTag(t *testing.T) {
	drv := &recordingDriver{}
	db := pg.New(drv)
	ctx := drops.WithQueryTags(context.Background(), drops.Tag{Key: "controller", Value: "orders"})
	ctx = pg.WithPlanHints(ctx, pg.SeqScan("orders"))

	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(drv.sql, "/*+ SeqScan(orders) */") {
		t.Errorf("hint not leading: %q", drv.sql)
	}
	if !strings.HasSuffix(drv.sql, "*/") || !strings.Contains(drv.sql, "controller='orders'") {
		t.Errorf("query tag lost: %q", drv.sql)
	}
}

func TestWithPlanHintsReplacesAndClears(t *testing.T) {
	ctx := pg.WithPlanHints(context.Background(), pg.SeqScan("a"))
	ctx = pg.WithPlanHints(ctx, pg.SeqScan("b"))
	got := pg.PlanHints(ctx)
	if len(got) != 1 || got[0].String() != "SeqScan(b)" {
		t.Fatalf("second call did not replace the first: %v", got)
	}

	cleared := pg.WithPlanHints(ctx)
	if len(pg.PlanHints(cleared)) != 0 {
		t.Error("WithPlanHints() with no hints did not clear")
	}
	drv := &recordingDriver{}
	if _, err := pg.New(drv).Exec(cleared, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(drv.sql, "/*+") {
		t.Errorf("cleared context still emitted a hint: %q", drv.sql)
	}
}

// The caller's slice must not be able to change the hints a context
// already carries.
func TestWithPlanHintsCopiesTheSlice(t *testing.T) {
	hints := []pg.Hint{pg.SeqScan("a")}
	ctx := pg.WithPlanHints(context.Background(), hints...)
	hints[0] = pg.SeqScan("b")
	if got := pg.PlanHints(ctx)[0].String(); got != "SeqScan(a)" {
		t.Errorf("context observed a later mutation: %q", got)
	}
}

// A statement with a hint still routes as a read; the leading comment
// must not confuse the keyword scan Replicated uses.
func TestPlanHintDoesNotBreakReplicaRouting(t *testing.T) {
	primary := &countingDriver{}
	replica := &countingDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))
	ctx := pg.WithPlanHints(context.Background(), pg.SeqScan("orders"))

	if _, err := db.Query(ctx, "SELECT * FROM orders"); err != nil {
		t.Fatal(err)
	}
	if replica.queries != 1 || primary.queries != 0 {
		t.Errorf("hinted read routed to primary=%d replica=%d, want 0/1",
			primary.queries, replica.queries)
	}
}
