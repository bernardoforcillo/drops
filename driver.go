package drops

import "context"

// Driver is the minimal contract a database connection must satisfy to be
// used with drops. Any pgx, database/sql or custom connection can be
// wrapped to implement it; drops itself imports no concrete driver.
type Driver interface {
	// Exec runs a statement that does not return rows.
	Exec(ctx context.Context, sql string, args ...any) (Result, error)
	// Query runs a statement that returns rows.
	Query(ctx context.Context, sql string, args ...any) (Rows, error)
	// Begin opens a transaction.
	Begin(ctx context.Context) (Tx, error)
}

// Tx is an in-flight transaction. It is itself a Driver so query builders
// work transparently inside or outside a transaction.
type Tx interface {
	Driver
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Result describes the outcome of a non-row-returning statement.
type Result interface {
	RowsAffected() (int64, error)
}

// Rows is a forward-only cursor over a result set.
type Rows interface {
	Next() bool
	Scan(dest ...any) error
	Columns() ([]string, error)
	Close() error
	Err() error
}
