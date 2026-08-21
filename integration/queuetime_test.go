package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
)

// A statement's latency is the wait for a connection plus the database's
// own work, and the two want opposite responses. drops cannot time the
// first half from where it stands — the pool is behind drops.Driver —
// so it offers pg.ConnAcquirer, and a pool that implements it gets the
// split. sqlPool below is that implementation for database/sql, in the
// dozen lines the doc comment claims it takes.
//
// These tests run against the live server because the thing being
// measured is a real connection being held by a real statement while a
// second caller waits for it. A fake can be made to sleep; only a
// server can make the pool actually run out.

type sqlPool struct{ db *sql.DB }

func (p *sqlPool) AcquireConn(ctx context.Context) (drops.Driver, func() error, error) {
	c, err := p.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	return &sqlConn{c: c}, c.Close, nil
}

type sqlConn struct{ c *sql.Conn }

func (s *sqlConn) Exec(ctx context.Context, q string, args ...any) (drops.Result, error) {
	return s.c.ExecContext(ctx, q, args...)
}

func (s *sqlConn) Query(ctx context.Context, q string, args ...any) (drops.Rows, error) {
	rows, err := s.c.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *sqlConn) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := s.c.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &sqlTx{tx: tx}, nil
}

type sqlTx struct{ tx *sql.Tx }

func (t *sqlTx) Exec(ctx context.Context, q string, args ...any) (drops.Result, error) {
	return t.tx.ExecContext(ctx, q, args...)
}

func (t *sqlTx) Query(ctx context.Context, q string, args ...any) (drops.Rows, error) {
	rows, err := t.tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (t *sqlTx) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("nested transactions are not supported here")
}

func (t *sqlTx) Commit(context.Context) error   { return t.tx.Commit() }
func (t *sqlTx) Rollback(context.Context) error { return t.tx.Rollback() }

// eventLog collects hook events from every goroutine in a test.
type eventLog struct {
	mu     sync.Mutex
	events []drops.QueryEvent
}

func (l *eventLog) hook() drops.Hook {
	return func(_ context.Context, e drops.QueryEvent) {
		l.mu.Lock()
		l.events = append(l.events, e)
		l.mu.Unlock()
	}
}

func (l *eventLog) find(t *testing.T, marker string) drops.QueryEvent {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if strings.Contains(e.SQL, marker) {
			return e
		}
	}
	t.Fatalf("no event for %q; saw %d events", marker, len(l.events))
	return drops.QueryEvent{}
}

func openQueueTimedPG(t *testing.T, maxConns int) (*sql.DB, *eventLog, *pg.DB) {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvPostgres)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	sqlDB.SetMaxOpenConns(maxConns)
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	log := &eventLog{}
	return sqlDB, log, pg.New(pg.QueueTimed(&sqlPool{db: sqlDB})).WithHook(log.hook())
}

// The measurement, end to end, against a pool that really is exhausted:
// one connection, one statement holding it for 600ms, and a second
// statement that arrives 150ms in. The second one is slow, and the
// split has to say it was slow queueing rather than slow in the
// database — which is the difference between "add connections" and
// "fix the query".
func TestPGQueueTimeIsSeparatedFromQueryTime(t *testing.T) {
	_, log, db := openQueueTimedPG(t, 1)
	ctx := context.Background()

	const hold = 600 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := db.Exec(ctx, "SELECT pg_sleep(0.6) -- hog"); err != nil {
			t.Errorf("hog: %v", err)
		}
	}()

	// Long enough that the hog owns the only connection.
	time.Sleep(150 * time.Millisecond)
	if _, err := db.Exec(ctx, "SELECT 1 -- waiter"); err != nil {
		t.Fatalf("waiter: %v", err)
	}
	<-done

	hog := log.find(t, "-- hog")
	if !hog.WaitKnown {
		t.Fatal("the pool implements ConnAcquirer, so the wait must be known")
	}
	if hog.WaitDuration > 100*time.Millisecond {
		t.Errorf("hog queued %v for an idle pool", hog.WaitDuration)
	}
	hogQuery, _ := hog.QueryDuration()
	if hogQuery < hold-100*time.Millisecond {
		t.Errorf("hog query time %v, want ~%v — pg_sleep is database time", hogQuery, hold)
	}

	waiter := log.find(t, "-- waiter")
	if !waiter.WaitKnown {
		t.Fatal("waiter's wait must be known")
	}
	// It arrived 150ms into a 600ms hold, so it queued for roughly the
	// remaining 450ms.
	if waiter.WaitDuration < 300*time.Millisecond {
		t.Errorf("waiter queued %v, want at least 300ms — it was blocked on the pool",
			waiter.WaitDuration)
	}
	waiterQuery, ok := waiter.QueryDuration()
	if !ok {
		t.Fatal("QueryDuration should be known")
	}
	if waiterQuery > 100*time.Millisecond {
		t.Errorf("waiter query time %v: SELECT 1 is not a slow query, and the whole "+
			"point of the split is that its latency is not blamed on the database",
			waiterQuery)
	}
	// The number every other instrumentation would have reported, and
	// the reason it is not enough on its own.
	if waiter.Duration < 300*time.Millisecond {
		t.Errorf("total %v should have carried the whole wait", waiter.Duration)
	}
}

// The same statements through a driver that cannot check out a single
// connection: the total is still reported, and the split is reported as
// unknown rather than as zero queue time.
func TestPGPlainDriverReportsNoQueueTime(t *testing.T) {
	dsn := integration.DSN(t, integration.EnvPostgres)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	log := &eventLog{}
	db := pg.New(stdlib.New(sqlDB)).WithHook(log.hook())
	if _, err := db.Exec(context.Background(), "SELECT 1 -- plain"); err != nil {
		t.Fatal(err)
	}
	e := log.find(t, "-- plain")
	if e.WaitKnown {
		t.Errorf("a driver that measured nothing reported a %v wait", e.WaitDuration)
	}
	if e.Duration <= 0 {
		t.Error("the total is always measured")
	}
}

// The wait that ends in failure is the one an operator most needs to
// see: a pool of one, already held, and a caller whose deadline runs
// out before a connection frees up.
func TestPGQueueTimeIsReportedWhenAcquisitionFails(t *testing.T) {
	_, log, db := openQueueTimedPG(t, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := db.Exec(context.Background(), "SELECT pg_sleep(1.0) -- blocker"); err != nil {
			t.Errorf("blocker: %v", err)
		}
	}()
	time.Sleep(150 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := db.Exec(ctx, "SELECT 1 -- timeout"); err == nil {
		t.Fatal("expected the acquisition to time out")
	}
	<-done

	e := log.find(t, "-- timeout")
	if !e.WaitKnown {
		t.Fatal("a failed checkout is still queue time, and still has to be reported")
	}
	if e.WaitDuration < 150*time.Millisecond {
		t.Errorf("reported %v of queue time for a 200ms timeout", e.WaitDuration)
	}
	if q, _ := e.QueryDuration(); q > 60*time.Millisecond {
		t.Errorf("query time %v: the database never saw this statement", q)
	}
}
