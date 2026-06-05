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
// The hook is propagated into the transaction-bound DBs returned by
// Begin and InTx, and InTx emits "begin"/"commit"/"rollback" events for
// the transaction lifecycle. For full lifecycle observability prefer
// InTx; with an explicit Begin you must call Commit/Rollback yourself,
// and those bypass the hook unless you wrap them.
type DB struct {
	drv  drops.Driver
	hook drops.Hook
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
	return &DB{drv: tx, hook: db.hook}, tx, nil
}

// InTx runs fn inside a transaction. The transaction is committed if fn
// returns nil and rolled back otherwise (including on panic, after which
// the panic is re-raised). Hook events are emitted for begin, commit
// and rollback alongside the ordinary exec/query events fn produces.
//
// Rollback uses a detached context with a short timeout so a cancelled
// or expired caller-ctx doesn't prevent cleanup.
func (db *DB) InTx(ctx context.Context, fn func(*DB) error) (err error) {
	bstart := time.Now()
	tx, berr := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(bstart), Err: berr})
	if berr != nil {
		return berr
	}
	inner := &DB{drv: tx, hook: db.hook}
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
	start := time.Now()
	res, err := db.drv.Exec(ctx, sql, args...)
	err = classifyError(err)
	db.emit(ctx, drops.QueryEvent{
		Kind: "exec", SQL: sql, Args: args,
		Duration: time.Since(start), Err: err,
	})
	return res, err
}

// Query runs a raw SQL query.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	start := time.Now()
	rows, err := db.drv.Query(ctx, sql, args...)
	err = classifyError(err)
	db.emit(ctx, drops.QueryEvent{
		Kind: "query", SQL: sql, Args: args,
		Duration: time.Since(start), Err: err,
	})
	return rows, err
}

// ExecExpr renders e to SQL and runs it as a statement. Convenience for
// DDL helpers like CreateTable.
func (db *DB) ExecExpr(ctx context.Context, e drops.Expression) (drops.Result, error) {
	sql, args := drops.String(e)
	return db.Exec(ctx, sql, args...)
}

// emit invokes the hook, if any, with the provided event. Uses
// drops.CallHook so a panicking user-supplied hook can't crash the
// request goroutine.
func (db *DB) emit(ctx context.Context, e drops.QueryEvent) {
	drops.CallHook(db.hook, ctx, e)
}
