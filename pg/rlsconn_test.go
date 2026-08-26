package pg_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// WHICH CONNECTION the identity is established on.
//
// [pg.DB.InTxAs] is worth nothing unless the SET LOCAL ROLE and the
// set_config reach the same connection as the work they authorise. SET
// LOCAL is transaction-local by definition: sent on the pool handle it
// lands on whatever connection the pool hands out for that one
// statement, is reverted at the end of that statement's own implicit
// transaction, and the body then runs on the transaction's connection
// as the pool's own user — the fail-open the method exists to refuse,
// with every statement in the query log looking exactly right.
//
// The unit suite could not see that. dropstest records a statement
// issued through a [dropstest.Tx] into the driver that opened it,
// deliberately — "an InTx path is one ordered stream of calls rather
// than two records to be correlated afterwards" — so the stream shows
// WHEN a statement was sent relative to the boundaries and not WHERE.
// Mutating establish to take the pool *DB instead of the transactional
// one therefore left every assertion in pg/rls_test.go passing: same
// statements, same order, same position between OpBegin and OpCommit.
// Only integration/pg_rls_test.go caught it, by asking a real server
// which role it was running as — and that file skips itself when no
// DSN is set, which is most of the time and all of CI.
//
// So this file keeps a double of its own, on the terms the dropstest
// package doc sets out for the doubles pg already keeps (listenDriver,
// copyingDriver, statsDriver): where the question under test is which
// handle a call went through, the recorder that answers it has to be
// the one that can tell them apart. connRecorder records the handle
// beside the statement; nothing else about it differs.

// connRecorder is a drops.Driver that records the handle each call was
// issued on: the pool itself, or one of the transactions opened from
// it, each named in the order it was opened.
type connRecorder struct {
	mu    sync.Mutex
	calls []connCall
	txs   int
}

// connCall is one recorded driver call. conn is "pool" or "tx1",
// "tx2", ... — the handle, not the statement, is what this double
// exists to report.
type connCall struct {
	conn string
	op   string
	sql  string
	args []any
}

func (c *connCall) String() string {
	if c.sql == "" {
		return c.conn + " " + c.op
	}
	if len(c.args) == 0 {
		return c.conn + " " + c.sql
	}
	return fmt.Sprintf("%s %s %v", c.conn, c.sql, c.args)
}

func (r *connRecorder) record(conn, op, sql string, args []any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, connCall{conn: conn, op: op, sql: sql, args: append([]any(nil), args...)})
}

// statements returns "<handle> <sql>" for every Exec and Query, in
// order. The boundaries are left out: they carry no SQL, and which
// handle a COMMIT went through is the transaction's own identity
// rather than a fact about the statements.
func (r *connRecorder) statements() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for i := range r.calls {
		if r.calls[i].op == "exec" || r.calls[i].op == "query" {
			out = append(out, r.calls[i].String())
		}
	}
	return out
}

func (r *connRecorder) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	r.record("pool", "exec", sql, args)
	return connResult{}, nil
}

func (r *connRecorder) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	r.record("pool", "query", sql, args)
	return connRows{}, nil
}

func (r *connRecorder) Begin(_ context.Context) (drops.Tx, error) {
	r.mu.Lock()
	r.txs++
	name := fmt.Sprintf("tx%d", r.txs)
	r.calls = append(r.calls, connCall{conn: "pool", op: "begin"})
	r.mu.Unlock()
	return &connTx{rec: r, name: name}, nil
}

// connTx is one transaction. Everything it is handed records under its
// own name, which is the whole point of the double.
type connTx struct {
	rec  *connRecorder
	name string
}

func (t *connTx) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	t.rec.record(t.name, "exec", sql, args)
	return connResult{}, nil
}

func (t *connTx) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	t.rec.record(t.name, "query", sql, args)
	return connRows{}, nil
}

