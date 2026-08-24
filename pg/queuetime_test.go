package pg_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// slowPool is a ConnAcquirer whose checkout takes a fixed, visible
// amount of time — the stand-in for a pool with every connection out on
// loan. The connection it hands back is a plain fakeDriver, so anything
// the event attributes to the database rather than the queue is a
// mistake in the accounting.
type slowPool struct {
	wait      time.Duration
	conn      *fakeDriver
	acquired  atomic.Int32
	released  atomic.Int32
	failWith  error
	statsCall atomic.Int32
	stats     pg.PoolStats
}

func (p *slowPool) AcquireConn(ctx context.Context) (drops.Driver, func() error, error) {
	time.Sleep(p.wait)
	if p.failWith != nil {
		return nil, nil, p.failWith
	}
	p.acquired.Add(1)
	return p.conn, func() error { p.released.Add(1); return nil }, nil
}

// statsPool is a slowPool that also reports pool stats, for the
// wrapping-must-not-hide-Stats case.
type statsPool struct{ *slowPool }

func (p *statsPool) Stats() pg.PoolStats {
	p.statsCall.Add(1)
	return p.stats
}

func captureOne(t *testing.T, drv drops.Driver, run func(*pg.DB)) drops.QueryEvent {
	t.Helper()
	var events []drops.QueryEvent
	db := pg.New(drv).WithHook(func(_ context.Context, e drops.QueryEvent) {
		events = append(events, e)
	})
	run(db)
	if len(events) == 0 {
		t.Fatal("the hook saw no event at all")
	}
	return events[0]
}

// The measurement that does not exist anywhere else: of a statement's
// total latency, how much was spent waiting for a connection and how
// much was the database.
func TestQueueTimedSplitsWaitFromQuery(t *testing.T) {
	pool := &slowPool{wait: 60 * time.Millisecond, conn: &fakeDriver{}}
	e := captureOne(t, pg.QueueTimed(pool), func(db *pg.DB) {
		_, _ = db.Exec(context.Background(), "UPDATE t SET a = $1", 1)
	})

	if !e.WaitKnown {
		t.Fatal("QueueTimed must report the wait; WaitKnown is false")
	}
	if e.WaitDuration < 55*time.Millisecond {
		t.Errorf("wait %v, want at least the 60ms the checkout took", e.WaitDuration)
	}
	qt, ok := e.QueryDuration()
	if !ok {
		t.Fatal("QueryDuration should be known once the wait is")
	}
	// The fake connection returns instantly, so anything the split
	// attributes to the database is slack in the accounting, not work.
	if qt > 20*time.Millisecond {
		t.Errorf("query time %v: an instant fake connection cannot have taken that long", qt)
	}
	if e.WaitDuration > e.Duration {
		t.Errorf("wait %v exceeds total %v", e.WaitDuration, e.Duration)
	}
}

