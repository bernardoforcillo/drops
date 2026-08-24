package integration_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mysql"
)

// mysqlOrderedOutbox creates an outbox table and emits kinds for one
// aggregate, in the order given.
func mysqlOrderedOutbox(t *testing.T, aggType, aggID string, kinds ...string) (*mysql.DB, *mysql.Outbox) {
	t.Helper()
	db := openMySQL(t)
	ob, _ := mysqlOutboxFixture(t, db)
	ctx := context.Background()
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		for _, k := range kinds {
			if err := ob.EmitWith(tx, ctx, k, map[string]string{},
				mysql.EmitOptions{AggregateType: aggType, AggregateID: aggID}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	return db, ob
}

// Two workers, one aggregate, one server — the same shape the
// PostgreSQL outbox is held to, and the same defect. The per-aggregate
// tick ended by falling through to the unordered drain, which selects
// every pending row whatever aggregate it belongs to, so the worker
// that lost the race for the named lock went on to deliver the very
// events the winner was working through.
func TestMySQLOutboxTwoWorkersKeepOneAggregateInOrder(t *testing.T) {
	_, ob := mysqlOrderedOutbox(t, "order", "acct-1", "e1", "e2", "e3", "e4", "e5", "e6")

	var mu sync.Mutex
	var seen []string
	worker := func() *mysql.OutboxWorker {
		return mysql.NewOutboxWorker(ob).
			WithOrdering(mysql.OrderingPerAggregate).
			WithInterval(10 * time.Millisecond).
			OnEvent(func(_ context.Context, e mysql.OutboxEvent) error {
				mu.Lock()
				seen = append(seen, e.Kind)
				mu.Unlock()
				// Long enough that the sibling worker ticks several
				// times while this one holds the aggregate's lock.
				time.Sleep(40 * time.Millisecond)
				return nil
			})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = worker().Run(ctx) }()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	want := []string{"e1", "e2", "e3", "e4", "e5", "e6"}
	if len(seen) != len(want) {
		t.Fatalf("delivered %v — an aggregate's stream went through both workers", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("delivered %v, want %v", seen, want)
		}
	}
}

// The other way into the same hole, and the one the mode was built for:
// an event whose handler failed is pushed into the future by its
// backoff, so a drain filtering on "available now" skips it and hands
// over the events emitted behind it.
func TestMySQLOutboxPerAggregateDoesNotOvertakeAFailedEvent(t *testing.T) {
	_, ob := mysqlOrderedOutbox(t, "match", "abc", "e1", "e2", "e3")

	var mu sync.Mutex
	var seen []string
	w := mysql.NewOutboxWorker(ob).
		WithOrdering(mysql.OrderingPerAggregate).
		WithInterval(20 * time.Millisecond).
		OnEvent(func(_ context.Context, e mysql.OutboxEvent) error {
			mu.Lock()
			seen = append(seen, e.Kind)
			mu.Unlock()
			if e.Kind == "e2" {
				return errors.New("handler refused e2")
			}
			return nil
		})

	// The default backoff puts e2's retry a second or more out, so
	// nothing legitimate can reach e3 inside this window.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_ = w.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	for _, k := range seen {
		if k == "e3" {
			t.Fatalf("e3 was delivered while e2 was still pending: %v", seen)
		}
	}
	if len(seen) < 2 || seen[0] != "e1" || seen[1] != "e2" {
		t.Errorf("expected the walk to stop at e2, got %v", seen)
	}
}

// Stopping the stream at a failed event is only right if it starts
// again: once the retry lands, the aggregate's remaining events have to
// follow it, in order.
func TestMySQLOutboxPerAggregateResumesAfterTheRetryLands(t *testing.T) {
	_, ob := mysqlOrderedOutbox(t, "match", "abc", "e1", "e2", "e3")

	var mu sync.Mutex
	var seen []string
	failedOnce := false
	w := mysql.NewOutboxWorker(ob).
		WithOrdering(mysql.OrderingPerAggregate).
		WithInterval(20 * time.Millisecond).
		WithBackoff(func(int) time.Duration { return 50 * time.Millisecond }).
		OnEvent(func(_ context.Context, e mysql.OutboxEvent) error {
			mu.Lock()
			seen = append(seen, e.Kind)
			first := e.Kind == "e2" && !failedOnce
			if first {
				failedOnce = true
			}
			mu.Unlock()
			if first {
				return errors.New("handler refused e2 once")
			}
			return nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = w.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"e1", "e2", "e2", "e3"}
	if len(seen) != len(want) {
		t.Fatalf("want e1,e2,e2,e3 — got %v", seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("delivery order %v, want %v", seen, want)
		}
	}
}

// An event with no aggregate has no order to keep, so the per-aggregate
// worker still has to drain it — which is what the fallthrough was
// there for and what its replacement has to keep doing.
func TestMySQLOutboxPerAggregateStillDrainsEventsWithNoAggregate(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	ob, _ := mysqlOutboxFixture(t, db)
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.Emit(tx, ctx, "loose", map[string]string{})
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	done := make(chan string, 4)
	w := mysql.NewOutboxWorker(ob).
		WithOrdering(mysql.OrderingPerAggregate).
		WithInterval(20 * time.Millisecond).
		OnEvent(func(_ context.Context, e mysql.OutboxEvent) error {
			select {
			case done <- e.Kind:
			default:
			}
			return nil
		})
	rctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	_ = w.Run(rctx)

	select {
	case got := <-done:
		if got != "loose" {
			t.Errorf("delivered %q, want loose", got)
		}
	default:
		t.Error("an event with no aggregate was never delivered")
	}
}

// The per-aggregate drain holds a transaction — and, on MySQL, a named
// lock taken on that transaction's session — for the whole batch, so
// anything it does while the batch is in flight has to go through that
// transaction rather than back to the pool. A handler failure is the
// case that did not: it wrote the attempts bump through
// Outbox.MarkFailed, which asks the pool for a connection the drain's
// own transaction is holding. On a pool of one the worker stops there
// and never comes back.
//
// This is the same defect PostgreSQL had and fixed; the two dialects
// disagreed about it until now. See mysql.OutboxWorker.failOneOn for
// the argument that lost.
func TestMySQLOutboxPerAggregateFailureFitsInOneConnection(t *testing.T) {
	db, _ := openMySQLPinnedConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ob, tbl := mysqlOutboxFixture(t, db)
	if err := db.InTx(ctx, func(tx *mysql.DB) error {
		return ob.EmitWith(tx, ctx, "boom", map[string]string{},
			mysql.EmitOptions{AggregateType: "cap", AggregateID: "one"})
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	delivered := make(chan struct{}, 4)
	w := mysql.NewOutboxWorker(ob).
		WithOrdering(mysql.OrderingPerAggregate).
		WithInterval(20 * time.Millisecond).
		WithBackoff(func(int) time.Duration { return time.Hour }).
		OnEvent(func(context.Context, mysql.OutboxEvent) error {
			select {
			case delivered <- struct{}{}:
			default:
			}
			return errors.New("handler refused it")
		})

	runCtx, stop := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer stop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(runCtx)
	}()

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never ran")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the worker never returned: the failure path waited for a connection its own transaction was holding")
	}

	// The bookkeeping landed, which is what says the write went through
	// the drain's transaction rather than being lost with it.
	var attempts int64
	rows, err := db.Query(context.Background(), "SELECT `attempts` FROM `"+tbl.Name()+"`")
	if err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("the outbox row is gone")
	}
	if err := rows.Scan(&attempts); err != nil {
		t.Fatalf("scan attempts: %v", err)
	}
	if attempts < 1 {
		t.Errorf("attempts is %d; the failure was never recorded", attempts)
	}
}
