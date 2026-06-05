package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// backfillDriver tracks the state-table interactions and serves the
// SELECT used by Status. The Fetch/Process callbacks are supplied
// per-test via the API.
type backfillDriver struct {
	mu       sync.Mutex
	rows     map[string]backfillRow // status table data
	calls    []string               // sql prefixes for ordering assertions
	execs    atomic.Int32
}

type backfillRow struct {
	lastID, processed int64
	completedAt       *time.Time
	lastError         string
}

func newBackfillDriver() *backfillDriver {
	return &backfillDriver{rows: map[string]backfillRow{}}
}

func (d *backfillDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execs.Add(1)
	d.calls = append(d.calls, sqlPrefix(sql))
	switch {
	case strings.HasPrefix(sqlPrefix(sql), "INSERT INTO") && strings.Contains(sql, "ON CONFLICT"):
		name := args[0].(string)
		row := d.rows[name]
		row.lastID = args[1].(int64)
		row.processed = args[2].(int64)
		switch {
		case strings.Contains(sql, `"completedAt"`):
			t := time.Now()
			row.completedAt = &t
			row.lastError = ""
		case len(args) >= 4:
			row.lastError = args[3].(string)
		default:
			row.lastError = ""
		}
		d.rows[name] = row
	case strings.HasPrefix(sqlPrefix(sql), "DELETE"):
		name := args[0].(string)
		delete(d.rows, name)
	}
	return backfillResult{}, nil
}

func (d *backfillDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, sqlPrefix(sql))
	if strings.HasPrefix(sqlPrefix(sql), "SELECT") && strings.Contains(sql, `"backfillJobs"`) {
		name := args[0].(string)
		row, ok := d.rows[name]
		if !ok {
			return &fakeRows{}, nil
		}
		var completedAt any
		if row.completedAt != nil {
			completedAt = *row.completedAt
		}
		var lastError any
		if row.lastError != "" {
			lastError = row.lastError
		}
		return &fakeRows{
			cols: []string{"name", "lastID", "processed", "completedAt", "lastError", "updatedAt"},
			data: [][]any{{name, row.lastID, row.processed, completedAt, lastError, time.Now()}},
		}, nil
	}
	return &fakeRows{}, nil
}

func (d *backfillDriver) Begin(_ context.Context) (drops.Tx, error) {
	return backfillTx{d}, nil
}

type backfillTx struct{ drv *backfillDriver }

func (tx backfillTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx backfillTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx backfillTx) Begin(ctx context.Context) (drops.Tx, error) { return tx.drv.Begin(ctx) }
func (backfillTx) Commit(_ context.Context) error                 { return nil }
func (backfillTx) Rollback(_ context.Context) error               { return nil }

type backfillResult struct{}

func (backfillResult) RowsAffected() (int64, error) { return 1, nil }

func sqlPrefix(sql string) string {
	sql = strings.TrimSpace(sql)
	if i := strings.IndexAny(sql, "\n"); i >= 0 {
		sql = strings.TrimSpace(sql[:i])
	}
	if len(sql) > 30 {
		sql = sql[:30]
	}
	return sql
}

func TestBackfillRunsAllChunks(t *testing.T) {
	drv := newBackfillDriver()
	db := pg.New(drv)

	source := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	var processed atomic.Int64

	bf := pg.NewBackfill(db, "test-job").
		ChunkSize(3).
		Throttle(0).
		Fetch(func(_ context.Context, lastID int64, limit int) ([]int64, int64, error) {
			var ids []int64
			for _, id := range source {
				if id > lastID && len(ids) < limit {
					ids = append(ids, id)
				}
			}
			next := lastID
			if len(ids) > 0 {
				next = ids[len(ids)-1]
			}
			return ids, next, nil
		}).
		Process(func(_ context.Context, _ *pg.DB, ids []int64) error {
			processed.Add(int64(len(ids)))
			return nil
		})

	if err := bf.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if processed.Load() != int64(len(source)) {
		t.Errorf("processed %d, want %d", processed.Load(), len(source))
	}

	status, err := bf.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Processed != int64(len(source)) {
		t.Errorf("status.Processed: %d", status.Processed)
	}
	if status.CompletedAt == nil {
		t.Error("status.CompletedAt should be set")
	}
}

