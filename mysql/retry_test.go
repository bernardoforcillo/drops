package mysql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mysql"
)

func TestDefaultRetryPolicyRetriesContentionAndNothingElse(t *testing.T) {
	p := mysql.DefaultRetryPolicy()
	db := mysql.New(&fakeDriver{}).WithRetry(p)

	retryable := []error{mysql.ErrDeadlock, mysql.ErrLockWaitTimeout}
	permanent := []error{
		mysql.ErrUniqueViolation, mysql.ErrForeignKeyViolation, mysql.ErrCheckViolation,
		mysql.ErrNotNullViolation, mysql.ErrUndefinedTable, mysql.ErrUndefinedColumn,
		context.Canceled,
	}
	for _, sentinel := range retryable {
		if n := attemptsUnder(t, db, sentinel); n != p.MaxAttempts {
			t.Errorf("%v ran %d attempts, want %d", sentinel, n, p.MaxAttempts)
		}
	}
	for _, sentinel := range permanent {
		if n := attemptsUnder(t, db, sentinel); n != 1 {
			t.Errorf("%v ran %d attempts, want 1 — retrying a permanent failure only wastes the time", sentinel, n)
		}
	}
}

// The sentinel arrives wrapped in a *ServerError, which is what a real
// failure looks like; errors.Is has to see through it.
func TestRetryMatchesAClassifiedError(t *testing.T) {
	deadlocking := &fakeDriver{execFail: newDriverError(1213, "40001",
		"Deadlock found when trying to get lock; try restarting transaction")}
	db := mysql.New(deadlocking).WithRetry(mysql.RetryPolicy{
		MaxAttempts: 3,
		Errors:      []error{mysql.ErrDeadlock},
	})
	attempts := 0
	err := db.InTx(context.Background(), func(tx *mysql.DB) error {
		attempts++
		_, e := tx.Exec(context.Background(), "UPDATE t SET a = 1")
		return e
	})
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	if !errors.Is(err, mysql.ErrDeadlock) {
		t.Errorf("the last error must still reach the caller: %v", err)
	}
}

// A callback that succeeds on a later attempt returns nil, and stops
// there rather than exhausting the budget.
func TestRetryStopsAtTheFirstSuccess(t *testing.T) {
	db := mysql.New(&fakeDriver{}).WithRetry(mysql.RetryPolicy{
		MaxAttempts: 5,
		Errors:      []error{mysql.ErrDeadlock},
	})
	attempts := 0
	err := db.InTx(context.Background(), func(*mysql.DB) error {
		attempts++
		if attempts < 3 {
			return mysql.ErrDeadlock
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Errorf("err = %v, attempts = %d", err, attempts)
	}
}

// Without a policy InTx behaves as it always did.
func TestNoRetryPolicyRunsOnce(t *testing.T) {
	db := mysql.New(&fakeDriver{})
	if got := db.RetryPolicyValue().MaxAttempts; got != 0 {
		t.Errorf("zero policy expected, got MaxAttempts %d", got)
	}
	if n := attemptsUnder(t, db, mysql.ErrDeadlock); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

// WithRetry copies the DB rather than mutating it, and the zero policy
// clears one.
func TestWithRetryIsACopy(t *testing.T) {
	base := mysql.New(&fakeDriver{})
	retrying := base.WithRetry(mysql.DefaultRetryPolicy())
	if base.RetryPolicyValue().MaxAttempts != 0 {
		t.Error("WithRetry must not mutate the receiver")
	}
	if retrying.RetryPolicyValue().MaxAttempts != 3 {
		t.Error("the copy must carry the policy")
	}
	if retrying.WithRetry(mysql.RetryPolicy{}).RetryPolicyValue().MaxAttempts != 0 {
		t.Error("the zero policy must clear")
	}
}

// A cancelled context must not be slept through: the loop gives up at
// the first backoff.
func TestRetryStopsOnContextCancellation(t *testing.T) {
	db := mysql.New(&fakeDriver{}).WithRetry(mysql.RetryPolicy{
		MaxAttempts: 4,
		Errors:      []error{mysql.ErrDeadlock},
		Backoff:     func(int) time.Duration { return time.Hour },
	})
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	start := time.Now()
	err := db.InTx(ctx, func(*mysql.DB) error {
		attempts++
		cancel()
		return mysql.ErrDeadlock
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the backoff sleep ignored the cancellation")
	}
}

func TestExponentialJitterGrowsAndCaps(t *testing.T) {
	base, maxN := 10*time.Millisecond, 100*time.Millisecond
	b := mysql.ExponentialJitter(base, maxN)
	prev := time.Duration(0)
	for attempt := 1; attempt <= 4; attempt++ {
		d := b(attempt)
		if d < 0 || d > maxN+base {
			t.Fatalf("attempt %d: %v out of range", attempt, d)
		}
		if attempt < 4 && d <= prev-base {
			t.Errorf("attempt %d: %v did not grow past %v", attempt, d, prev)
		}
		prev = d
	}
	// A degenerate configuration must still produce a usable backoff
	// rather than dividing by zero.
	if d := mysql.ExponentialJitter(0, 0)(1); d <= 0 {
		t.Errorf("zero base: %v", d)
	}
}

// attemptsUnder counts how many times InTx runs a callback that always
// fails with err.
func attemptsUnder(t *testing.T, db *mysql.DB, err error) int {
	t.Helper()
	attempts := 0
	_ = db.InTx(context.Background(), func(*mysql.DB) error {
		attempts++
		return err
	})
	return attempts
}
