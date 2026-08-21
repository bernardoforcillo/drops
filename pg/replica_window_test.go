package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// --- the window after a transactional write ---------------------------

// Most writes worth reading back go through InTx, and the window was
// armed only by Replicated.Exec — so the read that followed a committed
// transaction went straight to a replica that may not have replayed it.
func TestCommittedTransactionArmsTheWindow(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	err := db.InTx(ctx, func(tx *pg.DB) error {
		_, err := tx.Exec(ctx, "INSERT INTO t VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	rows, err := db.Query(ctx, "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	if n := countMatching(replica.queries, "SELECT"); n != 0 {
		t.Errorf("the read after a committed write went to a replica %d time(s); "+
			"the window was never armed", n)
	}
	if n := countMatching(primary.queries, "SELECT"); n != 1 {
		t.Errorf("primary served %d reads, want 1", n)
	}
}

// A rolled-back transaction wrote nothing, so there is nothing to read
// back and no reason to spend the primary's capacity on it.
func TestRolledBackTransactionDoesNotArmTheWindow(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	boom := errors.New("boom")
	if err := db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Exec(ctx, "INSERT INTO t VALUES (1)"); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("InTx: %v", err)
	}

	rows, err := db.Query(ctx, "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	if n := countMatching(replica.queries, "SELECT"); n != 1 {
		t.Errorf("replica served %d reads after a rollback, want 1", n)
	}
}

// A transaction that only read is not a write, whatever it was opened
// with.
func TestReadOnlyTransactionDoesNotArmTheWindow(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	if err := db.InTx(ctx, func(tx *pg.DB) error {
		rows, err := tx.Query(ctx, "SELECT 1")
		if err != nil {
			return err
		}
		return rows.Close()
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query(ctx, "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	if n := countMatching(replica.queries, "SELECT * FROM t"); n != 1 {
		t.Errorf("replica served %d reads after a read-only transaction, want 1", n)
	}
}

// --- the write delay --------------------------------------------------

// The point of the delay: LSN routing says the replica has replayed
// past the write and the delay overrules it anyway, which is the whole
// difference between "this row is there" and "everything that write set
// in motion is there".
func TestWriteDelayOverrulesACaughtUpReplica(t *testing.T) {
	primary := newLSNDriver("primary", "0/2000", "0/0")
	caughtUp := newLSNDriver("caught", "0/0", "0/9000")
	db := pg.New(pg.NewReplicated(primary, caughtUp).
		WithLSNTracking(10 * time.Millisecond).
		WithWriteDelay(250 * time.Millisecond))

	ctx := pg.WithReadYourWrites(context.Background(), 5*time.Second)
	if _, err := db.Exec(ctx, "INSERT ..."); err != nil {
		t.Fatal(err)
	}
	primary.reads.Store(0)
	caughtUp.reads.Store(0)

	readOnce(t, db, ctx)
	if caughtUp.reads.Load() != 0 {
		t.Errorf("a read inside the write delay reached the replica %d time(s)",
			caughtUp.reads.Load())
	}
	if primary.reads.Load() != 1 {
		t.Errorf("primary served %d reads inside the delay, want 1", primary.reads.Load())
	}

	// Past the delay, LSN routing takes over again — the delay is a
	// floor, not a replacement.
	time.Sleep(280 * time.Millisecond)
	readOnce(t, db, ctx)
	if caughtUp.reads.Load() == 0 {
		t.Error("after the delay the caught-up replica should be serving reads again")
	}
}

// A delay longer than the caller's window widens it: the session asked
// for one second of stickiness and the operator asked for two, and the
// safe reading of that disagreement is two.
func TestWriteDelayWidensAShorterWindow(t *testing.T) {
	primary := &taggedDriver{name: "primary"}
	replica := &taggedDriver{name: "r1"}
	db := pg.New(pg.NewReplicated(primary, replica).WithWriteDelay(400 * time.Millisecond))

	ctx := pg.WithReadYourWrites(context.Background(), 50*time.Millisecond)
	if _, err := db.Exec(ctx, "UPDATE ..."); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // past the caller's window
	readOnce(t, db, ctx)
	if replica.reads.Load() != 0 {
		t.Errorf("read escaped to a replica %d ms after the write, inside a 400ms delay",
			150)
	}

	time.Sleep(300 * time.Millisecond) // past the delay too
	readOnce(t, db, ctx)
	if replica.reads.Load() != 1 {
		t.Errorf("replica served %d reads once the delay expired, want 1", replica.reads.Load())
	}
}

// Zero is the documented way to clear the window, and a driver-level
// default must not undo a caller's explicit opt-out.
func TestWriteDelayDoesNotResurrectAClearedWindow(t *testing.T) {
	primary := &taggedDriver{name: "primary"}
	replica := &taggedDriver{name: "r1"}
	db := pg.New(pg.NewReplicated(primary, replica).WithWriteDelay(time.Second))

	ctx := pg.WithReadYourWrites(context.Background(), 0)
	if _, err := db.Exec(ctx, "UPDATE ..."); err != nil {
		t.Fatal(err)
	}
	readOnce(t, db, ctx)
	if replica.reads.Load() != 1 {
		t.Errorf("a cleared window still pinned the read to the primary")
	}
}

func TestWriteDelayIsReadable(t *testing.T) {
	r := pg.NewReplicated(&taggedDriver{}).WithWriteDelay(3 * time.Second)
	if r.WriteDelay() != 3*time.Second {
		t.Errorf("got %v", r.WriteDelay())
	}
	if pg.NewReplicated(&taggedDriver{}).WithWriteDelay(-1).WriteDelay() != 0 {
		t.Error("a negative delay should clamp to zero, not invert the comparison")
	}
}

// --- read-only enforcement -------------------------------------------

// roDriver is a driver that can open a read-only transaction itself —
// the ReadOnlyBeginner extension a database/sql or pgx adapter would
// implement.
type roDriver struct {
	*fakeDriver
	roBegins atomic.Int32
}

func (d *roDriver) BeginReadOnly(context.Context) (drops.Tx, error) {
	d.roBegins.Add(1)
	return &fakeTx{d.fakeDriver}, nil
}

// Without the extension, the guarantee still has to be real, and on
// PostgreSQL that means the statement the server understands.
func TestInReadTxIssuesSetTransactionReadOnly(t *testing.T) {
	drv := &fakeDriver{}
	db := pg.New(drv)

	err := db.InReadTx(context.Background(), func(rdb *pg.DB) error {
		rows, err := rdb.Query(context.Background(), "SELECT 1")
		if err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(drv.queries) == 0 || !strings.Contains(drv.queries[0], "SET TRANSACTION READ ONLY") {
		t.Fatalf("first statement of the transaction was %q, want SET TRANSACTION READ ONLY",
			firstOr(drv.queries, ""))
	}
}

// A driver that can ask the server directly should be asked directly,
// and not pay for a round trip drops only needs as a fallback.
func TestInReadTxPrefersTheDriverExtension(t *testing.T) {
	drv := &roDriver{fakeDriver: &fakeDriver{}}
	db := pg.New(drv)

	if err := db.InReadTx(context.Background(), func(rdb *pg.DB) error {
		rows, err := rdb.Query(context.Background(), "SELECT 1")
		if err != nil {
			return err
		}
		return rows.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if drv.roBegins.Load() != 1 {
		t.Errorf("BeginReadOnly called %d times, want 1", drv.roBegins.Load())
	}
	if n := countMatching(drv.queries, "SET TRANSACTION READ ONLY"); n != 0 {
		t.Errorf("issued the fallback statement %d times despite the extension", n)
	}
}

// A caller who asked for a transaction that cannot write, and silently
// got one that can, is worse off than one who got an error.
func TestInReadTxFailsRatherThanDegrading(t *testing.T) {
	drv := &fakeDriver{exec: func(sql string, _ []any) (drops.Result, error) {
		if strings.Contains(sql, "READ ONLY") {
			return nil, errors.New("this server has never heard of it")
		}
		return fakeResult{}, nil
	}}
	db := pg.New(drv)

	err := db.InReadTx(context.Background(), func(*pg.DB) error {
		t.Error("fn ran inside a transaction that is not read-only")
		return nil
	})
	if !errors.Is(err, pg.ErrNotReadOnly) {
		t.Fatalf("got %v, want ErrNotReadOnly", err)
	}
}

// The read-only transaction is a read, so it is routed like one.
func TestInReadTxRoutesToAReplica(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))

	if err := db.InReadTx(context.Background(), func(rdb *pg.DB) error {
		rows, err := rdb.Query(context.Background(), "SELECT * FROM t")
		if err != nil {
			return err
		}
		return rows.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if n := countMatching(replica.queries, "SELECT * FROM t"); n != 1 {
		t.Errorf("replica served %d reads, want 1 (queries: %v)", n, replica.queries)
	}
	if len(primary.queries) != 0 {
		t.Errorf("primary saw %v", primary.queries)
	}
}

// Inside the read-your-writes window a read belongs on the primary, and
// opening it as a transaction must not smuggle it onto a replica.
func TestInReadTxHonoursTheWindow(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	if _, err := db.Exec(ctx, "UPDATE t SET a = 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.InReadTx(ctx, func(rdb *pg.DB) error {
		rows, err := rdb.Query(ctx, "SELECT * FROM t")
		if err != nil {
			return err
		}
		return rows.Close()
	}); err != nil {
		t.Fatal(err)
	}
	if len(replica.queries) != 0 {
		t.Errorf("replica saw %v inside the write window", replica.queries)
	}
	if n := countMatching(primary.queries, "SELECT * FROM t"); n != 1 {
		t.Errorf("primary served %d reads, want 1", n)
	}
}

// --- writes that travel through Query ---------------------------------

// A write with a RETURNING clause is issued through Query, not Exec —
// InsertBuilder.Scan, UpdateBuilder.Scan and DeleteBuilder.Scan all do
// it, and it is how a generated key comes back. Routing that by where a
// read belongs sends an INSERT to a replica.
func TestReturningWriteRoutesToPrimary(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))

	rows, err := db.Query(context.Background(),
		"INSERT INTO t (a) VALUES ($1) RETURNING id", 1)
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	if n := countMatching(replica.queries, "INSERT"); n != 0 {
		t.Errorf("an INSERT was sent to a replica %d time(s): %v", n, replica.queries)
	}
	if n := countMatching(primary.queries, "INSERT"); n != 1 {
		t.Errorf("primary saw %d INSERTs, want 1", n)
	}
}

// And it is a write for the window's purposes as much as for routing's.
func TestReturningWriteArmsTheWindow(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	rows, err := db.Query(ctx, "UPDATE t SET a = 1 RETURNING id")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	readOnce(t, db, ctx)
	if n := countMatching(replica.queries, "SELECT 1"); n != 0 {
		t.Errorf("the read after an UPDATE ... RETURNING went to a replica %d time(s)", n)
	}
}

// The same shape inside a transaction: the commit has to arm the window
// even though nothing went through Tx.Exec.
func TestCommittedReturningWriteArmsTheWindow(t *testing.T) {
	primary := &fakeDriver{}
	replica := &fakeDriver{}
	db := pg.New(pg.NewReplicated(primary, replica))
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	err := db.InTx(ctx, func(tx *pg.DB) error {
		rows, err := tx.Query(ctx, "INSERT INTO t (a) VALUES ($1) RETURNING id", 1)
		if err != nil {
			return err
		}
		return rows.Close()
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	rows, err := db.Query(ctx, "SELECT * FROM t")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()

	if n := countMatching(replica.queries, "SELECT * FROM t"); n != 0 {
		t.Errorf("the read after a committed INSERT ... RETURNING went to a replica %d time(s)", n)
	}
	if n := countMatching(primary.queries, "SELECT * FROM t"); n != 1 {
		t.Errorf("primary served %d reads, want 1", n)
	}
}

// The classifier decides which statements pay the primary's capacity,
// so the cases it has to get right are worth pinning down: a column
// named updated_at is not an UPDATE, a data-modifying CTE is, and a
// leading comment must not hide the keyword.
func TestQueryWriteClassification(t *testing.T) {
	writes := []string{
		"INSERT INTO t VALUES (1) RETURNING id",
		"  update t set a = 1 returning id",
		"DELETE FROM t WHERE id = $1 RETURNING *",
		"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN UPDATE SET a = 1",
		"WITH moved AS (DELETE FROM t RETURNING *) SELECT * FROM moved",
		"-- a leading comment\nINSERT INTO t VALUES (1) RETURNING id",
		"/* tag */ INSERT INTO t VALUES (1) RETURNING id",
		"EXPLAIN ANALYZE INSERT INTO t VALUES (1)",
	}
	reads := []string{
		"SELECT * FROM t",
		"select updated_at from t",
		"WITH recent AS (SELECT * FROM t) SELECT * FROM recent",
		"VALUES (1), (2)",
		"SHOW transaction_isolation",
		"EXPLAIN SELECT * FROM t",
		"SELECT * FROM t WHERE note = 'no update here' -- trailing",
	}
	for _, sql := range writes {
		primary, replica := &fakeDriver{}, &fakeDriver{}
		db := pg.New(pg.NewReplicated(primary, replica))
		rows, err := db.Query(context.Background(), sql)
		if err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if len(replica.queries) != 0 {
			t.Errorf("%q went to a replica", sql)
		}
	}
	for _, sql := range reads {
		primary, replica := &fakeDriver{}, &fakeDriver{}
		db := pg.New(pg.NewReplicated(primary, replica))
		rows, err := db.Query(context.Background(), sql)
		if err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
		if len(replica.queries) != 1 {
			t.Errorf("%q was kept off the replicas; primary saw %v", sql, primary.queries)
		}
	}
}

// --- helpers ----------------------------------------------------------

func readOnce(t *testing.T, db *pg.DB, ctx context.Context) {
	t.Helper()
	rows, err := db.Query(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if rows != nil {
		_ = rows.Close()
	}
}

func countMatching(all []string, substr string) int {
	n := 0
	for _, s := range all {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func firstOr(all []string, fallback string) string {
	if len(all) == 0 {
		return fallback
	}
	return all[0]
}
