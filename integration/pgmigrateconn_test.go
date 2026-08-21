package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
)

// openPGCapped is openPG with the pool capped, so "how many connections
// does this path hold at once" becomes a thing a test can assert.
//
// A path that wants more than the cap does not fail — it waits on the
// pool, forever, which is exactly the shape of the deadlock the CLI hit
// when its cap was one. The ctx deadline every caller here passes turns
// that wait into an error, so the assertion is "finished" rather than
// "did not hang", and a regression costs a second instead of a test
// binary timeout.
func openPGCapped(t *testing.T, max int) *pg.DB {
	t.Helper()
	return openCappedDSN(t, integration.DSN(t, integration.EnvPostgres), max)
}

// openCappedDSN is the same, against a database the caller names.
func openCappedDSN(t *testing.T, dsn string, max int) *pg.DB {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB.SetMaxOpenConns(max)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return pg.New(stdlib.New(sqlDB))
}

// cappedCtx bounds a run so a path that wants a connection it cannot
// get reports it instead of blocking the suite.
func cappedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// waitedForAConnection reports whether err is the deadline this test's
// context imposes, which is how a path that wanted a third connection
// surfaces.
func waitedForAConnection(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// Two connections, and the second one is not slack: the migration lock
// pins one for the whole run and the migrations need another. Every
// path through a migration has to fit in that — a clean run, a failed
// one, a rollback, the drizzle migrator, and Status, which takes no
// lock at all.
//
// This is the rule cmd/drops/connect.go caps the CLI pool by. It is
// stated there and nowhere enforced, and the last time it was broken —
// a lock added on one side, a cap of one on the other — the integration
// suite hung until the test binary timed out.
func TestPGMigrationPathsFitInTwoConnections(t *testing.T) {
	db := openPGCapped(t, 2)
	ctx := cappedCtx(t)

	history := integration.UniqueName(t, "hist")
	target := integration.UniqueName(t, "target")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, target))
		_, _ = db.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, history))
	})

	m := pg.NewMigrator(db).WithTable(history).
		AddSQL("0001", "create",
			fmt.Sprintf(`CREATE TABLE %q (id bigint)`, target),
			fmt.Sprintf(`DROP TABLE %q`, target))

	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up under a two-connection pool: %v", err)
	}
	if _, err := m.Status(ctx); err != nil {
		t.Fatalf("Status under a two-connection pool: %v", err)
	}
	if err := m.Down(ctx); err != nil {
		t.Fatalf("Down under a two-connection pool: %v", err)
	}

	// A migration that fails takes its transaction down with it, and
	// the lock holder still has to be released on the way out.
	bad := pg.NewMigrator(db).WithTable(history).
		AddSQL("0002", "broken", `SELECT this_function_does_not_exist()`, "")
	err := bad.Up(ctx)
	if err == nil {
		t.Fatal("a migration calling a missing function should fail")
	}
	if waitedForAConnection(err) {
		t.Fatalf("a failing migration wanted a third connection: %v", err)
	}
	if !strings.Contains(err.Error(), "applying 0002_broken") {
		t.Errorf("the failure should name the migration: %v", err)
	}
	// The lock is free again, which is only true if the holder's
	// transaction was rolled back on the failure path.
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up after a failed run: %v", err)
	}
}

// The drizzle migrator takes the same lock through the same helper, so
// it owes the same budget.
func TestPGDrizzleMigrationFitsInTwoConnections(t *testing.T) {
	db := openPGCapped(t, 2)
	ctx := cappedCtx(t)

	schema := integration.UniqueName(t, "dzconn")
	target := integration.UniqueName(t, "dzconntgt")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.Exec(bg, fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema))
		_, _ = db.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, target))
	})

	fsys := fstest.MapFS{
		"meta/_journal.json": &fstest.MapFile{Data: []byte(
			`{"version":"7","dialect":"postgresql","entries":[{"idx":0,"version":"7","when":1,"tag":"0000_init","breakpoints":true}]}`)},
		"0000_init.sql": &fstest.MapFile{Data: []byte(
			fmt.Sprintf("CREATE TABLE %q (id bigint);", target))},
	}
	d := pg.NewDrizzleMigrator(db, fsys, ".").WithSchema(schema)
	if err := d.Up(ctx); err != nil {
		t.Fatalf("drizzle Up under a two-connection pool: %v", err)
	}
	if _, err := d.Status(ctx); err != nil {
		t.Fatalf("drizzle Status under a two-connection pool: %v", err)
	}
}

// Push takes no lock, so one connection is its whole budget — it
// introspects, probes in a transaction and applies in another, each
// after the last. Pinning that keeps a future lock around Push from
// being added without the cap that pays for it.
func TestPGPushFitsInOneConnection(t *testing.T) {
	// A database of its own. Push's previous side is everything it
	// introspects, so against the suite's shared database this push
	// would propose to drop every table another test left behind —
	// and, since Push learned to ask about renames, stop to ask
	// whether one of those is this test's table arriving under a new
	// name. The connection budget is what is being pinned here, so the
	// question is kept out of the way rather than answered.
	db := openCappedDSN(t, freshDatabase(t), 1)
	ctx := cappedCtx(t)

	tbl := pg.NewTable(integration.UniqueName(t, "pushconn"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	// A CHECK is what makes the second push run the expression probe,
	// which opens a transaction of its own after the introspection.
	tbl.AddCheck("name_not_blank", `"name" <> ''`)

	schema := pg.NewSchema(tbl)
	if _, err := pg.Push(ctx, db, schema); err != nil {
		t.Fatalf("Push under a one-connection pool: %v", err)
	}
	// Second push is a no-op, but it still runs the expression probe,
	// which is the part that opens a transaction of its own.
	res, err := pg.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("idempotent Push under a one-connection pool: %v", err)
	}
	if res.Applied {
		t.Errorf("the second push applied %v", res.Statements)
	}
}

// The other half of the rule, stated as a test rather than as a
// comment: one connection is not enough for a locked run, because the
// holder never gives its connection back until the run is over.
// [pg.Migrator.WithoutLock] is the way out, and it has to keep working.
func TestPGMigrationUnderOneConnectionNeedsWithoutLock(t *testing.T) {
	db := openPGCapped(t, 1)

	history := integration.UniqueName(t, "hist1")
	target := integration.UniqueName(t, "target1")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, target))
		_, _ = db.Exec(bg, fmt.Sprintf(`DROP TABLE IF EXISTS %q`, history))
	})
	migrator := func() *pg.Migrator {
		return pg.NewMigrator(db).WithTable(history).
			AddSQL("0001", "create", fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %q (id bigint)`, target), "")
	}

	locked, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := migrator().Up(locked); !waitedForAConnection(err) {
		t.Fatalf("a locked run under a one-connection pool should wait for the pool, got %v", err)
	}

	if err := migrator().WithoutLock().Up(cappedCtx(t)); err != nil {
		t.Fatalf("WithoutLock exists for exactly this pool and did not work: %v", err)
	}
}
