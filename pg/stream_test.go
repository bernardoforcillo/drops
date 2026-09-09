package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// cursorDriver plays the server side of DECLARE / FETCH / CLOSE:
// it hands out rows in batches and records every statement it saw.
type cursorDriver struct {
	remaining int
	stmts     []string
	declared  string
}

func (d *cursorDriver) Exec(_ context.Context, sql string, _ ...any) (drops.Result, error) {
	d.stmts = append(d.stmts, sql)
	if strings.HasPrefix(sql, "DECLARE") {
		d.declared = sql
	}
	return affectedResult(0), nil
}

func (d *cursorDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.stmts = append(d.stmts, sql)
	if !strings.HasPrefix(sql, "FETCH") {
		return &stubRows{}, nil
	}
	n := d.batchSize(sql)
	if n > d.remaining {
		n = d.remaining
	}
	d.remaining -= n
	return &stubRows{left: n}, nil
}

func (d *cursorDriver) batchSize(sql string) int {
	var n int
	// "FETCH FORWARD <n> FROM ..."
	if _, err := fmtSscan(sql, &n); err != nil {
		return 0
	}
	return n
}

func (d *cursorDriver) Begin(_ context.Context) (drops.Tx, error) {
	return &cursorTx{d: d}, nil
}

func (d *cursorDriver) saw(prefix string) int {
	n := 0
	for _, s := range d.stmts {
		if strings.HasPrefix(s, prefix) {
			n++
		}
	}
	return n
}

type cursorTx struct{ d *cursorDriver }

func (t *cursorTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return t.d.Exec(ctx, sql, args...)
}
func (t *cursorTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return t.d.Query(ctx, sql, args...)
}
func (t *cursorTx) Begin(context.Context) (drops.Tx, error) { return t, nil }
func (t *cursorTx) Commit(context.Context) error            { return nil }
func (t *cursorTx) Rollback(context.Context) error          { return nil }

// fmtSscan pulls the row count out of "FETCH FORWARD n FROM ...".
func fmtSscan(sql string, n *int) (int, error) {
	fields := strings.Fields(sql)
	if len(fields) < 3 {
		return 0, errors.New("short")
	}
	var v int
	for _, c := range fields[2] {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}

func TestStreamQueryFetchesInBatches(t *testing.T) {
	drv := &cursorDriver{remaining: 25}
	db := pg.New(drv)

	var seen int
	err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{Batch: 10},
		"SELECT id FROM users", nil, func(rows drops.Rows) error {
			for rows.Next() {
				seen++
			}
			return rows.Err()
		})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 25 {
		t.Errorf("read %d rows, want 25", seen)
	}
	// 10, 10, 5 — the short batch ends the stream, so no fourth
	// round trip.
	if got := drv.saw("FETCH"); got != 3 {
		t.Errorf("issued %d FETCHes, want 3", got)
	}
	if got := drv.saw("CLOSE"); got != 1 {
		t.Errorf("issued %d CLOSEs, want 1", got)
	}
	if !strings.Contains(drv.declared, "NO SCROLL CURSOR FOR SELECT id FROM users") {
		t.Errorf("declaration = %q", drv.declared)
	}
	if strings.Contains(drv.declared, "WITH HOLD") {
		t.Errorf("declared WITH HOLD without being asked: %q", drv.declared)
	}
}

// A result that ends exactly on a batch boundary needs one more FETCH
// to discover it has ended.
func TestStreamQueryHandlesAnExactBoundary(t *testing.T) {
	drv := &cursorDriver{remaining: 20}
	db := pg.New(drv)
	var seen int
	err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{Batch: 10},
		"SELECT 1", nil, func(rows drops.Rows) error {
			for rows.Next() {
				seen++
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 20 {
		t.Errorf("read %d rows, want 20", seen)
	}
	if got := drv.saw("FETCH"); got != 3 {
		t.Errorf("issued %d FETCHes, want 3 (the third proves the end)", got)
	}
}

func TestStreamQueryEmptyResult(t *testing.T) {
	drv := &cursorDriver{remaining: 0}
	db := pg.New(drv)
	called := false
	err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{Batch: 10},
		"SELECT 1", nil, func(rows drops.Rows) error {
			for rows.Next() {
				called = true
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("the callback saw a row in an empty result")
	}
	if got := drv.saw("CLOSE"); got != 1 {
		t.Errorf("the cursor was not closed: %d CLOSEs", got)
	}
}

// Stopping early is the whole point of a cursor over a materialised
// result.
func TestStreamQueryStopsEarly(t *testing.T) {
	drv := &cursorDriver{remaining: 1000}
	db := pg.New(drv)
	err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{Batch: 10},
		"SELECT 1", nil, func(rows drops.Rows) error {
			rows.Next()
			return pg.ErrStopStream
		})
	if err != nil {
		t.Fatalf("ErrStopStream surfaced as a failure: %v", err)
	}
	if got := drv.saw("FETCH"); got != 1 {
		t.Errorf("issued %d FETCHes after being told to stop, want 1", got)
	}
}

func TestStreamQueryPropagatesCallbackErrors(t *testing.T) {
	drv := &cursorDriver{remaining: 100}
	db := pg.New(drv)
	boom := errors.New("sink refused")
	err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{Batch: 10},
		"SELECT 1", nil, func(rows drops.Rows) error {
			rows.Next()
			return boom
		})
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want the callback's error", err)
	}
}

func TestStreamQueryWithHold(t *testing.T) {
	drv := &cursorDriver{remaining: 5}
	db := pg.New(drv)
	err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{Batch: 10, Hold: true},
		"SELECT 1", nil, func(rows drops.Rows) error {
			for rows.Next() {
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.declared, "WITH HOLD") {
		t.Errorf("declaration = %q, want WITH HOLD", drv.declared)
	}
}

func TestStreamQueryValidatesItsInputs(t *testing.T) {
	db := pg.New(&cursorDriver{})
	if err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{}, "SELECT 1", nil, nil); err == nil {
		t.Error("expected an error for a nil callback")
	}
	if err := pg.StreamQuery(context.Background(), db, pg.StreamOptions{}, "  ", nil,
		func(drops.Rows) error { return nil }); err == nil {
		t.Error("expected an error for an empty statement")
	}
}

func TestStreamRowsPresentsOneRowAtATime(t *testing.T) {
	drv := &cursorDriver{remaining: 7}
	db := pg.New(drv)
	var seen int
	err := pg.StreamRows(context.Background(), db, pg.StreamOptions{Batch: 3},
		"SELECT 1", nil, func(row drops.Rows) error {
			seen++
			// The callback holds one row and cannot advance past it;
			// a callback that could would silently skip rows.
			if row.Next() {
				t.Error("the per-row callback was able to advance the cursor")
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 7 {
		t.Errorf("saw %d rows, want 7", seen)
	}
}

func TestStreamRowsPropagatesErrors(t *testing.T) {
	drv := &cursorDriver{remaining: 100}
	db := pg.New(drv)
	boom := errors.New("bad row")
	err := pg.StreamRows(context.Background(), db, pg.StreamOptions{Batch: 10},
		"SELECT 1", nil, func(drops.Rows) error { return boom })
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want the callback's error", err)
	}
}
