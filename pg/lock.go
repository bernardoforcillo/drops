package pg

import (
	"context"
	"errors"
	"hash/fnv"
)

// PostgreSQL advisory locks let a fleet of N replicas elect a
// single executor for a piece of work without Redis, etcd, or
// Zookeeper. The lock space is per-database; keys are int64s and
// drops hashes string keys via FNV-1a so the caller writes
// human-readable identifiers instead of computing them.
//
//	err := pg.WithAdvisoryLock(db, ctx, "nightly-reconcile", func(tx *pg.DB) error {
//	    return runReconciliation(tx, ctx)
//	})
//	// only one fleet member runs this; the rest see ErrLockNotAcquired
//	// when using TryWithAdvisoryLock
//
// Two flavours:
//
//   - WithAdvisoryLock blocks until the lock is free, then runs
//     fn inside a transaction that holds the lock. Auto-released
//     at commit/rollback because the lock is xact-scoped. Use
//     this when you want every fleet member to eventually run
//     the work serially (e.g. a queue drain).
//
//   - TryWithAdvisoryLock attempts the lock without blocking. If
//     someone else holds it, the function returns
//     ErrLockNotAcquired immediately. Use this for "only one
//     replica per tick" cron jobs.
//
// Both use pg_advisory_xact_lock / pg_try_advisory_xact_lock so
// the lock is automatically released when the wrapping
// transaction ends — no leak if the application crashes mid-run.

// ErrLockNotAcquired is returned by TryWithAdvisoryLock when
// another holder has the lock. Distinct from a SQL error so
// callers can branch cleanly with errors.Is.
var ErrLockNotAcquired = errors.New("drops/pg: advisory lock not acquired")

// WithAdvisoryLock opens a transaction, takes the lock keyed by
// key (FNV-hashed to int64), runs fn against the tx, and
// releases the lock when the tx ends. Blocks until the lock is
// free or ctx is cancelled.
func WithAdvisoryLock(db *DB, ctx context.Context, key string, fn func(*DB) error) error {
	return db.InTx(ctx, func(tx *DB) error {
		k := lockKey(key)
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", k); err != nil {
			return err
		}
		return fn(tx)
	})
}

// TryWithAdvisoryLock is the non-blocking variant: returns
// ErrLockNotAcquired when someone else holds the lock. Useful
// for "elected leader per tick" patterns where missing the
// election is better than waiting.
func TryWithAdvisoryLock(db *DB, ctx context.Context, key string, fn func(*DB) error) error {
	return db.InTx(ctx, func(tx *DB) error {
		k := lockKey(key)
		rows, err := tx.Query(ctx, "SELECT pg_try_advisory_xact_lock($1)", k)
		if err != nil {
			return err
		}
		var ok bool
		if rows.Next() {
			if err := rows.Scan(&ok); err != nil {
				rows.Close()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !ok {
			return ErrLockNotAcquired
		}
		return fn(tx)
	})
}

// AdvisoryLockKey returns the int64 key drops would use for the
// supplied string. Exposed so callers can pass the hashed value
// directly when they want full control of the lock identifier
// (e.g. to match a key generated elsewhere).
func AdvisoryLockKey(key string) int64 { return lockKey(key) }

// lockKey is the internal string → int64 hash. FNV-1a is fast
// and stable across releases — the choice is exposed via
// AdvisoryLockKey so callers can reproduce it.
func lockKey(key string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return int64(h.Sum64())
}
