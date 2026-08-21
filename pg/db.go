package pg

import (
	"context"
	"time"

	"github.com/bernardoforcillo/drops"
)

// DB is the entry point for issuing PostgreSQL queries through a
// drops.Driver. Any driver implementation — database/sql, pgx, or a
// custom connection — can back a DB.
//
// A DB is safe for concurrent use by multiple goroutines provided the
// underlying Driver is. The builder types returned by Select/Insert/
// Update/Delete/Find are NOT safe for concurrent use; create one per
// query.
//
// An optional drops.Hook can be attached via WithHook to observe every
// driver operation — query logging, slow-query alerts, tracing, metrics.
// The hook and the [Tracer] are both propagated into the transaction-
// bound DBs returned by Begin and InTx — see bind — and InTx emits
// "begin"/"commit"/"rollback" events for the transaction lifecycle. For
// full lifecycle observability prefer InTx; with an explicit Begin you
// must call Commit/Rollback yourself, and those bypass the hook unless
// you wrap them.
type DB struct {
	drv    drops.Driver
	hook   drops.Hook
	tracer Tracer
	retry  *RetryPolicy
}

// New wraps a drops.Driver as a DB.
func New(drv drops.Driver) *DB { return &DB{drv: drv} }

// Driver returns the underlying driver. Useful for adapters or for
// dropping down to raw SQL.
func (db *DB) Driver() drops.Driver { return db.drv }

// WithHook returns a shallow copy of db with hook installed. Passing
// nil removes the hook. Compose multiple hooks via drops.ChainHooks.
func (db *DB) WithHook(hook drops.Hook) *DB {
	cp := *db
	cp.hook = hook
	return &cp
}

// Hook returns the currently attached hook, or nil.
func (db *DB) Hook() drops.Hook { return db.hook }

// Close shuts down the underlying driver if it implements io.Closer.
// For Drivers that don't (most pool wrappers do), it is a no-op and
// returns nil.
//
// Typical usage is `defer db.Close()` next to the connection-open call:
//
//	sqlDB, _ := sql.Open("pgx", dsn)
//	db := pg.New(stdlib.New(sqlDB))
//	defer db.Close()
func (db *DB) Close() error {
	type closer interface{ Close() error }
	if c, ok := db.drv.(closer); ok {
		return c.Close()
	}
	return nil
}

// Ping verifies that the database accepts a no-op query.
func (db *DB) Ping(ctx context.Context) error {
	start := time.Now()
	_, err := db.drv.Exec(ctx, "SELECT 1")
	db.emit(ctx, drops.QueryEvent{
		Kind:     "ping",
		Duration: time.Since(start),
		Err:      err,
	})
	return err
}

// Begin opens a transaction and returns a DB bound to it plus the raw
// Tx handle. Most callers should prefer InTx for automatic commit/
// rollback and full hook coverage.
func (db *DB) Begin(ctx context.Context) (*DB, drops.Tx, error) {
	start := time.Now()
	tx, err := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(start), Err: err})
	if err != nil {
		return nil, nil, err
	}
	return db.bind(tx), tx, nil
}

// bind returns the *DB every statement inside a transaction goes out
// through: this DB's configuration, with the transaction handle where
// the pool used to be.
//
// It exists because there were two of these literals — one in Begin,
// one in inTxOnce — and both were spelled &DB{drv: tx, hook: db.hook},
// which carried the hook and dropped the rest by omission. A field
// added to DB is dropped by both of them and by neither on purpose,
// which is the defect the tracer already was: WithTracer produced a
// span for every statement outside a transaction and none for any
// statement inside one, so a trace went dark at exactly the boundary a
// latency investigation follows it across, and the two statements
// [DB.InTxAs] sends — the SET LOCAL ROLE and the set_config that say
// which identity the work ran under — were the first to vanish. The
// hook was propagated throughout, so the audit trail was intact and
// nothing was ever unobservable twice: this is observability, not a
// leak.
//
// A field a transaction-bound DB must NOT carry is dropped here, once,
// with the reason, rather than at two call sites by not being typed:
//
//   - retry. A [RetryPolicy] re-runs a whole transaction, and this DB
//     is already inside one. A nested InTx that retried would re-run
//     its body against a transaction the failed attempt had already
//     aborted, where PostgreSQL answers every further statement with
//     25P02 until the outer transaction ends — so the retry could not
//     succeed, and would spend its attempts finding that out. Retrying
//     belongs to the outermost InTx, which has the policy.
//
// pg/db_bind_internal_test.go holds both halves to that: every field
// of DB has a disposition here, and each disposition is what bind
// does.
func (db *DB) bind(tx drops.Tx) *DB {
	return &DB{drv: tx, hook: db.hook, tracer: db.tracer}
}

