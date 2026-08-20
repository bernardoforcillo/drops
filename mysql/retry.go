package mysql

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// RetryPolicy declares how InTx reacts to transient failures — under
// InnoDB, the deadlock and the lock-wait timeout. Without a policy InTx
// runs the callback exactly once and propagates every error.
//
// Configure once at DB setup:
//
//	db := mysql.New(drv).WithRetry(mysql.RetryPolicy{
//	    MaxAttempts: 3,
//	    Errors:      []error{mysql.ErrDeadlock, mysql.ErrLockWaitTimeout},
//	    Backoff:     mysql.ExponentialJitter(10*time.Millisecond, time.Second),
//	})
//
// InTx then retries the supplied callback up to MaxAttempts times.
// Between attempts it sleeps for Backoff(attempt) — attempts are
// 1-based, so Backoff(1) returns the wait BEFORE the second try.
// Context cancellation short-circuits both the sleep and the loop.
//
// The retry is at the transaction level — the entire callback is re-run
// inside a fresh transaction each time. That is not merely tidier than
// retrying one statement; on MySQL it is the only correct level. A
// deadlock victim's transaction has already been rolled back by the
// server, so there is nothing left to retry inside. A lock wait timeout
// leaves the transaction open with its earlier statements intact
// (innodb_rollback_on_timeout is off by default), so retrying the
// statement in place would run it against a half-finished transaction
// whose locks are still held — the surest way to turn a timeout into a
// deadlock. Rolling back and starting over is what makes both cases the
// same case.
//
// Callbacks must therefore be idempotent across retries. Read what you
// wrote before the rollback? Re-do it. Side effects (emails, HTTP, …)?
// Push them to the outbox so the rollback also rolls them back.
type RetryPolicy struct {
	// MaxAttempts caps the total runs of the callback. Values < 1 are
	// treated as 1 (no retries).
	MaxAttempts int

	// Errors are sentinel values; a returned error is retried when
	// errors.Is(err, e) is true for any e in the slice. Nil means
	// "retry on every error", which is almost always wrong — set this
	// explicitly.
	//
	// Only [ErrDeadlock] and [ErrLockWaitTimeout] belong here. The
	// other classified failures are permanent: a duplicate key, a
	// missing parent row or a failed CHECK will fail the same way on
	// every attempt, and listing one turns a rejected write into three
	// rejected writes and a wasted second.
	Errors []error

	// Backoff returns the wait between attempt and attempt+1
	// (1-based). Use ExponentialJitter or supply your own. nil
	// disables sleeping between attempts.
	Backoff func(attempt int) time.Duration
}

// shouldRetry reports whether err matches one of the policy's
// retryable sentinels.
func (p RetryPolicy) shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range p.Errors {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}

// attempts returns MaxAttempts, clamped to >= 1.
func (p RetryPolicy) attempts() int {
	if p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// WithRetry returns a shallow copy of db with policy installed.
// Passing the zero RetryPolicy clears any previously-installed policy.
func (db *DB) WithRetry(policy RetryPolicy) *DB {
	cp := *db
	if policy.MaxAttempts == 0 && policy.Backoff == nil && policy.Errors == nil {
		cp.retry = nil
	} else {
		cp.retry = &policy
	}
	return &cp
}

// RetryPolicyValue returns the active retry policy, or the zero
// RetryPolicy when none is configured.
func (db *DB) RetryPolicyValue() RetryPolicy {
	if db.retry == nil {
		return RetryPolicy{}
	}
	return *db.retry
}

// ExponentialJitter returns a backoff that doubles each attempt
// starting from base, caps at maxN, and adds [0, base) jitter so
// concurrent retries don't synchronise into thundering herds.
//
//	attempt 1: base + jitter
//	attempt 2: 2*base + jitter
//	attempt 3: 4*base + jitter
//	...
//	clipped at maxN + jitter
func ExponentialJitter(base, maxN time.Duration) func(attempt int) time.Duration {
	if base <= 0 {
		base = 10 * time.Millisecond
	}
	if maxN <= 0 || maxN < base {
		maxN = base * 256
	}
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		shift := attempt - 1
		if shift > 30 {
			shift = 30
		}
		d := base * time.Duration(1<<shift)
		if d > maxN {
			d = maxN
		}
		// Deterministic-enough jitter source — we don't need
		// cryptographic randomness here, just decorrelation.
		j := time.Duration(rand.Int63n(int64(base)))
		return d + j
	}
}

// DefaultRetryPolicy returns the conventional safe default: up to 3
// attempts, retry on deadlock and lock wait timeout, 10ms base
// exponential backoff capped at 1s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		Errors:      []error{ErrDeadlock, ErrLockWaitTimeout},
		Backoff:     ExponentialJitter(10*time.Millisecond, time.Second),
	}
}

// retrySleep waits for d or until ctx is cancelled. Returns ctx.Err()
// on cancellation, nil when the timer elapses.
func retrySleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
