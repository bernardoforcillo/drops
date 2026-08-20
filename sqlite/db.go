package sqlite

import (
	"context"
	"time"

	"github.com/bernardoforcillo/drops"
)

// DB is the entry point for issuing SQLite queries through a
// drops.Driver. Any driver — database/sql with mattn/go-sqlite3 or
// modernc.org/sqlite, or a custom connection — can back it.
//
// It offers the same Hook / Ping / Close / InTx / Select / Insert /
// Update / Delete contract as drops/pg's DB, so switching a codebase
// from PostgreSQL to SQLite is largely a matter of swapping the
// constructor. Every builder it returns renders SQLite syntax ("?"
// placeholders) via the shared drops.Dialect.
//
// Safe for concurrent use by multiple goroutines provided the
// underlying Driver is; builders are not — create one per query.
type DB struct {
	drv    drops.Driver
	hook   drops.Hook
	retry  *RetryPolicy
	tracer Tracer

	// strictLoading, set by StrictLoading, makes Find refuse a query
	// that would leave a declared relation field unloaded. See
	// strict.go.
	strictLoading bool
}

// New wraps a drops.Driver as a SQLite DB.
func New(drv drops.Driver) *DB { return &DB{drv: drv} }

// Driver returns the underlying driver.
func (db *DB) Driver() drops.Driver { return db.drv }

// WithHook returns a shallow copy with hook installed; nil removes it.
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

// Begin opens a transaction and returns a DB bound to it plus the raw
// Tx. Prefer InTx for automatic commit/rollback.
func (db *DB) Begin(ctx context.Context) (*DB, drops.Tx, error) {
	start := time.Now()
	tx, err := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(start), Err: err})
	if err != nil {
		return nil, nil, err
	}
	return &DB{drv: tx, hook: db.hook, retry: db.retry, tracer: db.tracer}, tx, nil
}

// InTx runs fn inside a transaction, committing on nil and rolling back
// otherwise (including on panic, which is re-raised). Rollback uses a
// detached, short-timeout context so a cancelled caller-ctx can't block
// cleanup.
//
// When a RetryPolicy is installed via WithRetry, the whole callback is
// re-run inside a fresh transaction on a retryable error (SQLITE_BUSY /
// SQLITE_LOCKED by default), up to MaxAttempts times, sleeping Backoff
// between attempts. Callbacks must be idempotent across retries.
func (db *DB) InTx(ctx context.Context, fn func(*DB) error) error {
	if db.retry == nil {
		return db.inTxOnce(ctx, fn)
	}
	policy := *db.retry
	attempts := policy.attempts()
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		err = db.inTxOnce(ctx, fn)
		if err == nil || attempt == attempts || !policy.shouldRetry(err) {
			return err
		}
		if policy.Backoff != nil {
			if serr := retrySleep(ctx, policy.Backoff(attempt)); serr != nil {
				return serr
			}
		}
	}
	return err
}

// inTxOnce runs fn inside exactly one transaction.
func (db *DB) inTxOnce(ctx context.Context, fn func(*DB) error) (err error) {
	bstart := time.Now()
	tx, berr := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(bstart), Err: berr})
	if berr != nil {
		return berr
	}
	inner := &DB{drv: tx, hook: db.hook, retry: db.retry, tracer: db.tracer}
	rollback := func() {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
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

// Exec runs a raw SQL statement. Placeholders are SQLite "?" markers.
//
// Query tags on ctx (see [drops.WithQueryTags]) are appended to sql as
// a trailing comment before anything else looks at it, so the span,
// the hook and the server all report the same statement text.
func (db *DB) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	sql = drops.TagStatement(ctx, sql)
	ctx, span := db.startSpan(ctx, "sqlite.exec")
	defer span.End()
	db.annotateSpan(span, "exec", sql, args)
	start := time.Now()
	drvArgs := args
	if containsPII(args) {
		drvArgs = unwrapPII(args)
	}
	res, err := db.drv.Exec(ctx, sql, drvArgs...)
	err = classifyError(err)
	if err != nil {
		span.RecordError(err)
	}
	db.emit(ctx, drops.QueryEvent{Kind: "exec", SQL: sql, Args: args, Duration: time.Since(start), Err: err})
	return res, err
}

// Query runs a raw SQL query. Query tags on ctx are appended as a
// trailing comment, as in [DB.Exec].
func (db *DB) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	sql = drops.TagStatement(ctx, sql)
	ctx, span := db.startSpan(ctx, "sqlite.query")
	defer span.End()
	db.annotateSpan(span, "query", sql, args)
	start := time.Now()
	drvArgs := args
	if containsPII(args) {
		drvArgs = unwrapPII(args)
	}
	rows, err := db.drv.Query(ctx, sql, drvArgs...)
	err = classifyError(err)
	if err != nil {
		span.RecordError(err)
	}
	db.emit(ctx, drops.QueryEvent{Kind: "query", SQL: sql, Args: args, Duration: time.Since(start), Err: err})
	return rows, err
}

// ExecExpr renders e with the SQLite dialect and runs it. Convenience
// for DDL helpers like CreateTable.
func (db *DB) ExecExpr(ctx context.Context, e drops.Expression) (drops.Result, error) {
	sql, args := drops.StringWithDialect(Dialect, e)
	return db.Exec(ctx, sql, args...)
}

// ToSQL renders e with the SQLite dialect. Exposed for tests and
// logging (mirrors clickhouse.ToSQL).
func ToSQL(e drops.Expression) (sql string, args []any) {
	return drops.StringWithDialect(Dialect, e)
}

func (db *DB) emit(ctx context.Context, e drops.QueryEvent) {
	drops.CallHook(db.hook, ctx, e)
}
