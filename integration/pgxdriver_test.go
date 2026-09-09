package integration_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/pgxdriver"
)

// drops/pgxdriver exists for the four things database/sql cannot
// express, and every other test in this file reaches PostgreSQL through
// database/sql. So these are the ones that would notice if the module
// stopped delivering them — and they are the ones that could not have
// been written before it existed, because CopyFrom and Subscribe
// answered "the driver does not support this" for every caller.

func openPGX(t *testing.T) *pg.DB {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvPostgres)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return pg.New(pgxdriver.New(pool))
}

type copyRow struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

// COPY FROM STDIN is the bulk path drops documents and that no caller
// could reach: database/sql has no COPY, so pg.CopyFrom answered
// ErrCopyNotSupported behind drops/stdlib.
//
// It is also the way out the parameter-limit refusal points at — a
// batch too large for a statement's parameters is not too large for a
// COPY, because the rows are not parameters — so a refusal naming a
// path nobody could take would have been no answer at all.
func TestPGXCopyFromActuallyCopies(t *testing.T) {
	db := openPGX(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "copyrows"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	if _, err := db.ExecExpr(ctx, pg.CreateTable(tbl)); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.ExecExpr(ctx, pg.DropTableIfExists(tbl)) })

	ent := pg.NewEntity[copyRow](tbl)
	if !pg.SupportsCopy(db) {
		t.Fatal("the driver is not a Copier, so this module is not doing the one thing it is for")
	}

	rows := make([]copyRow, 5000)
	for i := range rows {
		rows[i] = copyRow{ID: int64(i + 1), Name: "row"}
	}
	n, err := pg.CopyFrom(db, ctx, ent, rows)
	if err != nil {
		t.Fatalf("CopyFrom: %v", err)
	}
	if n != int64(len(rows)) {
		t.Errorf("copied = %d, want %d", n, len(rows))
	}
	got, err := db.Select().From(tbl).Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != int64(len(rows)) {
		t.Errorf("the table holds %d rows, want %d", got, len(rows))
	}
}

// A COPY of more rows than a statement could carry parameters for. The
// same batch through CreateMany is refused by ErrTooManyParameters, and
// the refusal says to use this — so the two have to agree.
func TestPGXCopyPassesWhereTheParameterLimitRefuses(t *testing.T) {
	db := openPGX(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "copybig"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	if _, err := db.ExecExpr(ctx, pg.CreateTable(tbl)); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.ExecExpr(ctx, pg.DropTableIfExists(tbl)) })

	ent := pg.NewEntity[copyRow](tbl)
	// Two columns, so 40000 rows is 80000 parameters — over the 65535
	// the protocol can express.
	rows := make([]copyRow, 40000)
	for i := range rows {
		rows[i] = copyRow{ID: int64(i + 1), Name: "row"}
	}

	if _, err := ent.CreateMany(db, ctx, rows); !errors.Is(err, pg.ErrTooManyParameters) {
		t.Fatalf("CreateMany: %v, want ErrTooManyParameters", err)
	}
	n, err := pg.CopyFrom(db, ctx, ent, rows)
	if err != nil {
		t.Fatalf("CopyFrom of a batch the statement path refuses: %v", err)
	}
	if n != int64(len(rows)) {
		t.Errorf("copied = %d, want %d", n, len(rows))
	}
}

// LISTEN needs a connection the server writes to unprompted, which is
// the one thing a pool of interchangeable connections will not give
// you — so behind drops/stdlib the change feed had nothing to run on.
func TestPGXListenDeliversANotification(t *testing.T) {
	db := openPGX(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if !pg.SupportsListen(db) {
		t.Fatal("the driver is not a Listener, so pg.Subscribe has nothing to run on")
	}
	l, ok := db.Driver().(pg.Listener)
	if !ok {
		t.Fatal("not a Listener")
	}
	channel := integration.UniqueName(t, "chan")
	ch, err := l.Listen(ctx, channel)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	// NOTIFY on a second connection out of the same pool: the listener
	// holds one of its own, which is the property under test.
	if _, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", channel, "hello"); err != nil {
		t.Fatalf("pg_notify: %v", err)
	}
	select {
	case n := <-ch:
		if n.Channel != channel || n.Payload != "hello" {
			t.Errorf("notification = %+v, want %s/hello", n, channel)
		}
	case <-ctx.Done():
		t.Fatal("no notification arrived; the listener is not listening")
	}
}

// Cancelling the ctx is how a caller stops listening, and the channel
// closing is how the consumer is told. A listener that leaked its
// connection instead would take one out of the pool for good.
func TestPGXListenStopsWhenTheContextIsCancelled(t *testing.T) {
	db := openPGX(t)
	l, ok := db.Driver().(pg.Listener)
	if !ok {
		t.Fatal("not a Listener")
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := l.Listen(ctx, integration.UniqueName(t, "chan"))
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cancel()
	select {
	case _, open := <-ch:
		if open {
			t.Error("a notification arrived after the ctx was cancelled")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the channel never closed, so the connection was never released")
	}
}

// The pool stats are the numbers pg.StartPoolMetrics samples, and
// behind drops/stdlib they were database/sql's rather than the pool
// the statements actually go through.
func TestPGXReportsThePoolItActuallyUses(t *testing.T) {
	db := openPGX(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("exec: %v", err)
	}
	p, ok := db.Driver().(pg.PoolStatsProvider)
	if !ok {
		t.Fatal("not a PoolStatsProvider")
	}
	s := p.Stats()
	if s.MaxOpenConnections <= 0 {
		t.Errorf("MaxOpenConnections = %d", s.MaxOpenConnections)
	}
	if s.OpenConnections <= 0 {
		t.Errorf("OpenConnections = %d after a statement, want at least one", s.OpenConnections)
	}
}

// A transaction, and a nested one — which pgx implements as a SAVEPOINT
// and database/sql refuses outright. drops documents nesting as
// driver-dependent; this is the driver where it works.
func TestPGXNestsATransactionAsASavepoint(t *testing.T) {
	db := openPGX(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "savepoint"))
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	if _, err := db.ExecExpr(ctx, pg.CreateTable(tbl)); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.ExecExpr(ctx, pg.DropTableIfExists(tbl)) })

	err := db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Insert(tbl).Row(id.Val(1), name.Val("outer")).Exec(ctx); err != nil {
			return err
		}
		// The inner work is rolled back and the outer survives, which
		// is what a savepoint is for.
		inner := tx.InTx(ctx, func(itx *pg.DB) error {
			if _, err := itx.Insert(tbl).Row(id.Val(2), name.Val("inner")).Exec(ctx); err != nil {
				return err
			}
			return errors.New("roll the inner one back")
		})
		if inner == nil {
			return errors.New("the inner transaction did not report its error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	n, err := db.Select().From(tbl).Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1: the outer insert survives and the inner one does not", n)
	}
}
