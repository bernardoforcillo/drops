package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// Two workers, one aggregate, one server. This is the shape
// OrderingPerAggregate exists for and the shape a rendered SQL string
// cannot say anything about: the advisory lock is the whole mechanism,
// and whether it holds is a fact about the server.
//
// The per-aggregate tick used to end by falling through to the
// unordered drain, which selects every pending row whatever aggregate
// it belongs to. So the worker that lost the race for the lock went on
// to deliver the very events the winner was mid-way through — the two
// handlers running the same aggregate's stream at the same time, in
// whatever order the two picked rows up.
func TestPGOutboxTwoWorkersKeepOneAggregateInOrder(t *testing.T) {
	db, ob := pgOutboxFixture(t, "order", "acct-1", "e1", "e2", "e3", "e4", "e5", "e6")
	_ = db

	var mu sync.Mutex
	var seen []string
	handler := func(_ context.Context, e pg.OutboxEvent) error {
		mu.Lock()
		seen = append(seen, e.Kind)
		mu.Unlock()
		// Long enough that the sibling worker ticks several times
		// while this one holds the aggregate's lock.
		time.Sleep(40 * time.Millisecond)
		return nil
	}
	worker := func() *pg.OutboxWorker {
		return pg.NewOutboxWorker(ob).
			WithOrdering(pg.OrderingPerAggregate).
			WithInterval(10 * time.Millisecond).
			OnEvent(handler)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = worker().Run(ctx)
		}()
	}
	// Six events at 40ms each is under half a second; give the pair a
	// moment more and then stop them.
	time.Sleep(1200 * time.Millisecond)
	cancel()
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

// The per-aggregate drain holds a transaction for the whole batch, so
// anything it does while the batch is in flight has to go through that
// transaction rather than back to the pool. A handler failure is the
// case that used to: it wrote the attempts bump through Outbox.MarkFailed,
// which asks the pool for a connection the drain's own transaction is
// holding. On a pool of one the worker stopped there and never came
// back.
func TestPGOutboxPerAggregateFailureFitsInOneConnection(t *testing.T) {
	db := openPGCapped(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	name := integration.UniqueName(t, "ob1")
	tbl := pg.NewOutboxTable(name)
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	ob := pg.NewOutbox(db, name)
	if err := db.InTx(ctx, func(tx *pg.DB) error {
		return ob.EmitWith(tx, ctx, "boom", map[string]string{},
			pg.EmitOptions{AggregateType: "cap", AggregateID: "one"})
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	delivered := make(chan struct{}, 4)
	w := pg.NewOutboxWorker(ob).
		WithOrdering(pg.OrderingPerAggregate).
		WithInterval(20 * time.Millisecond).
		WithBackoff(func(int) time.Duration { return time.Hour }).
		OnEvent(func(context.Context, pg.OutboxEvent) error {
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
	if err := pg.ScanOne(mustQueryPG(t, db,
		fmt.Sprintf(`SELECT "attempts" FROM %q`, name)), &attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts < 1 {
		t.Errorf("attempts is %d; the failure was never recorded", attempts)
	}
}
