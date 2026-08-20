package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bernardoforcillo/drops/cmd/drops/pgwire"
	"github.com/bernardoforcillo/drops/pg"
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
func connect(ctx context.Context, flagValue string) (*pg.DB, func(), error) {
	dsn, err := resolveDSN(flagValue)
	if err != nil {
		// Not having been told which database to touch is a command
		// line that is wrong, not a database that is unreachable.
		return nil, nil, usageError{err}
	}
	conn, err := pgwire.Connect(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("%w\ncheck the connection string and that the server is reachable", err)
	}
	conn.Notice = func(e *pgwire.Error) {
		if pgwire.Boilerplate(e) {
			return
		}
		fmt.Fprintf(os.Stderr, "drops: server notice: %s\n", e.Message)
	}
	return pg.New(conn), func() { _ = conn.Close() }, nil
}
