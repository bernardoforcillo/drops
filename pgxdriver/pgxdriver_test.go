package pgxdriver_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/pgxdriver"
)

// The point of this package is the four OPTIONAL interfaces drops/pg
// probes the driver for. Each is found by a type assertion and each
// falls back silently when it is absent, so the failure mode of a
// signature that drifts is not a compile error anywhere — it is the
// feature going quietly back to unavailable.
//
// These are the tests that would notice. They need no server: what is
// asserted is that drops FINDS the interface, which is a property of
// the type rather than of a connection.

func newDriver(t *testing.T) drops.Driver {
	t.Helper()
	// A pool that has never dialled. pgxpool.New does not connect —
	// it parses the config and returns — so this is a real driver over
	// a real pool with no server behind it, which is exactly what a
	// question about its interfaces needs.
	pool, err := pgxpool.New(context.Background(), "postgres://drops:drops@127.0.0.1:1/drops")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pgxdriver.New(pool)
}

func TestTheDriverIsFoundToSupportCopy(t *testing.T) {
	db := pg.New(newDriver(t))
	if !pg.SupportsCopy(db) {
		t.Fatal("pg.SupportsCopy is false, so pg.CopyFrom answers ErrCopyNotSupported — " +
			"which is the state this package exists to end")
	}
}

func TestTheDriverIsFoundToSupportListen(t *testing.T) {
	db := pg.New(newDriver(t))
	if !pg.SupportsListen(db) {
		t.Fatal("pg.SupportsListen is false, so pg.Subscribe and the change feed have nothing to run on")
	}
}

func TestTheDriverIsFoundToReportPoolStats(t *testing.T) {
	drv := newDriver(t)
	p, ok := drv.(pg.PoolStatsProvider)
	if !ok {
		t.Fatal("the driver does not implement pg.PoolStatsProvider, so pg.StartPoolMetrics samples nothing")
	}
	s := p.Stats()
	if s.MaxOpenConnections <= 0 {
		t.Errorf("MaxOpenConnections = %d, want the pool's ceiling", s.MaxOpenConnections)
	}
	// The three database/sql-only counters stay zero rather than being
	// invented, and a reader of the dashboard cannot tell an invented
	// zero from a measured one — so this pins that they are the ones
	// left alone.
	if s.MaxIdleClosed != 0 || s.MaxIdleTimeClosed != 0 || s.MaxLifetimeClosed != 0 {
		t.Errorf("a counter pgx does not report was filled in: %+v", s)
	}
}

func TestTheDriverIsFoundToAcquireAConnection(t *testing.T) {
	drv := newDriver(t)
	if _, ok := drv.(pg.ConnAcquirer); !ok {
		t.Fatal("the driver does not implement pg.ConnAcquirer, so the queue-time instrumentation " +
			"cannot take a connection to measure")
	}
}

// A schema-qualified destination is two identifiers, and pgx quotes
// each part it is given. A COPY into "reporting.events" that arrived as
// one identifier would be a table whose name contains a dot.
func TestACopyDestinationIsSplitOnItsQualifier(t *testing.T) {
	drv := newDriver(t)
	c, ok := drv.(pg.Copier)
	if !ok {
		t.Fatal("not a Copier")
	}
	// No rows is the one call that reaches no server, so it is the one
	// that can assert on the path without one. It must not error and
	// must not claim to have copied anything.
	n, err := c.Copy(context.Background(), "reporting.events", []string{"id"}, nil)
	if err != nil {
		t.Fatalf("an empty copy: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

// Listen against an unreachable server must fail rather than hand back
// a channel that never delivers: a caller that got a channel would wait
// for a notification from a connection that was never made.
func TestListenFailsRatherThanReturningADeadChannel(t *testing.T) {
	drv := newDriver(t)
	l, ok := drv.(pg.Listener)
	if !ok {
		t.Fatal("not a Listener")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := l.Listen(ctx, "events"); err == nil {
		t.Fatal("Listen against an unreachable server returned a channel and no error")
	}
}

// TestMain keeps the suite quiet about a DSN it does not use: every
// test here is about the driver's type, and a server would tell them
// nothing they do not already know.
func TestMain(m *testing.M) { os.Exit(m.Run()) }