// InTx runs fn inside a transaction. The transaction is committed if fn
// returns nil and rolled back otherwise (including on panic, after which
// the panic is re-raised). Hook events are emitted for begin, commit
// and rollback alongside the ordinary exec/query events fn produces.
//
// Rollback uses a detached context with a short timeout so a cancelled
// or expired caller-ctx doesn't prevent cleanup. When a RetryPolicy is
// installed via WithRetry, transient failures (those the policy marks
// retryable) cause the transaction to be re-opened and fn re-run, up
// to MaxAttempts times.
//
// A transaction that has to run under a particular PostgreSQL identity
// — a role, or the session settings a row-level security policy reads
// with current_setting — wants [DB.InTxAs] instead. It is this method
// with the identity established inside the transaction and reverted
// with it, and with a failure to establish it aborting rather than
// falling back to the pool's own user.
func (db *DB) InTx(ctx context.Context, fn func(*DB) error) error {
	if db.retry == nil {
		return db.inTxOnce(ctx, fn)
	}
	policy := *db.retry
	var lastErr error
	for attempt := 1; attempt <= policy.attempts(); attempt++ {
		if attempt > 1 && policy.Backoff != nil {
			if cerr := retrySleep(ctx, policy.Backoff(attempt-1)); cerr != nil {
				return cerr
			}
		}
		err := db.inTxOnce(ctx, fn)
		if err == nil {
			return nil
		}
		if !policy.shouldRetry(err) {
			return err
		}
		lastErr = err
		// Bail out early if the context expired between attempts.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
	}
	return lastErr
}

// inTxOnce runs fn inside a single transaction without any retry
// logic. Used by InTx — extracted so the retry loop stays readable.
func (db *DB) inTxOnce(ctx context.Context, fn func(*DB) error) (err error) {
	bstart := time.Now()
	tx, berr := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(bstart), Err: berr})
	if berr != nil {
		return berr
	}
	inner := db.bind(tx)
	rollback := func() error {
		rctx, cancel := rollbackCtx(ctx)
		defer cancel()
		rstart := time.Now()
		rerr := tx.Rollback(rctx)
		db.emit(rctx, drops.QueryEvent{Kind: "rollback", Duration: time.Since(rstart), Err: rerr})
		return rerr
	}
	defer func() {
		if p := recover(); p != nil {
			_ = rollback()
			panic(p)
		}
		if err != nil {
			_ = rollback()
			return
		}
		cstart := time.Now()
		cerr := tx.Commit(ctx)
		db.emit(ctx, drops.QueryEvent{Kind: "commit", Duration: time.Since(cstart), Err: cerr})
		if cerr != nil {
			err = cerr
		}
	}()
	err = fn(inner)
	return err
}

// rollbackCtx re-uses drops.rollbackCtx semantics (detached + short
// timeout) for the pg-level InTx implementation.
func rollbackCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}

// Select begins a SELECT. With no columns the projection is "*".
func (db *DB) Select(cols ...drops.Expression) *SelectBuilder {
	return &SelectBuilder{db: db, columns: cols}
}

// Insert begins an INSERT INTO <t>.
func (db *DB) Insert(t *Table) *InsertBuilder {
	return &InsertBuilder{db: db, table: t}
}

// Update begins an UPDATE <t>.
func (db *DB) Update(t *Table) *UpdateBuilder {
	return &UpdateBuilder{db: db, table: t}
}

// Delete begins a DELETE FROM <t>.
func (db *DB) Delete(t *Table) *DeleteBuilder {
	return &DeleteBuilder{db: db, table: t}
}

