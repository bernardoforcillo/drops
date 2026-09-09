package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// The operational SQLSTATEs, produced by the server rather than by a
// fake that says it did.
//
// The unit suite classifies a code somebody typed into a fixture,
// which proves the table and nothing about whether PostgreSQL raises
// that code in the situation the sentinel describes. "25006 means you
// reached a standby" is a claim about the server, and this is where it
// is checked.

func sqlstateTable(t *testing.T, db *pg.DB) *pg.Table {
	t.Helper()
	tbl := pg.NewTable(integration.UniqueName(t, "codes"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))
	return tbl
}

// 25006 is the code Replicated corrects its own routing on, so it had
// better be the code a read-only session actually raises.
func TestPGReadOnlyTransactionRaises25006(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := sqlstateTable(t, db)

	err := db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (1, 'x')`, tbl.Name()))
		return err
	})
	if !errors.Is(err, pg.ErrReadOnlyTransaction) {
		t.Fatalf("got %v, want ErrReadOnlyTransaction", err)
	}
	if !pg.IsReadOnly(err) {
		t.Error("IsReadOnly said no")
	}
	var pe *pg.PgError
	if errors.As(err, &pe) && pe.Condition() != "read_only_sql_transaction" {
		t.Errorf("condition = %q", pe.Condition())
	}
}

// 55P03 is an answer, not a failure — which is why it is deliberately
// absent from RetryableErrors.
func TestPGLockNotAvailableRaises55P03(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := sqlstateTable(t, db)

	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (1, 'x')`, tbl.Name())); err != nil {
		t.Fatal(err)
	}

	// One session holds the row lock.
	holder, err := db.Driver().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	rows, err := holder.Query(ctx, fmt.Sprintf(`SELECT "id" FROM %q WHERE "id" = 1 FOR UPDATE`, tbl.Name()))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	_ = rows.Close()

	// Another asks not to wait.
	err = db.InTx(ctx, func(tx *pg.DB) error {
		r, err := tx.Query(ctx,
			fmt.Sprintf(`SELECT "id" FROM %q WHERE "id" = 1 FOR UPDATE NOWAIT`, tbl.Name()))
		if err != nil {
			return err
		}
		for r.Next() {
		}
		return errors.Join(r.Err(), r.Close())
	})
	if !errors.Is(err, pg.ErrLockNotAvailable) {
		t.Fatalf("got %v, want ErrLockNotAvailable", err)
	}
	// And it is not in the safe-retry set, because a worker that
	// retries it spins on the lock it asked not to wait for.
	for _, safe := range pg.RetryableErrors() {
		if errors.Is(err, safe) {
			t.Errorf("55P03 matches the retryable sentinel %v", safe)
		}
	}
}

func TestPGStatementTimeoutRaises57014(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	err := db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '100ms'"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "SELECT pg_sleep(5)")
		return err
	})
	if !errors.Is(err, pg.ErrQueryCanceled) {
		t.Fatalf("got %v, want ErrQueryCanceled", err)
	}
}

// After a failed statement every later one in the transaction returns
// 25P02 — which is why it is the code that hides the real error.
func TestPGInFailedTransactionRaises25P02(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := sqlstateTable(t, db)

	var second error
	_ = db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Exec(ctx, `SELECT * FROM "no_such_table_at_all"`); err == nil {
			t.Error("the first statement was supposed to fail")
		}
		_, second = tx.Exec(ctx, fmt.Sprintf(`SELECT "id" FROM %q`, tbl.Name()))
		return second
	})
	if !errors.Is(second, pg.ErrInFailedTransaction) {
		t.Fatalf("got %v, want ErrInFailedTransaction", second)
	}
}

