package pg

import (
	"context"
	"sync"
	"time"

	"github.com/bernardoforcillo/drops"
)

// SREs don't deploy databases without /metrics. drops exposes
// the pool surface via duck-typed Stats() probing: any driver
// that implements PoolStatsProvider gets read-through metrics
// pushed to a sink at a chosen interval. Both database/sql and
// pgx report the same numbers under their own return types
// (sql.DBStats, pgxpool.Stat), so each needs a one-method
// adapter to satisfy PoolStatsProvider — see the note there.
//
//	stop := db.StartPoolMetrics(ctx, 5*time.Second,
//	    func(s pg.PoolStats) {
//	        prometheus.DropsPoolInUse.Set(float64(s.InUse))
//	        prometheus.DropsPoolIdle.Set(float64(s.Idle))
//	        prometheus.DropsPoolWaitDuration.Observe(s.WaitDuration.Seconds())
//	    })
//	defer stop()
//
// PoolStats fields mirror database/sql.DBStats so the
// translation is one-line. Drivers that don't expose stats
// return PoolStats{} + ok=false from db.PoolStats — the metrics
// goroutine never starts in that case.

// PoolStats is a snapshot of the underlying pool's health
// counters. Mirrors database/sql.DBStats with descriptive names
// so prometheus / OpenTelemetry adapters are direct.
type PoolStats struct {
	// MaxOpenConnections is the configured pool ceiling.
	MaxOpenConnections int
	// OpenConnections is the current count of live conns.
	OpenConnections int
	// InUse is the count currently held by a caller.
	InUse int
	// Idle is the count parked in the pool.
	Idle int
	// WaitCount is the cumulative count of waits for a free
	// conn since pool creation.
	WaitCount int64
	// WaitDuration is the cumulative time waited.
	WaitDuration time.Duration
	// MaxIdleClosed is the cumulative count of conns closed
	// because Idle exceeded MaxIdleConns.
	MaxIdleClosed int64
	// MaxIdleTimeClosed is the cumulative count closed because
	// they sat idle longer than MaxIdleTime.
	MaxIdleTimeClosed int64
	// MaxLifetimeClosed is the cumulative count closed because
	// they lived longer than ConnMaxLifetime.
	MaxLifetimeClosed int64
}

// PoolStatsProvider is the contract a driver implements when it
// can surface pool stats. *sql.DB does not satisfy it as it
// stands — its Stats returns a sql.DBStats, and Go matches method
// sets by type, not by shape — so an adapter restates the fields
// it already carries:
//
//	func (d *myDriver) Stats() pg.PoolStats {
//	    s := d.db.Stats()
//	    return pg.PoolStats{MaxOpenConnections: s.MaxOpenConnections, ...}
//	}
type PoolStatsProvider interface {
	Stats() PoolStats
}

// PoolStats returns a snapshot when the underlying driver
// implements PoolStatsProvider, plus ok=true. Drivers without
// pool introspection return the zero value + ok=false.
func (db *DB) PoolStats() (PoolStats, bool) {
	if p, ok := db.drv.(PoolStatsProvider); ok {
		return p.Stats(), true
	}
	return PoolStats{}, false
}

// StartPoolMetrics launches a goroutine that polls PoolStats
// every interval and pushes each snapshot to sink. Returns a
// cancel function the caller must call to stop the goroutine —
// usually via defer.
//
// Returns nil (and never starts the goroutine) when the driver
// does not implement PoolStatsProvider — callers can branch on
// db.PoolStats's ok return to know in advance.
func (db *DB) StartPoolMetrics(ctx context.Context, interval time.Duration, sink func(PoolStats)) func() {
	if sink == nil {
		return func() {}
	}
	p, ok := db.drv.(PoolStatsProvider)
	if !ok {
		return func() {}
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	stopCh := make(chan struct{})
	go func() {
		// Emit one snapshot immediately so the metric pipeline
		// shows current state without waiting for the first tick.
		sink(p.Stats())
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-t.C:
				sink(p.Stats())
			}
		}
	}()
	return func() { close(stopCh) }
}

// SupportsPoolStats reports whether the underlying driver
// implements PoolStatsProvider. Useful for code paths that want
// to register metrics only when introspection is available.
func SupportsPoolStats(db *DB) bool {
	_, ok := db.drv.(PoolStatsProvider)
	return ok
}

// ----------------------------------------------------------------------
// Queue time
// ----------------------------------------------------------------------

// PoolStats answers "how is the pool doing" in aggregate: cumulative
// waits, cumulative time waited, across every caller since the process
// started. It cannot answer "was this slow statement slow in the
// database, or slow getting to it" — that is per-statement, and the two
// halves have opposite remedies. The rest of this file is that second
// question.
//
// drops cannot time acquisition itself. [drops.Driver] is three methods
// and the pool lives behind them, so a wrapper that "measures the wait"
// by timing around Exec measures the wait plus the query and reports it
// under the wrong name. The honest seam is an optional interface a
// pool-backed driver may implement — one that hands out a connection as
// a Driver of its own, so drops can put a clock around the acquisition
// and nothing else. That is ConnAcquirer, and QueueTimed is the wrapper
// built on it. Pools that cannot check out a single connection get no
// queue-time metric rather than a wrong one; see [drops.ReportConnWait]
// for a driver that measures the wait internally and wants only to
// report it.