func TestQueueTimedSplitsWaitForQueriesToo(t *testing.T) {
	pool := &slowPool{wait: 40 * time.Millisecond, conn: &fakeDriver{}}
	e := captureOne(t, pg.QueueTimed(pool), func(db *pg.DB) {
		rows, err := db.Query(context.Background(), "SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	})
	if !e.WaitKnown || e.WaitDuration < 35*time.Millisecond {
		t.Errorf("wait %v known=%v, want at least 40ms measured", e.WaitDuration, e.WaitKnown)
	}
}

// A driver that cannot check out a single connection reports nothing,
// and drops must pass that ignorance through instead of publishing a
// zero that reads as a pool under no pressure.
func TestPlainDriverLeavesQueueTimeUnknown(t *testing.T) {
	e := captureOne(t, &fakeDriver{}, func(db *pg.DB) {
		_, _ = db.Exec(context.Background(), "UPDATE t SET a = 1")
	})
	if e.WaitKnown {
		t.Errorf("a plain driver measures no wait, yet the event claims %v", e.WaitDuration)
	}
	if _, ok := e.QueryDuration(); ok {
		t.Error("QueryDuration cannot be known when the wait is not")
	}
}

// The wait that matters most is the one that ends in failure: a pool
// exhausted until the caller's deadline expires.
func TestQueueTimedReportsTheWaitThatFailed(t *testing.T) {
	pool := &slowPool{wait: 40 * time.Millisecond, failWith: errors.New("pool exhausted")}
	e := captureOne(t, pg.QueueTimed(pool), func(db *pg.DB) {
		if _, err := db.Exec(context.Background(), "UPDATE t SET a = 1"); err == nil {
			t.Fatal("expected the acquisition to fail")
		}
	})
	if !e.WaitKnown || e.WaitDuration < 35*time.Millisecond {
		t.Errorf("wait %v known=%v: a failed checkout is still queue time",
			e.WaitDuration, e.WaitKnown)
	}
	if e.Err == nil {
		t.Error("the acquisition error must reach the hook")
	}
}

// The connection has to stay checked out while the caller is still
// reading from the cursor.
func TestQueueTimedHoldsTheConnUntilRowsClose(t *testing.T) {
	pool := &slowPool{conn: &fakeDriver{}}
	drv := pg.QueueTimed(pool)
	rows, err := drv.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if pool.released.Load() != 0 {
		t.Fatal("connection released while the cursor was still open")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if pool.released.Load() != 1 {
		t.Errorf("released %d times after Close, want 1", pool.released.Load())
	}
	// Closing twice must not hand the same connection back twice.
	_ = rows.Close()
	if pool.released.Load() != 1 {
		t.Errorf("double Close released the connection %d times", pool.released.Load())
	}
}

func TestQueueTimedHoldsTheConnForTheTransaction(t *testing.T) {
	pool := &slowPool{conn: &fakeDriver{}}
	drv := pg.QueueTimed(pool)
	tx, err := drv.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), "INSERT ..."); err != nil {
		t.Fatal(err)
	}
	if pool.released.Load() != 0 {
		t.Fatal("connection released mid-transaction")
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pool.released.Load() != 1 {
		t.Errorf("released %d times after Commit, want 1", pool.released.Load())
	}
}

// Inside a transaction the connection is already in hand, so a
// statement there has no queue time — and reporting one would be a lie
// about where the latency went.
func TestQueueTimedReportsNoWaitInsideATransaction(t *testing.T) {
	pool := &slowPool{wait: 30 * time.Millisecond, conn: &fakeDriver{}}
	var events []drops.QueryEvent
	db := pg.New(pg.QueueTimed(pool)).WithHook(func(_ context.Context, e drops.QueryEvent) {
		events = append(events, e)
	})
	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		_, err := tx.Exec(context.Background(), "INSERT ...")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var begin, exec *drops.QueryEvent
	for i := range events {
		switch events[i].Kind {
		case "begin":
			begin = &events[i]
		case "exec":
			exec = &events[i]
		}
	}
	if begin == nil || exec == nil {
		t.Fatalf("expected begin and exec events, got %+v", events)
	}
	if !begin.WaitKnown || begin.WaitDuration < 25*time.Millisecond {
		t.Errorf("begin is where the checkout happens: wait %v known=%v",
			begin.WaitDuration, begin.WaitKnown)
	}
	if exec.WaitKnown {
		t.Errorf("a statement inside the transaction reported a %v wait; there was none to have",
			exec.WaitDuration)
	}
}

// Wrapping a pool for queue timing must not cost it its pool stats.
func TestQueueTimedKeepsPoolStats(t *testing.T) {
	pool := &statsPool{slowPool: &slowPool{conn: &fakeDriver{}, stats: pg.PoolStats{InUse: 4, Idle: 2}}}
	db := pg.New(pg.QueueTimed(pool))
	if !pg.SupportsPoolStats(db) {
		t.Fatal("QueueTimed hid the pool's Stats")
	}
	s, ok := db.PoolStats()
	if !ok || s.InUse != 4 || s.Idle != 2 {
		t.Errorf("stats %+v ok=%v", s, ok)
	}

	// And a pool without stats must keep reporting that it has none,
	// rather than gaining a Stats method that returns zeroes.
	plain := pg.New(pg.QueueTimed(&slowPool{conn: &fakeDriver{}}))
	if pg.SupportsPoolStats(plain) {
		t.Error("QueueTimed invented pool stats for a pool that has none")
	}
}

// With no hook installed there is nobody to read the measurement, so
// the slot is never allocated — and a driver reporting into that
// context has to be a no-op rather than a panic.
func TestQueueTimedWithoutHookIsHarmless(t *testing.T) {
	pool := &slowPool{conn: &fakeDriver{}}
	db := pg.New(pg.QueueTimed(pool))
	if _, err := db.Exec(context.Background(), "UPDATE t SET a = 1"); err != nil {
		t.Fatal(err)
	}
	if pool.acquired.Load() != 1 || pool.released.Load() != 1 {
		t.Errorf("acquired %d released %d", pool.acquired.Load(), pool.released.Load())
	}
}
