package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
)

// What one server can and cannot prove about replica routing.
//
// These tests point two connection pools at the same PostgreSQL and
// call one of them the primary and the other the replica. That is
// enough for the thing under test — where a statement is sent — because
// each pool announces itself with a different application_name, and the
// server will tell you which one it is talking to. Every routing
// assertion below is the server's own answer to
// current_setting('application_name'), not a counter drops kept.
//
// It proves nothing about replication. There is no lag here, so it
// cannot show that the read-your-writes window rescues a read from a
// replica that is behind; that is what the window is for, and it is
// covered by the unit tests in pg, which can put a replica arbitrarily
// far behind. Nor can it exercise LSN routing at all: a primary has no
// pg_last_wal_replay_lsn — the function returns NULL — so no replica
// here is ever "caught up" in the sense pg.WithLSNTracking means, and
// the routing falls back to the primary. That interaction is likewise a
// unit test.
//
// What it does prove, on a real server, is the routing decision itself,
// the post-write window and delay that drive it, and — the one thing no
// fake can stand in for — that a read-routed transaction is refused
// permission to write by PostgreSQL rather than by drops.

const (
	appPrimary = "drops_primary"
	appReplica = "drops_replica"
)

// openTaggedPG opens a pool that identifies itself to the server, so a
// query can report which pool served it.
func openTaggedPG(t *testing.T, appName string) drops.Driver {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvPostgres)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	sqlDB, err := sql.Open("pgx", dsn+sep+"application_name="+appName)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return stdlib.New(sqlDB)
}

// servedBy asks the server which pool this read landed on.
func servedBy(t *testing.T, db *pg.DB, ctx context.Context) string {
	t.Helper()
	rows, err := db.Query(ctx, "SELECT current_setting('application_name')")
	if err != nil {
		t.Fatalf("routing probe: %v", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("routing probe returned no row")
	}
	var name string
	if err := rows.Scan(&name); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return name
}

func replicatedPG(t *testing.T) (*pg.Replicated, *pg.DB) {
	t.Helper()
	repl := pg.NewReplicated(openTaggedPG(t, appPrimary), openTaggedPG(t, appReplica))
	return repl, pg.New(repl)
}

// The baseline the rest of the file is measured against: with no window
// in play, the server confirms reads land on the replica pool and
// writes on the primary.
func TestPGReplicaRoutingIsVisibleToTheServer(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := context.Background()

	if got := servedBy(t, db, ctx); got != appReplica {
		t.Errorf("read served by %q, want %q", got, appReplica)
	}
	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
}

// The window, live: a write arms it and the following read is served by
// the primary; once it lapses the reads go back to the replica.
func TestPGReadYourWritesWindowRoutesToPrimary(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := pg.WithReadYourWrites(context.Background(), 400*time.Millisecond)

	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if got := servedBy(t, db, ctx); got != appPrimary {
		t.Errorf("read inside the window served by %q, want %q", got, appPrimary)
	}

	time.Sleep(450 * time.Millisecond)
	if got := servedBy(t, db, ctx); got != appReplica {
		t.Errorf("read after the window served by %q, want %q", got, appReplica)
	}
}

// The gap the window had: a write committed through InTx left it
// unarmed, so the read that followed went to a replica that, on a real
// pair of servers, may not have replayed the commit yet.
func TestPGCommittedTransactionArmsTheWindow(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)
	tbl := integration.UniqueName(t, "rywtx")

	if _, err := db.Exec(context.Background(),
		"CREATE TABLE IF NOT EXISTS "+tbl+" (id int primary key)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS "+tbl)
	})

	err := db.InTx(ctx, func(tx *pg.DB) error {
		_, err := tx.Exec(ctx, "INSERT INTO "+tbl+" (id) VALUES (1)")
		return err
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	if got := servedBy(t, db, ctx); got != appPrimary {
		t.Errorf("the read after a committed write was served by %q, want %q — "+
			"a commit is a write and has to arm the window", got, appPrimary)
	}
}

// A write with a RETURNING clause is issued through Query — that is how
// InsertBuilder.Scan and friends get the generated key back — and Query
// is the method that routes to a replica. Here the server itself says
// which pool the INSERT ran on, in the RETURNING clause of the INSERT.
func TestPGReturningWriteRunsOnThePrimary(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)
	tbl := integration.UniqueName(t, "rywret")

	if _, err := db.Exec(context.Background(),
		"CREATE TABLE IF NOT EXISTS "+tbl+" (id serial primary key)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS "+tbl)
	})

	rows, err := db.Query(ctx, "INSERT INTO "+tbl+
		" DEFAULT VALUES RETURNING id, current_setting('application_name')")
	if err != nil {
		t.Fatalf("insert ... returning: %v", err)
	}
	var id int64
	var served string
	if !rows.Next() {
		_ = rows.Close()
		t.Fatal("RETURNING produced no row")
	}
	if err := rows.Scan(&id, &served); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	_ = rows.Close()

	if served != appPrimary {
		t.Errorf("the INSERT ran on %q, want %q — a write routed to a replica is "+
			"a write in the wrong database", served, appPrimary)
	}
	// And it counts as a write for the window, not just for routing.
	if got := servedBy(t, db, ctx); got != appPrimary {
		t.Errorf("the read after an INSERT ... RETURNING was served by %q, want %q",
			got, appPrimary)
	}
}

