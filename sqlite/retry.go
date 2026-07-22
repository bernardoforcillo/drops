package sqlite

import (
	"context"
	"errors"
	"math/rand"
	"strings"
	"time"
)

// RetryPolicy declares how InTx reacts to transient failures — under
// SQLite, principally SQLITE_BUSY / SQLITE_LOCKED when another
// connection holds a write lock. Without a policy InTx runs the callback
// exactly once and propagates every error.
//
//	db := sqlite.New(drv).WithRetry(sqlite.DefaultRetryPolicy())
//
// InTx then retries the callback up to MaxAttempts times, sleeping
// Backoff(attempt) between tries (attempts are 1-based, so Backoff(1) is
// the wait before the second try). Context cancellation short-circuits
// both the sleep and the loop. The retry is at the transaction level —
// the whole callback re-runs in a fresh transaction — so callbacks must
// be idempotent across retries.
type RetryPolicy struct {
	// MaxAttempts caps the total callback runs. Values < 1 mean 1.
	MaxAttempts int

	// Errors are the retryable sentinels; an error is retried when it
	// matches one (via errors.Is, or — for ErrBusy / ErrLocked — the
	// driver error's message). Nil means "retry on every error", which
	// is almost always wrong; set this explicitly.
	Errors []error

	// Backoff returns the wait between attempt and attempt+1 (1-based).
	// nil disables sleeping.
	Backoff func(attempt int) time.Duration
}

// shouldRetry reports whether err matches one of the policy's retryable
// sentinels, either by errors.Is or, for the SQLite contention
// sentinels, by inspecting the driver error's message.
func (p RetryPolicy) shouldRetry(err error) bool {
	if err == nil {
		return false
	}
	for _, target := range p.Errors {
		if errors.Is(err, target) {
			return true
		}
		if (target == ErrBusy || target == ErrLocked) && messageIndicatesContention(err) {
			return true
		}
	}
	return false
}

// messageIndicatesContention matches the busy/locked strings SQLite
// drivers (mattn/go-sqlite3, modernc.org/sqlite) surface. Drivers seldom
// wrap an errors.Is-comparable sentinel, so message matching is what
// makes DefaultRetryPolicy actually fire in production.
func messageIndicatesContention(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked")
}

// attempts returns MaxAttempts clamped to >= 1.
func (p RetryPolicy) attempts() int {
	if p.MaxAttempts < 1 {
		return 1
	}
	return p.MaxAttempts
}

// WithRetry returns a shallow copy of db with policy installed. The zero
// RetryPolicy clears any previously-installed policy.
func (db *DB) WithRetry(policy RetryPolicy) *DB {
	cp := *db
	if policy.MaxAttempts == 0 && policy.Backoff == nil && policy.Errors == nil {
		cp.retry = nil
	} else {
		cp.retry = &policy
	}
	return &cp
}

// RetryPolicyValue returns the active retry policy, or the zero value.
func (db *DB) RetryPolicyValue() RetryPolicy {
	if db.retry == nil {
		return RetryPolicy{}
	}
	return *db.retry
}

// ExponentialJitter returns a backoff that doubles each attempt from
// base, caps at maxN, and adds [0, base) jitter so concurrent retries
// don't synchronise into thundering herds.
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
		j := time.Duration(rand.Int63n(int64(base)))
		return d + j
	}
}

// DefaultRetryPolicy returns the conventional safe default: up to 3
// attempts, retry on SQLITE_BUSY / SQLITE_LOCKED, 10ms base exponential
// backoff capped at 1s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		Errors:      []error{ErrBusy, ErrLocked},
		Backoff:     ExponentialJitter(10*time.Millisecond, time.Second),
	}
}

// retrySleep waits for d or until ctx is cancelled.
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
