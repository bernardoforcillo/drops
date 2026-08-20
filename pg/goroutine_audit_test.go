package pg_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Constraint 14 — concurrency safety by construction — is the one rule
// in the design set that nothing was checking. Every background worker
// in the package is started by a caller who then stops thinking about
// it, so the two questions that matter are whether it stops when told
// and whether stopping it is safe to do twice, from a defer, after the
// context is already dead. These tests answer them for each Start /
// Run entry point in drops/pg, and are written to run under -race
// because that is the only way the answer means anything.

// --- StartPoolMetrics ---------------------------------------------

// The stop function is documented as the way to end the goroutine, and
// a shutdown path that both calls it and defers it is ordinary. Closing
// a channel twice panics, so the second call has to be a no-op.
func TestStartPoolMetricsStopIsIdempotent(t *testing.T) {
	drv := &statsDriver{}
	stop := pg.New(drv).StartPoolMetrics(context.Background(), time.Millisecond, func(pg.PoolStats) {})
	stop()
	stop()
	stop()
}

// After stop returns, no call may still be inside the sink: the
// caller's next move is usually to unregister the metric or close the
// writer behind it, and a snapshot already in flight lands on a
// half-torn-down collector. A stop that only asks the goroutine to
// leave, without waiting for it, cannot promise that — the sink here
// is deliberately slow so that stop is called while one is running.
func TestStartPoolMetricsStopWaitsForTheSinkToReturn(t *testing.T) {
	drv := &statsDriver{}
	var inSink atomic.Bool
	var calls atomic.Int64
	stop := pg.New(drv).StartPoolMetrics(context.Background(), time.Millisecond, func(pg.PoolStats) {
		inSink.Store(true)
		time.Sleep(30 * time.Millisecond)
		calls.Add(1)
		inSink.Store(false)
	})
	time.Sleep(5 * time.Millisecond)
	stop()

	if inSink.Load() {
		t.Error("stop returned while the sink was still running")
	}
	after := calls.Load()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != after {
		t.Errorf("sink called %d more times after stop returned, want 0", got-after)
	}
}

// Cancelling the context is the other half of the contract, and the
// half a server usually uses: one root context cancelled on shutdown,
// no bookkeeping of stop functions.
func TestStartPoolMetricsHonoursContextCancellation(t *testing.T) {
	drv := &statsDriver{}
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int64
	stop := pg.New(drv).StartPoolMetrics(ctx, time.Millisecond, func(pg.PoolStats) {
		calls.Add(1)
	})
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)

	after := calls.Load()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != after {
		t.Errorf("sink called %d more times after ctx was cancelled, want 0", got-after)
	}
	// And the stop function still has to be safe on a context that is
	// already dead — the goroutine it waits for has already returned.
	stop()
}

// --- OutboxWorker -------------------------------------------------

// drainCountingDriver serves an empty outbox and counts the drains,
// and satisfies pg.Listener so the worker takes its NOTIFY-driven
// path. Counting at the driver is what the spin test needs: with no
// rows to serve the handler never fires, and it is the query that a
// spinning worker hammers.
type drainCountingDriver struct {
	drains atomic.Int64
	notify chan pg.Notification
}

func (d *drainCountingDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return sweepResult{}, nil
}

func (d *drainCountingDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	d.drains.Add(1)
	return &fakeRows{cols: outboxDrainCols()}, nil
}

