package sqlite

import "context"

// Existence checks against SQLite's catalog. Convenience helpers for
// code that branches on the live state of the database — migration
// callbacks, idempotent setup scripts, conditional fixtures.
//
// SQLite has no information_schema; object metadata lives in the
// sqlite_master table and the PRAGMA functions. These helpers query
// the schema of the currently-attached "main" database.

// TableExists reports whether a table (or view) named table exists.
func TableExists(ctx context.Context, db *DB, table string) (bool, error) {
	rows, err := db.Query(ctx,
		`SELECT 1 FROM sqlite_master WHERE type IN ('table','view') AND name = ? LIMIT 1`,
		table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// IndexExists reports whether an index named index exists.
func IndexExists(ctx context.Context, db *DB, index string) (bool, error) {
	rows, err := db.Query(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ? LIMIT 1`,
		index)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// TriggerExists reports whether a trigger named trigger exists.
func TriggerExists(ctx context.Context, db *DB, trigger string) (bool, error) {
	rows, err := db.Query(ctx,
		`SELECT 1 FROM sqlite_master WHERE type = 'trigger' AND name = ? LIMIT 1`,
		trigger)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// ColumnExists reports whether column exists on table. It uses
// pragma_table_info, so it correctly reflects the live column set even
// after ALTER TABLE ADD/DROP COLUMN.
func ColumnExists(ctx context.Context, db *DB, table, column string) (bool, error) {
	rows, err := db.Query(ctx,
		`SELECT 1 FROM pragma_table_info(?) WHERE name = ? LIMIT 1`,
		table, column)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}