// ConnAcquirer is the optional extension a pool-backed driver
// implements when it can check out one connection and hand it back as
// a driver bound to it. Implementing it is what makes the queue-time
// half of [drops.QueryEvent] measurable for that pool.
//
// A *sql.DB satisfies it in a handful of lines via sql.DB.Conn; a pgx
// pool via Pool.Acquire.
type ConnAcquirer interface {
	// AcquireConn takes one connection out of the pool and returns a
	// Driver bound to it, together with a release function the caller
	// must invoke exactly once when finished with the connection.
	//
	// Everything AcquireConn does is counted as queue time, so it must
	// do no more than acquire — no session setup, no round trips.
	AcquireConn(ctx context.Context) (drops.Driver, func() error, error)
}

// QueueTimed wraps a pool that implements ConnAcquirer so every
// statement's pool wait is measured and reported through
// [drops.ReportConnWait]. Wire it in place of the plain driver:
//
//	drv := pg.QueueTimed(myPool)
//	db  := pg.New(drv).WithHook(inst.Hook())
//
//	// in the hook:
//	if e.WaitKnown {
//	    queueTime.Observe(e.WaitDuration.Seconds())
//	}
//	if qt, ok := e.QueryDuration(); ok {
//	    serverTime.Observe(qt.Seconds())
//	}
//
// The connection is held for exactly as long as the statement needs it:
// Exec releases on return, Query on Rows.Close, Begin on Commit or
// Rollback. Statements issued inside such a transaction report no queue
// time, correctly — the connection was already in hand.
//
// A PoolStatsProvider stays one: wrapping does not hide Stats.
func QueueTimed(a ConnAcquirer) drops.Driver {
	if a == nil {
		panic("drops/pg: QueueTimed acquirer cannot be nil")
	}
	q := &queueTimed{acq: a}
	if p, ok := a.(PoolStatsProvider); ok {
		return &queueTimedStats{queueTimed: q, stats: p}
	}
	return q
}

type queueTimed struct{ acq ConnAcquirer }

// queueTimedStats is queueTimed for an acquirer that also reports pool
// stats. Two types rather than one Stats method that returns zeroes
// when the pool has none: SupportsPoolStats must keep saying no for a
// pool that really cannot introspect itself.
type queueTimedStats struct {
	*queueTimed
	stats PoolStatsProvider
}

func (q *queueTimedStats) Stats() PoolStats { return q.stats.Stats() }

// Close forwards to the wrapped pool when it closes, so DB.Close keeps
// shutting the pool down through the wrapper.
func (q *queueTimed) Close() error {
	if c, ok := q.acq.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// acquire checks out a connection and reports how long that took. The
// wait is reported even when acquisition failed: a pool that stays
// exhausted until the caller's deadline expires is exactly the
// condition the measurement exists to expose, and hiding it there would
// leave the worst case as the one with no data.
func (q *queueTimed) acquire(ctx context.Context) (drops.Driver, func() error, error) {
	start := time.Now()
	conn, release, err := q.acq.AcquireConn(ctx)
	drops.ReportConnWait(ctx, time.Since(start))
	if err != nil {
		return nil, nil, err
	}
	if release == nil {
		release = func() error { return nil }
	}
	return conn, release, nil
}

func (q *queueTimed) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	conn, release, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = release() }()
	return conn.Exec(ctx, sql, args...)
}

func (q *queueTimed) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	conn, release, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, sql, args...)
	if err != nil {
		_ = release()
		return nil, err
	}
	return &releasingRows{Rows: rows, release: release}, nil
}

func (q *queueTimed) Begin(ctx context.Context) (drops.Tx, error) {
	conn, release, err := q.acquire(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		_ = release()
		return nil, err
	}
	return &releasingTx{Tx: tx, release: release}, nil
}

// releasingRows keeps the connection checked out until the cursor is
// closed. Returning the raw Rows and releasing on the way out would
// hand the connection to another goroutine while this one is still
// reading from it.
type releasingRows struct {
	drops.Rows
	once    sync.Once
	release func() error
}

func (r *releasingRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(func() {
		if rerr := r.release(); err == nil {
			err = rerr
		}
	})
	return err
}

// releasingTx holds the connection for the life of the transaction.
type releasingTx struct {
	drops.Tx
	once    sync.Once
	release func() error
}

func (t *releasingTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(ctx)
	t.done()
	return err
}

func (t *releasingTx) Rollback(ctx context.Context) error {
	err := t.Tx.Rollback(ctx)
	t.done()
	return err
}

func (t *releasingTx) done() {
	t.once.Do(func() { _ = t.release() })
}

// armConnWait installs a connection-wait slot on ctx, but only when a
// hook is attached: with nobody listening the measurement would be
// allocated, filled in and thrown away on every statement.
func (db *DB) armConnWait(ctx context.Context) (context.Context, func() (time.Duration, bool)) {
	if db.hook == nil {
		return ctx, connWaitUnknown
	}
	return drops.NewConnWait(ctx)
}

func connWaitUnknown() (time.Duration, bool) { return 0, false }