func TestBackfillResumeContinuesFromState(t *testing.T) {
	drv := newBackfillDriver()
	drv.rows["test-job"] = backfillRow{lastID: 5, processed: 5}
	db := pg.New(drv)

	var seen []int64
	bf := pg.NewBackfill(db, "test-job").
		ChunkSize(10).
		Throttle(0).
		Fetch(func(_ context.Context, lastID int64, limit int) ([]int64, int64, error) {
			if lastID >= 10 {
				return nil, lastID, nil
			}
			var ids []int64
			for i := lastID + 1; i <= 10; i++ {
				ids = append(ids, i)
			}
			return ids, 10, nil
		}).
		Process(func(_ context.Context, _ *pg.DB, ids []int64) error {
			seen = append(seen, ids...)
			return nil
		})

	if err := bf.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 5 || seen[0] != 6 || seen[4] != 10 {
		t.Errorf("resume should have processed 6..10, got %v", seen)
	}
}

func TestBackfillSkipsAlreadyCompletedJob(t *testing.T) {
	drv := newBackfillDriver()
	done := time.Now()
	drv.rows["done-job"] = backfillRow{lastID: 100, processed: 100, completedAt: &done}
	db := pg.New(drv)

	called := false
	bf := pg.NewBackfill(db, "done-job").
		Fetch(func(context.Context, int64, int) ([]int64, int64, error) {
			called = true
			return nil, 0, nil
		}).
		Process(func(context.Context, *pg.DB, []int64) error { return nil })

	if err := bf.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called {
		t.Error("completed job must skip Fetch")
	}
}

func TestBackfillProcessErrorAborts(t *testing.T) {
	drv := newBackfillDriver()
	db := pg.New(drv)

	bf := pg.NewBackfill(db, "err-job").
		ChunkSize(2).
		Throttle(0).
		Fetch(func(_ context.Context, lastID int64, _ int) ([]int64, int64, error) {
			return []int64{lastID + 1, lastID + 2}, lastID + 2, nil
		}).
		Process(func(context.Context, *pg.DB, []int64) error {
			return errors.New("boom")
		})

	err := bf.Run(context.Background())
	if err == nil || err.Error() != "boom" {
		t.Errorf("expected boom, got %v", err)
	}
	status, _ := bf.Status(context.Background())
	if status.LastError != "boom" {
		t.Errorf("status.LastError: %q", status.LastError)
	}
}

func TestBackfillRunWithoutCallbacksErrors(t *testing.T) {
	drv := newBackfillDriver()
	db := pg.New(drv)

	if err := pg.NewBackfill(db, "x").Run(context.Background()); err == nil {
		t.Error("Run without Fetch must error")
	}
	if err := pg.NewBackfill(db, "x").
		Fetch(func(context.Context, int64, int) ([]int64, int64, error) { return nil, 0, nil }).
		Run(context.Background()); err == nil {
		t.Error("Run without Process must error")
	}
}

func TestBackfillResetClearsState(t *testing.T) {
	drv := newBackfillDriver()
	drv.rows["job"] = backfillRow{lastID: 50, processed: 50}
	db := pg.New(drv)

	bf := pg.NewBackfill(db, "job")
	if err := bf.Reset(context.Background()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, ok := drv.rows["job"]; ok {
		t.Error("Reset must delete the row")
	}
}

func TestBackfillThrottlePausesBetweenChunks(t *testing.T) {
	drv := newBackfillDriver()
	db := pg.New(drv)

	calls := 0
	bf := pg.NewBackfill(db, "throttle-job").
		ChunkSize(1).
		Throttle(20 * time.Millisecond).
		Fetch(func(_ context.Context, lastID int64, _ int) ([]int64, int64, error) {
			if lastID >= 3 {
				return nil, lastID, nil
			}
			return []int64{lastID + 1}, lastID + 1, nil
		}).
		Process(func(context.Context, *pg.DB, []int64) error {
			calls++
			return nil
		})

	start := time.Now()
	if err := bf.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls: %d", calls)
	}
	// 3 chunks * 20ms = 60ms; allow some slack but ensure throttle
	// produced a measurable delay.
	if time.Since(start) < 40*time.Millisecond {
		t.Errorf("throttle did not delay run: %v", time.Since(start))
	}
}

func TestBackfillStateTableHasExpectedColumns(t *testing.T) {
	tbl := pg.NewBackfillStateTable("backfillJobs")
	got := map[string]bool{}
	for _, c := range tbl.Columns() {
		got[c.Name()] = true
	}
	want := []string{"name", "lastID", "processed", "completedAt", "lastError", "updatedAt"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing column %q", w)
		}
	}
}
