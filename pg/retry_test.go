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

// flakyDriver fails the first N transactions with err, then
// succeeds. Used to verify the retry loop re-runs the callback.
type flakyDriver struct {
	failuresLeft atomic.Int32
	err          error
	begins       atomic.Int32
	commits      atomic.Int32
	rollbacks    atomic.Int32
}

func (d *flakyDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, nil
}
func (d *flakyDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (d *flakyDriver) Begin(_ context.Context) (drops.Tx, error) {
	d.begins.Add(1)
	return &flakyTx{drv: d}, nil
}

type flakyTx struct{ drv *flakyDriver }

func (tx *flakyTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx *flakyTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx *flakyTx) Begin(ctx context.Context) (drops.Tx, error) {
	return tx.drv.Begin(ctx)
}
func (tx *flakyTx) Commit(_ context.Context) error {
	tx.drv.commits.Add(1)
	return nil
}
func (tx *flakyTx) Rollback(_ context.Context) error {
	tx.drv.rollbacks.Add(1)
	return nil
}

func TestInTxRetriesOnConfiguredErrors(t *testing.T) {
	drv := &flakyDriver{}
	drv.failuresLeft.Store(2) // first two attempts fail
	db := pg.New(drv).WithRetry(pg.RetryPolicy{
		MaxAttempts: 3,
		Errors:      []error{pg.ErrSerializationFailure},
		Backoff:     func(int) time.Duration { return time.Microsecond },
	})

	attempts := atomic.Int32{}
	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		attempts.Add(1)
		if drv.failuresLeft.Add(-1) >= 0 {
			return pg.ErrSerializationFailure
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx must succeed on 3rd attempt: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 callback attempts, got %d", attempts.Load())
	}
	if drv.rollbacks.Load() != 2 || drv.commits.Load() != 1 {
		t.Errorf("expected 2 rollbacks + 1 commit, got rollbacks=%d commits=%d",
			drv.rollbacks.Load(), drv.commits.Load())
	}
}

func TestInTxStopsOnNonRetryableError(t *testing.T) {
	drv := &flakyDriver{}
	db := pg.New(drv).WithRetry(pg.RetryPolicy{
		MaxAttempts: 5,
		Errors:      []error{pg.ErrSerializationFailure},
		Backoff:     func(int) time.Duration { return time.Microsecond },
	})
	boom := errors.New("application bug")
	attempts := atomic.Int32{}
	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		attempts.Add(1)
		return boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("non-retryable error must propagate, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("non-retryable error must NOT retry, got %d attempts", attempts.Load())
	}
}

func TestInTxBailsWhenContextCancelled(t *testing.T) {
	drv := &flakyDriver{}
	drv.failuresLeft.Store(10)
	db := pg.New(drv).WithRetry(pg.RetryPolicy{
		MaxAttempts: 10,
		Errors:      []error{pg.ErrSerializationFailure},
		Backoff:     func(int) time.Duration { return 100 * time.Millisecond },
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	attempts := atomic.Int32{}
	err := db.InTx(ctx, func(tx *pg.DB) error {
		attempts.Add(1)
		return pg.ErrSerializationFailure
	})
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Errorf("ctx cancellation must surface, got %v", err)
	}
	if attempts.Load() >= 10 {
		t.Errorf("retries should stop early on ctx cancellation, got %d", attempts.Load())
	}
}

func TestInTxWithoutPolicyKeepsLegacyBehavior(t *testing.T) {
	drv := &flakyDriver{}
	db := pg.New(drv) // no WithRetry
	attempts := atomic.Int32{}
	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		attempts.Add(1)
		return pg.ErrSerializationFailure
	})
	if !errors.Is(err, pg.ErrSerializationFailure) {
		t.Errorf("error must propagate on first failure, got %v", err)
	}
	if attempts.Load() != 1 {
		t.Errorf("no retries without policy, got %d attempts", attempts.Load())
	}
}

func TestExponentialJitterRespectsBounds(t *testing.T) {
	bo := pg.ExponentialJitter(10*time.Millisecond, 200*time.Millisecond)
	last := time.Duration(0)
	for i := 1; i <= 10; i++ {
		d := bo(i)
		if d <= 0 {
			t.Errorf("attempt %d: backoff must be > 0, got %v", i, d)
		}
		if d > 200*time.Millisecond+10*time.Millisecond {
			t.Errorf("attempt %d: backoff exceeds cap, got %v", i, d)
		}
		last = d
	}
	_ = last
}

func TestDefaultRetryPolicyMatchesSentinels(t *testing.T) {
	p := pg.DefaultRetryPolicy()
	if p.MaxAttempts < 2 {
		t.Errorf("DefaultRetryPolicy should retry at least once, got MaxAttempts=%d", p.MaxAttempts)
	}
	// Spot-check that the sentinels are listed.
	wantSerial := false
	wantDeadlock := false
	for _, e := range p.Errors {
		if errors.Is(e, pg.ErrSerializationFailure) {
			wantSerial = true
		}
		if errors.Is(e, pg.ErrDeadlock) {
			wantDeadlock = true
		}
	}
	if !wantSerial || !wantDeadlock {
		t.Errorf("default policy missing serialization/deadlock sentinels: %+v", p.Errors)
	}
}
