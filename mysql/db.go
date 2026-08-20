package mysql

import (
	"context"
	"time"

	"github.com/bernardoforcillo/drops"
)

// DB issues MySQL queries through a drops.Driver — database/sql with
// go-sql-driver/mysql, or any other connection wrapped to satisfy the
// interface.
//
// It offers the same Hook / Ping / Close / InTx / Select / Insert /
// Update / Delete contract as drops/pg and drops/sqlite, and every
// builder it returns renders MySQL syntax through the shared
// drops.Dialect.
//
// Safe for concurrent use if the underlying Driver is; builders are
// not — create one per query.
type DB struct {
	drv   drops.Driver
	hook  drops.Hook
	retry *RetryPolicy
}

// New wraps a drops.Driver as a MySQL DB.
func New(drv drops.Driver) *DB { return &DB{drv: drv} }

// Driver returns the underlying driver.
func (db *DB) Driver() drops.Driver { return db.drv }

// WithHook returns a shallow copy with hook installed; nil removes it.
func (db *DB) WithHook(hook drops.Hook) *DB {
	cp := *db
	cp.hook = hook
	return &cp
}

// Hook returns the attached hook, or nil.
func (db *DB) Hook() drops.Hook { return db.hook }

// Close releases the driver if it implements io.Closer.
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

// Begin opens a transaction, returning a DB bound to it and the raw
// Tx. Prefer InTx, which handles commit and rollback.
func (db *DB) Begin(ctx context.Context) (*DB, drops.Tx, error) {
	start := time.Now()
	tx, err := db.drv.Begin(ctx)
	db.emit(ctx, drops.QueryEvent{Kind: "begin", Duration: time.Since(start), Err: err})
	if err != nil {
		return nil, nil, err
	}
	return &DB{drv: tx, hook: db.hook}, tx, nil
}

// InTx runs fn in a transaction, committing on nil and rolling back
// otherwise — including on panic, which is re-raised after the
// rollback.
//
// With a [RetryPolicy] installed by [DB.WithRetry], a failure the
// policy calls transient — a deadlock, a lock wait timeout — rolls the
// transaction back and re-runs fn in a fresh one. See RetryPolicy for
// what that demands of fn.
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

// inTxOnce runs fn inside a single transaction, without any retry.
// Rollback uses a detached context with a short timeout so a cancelled
// caller cannot leave the transaction open.
func (db *DB) inTxOnce(ctx context.Context, fn func(*DB) error) (err error) {
	txDB, tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		start := time.Now()
		rbErr := tx.Rollback(rbCtx)
		db.emit(ctx, drops.QueryEvent{Kind: "rollback", Duration: time.Since(start), Err: rbErr})
		if r := recover(); r != nil {
			panic(r)
		}
	}()
	if err := fn(txDB); err != nil {
		return err
	}
	start := time.Now()
	if err := tx.Commit(ctx); err != nil {
		db.emit(ctx, drops.QueryEvent{Kind: "commit", Duration: time.Since(start), Err: err})
		return err
	}
	committed = true
	db.emit(ctx, drops.QueryEvent{Kind: "commit", Duration: time.Since(start)})
	return nil
}

// Select begins a SELECT. With no columns it projects *.
func (db *DB) Select(cols ...drops.Expression) *SelectBuilder {
	return &SelectBuilder{db: db, columns: cols}
}

// Insert begins an INSERT into t.
func (db *DB) Insert(t *Table) *InsertBuilder { return &InsertBuilder{db: db, table: t} }

// Update begins an UPDATE of t.
func (db *DB) Update(t *Table) *UpdateBuilder { return &UpdateBuilder{db: db, table: t} }

// Delete begins a DELETE from t.
func (db *DB) Delete(t *Table) *DeleteBuilder { return &DeleteBuilder{db: db, table: t} }

// Exec runs a statement that returns no rows.
func (db *DB) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	start := time.Now()
	res, err := db.drv.Exec(ctx, sql, args...)
	// Classify before the hook sees it, so a log line and a caller's
	// errors.Is agree about what happened — see [ServerError].
	err = classifyError(err)
	db.emit(ctx, drops.QueryEvent{Kind: "exec", SQL: sql, Args: args, Duration: time.Since(start), Err: err})
	return res, err
}

// Query runs a statement that returns rows.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	start := time.Now()
	rows, err := db.drv.Query(ctx, sql, args...)
	err = classifyError(err)
	db.emit(ctx, drops.QueryEvent{Kind: "query", SQL: sql, Args: args, Duration: time.Since(start), Err: err})
	return rows, err
}

// ExecExpr renders e and executes it — the usual way to run DDL.
func (db *DB) ExecExpr(ctx context.Context, e drops.Expression) (drops.Result, error) {
	sql, args := render(e)
	return db.Exec(ctx, sql, args...)
}

// LastInsertID reads the generated key out of a Result.
//
// MySQL has no RETURNING, so this is how an AUTO_INCREMENT value comes
// back. The second result is false when the driver does not expose one
// — the interface drops requires of a Result is only RowsAffected, and
// database/sql happens to offer more.
func LastInsertID(res drops.Result) (int64, bool) {
	type lastInsertIDer interface{ LastInsertId() (int64, error) }
	l, ok := res.(lastInsertIDer)
	if !ok {
		return 0, false
	}
	id, err := l.LastInsertId()
	if err != nil {
		return 0, false
	}
	return id, true
}

// render renders an expression with the MySQL dialect installed.
func render(e drops.Expression) (string, []any) {
	b := drops.NewBuilder(drops.WithDialect(Dialect))
	b.Append(e)
	return b.SQL()
}

func (db *DB) emit(ctx context.Context, e drops.QueryEvent) {
	drops.CallHook(db.hook, ctx, e)
}
