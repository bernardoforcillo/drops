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

// statsDriver implements both drops.Driver and pg.PoolStatsProvider.
type statsDriver struct {
	mu    sync.Mutex
	calls atomic.Int32
	next  pg.PoolStats
}

func (d *statsDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}
func (d *statsDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (d *statsDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }
func (d *statsDriver) Stats() pg.PoolStats {
	d.calls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.next
}

func TestPoolStatsReturnsSnapshotWhenSupported(t *testing.T) {
	drv := &statsDriver{next: pg.PoolStats{InUse: 3, Idle: 5, OpenConnections: 8, MaxOpenConnections: 16}}
	db := pg.New(drv)
	s, ok := db.PoolStats()
	if !ok {
		t.Fatal("PoolStats should report ok=true for supporting driver")
	}
	if s.InUse != 3 || s.Idle != 5 || s.MaxOpenConnections != 16 {
		t.Errorf("snapshot: %+v", s)
	}
}

func TestPoolStatsReturnsFalseWhenUnsupported(t *testing.T) {
	db := pg.New(&fakeDriver{})
	_, ok := db.PoolStats()
	if ok {
		t.Error("plain driver should return ok=false")
	}
}

func TestSupportsPoolStats(t *testing.T) {
	if !pg.SupportsPoolStats(pg.New(&statsDriver{})) {
		t.Error("SupportsPoolStats should be true for stats driver")
	}
	if pg.SupportsPoolStats(pg.New(&fakeDriver{})) {
		t.Error("SupportsPoolStats should be false for plain driver")
	}
}

func TestStartPoolMetricsCallsSink(t *testing.T) {
	drv := &statsDriver{next: pg.PoolStats{InUse: 1, Idle: 2}}
	db := pg.New(drv)
	var emitted atomic.Int32
	var captured pg.PoolStats
	var capMu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := db.StartPoolMetrics(ctx, 10*time.Millisecond, func(s pg.PoolStats) {
		emitted.Add(1)
		capMu.Lock()
		captured = s
		capMu.Unlock()
	})
	defer stop()
	// Wait for at least 3 emissions.
	for i := 0; i < 30 && emitted.Load() < 3; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if emitted.Load() < 3 {
		t.Errorf("expected at least 3 emissions, got %d", emitted.Load())
	}
	capMu.Lock()
	if captured.InUse != 1 {
		t.Errorf("captured snapshot: %+v", captured)
	}
	capMu.Unlock()
}

func TestStartPoolMetricsIsNoopForUnsupportedDriver(t *testing.T) {
	db := pg.New(&fakeDriver{})
	called := atomic.Int32{}
	stop := db.StartPoolMetrics(context.Background(), 10*time.Millisecond, func(pg.PoolStats) {
		called.Add(1)
	})
	defer stop()
	time.Sleep(50 * time.Millisecond)
	if called.Load() != 0 {
		t.Errorf("metrics goroutine should not run without PoolStatsProvider, got %d calls", called.Load())
	}
}

func TestStartPoolMetricsHonoursCtxCancel(t *testing.T) {
	drv := &statsDriver{}
	db := pg.New(drv)
	ctx, cancel := context.WithCancel(context.Background())
	stop := db.StartPoolMetrics(ctx, 5*time.Millisecond, func(pg.PoolStats) {})
	defer stop()
	cancel()
	// Give the goroutine a moment to exit, then verify Stats
	// calls plateau.
	time.Sleep(50 * time.Millisecond)
	before := drv.calls.Load()
	time.Sleep(50 * time.Millisecond)
	after := drv.calls.Load()
	if after-before > 2 {
		t.Errorf("metrics goroutine kept running after ctx cancel (before=%d after=%d)",
			before, after)
	}
}

func TestStartPoolMetricsStopFunction(t *testing.T) {
	drv := &statsDriver{}
	db := pg.New(drv)
	stop := db.StartPoolMetrics(context.Background(), 5*time.Millisecond, func(pg.PoolStats) {})
	time.Sleep(30 * time.Millisecond)
	stop()
	time.Sleep(30 * time.Millisecond)
	before := drv.calls.Load()
	time.Sleep(50 * time.Millisecond)
	after := drv.calls.Load()
	if after-before > 1 {
		t.Errorf("stop() should halt the goroutine (before=%d after=%d)", before, after)
	}
}

func TestStartPoolMetricsNilSinkIsNoop(t *testing.T) {
	drv := &statsDriver{}
	db := pg.New(drv)
	stop := db.StartPoolMetrics(context.Background(), 5*time.Millisecond, nil)
	defer stop()
	time.Sleep(30 * time.Millisecond)
	if drv.calls.Load() != 0 {
		t.Errorf("nil sink should not invoke Stats, got %d calls", drv.calls.Load())
	}
}