func (d *drainCountingDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

func (d *drainCountingDriver) Listen(context.Context, string) (<-chan pg.Notification, error) {
	return d.notify, nil
}

// The LISTEN connection is the first thing to go when the database
// restarts or a proxy times the session out, and the driver reports
// that by closing the channel. A closed channel is receivable for
// ever, so a select that does not notice turns the outbox worker into
// a hot loop: no error, no log line, just every replica draining the
// outbox table as fast as the server will answer, starting the moment
// the connection drops. The worker has to fall back to its ticker.
func TestOutboxWorkerDoesNotSpinWhenTheListenChannelCloses(t *testing.T) {
	drv := &drainCountingDriver{notify: make(chan pg.Notification)}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox").WithNotifyChannel("outbox_event")

	worker := pg.NewOutboxWorker(ob).
		WithInterval(time.Hour).
		OnEvent(func(context.Context, pg.OutboxEvent) error { return nil })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- worker.Run(ctx) }()

	close(drv.notify)
	time.Sleep(50 * time.Millisecond)

	// One drain on entry, at most one more from the closed-channel
	// wakeup. The pre-fix behaviour is thousands.
	if got := drv.drains.Load(); got > 5 {
		t.Errorf("drains = %d after the LISTEN channel closed, want the ticker to take over", got)
	}

	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Error("Run returned nil on cancellation, want ctx.Err()")
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// The plain worker — no NOTIFY channel — has to return on cancellation
// too, and return the reason rather than nil, because a supervisor
// that restarts on a nil error restarts on a clean shutdown.
func TestOutboxWorkerReturnsOnCancellation(t *testing.T) {
	db := pg.New(&outboxDriver{})
	worker := pg.NewOutboxWorker(pg.NewOutbox(db, "outbox")).
		WithInterval(time.Hour).
		OnEvent(func(context.Context, pg.OutboxEvent) error { return nil })

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

// --- Listen / Subscribe -------------------------------------------

// Listen's goroutine forwards from the driver's channel to a typed
// one. It has to close the typed channel when the context ends, or the
// consumer's range never terminates and both goroutines are pinned.
func TestListenClosesItsChannelOnCancellation(t *testing.T) {
	drv := &listeningDriver{out: make(chan pg.Notification)}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := pg.Listen[invoiceEvent](pg.New(drv), ctx, "invoice_paid")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel yielded a value after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("Listen's channel never closed after its context was cancelled")
	}
}

// The other exit is the driver ending the subscription. The forwarding
// goroutine must follow it out rather than blocking on a channel
// nothing will send on again.
func TestListenClosesItsChannelWhenTheDriverStops(t *testing.T) {
	raw := make(chan pg.Notification)
	drv := &listeningDriver{out: raw}
	ch, err := pg.Listen[invoiceEvent](pg.New(drv), context.Background(), "invoice_paid")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	close(raw)
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel yielded a value after the driver closed")
		}
	case <-time.After(time.Second):
		t.Fatal("Listen's channel never closed after the driver's did")
	}
}

// --- MatViewManager.Start -----------------------------------------

// Start blocks, so it is always someone else's goroutine, and the
// context is the only handle on it. It also has to return the reason:
// a scheduler that stops for an unknown reason gets restarted by a
// supervisor, and one that stops on shutdown must not be.
func TestMatViewManagerStartReturnsOnCancellation(t *testing.T) {
	m := pg.NewMatViewManager(pg.New(&statsDriver{})).WithPollInterval(time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- m.Start(ctx) }()
	cancel()

	select {
	case err := <-errCh:
		if err != context.Canceled {
			t.Errorf("Start returned %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after its context was cancelled")
	}
}

// --- Subscribe ----------------------------------------------------

// Subscribe stacks a second goroutine on Listen's, and it is the one
// that hydrates rows — so it can be inside a Get when the context is
// cancelled. Its channel still has to close, because the consumer is
// a range loop and a range loop over a channel that never closes is a
// goroutine that never exits.
func TestSubscribeClosesItsChannelOnCancellation(t *testing.T) {
	tbl := pg.NewTable("players")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	type player struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	ent := pg.NewEntity[player](tbl)

	drv := newListenDriver()
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := pg.Subscribe[player](pg.New(drv), ctx, ent)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drv.waitForListener("drops_players")
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel yielded a value after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe's channel never closed after its context was cancelled")
	}
}

// --- IdempotencyStore.SweepEvery ----------------------------------

// SweepEvery returns nothing, so the context is the only handle on the
// goroutine it starts. If it did not honour it there would be no way
// to stop the sweeper at all.
func TestIdempotencySweepEveryStopsWithItsContext(t *testing.T) {
	drv := &countingExecDriver{}
	store := pg.NewIdempotencyStore(pg.New(drv), "idempotency", time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	store.SweepEvery(ctx, time.Millisecond, nil)
	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)

	after := drv.count()
	if after == 0 {
		t.Fatal("the sweeper never ran; the test proves nothing")
	}
	time.Sleep(20 * time.Millisecond)
	if got := drv.count(); got != after {
		t.Errorf("Cleanup ran %d more times after cancellation, want 0", got-after)
	}
}

// countingExecDriver counts Exec calls, which is all a sweep is.
type countingExecDriver struct {
	mu sync.Mutex
	n  int
}

func (d *countingExecDriver) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func (d *countingExecDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	d.mu.Lock()
	d.n++
	d.mu.Unlock()
	return sweepResult{}, nil
}

func (d *countingExecDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}

func (d *countingExecDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

type sweepResult struct{}

func (sweepResult) RowsAffected() (int64, error) { return 0, nil }
