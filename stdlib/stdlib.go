// Package stdlib adapts a *database/sql.DB to drops.Driver.
//
// Use it with any database/sql-compatible driver — for PostgreSQL, that
// typically means github.com/jackc/pgx/v5/stdlib or github.com/lib/pq:
//
//	import (
//	    _ "github.com/jackc/pgx/v5/stdlib"
//	    "github.com/bernardoforcillo/drops/pg"
//	    "github.com/bernardoforcillo/drops/stdlib"
//	)
//
//	sqlDB, _ := sql.Open("pgx", dsn)
//	db := pg.New(stdlib.New(sqlDB))
package stdlib

import (
	"context"
	"database/sql"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// New wraps a *sql.DB as a drops.Driver.
func New(db *sql.DB) drops.Driver { return &poolDriver{db: db} }

type poolDriver struct{ db *sql.DB }

// Close releases the underlying *sql.DB pool. drops.DB.Close will call
// this when the user invokes it.
func (d *poolDriver) Close() error { return d.db.Close() }

func (d *poolDriver) Exec(ctx context.Context, sqlStr string, args ...any) (drops.Result, error) {
	return d.db.ExecContext(ctx, sqlStr, args...)
}

func (d *poolDriver) Query(ctx context.Context, sqlStr string, args ...any) (drops.Rows, error) {
	return d.db.QueryContext(ctx, sqlStr, args...)
}

func (d *poolDriver) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &txDriver{tx: tx}, nil
}

type txDriver struct{ tx *sql.Tx }

func (t *txDriver) Exec(ctx context.Context, sqlStr string, args ...any) (drops.Result, error) {
	return t.tx.ExecContext(ctx, sqlStr, args...)
}

func (t *txDriver) Query(ctx context.Context, sqlStr string, args ...any) (drops.Rows, error) {
	return t.tx.QueryContext(ctx, sqlStr, args...)
}

func (t *txDriver) Begin(ctx context.Context) (drops.Tx, error) {
	return nil, errors.New("drops/stdlib: database/sql does not support nested transactions; use SAVEPOINT manually")
}

func (t *txDriver) Commit(ctx context.Context) error   { return t.tx.Commit() }
func (t *txDriver) Rollback(ctx context.Context) error { return t.tx.Rollback() }
