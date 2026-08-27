package pg_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// blockingDriver holds each statement until released, so a test can
// observe something that is genuinely in flight.
type blockingDriver struct {
	release chan struct{}
	entered chan struct{}
	rows    drops.Rows
}

func newBlockingDriver() *blockingDriver {
	return &blockingDriver{
		release: make(chan struct{}),
		entered: make(chan struct{}, 16),
	}
}

func (d *blockingDriver) wait(ctx context.Context) error {
	d.entered <- struct{}{}
	select {
	case <-d.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *blockingDriver) Exec(ctx context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, d.wait(ctx)
}

func (d *blockingDriver) Query(ctx context.Context, _ string, _ ...any) (drops.Rows, error) {
	if err := d.wait(ctx); err != nil {
		return nil, err
	}
	return d.rows, nil
}

func (d *blockingDriver) Begin(ctx context.Context) (drops.Tx, error) {
	if err := d.wait(ctx); err != nil {
		return nil, err
	}
	return &regFakeTx{}, nil
}

type regFakeTx struct{ rows drops.Rows }

func (t *regFakeTx) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, nil
}
func (t *regFakeTx) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	return t.rows, nil
}
func (t *regFakeTx) Begin(_ context.Context) (drops.Tx, error) { return &regFakeTx{}, nil }
func (t *regFakeTx) Commit(_ context.Context) error            { return nil }
func (t *regFakeTx) Rollback(_ context.Context) error          { return nil }

// stubRows is a cursor over a fixed number of rows.
type stubRows struct {
	left   int
	closed bool
}

func (r *stubRows) Next() bool {
	if r.left <= 0 {
		return false
	}
	r.left--
	return true
}
func (r *stubRows) Scan(...any) error          { return nil }
func (r *stubRows) Columns() ([]string, error) { return []string{"x"}, nil }
func (r *stubRows) Close() error               { r.closed = true; return nil }
func (r *stubRows) Err() error                 { return nil }

func TestRegistryTracksAndReleasesExec(t *testing.T) {
	drv := newBlockingDriver()
	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(drv))

	done := make(chan error, 1)
	go func() {
		_, err := db.Exec(context.Background(), "UPDATE t SET x = 1")
		done <- err
	}()
	<-drv.entered

	if got := reg.InFlightCount(); got != 1 {
		t.Fatalf("InFlightCount() = %d, want 1", got)
	}
	snap := reg.Snapshot()
	if len(snap) != 1 || snap[0].Kind != pg.KindExec || !snap[0].Write {
		t.Fatalf("snapshot = %+v, want one write exec", snap)
	}
	if snap[0].SQL != "UPDATE t SET x = 1" {
		t.Errorf("SQL = %q", snap[0].SQL)
	}

	close(drv.release)
	if err := <-done; err != nil {
		t.Fatalf("exec: %v", err)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount() = %d after completion, want 0", got)
	}
}

// A query is not finished when Query returns — the rows are still on
// the wire. The entry has to outlive the call.
func TestRegistryHoldsQueryUntilRowsClosed(t *testing.T) {
	drv := newBlockingDriver()
	drv.rows = &stubRows{left: 2}
	close(drv.release)
	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(drv))

	rows, err := db.Query(context.Background(), "SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.InFlightCount(); got != 1 {
		t.Fatalf("InFlightCount() = %d while rows are open, want 1", got)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount() = %d after Close, want 0", got)
	}
}

// Reading to the end without closing is legal with database/sql, so
// it must not leak an entry either.
func TestRegistryReleasesExhaustedRows(t *testing.T) {
	drv := newBlockingDriver()
	drv.rows = &stubRows{left: 2}
	close(drv.release)
	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(drv))

	rows, err := db.Query(context.Background(), "SELECT x FROM t")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount() = %d after exhausting the cursor, want 0", got)
	}
	// Closing afterwards must not double-deregister.
	_ = rows.Close()
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount() = %d after a redundant Close, want 0", got)
	}
}

func TestRegistryTracksOpenTransaction(t *testing.T) {
	drv := newBlockingDriver()
	close(drv.release)
	reg := pg.NewStatementRegistry()
	drv2 := reg.Wrap(drv)

	tx, err := drv2.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	snap := reg.Snapshot()
	if len(snap) != 1 || snap[0].Kind != pg.KindTx || !snap[0].Write {
		t.Fatalf("snapshot = %+v, want one open transaction counted as a write", snap)
	}
	// A statement inside it gets an entry of its own.
	if _, err := tx.Exec(context.Background(), "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount() = %d after commit, want 0", got)
	}
}

func TestQuiesceWaitsThenRefuses(t *testing.T) {
	drv := newBlockingDriver()
	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(drv))

	done := make(chan error, 1)
	go func() {
		_, err := db.Exec(context.Background(), "UPDATE t SET x = 1")
		done <- err
	}()
	<-drv.entered

	// A drain that cannot finish reports so, and stays quiesced.
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := reg.Quiesce(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Quiesce returned %v, want DeadlineExceeded", err)
	}
	if !reg.Quiesced() {
		t.Fatal("registry is not quiesced after a timed-out drain")
	}
	// New work is refused while quiesced — a drain that admits work
	// behind it never finishes.
	if _, err := db.Exec(context.Background(), "SELECT 1"); !errors.Is(err, pg.ErrQuiesced) {
		t.Fatalf("Exec while quiesced returned %v, want ErrQuiesced", err)
	}

	close(drv.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := reg.Quiesce(context.Background()); err != nil {
		t.Fatalf("second Quiesce: %v", err)
	}

	reg.Resume()
	if reg.Quiesced() {
		t.Fatal("still quiesced after Resume")
	}
	if _, err := db.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Exec after Resume: %v", err)
	}
}

