package mysql_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

// mysql has two background workers — the outbox worker's Run loop and
// the idempotency sweeper — and neither is handed back a stop
// function, so cancelling the context is the only way to end either.
// These tests hold both to that, and run under -race because a worker
// that is only ever started in production is a worker whose shutdown
// path is never exercised.

// auditDriver is a concurrency-safe do-nothing driver: the workers
// under test run on their own goroutine while the test reads their
// counters, which the package's shared fakeDriver is not built for.
type auditDriver struct {
	mu    sync.Mutex
	execs int
}

func (d *auditDriver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.execs
}

func (d *auditDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	d.mu.Lock()
	d.execs++
	d.mu.Unlock()
	return auditResult{}, nil
}

func (d *auditDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return emptyRows{}, nil
}

func (d *auditDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

type auditResult struct{}

func (auditResult) RowsAffected() (int64, error) { return 0, nil }

type emptyRows struct{}

func (emptyRows) Next() bool                 { return false }
func (emptyRows) Scan(...any) error          { return nil }
func (emptyRows) Columns() ([]string, error) { return nil, nil }
func (emptyRows) Close() error               { return nil }
func (emptyRows) Err() error                 { return nil }

// Run must return the reason it stopped rather than nil, because a
// supervisor that restarts on a nil error restarts on a clean
// shutdown — and it must return promptly, not on the next tick.
func TestOutboxWorkerReturnsOnCancellation(t *testing.T) {
	db := mysql.New(&auditDriver{})
	worker := mysql.NewOutboxWorker(mysql.NewOutbox(db, "outbox")).
		WithInterval(time.Hour).
		OnEvent(func(context.Context, mysql.OutboxEvent) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// SweepEvery returns nothing at all, so if it did not honour the
// context there would be no way to stop the sweeper short of exiting.
func TestIdempotencySweepEveryStopsWithItsContext(t *testing.T) {
	drv := &auditDriver{}
	store := mysql.NewIdempotencyStore(mysql.New(drv), "idempotency", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	store.SweepEvery(ctx, time.Millisecond, nil)

	deadline := time.Now().Add(time.Second)
	for drv.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if drv.count() == 0 {
		t.Fatal("the sweeper never ran; the test proves nothing")
	}
	cancel()
	time.Sleep(20 * time.Millisecond)

	after := drv.count()
	time.Sleep(20 * time.Millisecond)
	if got := drv.count(); got != after {
		t.Errorf("Cleanup ran %d more times after cancellation, want 0", got-after)
	}
}
