package mysql

import "context"

// Existence checks against information_schema — the convenience
// helpers a migration callback or an idempotent setup script needs to
// branch on the live state of the server. Ported from drops/pg's
// exists.go.
//
// # Where the port diverges
//
// PostgreSQL has schemas inside a database and defaults them to
// "public". MySQL has no such level: a "schema" *is* a database, which
// is why information_schema's TABLE_SCHEMA column holds the database
// name. So every helper here takes a database rather than a schema,
// and an empty one means the connection's current database — DATABASE()
// — rather than a fixed name.
//
// # Case
//
// Whether these match case-insensitively is a property of the server,
// not of the query: lower_case_table_names is 0 on Linux (names are
// case sensitive), 1 on Windows and 2 on macOS. Column and constraint
// names are case-insensitive everywhere. Pass the name as it was
// declared and the answer is right on every platform.

// DatabaseExists reports whether the named database exists. It is the
// MySQL counterpart of pg's SchemaExists.
func DatabaseExists(ctx context.Context, db *DB, database string) (bool, error) {
	return existsQuery(ctx, db,
		`SELECT 1 FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ? LIMIT 1`,
		database)
}

// TableExists reports whether a table exists in the given database.
// database may be empty to mean the connection's current one.
func TableExists(ctx context.Context, db *DB, database, table string) (bool, error) {
	return existsQuery(ctx, db, `
		SELECT 1 FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())
		  AND TABLE_NAME = ? LIMIT 1`,
		database, table)
}

// ColumnExists reports whether a column exists on a table.
func ColumnExists(ctx context.Context, db *DB, database, table, column string) (bool, error) {
	return existsQuery(ctx, db, `
		SELECT 1 FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())
		  AND TABLE_NAME = ? AND COLUMN_NAME = ? LIMIT 1`,
		database, table, column)
}

// ConstraintExists reports whether a named constraint exists on a
// table.
//
// MySQL records PRIMARY KEY, UNIQUE, FOREIGN KEY and CHECK here, and
// nothing else — a plain secondary index is not a constraint and does
// not appear. [IndexExists] is the question to ask about one.
func ConstraintExists(ctx context.Context, db *DB, database, table, constraint string) (bool, error) {
	return existsQuery(ctx, db, `
		SELECT 1 FROM information_schema.TABLE_CONSTRAINTS
		WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())
		  AND TABLE_NAME = ? AND CONSTRAINT_NAME = ? LIMIT 1`,
		database, table, constraint)
}

// IndexExists reports whether a named index exists on a table. It has
// no pg counterpart because PostgreSQL indexes are schema-level
// objects with unique names, while MySQL scopes an index name to its
// table — two tables may each carry an index called "idx_created_at".
func IndexExists(ctx context.Context, db *DB, database, table, index string) (bool, error) {
	return existsQuery(ctx, db, `
		SELECT 1 FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE())
		  AND TABLE_NAME = ? AND INDEX_NAME = ? LIMIT 1`,
		database, table, index)
}

func existsQuery(ctx context.Context, db *DB, sql string, args ...any) (bool, error) {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if rows.Next() {
		return true, rows.Err()
	}
	return false, rows.Err()
}
