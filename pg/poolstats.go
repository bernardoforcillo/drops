package pg

import (
	"context"
	"time"
)

// SREs don't deploy databases without /metrics. drops exposes
// the pool surface via duck-typed Stats() probing: any driver
// that implements PoolStatsProvider gets read-through metrics
// pushed to a sink at a chosen interval. The database/sql
// adapter satisfies it natively (sql.DB.Stats()); pgx pools
// expose their own Stats() shape that a thin adapter can wrap.
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
// can surface pool stats. database/sql.DB satisfies it natively
// (Stats() returns sql.DBStats which is the same shape).
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