// Exec runs a raw SQL statement with positional ($1, $2, ...) placeholders.
func (db *DB) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	spanCtx, span := db.startSpan(ctx, "drops.exec")
	span.SetAttribute(AttrSystem, AttrSystemPG)
	span.SetAttribute(AttrOperation, "exec")
	span.SetAttribute(AttrStatement, sql)
	span.SetAttribute(AttrArgsCount, len(args))
	start := time.Now()
	// Unwrap PII markers before reaching the driver — they're a
	// pure observability concept and must not affect the wire form.
	driverArgs := args
	if containsPII(args) {
		driverArgs = unwrapPII(args)
	}
	res, err := db.drv.Exec(spanCtx, sql, driverArgs...)
	err = classifyError(err)
	if err != nil {
		span.RecordError(err)
	}
	span.End()
	db.emit(ctx, drops.QueryEvent{
		Kind: "exec", SQL: sql, Args: args,
		Duration: time.Since(start), Err: err,
	})
	return res, err
}

// Query runs a raw SQL query.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	spanCtx, span := db.startSpan(ctx, "drops.query")
	span.SetAttribute(AttrSystem, AttrSystemPG)
	span.SetAttribute(AttrOperation, "query")
	span.SetAttribute(AttrStatement, sql)
	span.SetAttribute(AttrArgsCount, len(args))
	start := time.Now()
	driverArgs := args
	if containsPII(args) {
		driverArgs = unwrapPII(args)
	}
	rows, err := db.drv.Query(spanCtx, sql, driverArgs...)
	err = classifyError(err)
	if err != nil {
		span.RecordError(err)
	}
	span.End()
	db.emit(ctx, drops.QueryEvent{
		Kind: "query", SQL: sql, Args: args,
		Duration: time.Since(start), Err: err,
	})
	return rows, err
}

// ExecExpr renders e to SQL and runs it as a statement. It is the
// convenience path for the expressions that have no executor of their
// own — the DDL helpers, CreateTable and CreateIndex and
// CreateExtensionIfNotExists.
//
// The ctx reaches the render, which is the rule this package holds
// every ctx-taking method to: a method that accepts a context.Context
// resolves the statement against it, or its doc says why it cannot.
// This one used to be the counter-example. It rendered through
// drops.String — the context-free path — and handed the result to Exec
// together with the ctx it had just discarded. All four builders
// satisfy drops.Expression, so a caller could pass one and be told
// nothing was wrong: on a table whose every read carries a tenant
// predicate, db.ExecExpr(ctx, db.Delete(Posts)) sent DELETE FROM
// "posts" with no WHERE clause and reported the destruction of every
// tenant's rows as a success. "Convenience for DDL helpers" described
// how the method was meant to be called; it did not stop the other
// call, and a description is not a guard.
//
// What the ctx can contribute depends on what e is, and the third case
// is as deliberate as the first two:
//
//   - A statement drops can render per request — the Select, Insert,
//     Update and Delete builders, or any type carrying the same
//     ToSQLCtx method — goes through ToSQLCtx, so the context filters
//     of every table it names are resolved into it, and a filter that
//     refuses ([ErrTenantMissing]) stops the statement instead of
//     scoping it.
//   - An expression with a statement inside it — [Subquery] and the
//     other operands built on opExpr — is walked to that statement and
//     resolved there.
//   - Anything genuinely opaque — a DDL helper, a [drops.Raw], a
//     caller's own [drops.ExprFunc] — has nothing drops can resolve:
//     no builder under the closure, no table to ask. It renders exactly
//     as it did before, byte for byte, and stays the caller's to scope.
//     That is the documented escape hatch, and it is still open.
func (db *DB) ExecExpr(ctx context.Context, e drops.Expression) (drops.Result, error) {
	sql, args, err := renderForCtx(ctx, e)
	if err != nil {
		return nil, err
	}
	return db.Exec(ctx, sql, args...)
}

// emit invokes the hook, if any, with the provided event. Uses
// drops.CallHook so a panicking user-supplied hook can't crash the
// request goroutine.
func (db *DB) emit(ctx context.Context, e drops.QueryEvent) {
	drops.CallHook(db.hook, ctx, e)
}
