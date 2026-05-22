package clickhouse

import (
	"context"
	"errors"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Placeholder is the placeholder strategy used by the ClickHouse
// dialect — bare positional question marks, matching the
// clickhouse-go database/sql driver.
var Placeholder = drops.WithPlaceholder(func(int) string { return "?" })

// Sentinel errors for assertable failure modes.
var (
	ErrNoRows               = errors.New("drops/clickhouse: no rows in result set")
	ErrNoRowsToInsert       = errors.New("drops/clickhouse: INSERT with no rows")
	ErrReturningUnsupported = errors.New("drops/clickhouse: RETURNING is not supported by ClickHouse")
)

// DB is the entry point for issuing ClickHouse queries through a
// drops.Driver. The same Hook / Ping / Close / InTx contract as
// drops/pg's DB, but every emitted statement uses "?" placeholders.
//
// Safe for concurrent use by multiple goroutines provided the
// underlying Driver is. Builders returned by Select / Insert are not
// — create one per query.
type DB struct {
	drv  drops.Driver
	hook drops.Hook
}

// New wraps a drops.Driver as a DB.
func New(drv drops.Driver) *DB { return &DB{drv: drv} }

// Driver returns the underlying driver.
func (db *DB) Driver() drops.Driver { return db.drv }

// WithHook returns a shallow copy with hook installed; nil removes.
func (db *DB) WithHook(hook drops.Hook) *DB {
	cp := *db
	cp.hook = hook
	return &cp
}

// Hook returns the currently attached hook, or nil.
func (db *DB) Hook() drops.Hook { return db.hook }

// Close releases the underlying driver if it implements io.Closer.
func (db *DB) Close() error {
	type closer interface{ Close() error }
	if c, ok := db.drv.(closer); ok {
		return c.Close()
	}
	return nil
}

// Ping verifies the connection with SELECT 1.
func (db *DB) Ping(ctx context.Context) error {
	start := time.Now()
	_, err := db.drv.Exec(ctx, "SELECT 1")
	db.emit(ctx, drops.QueryEvent{Kind: "ping", Duration: time.Since(start), Err: err})
	return err
}

// Select begins a SELECT.
func (db *DB) Select(cols ...drops.Expression) *SelectBuilder {
	return &SelectBuilder{db: db, columns: cols}
}

// Insert begins an INSERT INTO <t>.
func (db *DB) Insert(t *Table) *InsertBuilder {
	return &InsertBuilder{db: db, table: t}
}

// Exec runs a raw SQL statement (with "?" placeholders).
func (db *DB) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	start := time.Now()
	res, err := db.drv.Exec(ctx, sql, args...)
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
	db.emit(ctx, drops.QueryEvent{
		Kind: "query", SQL: sql, Args: args,
		Duration: time.Since(start), Err: err,
	})
	return rows, err
}

// ExecExpr renders e to SQL and runs it. Convenience for DDL helpers.
func (db *DB) ExecExpr(ctx context.Context, e drops.Expression) (drops.Result, error) {
	sql, args := ToSQL(e)
	return db.Exec(ctx, sql, args...)
}

// Begin opens a transaction. Note that ClickHouse only supports
// transactions in the MergeTree family from recent versions and the
// guarantees are weaker than PostgreSQL — use sparingly.
func (db *DB) Begin(ctx context.Context) (*DB, drops.Tx, error) {
	start := time.Now()
	tx, err := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(start), Err: err})
	if err != nil {
		return nil, nil, err
	}
	return &DB{drv: tx, hook: db.hook}, tx, nil
}

// InTx runs fn inside a transaction. Rollback uses a detached context
// with a short timeout so a cancelled caller-ctx doesn't poison
// cleanup. Hook events fire for begin/commit/rollback.
func (db *DB) InTx(ctx context.Context, fn func(*DB) error) (err error) {
	bstart := time.Now()
	tx, berr := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(bstart), Err: berr})
	if berr != nil {
		return berr
	}
	inner := &DB{drv: tx, hook: db.hook}
	rollback := func() {
		rctx, cancel := rollbackCtx(ctx)
		defer cancel()
		rstart := time.Now()
		rerr := tx.Rollback(rctx)
		db.emit(rctx, drops.QueryEvent{Kind: "rollback", Duration: time.Since(rstart), Err: rerr})
	}
	defer func() {
		if p := recover(); p != nil {
			rollback()
			panic(p)
		}
		if err != nil {
			rollback()
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

// emit invokes the hook, if any.
func (db *DB) emit(ctx context.Context, e drops.QueryEvent) {
	if db.hook != nil {
		db.hook(ctx, e)
	}
}

// ToSQL renders an Expression with the ClickHouse placeholder style.
// Use it when you need to inspect generated SQL outside of the
// builders (logging, snapshotting, tests).
func ToSQL(e drops.Expression) (string, []any) {
	b := drops.NewBuilder(Placeholder)
	b.Append(e)
	return b.SQL()
}

// rollbackCtx is the detached cleanup ctx pattern shared with pg.
func rollbackCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
}
