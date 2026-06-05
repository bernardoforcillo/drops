package pg

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// RetryPolicy declares how InTx (and InTxRetry) should react to
// transient failures — typically SerializationFailure and Deadlock
// under SERIALIZABLE isolation. Without a policy InTx behaves as
// before: every error propagates immediately.
//
// Configure once at DB setup:
//
//	db := pg.New(drv).WithRetry(pg.RetryPolicy{
//	    MaxAttempts: 3,
//	    Errors:      []error{pg.ErrSerializationFailure, pg.ErrDeadlock},
//	    Backoff:     pg.ExponentialJitter(10*time.Millisecond, 1*time.Second),
//	})
//
// InTx then retries the supplied callback up to MaxAttempts times.
// Between attempts it sleeps for Backoff(attempt) — attempts are
// 1-based, so Backoff(1) returns the wait BEFORE the second try.
// Context cancellation short-circuits both the sleep and the loop.
//
// The retry is at the transaction level — the entire callback is
// re-run inside a fresh transaction each time. Callbacks must be
// idempotent across retries. Read what you wrote before the
// rollback? Re-do it. Side effects (emails, HTTP, …)? Push them to
// the outbox so the rollback also rolls them back.
type RetryPolicy struct {
	// MaxAttempts caps the total runs of the callback. Values < 1
	// are treated as 1 (no retries).
	MaxAttempts int

	// Errors are sentinel values; a returned error is retried when
	// errors.Is(err, e) is true for any e in the slice. Nil means
	// "retry on every error", which is almost always wrong — set
	// this explicitly.
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
// Passing the zero RetryPolicy clears any previously-installed
// policy.
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
// starting from base, caps at max, and adds [0, base) jitter so
// concurrent retries don't synchronise into thundering herds.
//
//	attempt 1: base + jitter
//	attempt 2: 2*base + jitter
//	attempt 3: 4*base + jitter
//	...
//	clipped at max + jitter
func ExponentialJitter(base, max time.Duration) func(attempt int) time.Duration {
	if base <= 0 {
		base = 10 * time.Millisecond
	}
	if max <= 0 || max < base {
		max = base * 256
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
		if d > max {
			d = max
		}
		// Deterministic-enough jitter source — we don't need
		// cryptographic randomness here, just decorrelation.
		j := time.Duration(rand.Int63n(int64(base)))
		return d + j
	}
}

// DefaultRetryPolicy returns the conventional safe default: up to
// 3 attempts, retry on SerializationFailure and Deadlock, 10ms
// base exponential backoff capped at 1s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		Errors:      []error{ErrSerializationFailure, ErrDeadlock},
		Backoff:     ExponentialJitter(10*time.Millisecond, time.Second),
	}
}

// retrySleep waits for d or until ctx is cancelled. Returns
// ctx.Err() on cancellation, nil when the timer elapses.
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
