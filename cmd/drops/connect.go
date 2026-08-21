package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	pgxstdlib "github.com/jackc/pgx/v5/stdlib"

	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
)

// resolveDSN returns the connection string to use, preferring the flag
// and falling back to the environment variables a PostgreSQL user
// already has set.
func resolveDSN(flagValue string) (string, error) {
	for _, candidate := range []string{
		flagValue,
		os.Getenv("DROPS_PG_DSN"),
		os.Getenv("DATABASE_URL"),
	} {
		if candidate != "" {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no database to connect to: pass --dsn, or set DROPS_PG_DSN or DATABASE_URL")
}

// connect opens a connection and returns it wrapped as a *pg.DB along
// with the closer the caller must run.
//
// Server notices go to stderr. A migration that says "relation does
// not exist, skipping" has told you something about your migration,
// and a CLI that swallows it has not.
//
// The driver is pgx behind drops/stdlib, with nothing in between.
// Nothing has to translate its errors either: drops/pg reads the
// SQLSTATE from pgconn's SQLState method and the constraint name from
// *pgconn.PgError's ConstraintName field, so a violation raised here
// reaches the operator naming the constraint it broke, the way the
// wire client's errors did.
func connect(ctx context.Context, flagValue string) (*pg.DB, func(), error) {
	dsn, err := resolveDSN(flagValue)
	if err != nil {
		// Not having been told which database to touch is a command
		// line that is wrong, not a database that is unreachable.
		return nil, nil, usageError{err}
	}
	sqlDB, err := openPostgres(ctx, dsn)
	if err != nil {
		return nil, nil, err
	}
	return pg.New(stdlib.New(sqlDB)), func() { _ = sqlDB.Close() }, nil
}

// openPostgres dials the server and hands back the pool, or an error
// naming what to check.
//
// database/sql opens lazily, so the connection is made here rather
// than at the first statement: "the server is unreachable" belongs to
// the command that was asked to reach it, not to the middle of a
// migration.
func openPostgres(ctx context.Context, dsn string) (*sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w\ncheck the connection string and that the server is reachable", err)
	}
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if boilerplateNotice(n) {
			return
		}
		fmt.Fprintf(os.Stderr, "drops: server notice: %s\n", n.Message)
	}
	db := pgxstdlib.OpenDB(*cfg)
	// Two, and the second one is not slack.
	//
	// A migration run holds the advisory lock in a transaction of its
	// own for the whole run — see pg.withMigrationLock, which explains
	// why the lock can be neither session-scoped nor folded into a
	// migration's own transaction. That transaction pins one
	// connection and does not release it until the last migration has
	// applied, so the migrations themselves need a second. Capped at
	// one, the run deadlocks: the lock holder waits for the migrations
	// and the migrations wait for a connection.
	//
	// Statement affinity, which is what a cap of one was reaching for,
	// comes from Begin pinning a connection for its transaction rather
	// than from the size of the pool. So the rule is the number of
	// connections held at once, and anything that grows that number
	// has to grow this.
	db.SetMaxOpenConns(2)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w\ncheck the connection string and that the server is reachable", err)
	}
	return db, nil
}