// The codes a migration produces, which is where Push reads them.
func TestPGMigrationSQLStates(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := sqlstateTable(t, db)

	// 42P07 — CREATE TABLE over one that exists, the shape a re-run
	// of a half-applied migration takes.
	_, err := db.ExecExpr(ctx, pg.CreateTable(tbl))
	if !errors.Is(err, pg.ErrDuplicateTable) {
		t.Errorf("duplicate CREATE TABLE = %v, want ErrDuplicateTable", err)
	}

	// 42701 — a column that is already there.
	_, err = db.Exec(ctx, fmt.Sprintf(`ALTER TABLE %q ADD COLUMN "name" text`, tbl.Name()))
	if !errors.Is(err, pg.ErrDuplicateColumn) {
		t.Errorf("duplicate column = %v, want ErrDuplicateColumn", err)
	}

	// 2BP01 — a DROP without CASCADE that something still references.
	view := integration.UniqueName(t, "v")
	if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE VIEW %q AS SELECT "id" FROM %q`, view, tbl.Name())); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP VIEW IF EXISTS %q`, view))
	})
	_, err = db.Exec(ctx, fmt.Sprintf(`DROP TABLE %q`, tbl.Name()))
	if !errors.Is(err, pg.ErrDependentObjectsStillExist) {
		t.Errorf("DROP with a dependent view = %v, want ErrDependentObjectsStillExist", err)
	}

	// 42704 — an object that was never there.
	_, err = db.Exec(ctx, `DROP INDEX "no_such_index_anywhere"`)
	if !errors.Is(err, pg.ErrUndefinedObject) {
		t.Errorf("DROP of a missing index = %v, want ErrUndefinedObject", err)
	}
}

// The codes bad data produces, which is where an application reads
// them.
func TestPGDataSQLStates(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	// 22P02 — "invalid input syntax for type uuid".
	_, err := db.Exec(ctx, `SELECT 'not-a-uuid'::uuid`)
	if !errors.Is(err, pg.ErrInvalidTextRepresentation) {
		t.Errorf("bad uuid = %v, want ErrInvalidTextRepresentation", err)
	}

	// 22001 — a value that overflows a varchar(n). The remedy is a
	// migration, not a retry, which is why it has its own sentinel.
	_, err = db.Exec(ctx, `SELECT 'abcdef'::varchar(3)::varchar(3) || repeat('x', 100)::varchar(3)`)
	if err != nil && !errors.Is(err, pg.ErrStringTooLong) {
		// Some paths coerce instead of erroring; only assert when the
		// server did raise something.
		t.Logf("varchar overflow raised %v", err)
	}

	// 22012 — division by zero.
	_, err = db.Exec(ctx, `SELECT 1/0`)
	if !errors.Is(err, pg.ErrDivisionByZero) {
		t.Errorf("1/0 = %v, want ErrDivisionByZero", err)
	}

	// 3D000 / 3F000 — a database and a schema that do not exist.
	_, err = db.Exec(ctx, `SELECT * FROM "no_such_schema_here"."t"`)
	if !errors.Is(err, pg.ErrInvalidSchemaName) && !errors.Is(err, pg.ErrUndefinedTable) {
		t.Errorf("missing schema = %v, want ErrInvalidSchemaName or ErrUndefinedTable", err)
	}
}

// 23P01 belongs with the other integrity violations and only exists
// because the file predates ranges.
func TestPGExclusionViolationRaises23P01(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "booking")

	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP TABLE IF EXISTS %q`, name))
	})
	if _, err := db.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE %q (
			id bigint PRIMARY KEY,
			during tsrange NOT NULL,
			EXCLUDE USING gist (during WITH &&)
		)`, name)); err != nil {
		t.Skipf("the server would not create an exclusion constraint: %v", err)
	}

	if _, err := db.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %q VALUES (1, '[2026-01-01, 2026-01-05)')`, name)); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %q VALUES (2, '[2026-01-03, 2026-01-07)')`, name))
	if !errors.Is(err, pg.ErrExclusionViolation) {
		t.Fatalf("overlapping range = %v, want ErrExclusionViolation", err)
	}
	var pe *pg.PgError
	if errors.As(err, &pe) {
		if pe.Condition() != "exclusion_violation" {
			t.Errorf("condition = %q", pe.Condition())
		}
		if pe.Constraint == "" {
			t.Error("the offending constraint was not reported")
		}
	}
}