func TestQuiesceReturnsImmediatelyWhenIdle(t *testing.T) {
	reg := pg.NewStatementRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := reg.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce on an idle registry: %v", err)
	}
}

func TestCancelAllStopsInFlightStatements(t *testing.T) {
	drv := newBlockingDriver()
	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(drv))

	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.Exec(context.Background(), "UPDATE t SET x = 1")
			errs <- err
		}()
	}
	for i := 0; i < 3; i++ {
		<-drv.entered
	}

	if n := reg.CancelAll(); n != 3 {
		t.Fatalf("CancelAll() = %d, want 3", n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("statement ended with %v, want context.Canceled", err)
		}
	}
	if got := reg.Canceled(); got != 3 {
		t.Errorf("Canceled() = %d, want 3", got)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount() = %d, want 0", got)
	}
}

// The gentler failover move: a read still running against a primary
// about to be demoted returns correct data; a write cannot commit
// anywhere useful.
func TestCancelWritesLeavesReadsAlone(t *testing.T) {
	drv := newBlockingDriver()
	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(drv))

	writeDone := make(chan error, 1)
	go func() {
		_, err := db.Exec(context.Background(), "UPDATE t SET x = 1")
		writeDone <- err
	}()
	<-drv.entered

	readDone := make(chan error, 1)
	go func() {
		_, err := db.Exec(context.Background(), "SELECT pg_sleep(10)")
		readDone <- err
	}()
	<-drv.entered

	if n := reg.CancelWrites(); n != 1 {
		t.Fatalf("CancelWrites() = %d, want 1", n)
	}
	if err := <-writeDone; !errors.Is(err, context.Canceled) {
		t.Errorf("write ended with %v, want context.Canceled", err)
	}

	select {
	case err := <-readDone:
		t.Fatalf("read was cancelled too: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(drv.release)
	if err := <-readDone; err != nil {
		t.Errorf("read: %v", err)
	}
}

func TestCancelOlderThan(t *testing.T) {
	drv := newBlockingDriver()
	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(drv))

	done := make(chan error, 1)
	go func() {
		_, err := db.Exec(context.Background(), "SELECT slow()")
		done <- err
	}()
	<-drv.entered

	// Nothing is older than an hour yet.
	if n := reg.CancelOlderThan(time.Hour); n != 0 {
		t.Fatalf("CancelOlderThan(1h) = %d, want 0", n)
	}
	time.Sleep(5 * time.Millisecond)
	if n := reg.CancelOlderThan(time.Millisecond); n != 1 {
		t.Fatalf("CancelOlderThan(1ms) = %d, want 1", n)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("statement ended with %v, want context.Canceled", err)
	}
}

// Cancelling the context a transaction was begun on ends it — the
// driver rolls it back itself. So a caller's `defer tx.Rollback()`
// finds it already gone, and that is success rather than a failure to
// report: the integration suite caught this as an error on the one
// outcome that is entirely correct.
func TestRollbackAfterCancellationReportsSuccess(t *testing.T) {
	drv := newBlockingDriver()
	close(drv.release)
	reg := pg.NewStatementRegistry()
	wrapped := reg.Wrap(&rollbackFailsDriver{blockingDriver: drv})

	tx, err := wrapped.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reg.CancelAll()
	if err := tx.Rollback(context.Background()); err != nil {
		t.Errorf("Rollback after CancelAll = %v, want nil", err)
	}

	// Without a cancellation, a driver's rollback error is the
	// caller's to see.
	tx2, err := wrapped.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Rollback(context.Background()); err == nil {
		t.Error("an uncancelled rollback swallowed the driver's error")
	}
}

// rollbackFailsDriver hands out transactions whose Rollback always
// fails, the way a driver reports one that has already ended.
type rollbackFailsDriver struct {
	*blockingDriver
}

func (d *rollbackFailsDriver) Begin(ctx context.Context) (drops.Tx, error) {
	if err := d.wait(ctx); err != nil {
		return nil, err
	}
	return &failingRollbackTx{}, nil
}

type failingRollbackTx struct{ regFakeTx }

func (t *failingRollbackTx) Rollback(context.Context) error {
	return errors.New("sql: transaction has already been committed or rolled back")
}

// A rollback has to reach the server even when the transaction's own
// context was cancelled, or the transaction stays open on a
// connection nobody will clean up.
func TestRollbackSurvivesCancellation(t *testing.T) {
	drv := newBlockingDriver()
	close(drv.release)
	reg := pg.NewStatementRegistry()
	wrapped := reg.Wrap(drv)

	tx, err := wrapped.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reg.CancelAll()
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback after CancelAll: %v", err)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("InFlightCount() = %d after rollback, want 0", got)
	}
}

func TestRegistryWrapNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected a panic on a nil driver")
		}
	}()
	pg.NewStatementRegistry().Wrap(nil)
}
