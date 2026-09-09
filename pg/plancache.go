package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// The failure every migration causes and nobody predicts.
//
// PostgreSQL caches an execution plan per prepared statement, per
// session. A migration that changes what a statement returns — adding
// a column under a `SELECT *`, changing a column's type, dropping one
// — invalidates those plans. PostgreSQL re-plans on its own in most
// cases, but when the *result type* would change it refuses instead,
// with SQLSTATE 0A000 and the message "cached plan must not change
// result type".
//
// What makes it nasty is the timing. The migration succeeds. Nothing
// fails at that moment. The failures arrive afterwards, in ordinary
// requests, one pooled connection at a time as each one next reaches
// for a plan it prepared before the DDL — so the error looks
// unrelated to the deploy that caused it, appears intermittently, and
// clears itself once every connection has been through it.
//
// There are two remedies here and they are not interchangeable:
//
//   - [RetryCachedPlans] wraps a driver and re-issues the statement
//     once when it hits this error. This is the one that works on a
//     pool, because the fix has to happen on whichever connection
//     tripped, and only that connection's next statement can do it.
//   - [DiscardPlans] issues DISCARD PLANS, which clears the plan cache
//     of *the session it runs on* and no other. On a pool that is one
//     connection out of however many, chosen at random. It is the
//     right tool when you hold a single connection — a migration
//     worker, a maintenance job — and the wrong one everywhere else.
//
// The retry is safe in a way retries usually are not: the statement
// was rejected during planning, so nothing executed. There is no
// partial write to reason about, which is why this wrapper retries
// exactly this error and nothing else.

// RetryCachedPlans wraps drv so that a statement rejected with
// "cached plan must not change result type" is issued a second time.
//
//	drv := pg.RetryCachedPlans(stdlib.New(sqlDB))
//	db  := pg.New(drv)
//
// Compose it under [NewReplicated] rather than over it — the retry
// belongs next to the connection whose plan went stale, and wrapping
// the router instead would re-run the routing decision as well:
//
//	repl := pg.NewReplicated(
//	    pg.RetryCachedPlans(primary),
//	    pg.RetryCachedPlans(r1),
//	)
//
// Only one retry is attempted. A second identical failure is not a
// stale plan — it is a driver that re-sends the same invalidated
// prepared statement, which is a bug the wrapper must surface rather
// than hide in a loop.
//
// Transactions are passed through untouched. A failure inside one
// aborts it (every later statement returns [ErrInFailedTransaction]),
// so re-issuing the statement there would not work; the transaction
// has to be retried whole, which is what [RetryPolicy] is for. Add
// [ErrFeatureNotSupported] to a policy's Errors only if you have
// established it cannot mean an actually unsupported feature in your
// migrations, and note that [IsCachedPlanChanged] is the narrower
// check.
func RetryCachedPlans(drv drops.Driver) drops.Driver {
	if drv == nil {
		panic("drops/pg: RetryCachedPlans driver cannot be nil")
	}
	return &planCacheDriver{Driver: drv}
}

type planCacheDriver struct {
	drops.Driver
}

func (d *planCacheDriver) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	res, err := d.Driver.Exec(ctx, sql, args...)
	if err != nil && IsCachedPlanChanged(err) {
		return d.Driver.Exec(ctx, sql, args...)
	}
	return res, err
}

func (d *planCacheDriver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	rows, err := d.Driver.Query(ctx, sql, args...)
	if err != nil && IsCachedPlanChanged(err) {
		return d.Driver.Query(ctx, sql, args...)
	}
	return rows, err
}

// Unwrap returns the wrapped driver, so the duck-typed capability
// probes elsewhere in the package ([Copier], [Listener],
// [PoolStatsProvider], [LogicalStreamer]) can still reach a driver
// that implements them. Embedding alone promotes the methods, but a
// probe that type-asserts on the concrete type needs this.
func (d *planCacheDriver) Unwrap() drops.Driver { return d.Driver }

// DiscardPlans clears the cached plans and prepared statements of the
// session the call lands on, and only that one.
//
//	// A maintenance worker holding one connection, after its own DDL.
//	if err := pg.DiscardPlans(ctx, db); err != nil { ... }
//
// On a pooled *DB this clears one arbitrary connection out of the
// pool, which is almost never what a caller wants and is why this is
// documented rather than wired into [Migrator.Up] or [Push]. Reach
// for [RetryCachedPlans] there instead.
//
// DISCARD PLANS cannot run inside a transaction block; PostgreSQL
// rejects it with SQLSTATE 25001. Call it outside one.
func DiscardPlans(ctx context.Context, db *DB) error {
	_, err := db.Exec(ctx, "DISCARD PLANS")
	return err
}

// DiscardAll resets the session completely: plans, prepared
// statements, temporary tables, sequences, and every SET made on it.
//
// It is the heavier neighbour of [DiscardPlans], and it belongs at
// the point a pooled connection is handed back rather than after a
// migration — a connection that a request left with a stray SET
// statement_timeout or an open temp table carries that into whoever
// gets it next. Same session-scoped caveat, and the same rule about
// transaction blocks.
func DiscardAll(ctx context.Context, db *DB) error {
	_, err := db.Exec(ctx, "DISCARD ALL")
	return err
}