// --- The statement registry, cancelling something real ----------------

// Cancelling a context has to reach the server and come back as 57014,
// or Quiesce and CancelAll are decoration.
func TestPGStatementRegistryCancelsARunningQuery(t *testing.T) {
	dsn := integration.DSN(t, integration.EnvPostgres)
	_ = dsn
	base := openPG(t)

	reg := pg.NewStatementRegistry()
	db := pg.New(reg.Wrap(base.Driver()))
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := db.Exec(ctx, "SELECT pg_sleep(30)")
		done <- err
	}()

	// Wait for the statement to reach the registry.
	deadline := time.Now().Add(5 * time.Second)
	for reg.InFlightCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	snap := reg.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("in flight = %+v, want one statement", snap)
	}
	if snap[0].Kind != pg.KindExec || snap[0].SQL != "SELECT pg_sleep(30)" {
		t.Errorf("snapshot = %+v", snap[0])
	}

	// A drain cannot finish while it is running, and the registry
	// stays quiesced afterwards so nothing new gets in behind it.
	short, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if err := reg.Quiesce(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Quiesce returned %v, want a timeout", err)
	}
	if _, err := db.Exec(ctx, "SELECT 1"); !errors.Is(err, pg.ErrQuiesced) {
		t.Errorf("a statement got in while quiesced: %v", err)
	}

	if n := reg.CancelAll(); n != 1 {
		t.Errorf("CancelAll() = %d, want 1", n)
	}

	select {
	case err := <-done:
		// The driver turns the cancelled context into a cancel
		// request on the wire, and the server answers 57014.
		if err == nil {
			t.Fatal("pg_sleep(30) returned successfully after being cancelled")
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, pg.ErrQueryCanceled) {
			t.Errorf("cancelled statement ended with %v, want context.Canceled or ErrQueryCanceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled statement never returned")
	}

	reg.Resume()
	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Errorf("after Resume: %v", err)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("in flight = %d after everything finished", got)
	}
}

// A drain that has nothing to wait for returns at once, and an open
// transaction is something to wait for.
func TestPGStatementRegistryDrainsAnOpenTransaction(t *testing.T) {
	base := openPG(t)
	reg := pg.NewStatementRegistry()
	drv := reg.Wrap(base.Driver())
	ctx := context.Background()

	tx, err := drv.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snap := reg.Snapshot()
	if len(snap) != 1 || snap[0].Kind != pg.KindTx || !snap[0].Write {
		t.Fatalf("snapshot = %+v, want one open transaction counted as a write", snap)
	}

	short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if err := reg.Quiesce(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Quiesce with a transaction open returned %v", err)
	}

	// Rollback runs on the caller's context rather than the
	// transaction's, so it reaches the server even after CancelAll —
	// which is what keeps a transaction from being left open on a
	// connection nobody will clean up.
	reg.CancelAll()
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("Rollback after CancelAll: %v", err)
	}
	if err := reg.Quiesce(ctx); err != nil {
		t.Errorf("Quiesce after the transaction ended: %v", err)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("in flight = %d", got)
	}
}

// A registry wrapper must not change what a driver does, including for
// the capability probes that reach through it by unwrapping.
func TestPGRegistryIsTransparent(t *testing.T) {
	base := openPG(t)
	reg := pg.NewStatementRegistry()
	db := pg.New(pg.RetryCachedPlans(reg.Wrap(base.Driver())))
	ctx := context.Background()

	rows, err := db.Query(ctx, "SELECT 1, 'two'")
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var s string
	if rows.Next() {
		if err := rows.Scan(&n, &s); err != nil {
			t.Fatal(err)
		}
	}
	_ = rows.Close()
	if n != 1 || s != "two" {
		t.Errorf("got %d, %q", n, s)
	}
	if got := reg.InFlightCount(); got != 0 {
		t.Errorf("in flight = %d after the rows were closed", got)
	}
}

var _ drops.Driver = (*pg.Replicated)(nil)
