package pg

import "context"

// Existence checks via information_schema. These are convenience helpers
// for code that needs to branch on the live state of the database —
// migration callbacks, idempotent setup scripts, conditional fixtures.
//
// All checks default the schema to "public" when the schema argument is
// empty, matching PostgreSQL's default search_path.

// SchemaExists reports whether the named PostgreSQL schema exists.
func SchemaExists(ctx context.Context, db *DB, schema string) (bool, error) {
	rows, err := db.Query(ctx,
		`SELECT 1 FROM information_schema.schemata WHERE schema_name = $1 LIMIT 1`,
		schema)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// TableExists reports whether a table exists in the given schema.
// schema may be empty to mean "public".
func TableExists(ctx context.Context, db *DB, schema, table string) (bool, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := db.Query(ctx, `
		SELECT 1 FROM information_schema.tables
		WHERE table_schema = $1 AND table_name = $2 LIMIT 1`,
		schema, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// ColumnExists reports whether a column exists on a table.
// schema may be empty to mean "public".
func ColumnExists(ctx context.Context, db *DB, schema, table, column string) (bool, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := db.Query(ctx, `
		SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3 LIMIT 1`,
		schema, table, column)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}

// ConstraintExists reports whether a named constraint exists on a table.
// schema may be empty to mean "public".
func ConstraintExists(ctx context.Context, db *DB, schema, table, constraint string) (bool, error) {
	if schema == "" {
		schema = "public"
	}
	rows, err := db.Query(ctx, `
		SELECT 1 FROM information_schema.table_constraints
		WHERE table_schema = $1 AND table_name = $2 AND constraint_name = $3 LIMIT 1`,
		schema, table, constraint)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	return rows.Next(), nil
}