// A rolled-back transaction wrote nothing, and pinning a session to the
// primary for a second because of it is capacity spent on nothing.
func TestPGRolledBackTransactionLeavesTheWindowUnarmed(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	boom := errors.New("rollback please")
	if err := db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("InTx: %v", err)
	}

	if got := servedBy(t, db, ctx); got != appReplica {
		t.Errorf("read after a rollback served by %q, want %q", got, appReplica)
	}
}

// The post-write delay, live: it widens a window the caller made too
// short, and the server confirms where each read landed.
func TestPGWriteDelayWidensTheWindow(t *testing.T) {
	repl, db := replicatedPG(t)
	repl.WithWriteDelay(700 * time.Millisecond)
	ctx := pg.WithReadYourWrites(context.Background(), 50*time.Millisecond)

	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond) // past the caller's 50ms window
	if got := servedBy(t, db, ctx); got != appPrimary {
		t.Errorf("read 250ms after the write served by %q, want %q — "+
			"the 700ms delay should still hold it on the primary", got, appPrimary)
	}

	time.Sleep(550 * time.Millisecond) // past the delay too
	if got := servedBy(t, db, ctx); got != appReplica {
		t.Errorf("read after the delay served by %q, want %q", got, appReplica)
	}
}

// The enforcement, and the only part of this file a fake could not
// stand in for: PostgreSQL, not drops, refuses the write.
func TestPGReadOnlyTransactionRefusesWrites(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := context.Background()
	tbl := integration.UniqueName(t, "readonly")

	if _, err := db.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+tbl+" (id int primary key)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = db.Exec(ctx, "DROP TABLE IF EXISTS "+tbl) })

	err := db.InReadTx(ctx, func(rdb *pg.DB) error {
		_, err := rdb.Exec(ctx, "INSERT INTO "+tbl+" (id) VALUES (1)")
		return err
	})
	if err == nil {
		t.Fatal("the INSERT was accepted inside a read-only transaction")
	}
	var pgErr *pg.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("got %v, want SQLSTATE 25006 (read_only_sql_transaction)", err)
	}

	// And the write really is absent — enforcement, not just an error
	// on the way back.
	rows, err := db.Query(ctx, "SELECT count(*) FROM "+tbl)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatal("count returned no row")
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d row(s) landed despite the read-only transaction", n)
	}
}

// Refusing writes must not cost the transaction its reads, and the
// reads have to go where a read goes.
func TestPGReadOnlyTransactionStillReadsAndRoutesToTheReplica(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := context.Background()

	var served string
	err := db.InReadTx(ctx, func(rdb *pg.DB) error {
		served = servedBy(t, rdb, ctx)
		return nil
	})
	if err != nil {
		t.Fatalf("InReadTx: %v", err)
	}
	if served != appReplica {
		t.Errorf("the read-only transaction ran on %q, want %q", served, appReplica)
	}
}

// Inside the write window a read belongs on the primary, and wrapping
// it in a read-only transaction must not route around that.
func TestPGReadOnlyTransactionHonoursTheWindow(t *testing.T) {
	_, db := replicatedPG(t)
	ctx := pg.WithReadYourWrites(context.Background(), time.Second)

	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	var served string
	if err := db.InReadTx(ctx, func(rdb *pg.DB) error {
		served = servedBy(t, rdb, ctx)
		return nil
	}); err != nil {
		t.Fatalf("InReadTx: %v", err)
	}
	if served != appPrimary {
		t.Errorf("the read-only transaction ran on %q inside the window, want %q",
			served, appPrimary)
	}
}

// A DB with no replicas at all still gets the guarantee — the
// enforcement is about what the transaction may do, not about which
// server it is on.
func TestPGReadOnlyTransactionOnAPlainDB(t *testing.T) {
	db := pg.New(openTaggedPG(t, appPrimary))
	ctx := context.Background()

	tbl := "should_not_exist_" + integration.UniqueName(t, "x")
	// The table only comes into existence if the guarantee is broken,
	// which is exactly when it must not be left behind: a leaked table
	// makes the next run fail with 42P07 instead of the 25006 this test
	// is about, and the second failure hides the first.
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS "+tbl) })

	err := db.InReadTx(ctx, func(rdb *pg.DB) error {
		_, err := rdb.Exec(ctx, "CREATE TABLE "+tbl+" (id int)")
		return err
	})
	var pgErr *pg.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
		t.Fatalf("got %v, want SQLSTATE 25006", err)
	}
}