func (t *connTx) Begin(ctx context.Context) (drops.Tx, error) { return t.rec.Begin(ctx) }
func (t *connTx) Commit(context.Context) error {
	t.rec.record(t.name, "commit", "", nil)
	return nil
}

func (t *connTx) Rollback(context.Context) error {
	t.rec.record(t.name, "rollback", "", nil)
	return nil
}

type connResult struct{}

func (connResult) RowsAffected() (int64, error) { return 1, nil }

type connRows struct{}

func (connRows) Next() bool                 { return false }
func (connRows) Scan(...any) error          { return nil }
func (connRows) Columns() ([]string, error) { return nil, nil }
func (connRows) Close() error               { return nil }
func (connRows) Err() error                 { return nil }

// TestInTxAsEstablishesTheIdentityOnTheTransactionsConnection is the
// assertion the recorded order could not make: every statement InTxAs
// sends for the session goes through the transaction's handle, and the
// pool's handle carries no statement at all.
//
// A body statement is issued too, because "the identity is on the same
// connection as the work" is the claim — the identity alone on the
// right connection would still be true of a body that ran somewhere
// else.
func TestInTxAsEstablishesTheIdentityOnTheTransactionsConnection(t *testing.T) {
	rec := &connRecorder{}
	db := pg.New(rec)
	sess := pg.Session{
		Role:     "app_tenant",
		Settings: map[string]string{"app.tenantId": "7"},
	}
	ctx := context.Background()
	if err := db.InTxAs(ctx, sess, func(tx *pg.DB) error {
		_, err := tx.Exec(ctx, `SELECT "id" FROM "notes"`)
		return err
	}); err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	want := []string{
		`tx1 SET LOCAL ROLE "app_tenant"`,
		`tx1 SELECT set_config($1, $2, true) [app.tenantId 7]`,
		`tx1 SELECT "id" FROM "notes"`,
	}
	got := rec.statements()
	if len(got) != len(want) {
		t.Fatalf("statements =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, s := range got {
		if strings.HasPrefix(s, "pool ") {
			t.Errorf("a statement went out on the pool handle: %s\n"+
				"SET LOCAL on a pooled connection is reverted with that statement's own implicit "+
				"transaction, so the body would run as the pool's user with the log still reading right", s)
		}
	}
}

// TestInTxAsIdentitiesDoNotShareAConnection is the same property under
// the shape that makes it matter: two sessions in flight at once. Each
// transaction is its own handle, so neither identity is established on
// the connection the other's body runs on — which is what "for exactly
// the lifetime of that transaction" has to mean when a pool is
// recycling connections between requests.
func TestInTxAsIdentitiesDoNotShareAConnection(t *testing.T) {
	rec := &connRecorder{}
	db := pg.New(rec)
	ctx := context.Background()

	outer := pg.Session{Settings: map[string]string{"app.tenantId": "7"}}
	inner := pg.Session{Settings: map[string]string{"app.tenantId": "9"}}
	if err := db.InTxAs(ctx, outer, func(tx *pg.DB) error {
		if _, err := tx.Exec(ctx, `SELECT 'outer'`); err != nil {
			return err
		}
		// A second transaction opened from the pool while the first is
		// still open: the double allows it, as a driver with real
		// nesting would, and the question is only whose connection the
		// second identity lands on.
		if err := db.InTxAs(ctx, inner, func(tx2 *pg.DB) error {
			_, err := tx2.Exec(ctx, `SELECT 'inner'`)
			return err
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	want := []string{
		`tx1 SELECT set_config($1, $2, true) [app.tenantId 7]`,
		`tx1 SELECT 'outer'`,
		`tx2 SELECT set_config($1, $2, true) [app.tenantId 9]`,
		`tx2 SELECT 'inner'`,
	}
	got := rec.statements()
	if len(got) != len(want) {
		t.Fatalf("statements =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("statement %d = %q, want %q", i, got[i], want[i])
		}
	}
}
