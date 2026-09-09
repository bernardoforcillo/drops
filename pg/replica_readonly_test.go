package pg_test

import (
	"context"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// countingDriver answers every statement with err (nil for success)
// and records how many it saw.
type countingDriver struct {
	err     error
	queries int
	execs   int
}

func (d *countingDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	d.execs++
	return nil, d.err
}

func (d *countingDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	d.queries++
	return nil, d.err
}

func (d *countingDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, d.err }

func readOnlyErr() error {
	return &sqlStateOnlyError{
		code: "25006",
		msg:  "ERROR: cannot execute UPDATE in a read-only transaction",
	}
}

// The keyword scan cannot see every write. When it misses one, the
// standby says so with 25006 and the statement belongs on the primary.
func TestReplicatedRedirectsReadOnlyRefusalToPrimary(t *testing.T) {
	primary := &countingDriver{}
	replica := &countingDriver{err: readOnlyErr()}
	repl := pg.NewReplicated(primary, replica)
	db := pg.New(repl)

	// Reads as a SELECT to the keyword scan, so it routes to the
	// replica — which refuses it.
	if _, err := db.Query(context.Background(),
		`SELECT * FROM insert_audit_row($1)`, 7); err != nil {
		t.Fatalf("query failed after redirect: %v", err)
	}
	if replica.queries != 1 {
		t.Errorf("replica saw %d queries, want 1", replica.queries)
	}
	if primary.queries != 1 {
		t.Errorf("primary saw %d queries, want 1 (the redirect)", primary.queries)
	}
	if got := repl.ReadOnlyRedirects(); got != 1 {
		t.Errorf("ReadOnlyRedirects() = %d, want 1", got)
	}
}

// A primary that returns 25006 has been demoted. There is nowhere
// better to send the statement, so it must surface rather than loop.
func TestReplicatedDoesNotRedirectPrimaryRefusal(t *testing.T) {
	primary := &countingDriver{err: readOnlyErr()}
	repl := pg.NewReplicated(primary)
	db := pg.New(repl)

	if _, err := db.Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("expected the refusal to surface")
	}
	if primary.queries != 1 {
		t.Errorf("primary saw %d queries, want 1", primary.queries)
	}
	if got := repl.ReadOnlyRedirects(); got != 0 {
		t.Errorf("ReadOnlyRedirects() = %d, want 0", got)
	}
}

// Any other failure from a replica is the caller's answer, unchanged.
func TestReplicatedDoesNotRedirectOtherErrors(t *testing.T) {
	primary := &countingDriver{}
	replica := &countingDriver{err: &sqlStateOnlyError{code: "42P01", msg: "no such table"}}
	repl := pg.NewReplicated(primary, replica)
	db := pg.New(repl)

	if _, err := db.Query(context.Background(), "SELECT 1"); err == nil {
		t.Fatal("expected the failure to surface")
	}
	if primary.queries != 0 {
		t.Errorf("primary saw %d queries, want 0", primary.queries)
	}
	if got := repl.ReadOnlyRedirects(); got != 0 {
		t.Errorf("ReadOnlyRedirects() = %d, want 0", got)
	}
}
