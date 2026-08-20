package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// sqlite has two background workers — the outbox worker's Run loop and
// the idempotency sweeper — and neither is handed back a stop
// function, so cancelling the context is the only way to end either.
// These tests hold both to that, and run under -race because a worker
// that is only ever started in production is a worker whose shutdown
// path is never exercised.
//
// The driver is dropstest's. What stood here before was a
// "concurrency-safe do-nothing driver" — a mutex, a counter, and five
// one-line no-ops — declared identically in this file and in
// mysql/goroutine_audit_test.go, which is dropstest.New() written out
// twice in a phase that shipped dropstest as the canonical double.
// Reading the recorder rather than a private counter is also the
// stricter test: it says which statements the sweeper sent, not merely
// how many times it woke up.

// execs counts the statements a worker has sent. dropstest.Driver is
// safe to read while the worker goroutine is still writing to it, which
// is the property these tests need and the only reason a bespoke double
// looked necessary.
func execs(drv *dropstest.Driver) int {
	n := 0
	for _, s := range drv.Statements() {
		if s.Op == dropstest.OpExec {
			n++
		}
	}
	return n
}

// Run must return the reason it stopped rather than nil, because a
// supervisor that restarts on a nil error restarts on a clean
// shutdown — and it must return promptly, not on the next tick.
func TestOutboxWorkerReturnsOnCancellation(t *testing.T) {
	db := sqlite.New(dropstest.New())
	worker := sqlite.NewOutboxWorker(sqlite.NewOutbox(db, "outbox")).
		WithInterval(time.Hour).
		OnEvent(func(context.Context, sqlite.OutboxEvent) error { return nil })

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
	drv := dropstest.New()
	store := sqlite.NewIdempotencyStore(sqlite.New(drv), "idempotency", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	store.SweepEvery(ctx, time.Millisecond, nil)

	deadline := time.Now().Add(time.Second)
	for execs(drv) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if execs(drv) == 0 {
		t.Fatal("the sweeper never ran; the test proves nothing")
	}
	cancel()
	time.Sleep(20 * time.Millisecond)

	after := execs(drv)
	time.Sleep(20 * time.Millisecond)
	if got := execs(drv); got != after {
		t.Errorf("Cleanup ran %d more times after cancellation, want 0", got-after)
	}
}
