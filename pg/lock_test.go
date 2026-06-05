package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// lockDriver tracks the calls so tests can assert on the
// pg_advisory_xact_lock / pg_try_advisory_xact_lock dispatch.
type lockDriver struct {
	mu      sync.Mutex
	queries []string
	args    [][]any
	tryOK   bool // value pg_try_advisory_xact_lock returns
}

func (d *lockDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	return nil, nil
}
func (d *lockDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if strings.Contains(sql, "pg_try_advisory_xact_lock") {
		return &fakeRows{
			cols: []string{"ok"},
			data: [][]any{{d.tryOK}},
		}, nil
	}
	return &fakeRows{}, nil
}
func (d *lockDriver) Begin(_ context.Context) (drops.Tx, error) { return &lockTx{drv: d}, nil }

type lockTx struct{ drv *lockDriver }

func (tx *lockTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx *lockTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx *lockTx) Begin(ctx context.Context) (drops.Tx, error) { return tx.drv.Begin(ctx) }
func (*lockTx) Commit(context.Context) error                   { return nil }
func (*lockTx) Rollback(context.Context) error                 { return nil }

func TestWithAdvisoryLockTakesXactLock(t *testing.T) {
	drv := &lockDriver{}
	db := pg.New(drv)
	called := false
	err := pg.WithAdvisoryLock(db, context.Background(), "nightly-job", func(tx *pg.DB) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithAdvisoryLock: %v", err)
	}
	if !called {
		t.Error("fn should have been invoked")
	}
	// At least one query should be pg_advisory_xact_lock($1).
	saw := false
	for _, q := range drv.queries {
		if strings.Contains(q, "pg_advisory_xact_lock($1)") {
			saw = true
			break
		}
	}
	if !saw {
		t.Errorf("expected pg_advisory_xact_lock call, got: %v", drv.queries)
	}
}

func TestTryWithAdvisoryLockReturnsErrWhenContended(t *testing.T) {
	drv := &lockDriver{tryOK: false}
	db := pg.New(drv)
	called := false
	err := pg.TryWithAdvisoryLock(db, context.Background(), "tick", func(tx *pg.DB) error {
		called = true
		return nil
	})
	if !errors.Is(err, pg.ErrLockNotAcquired) {
		t.Errorf("expected ErrLockNotAcquired, got %v", err)
	}
	if called {
		t.Error("fn must not run when lock not acquired")
	}
}

func TestTryWithAdvisoryLockRunsFnWhenFree(t *testing.T) {
	drv := &lockDriver{tryOK: true}
	db := pg.New(drv)
	called := false
	err := pg.TryWithAdvisoryLock(db, context.Background(), "tick", func(tx *pg.DB) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("TryWithAdvisoryLock: %v", err)
	}
	if !called {
		t.Error("fn should have run when lock acquired")
	}
}

func TestAdvisoryLockKeyIsDeterministic(t *testing.T) {
	k1 := pg.AdvisoryLockKey("foo")
	k2 := pg.AdvisoryLockKey("foo")
	if k1 != k2 {
		t.Errorf("same key should hash to same int64: %d != %d", k1, k2)
	}
	if pg.AdvisoryLockKey("bar") == k1 {
		t.Error("different keys should hash distinctly")
	}
}

func TestWithAdvisoryLockPropagatesFnError(t *testing.T) {
	drv := &lockDriver{}
	db := pg.New(drv)
	boom := errors.New("inside fn")
	err := pg.WithAdvisoryLock(db, context.Background(), "k", func(tx *pg.DB) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("fn error must propagate, got %v", err)
	}
}
