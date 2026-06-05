package pg_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// txTrackingDriver records every Begin / Commit / Rollback so we can
// verify TestTx rolls back exactly once.
type txTrackingDriver struct {
	begins    atomic.Int32
	commits   atomic.Int32
	rollbacks atomic.Int32
}

func (d *txTrackingDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, nil
}
func (d *txTrackingDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	return nil, nil
}
func (d *txTrackingDriver) Begin(_ context.Context) (drops.Tx, error) {
	d.begins.Add(1)
	return &txTrackingTx{drv: d}, nil
}

type txTrackingTx struct{ drv *txTrackingDriver }

func (tx *txTrackingTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx *txTrackingTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx *txTrackingTx) Begin(ctx context.Context) (drops.Tx, error) {
	return tx.drv.Begin(ctx)
}
func (tx *txTrackingTx) Commit(_ context.Context) error {
	tx.drv.commits.Add(1)
	return nil
}
func (tx *txTrackingTx) Rollback(_ context.Context) error {
	tx.drv.rollbacks.Add(1)
	return nil
}

func TestTestTxRollsBack(t *testing.T) {
	drv := &txTrackingDriver{}
	db := pg.New(drv)
	pg.TestTx(t, db, context.Background(), func(tx *pg.DB) {
		if _, err := tx.Exec(context.Background(), "SELECT 1"); err != nil {
			t.Fatalf("exec inside TestTx: %v", err)
		}
	})
	// Cleanup fires when t finishes, so we have to defer the assert
	// via a sub-test.
	t.Cleanup(func() {
		if drv.commits.Load() != 0 {
			t.Errorf("TestTx must never commit, got %d commits", drv.commits.Load())
		}
		if drv.rollbacks.Load() != 1 {
			t.Errorf("TestTx must rollback exactly once, got %d", drv.rollbacks.Load())
		}
	})
}

func TestTestTxRollsBackOnPanic(t *testing.T) {
	drv := &txTrackingDriver{}
	db := pg.New(drv)
	func() {
		defer func() { _ = recover() }()
		pg.TestTx(t, db, context.Background(), func(tx *pg.DB) {
			panic("boom")
		})
	}()
	if drv.rollbacks.Load() == 0 {
		t.Error("TestTx must rollback even when fn panics")
	}
}
