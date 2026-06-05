package pg

import "context"

// TB is the subset of *testing.T the helpers below need. Mirroring
// the testing.TB interface lets the helpers be called from anything
// that satisfies it (including testing.T and testing.B) without
// pulling testing into the import graph of production code.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Cleanup(fn func())
}

// TestTx runs fn inside a freshly-opened transaction that is
// unconditionally rolled back at the end of the test. The handle
// passed to fn is a *DB scoped to the transaction; every Exec / Query
// / Entity operation against it lives or dies with the rollback —
// the underlying schema and rows are untouched after the test
// returns.
//
//	pg.TestTx(t, db, ctx, func(tx *pg.DB) {
//	    if err := UserEntity.Create(tx, ctx, &u); err != nil {
//	        t.Fatalf("create: %v", err)
//	    }
//	    ...
//	})
//
// Use it to keep test cases hermetic without writing manual
// setup / teardown. Tests that need to verify post-commit
// behaviour (triggers that look at xact id, etc.) should open a
// transaction directly via db.Begin instead.
func TestTx(t TB, db *DB, ctx context.Context, fn func(tx *DB)) {
	t.Helper()
	txDB, drvTx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("drops/pg: TestTx begin: %v", err)
		return
	}
	rolledBack := false
	rollback := func() {
		if rolledBack {
			return
		}
		rolledBack = true
		if rerr := drvTx.Rollback(ctx); rerr != nil {
			t.Logf("drops/pg: TestTx rollback: %v", rerr)
		}
	}
	// Cleanup runs after the test (or sub-test) finishes, so we
	// always release the transaction even if fn panics or t.Fatal
	// is invoked further down the stack.
	t.Cleanup(rollback)
	defer rollback()
	fn(txDB)
}
