package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// The round trip the whole migration story rests on: declare a schema,
// create it, read the live schema back, and diff. A non-empty diff
// means drops would generate a migration for a database that already
// matches — which in a deploy loop is an ALTER on every run.
//
// Neither introspection nor diffing can be checked without a server:
// one reads the engine's catalogue, the other compares against it.
func TestSchemaRoundTripsThroughTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("widgets")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())
	sqlite.Add(tbl, sqlite.Integer("count").NotNull().Default("0"))
	sqlite.Add(tbl, sqlite.Text("note"))
	exec(t, db, sqlite.CreateTable(tbl))

	live, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	declared := sqlite.BuildSnapshot(sqlite.NewSchema(tbl))

	if stmts := sqlite.Diff(live, declared); len(stmts) != 0 {
		t.Errorf("a freshly created schema still diffs against its declaration:\n%s",
			strings.Join(stmts, "\n"))
	}
}

// And the other direction: a declaration that has moved ahead of the
// database must produce the statement that closes the gap, and that
// statement must be one the engine accepts.
func TestDiffProducesAnApplicableMigration(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("widgets")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(before, sqlite.Text("name").NotNull())
	exec(t, db, sqlite.CreateTable(before))

	after := sqlite.NewTable("widgets")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(after, sqlite.Text("name").NotNull())
	sqlite.Add(after, sqlite.Text("colour"))

	live, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	stmts := sqlite.Diff(live, sqlite.BuildSnapshot(sqlite.NewSchema(after)))
	if len(stmts) == 0 {
		t.Fatal("adding a column produced no migration")
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("the generated migration was rejected: %v\n%s", err, s)
		}
	}

	// Applying it must close the gap, or the migration is not
	// convergent and a second deploy would emit it again.
	live2, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if again := sqlite.Diff(live2, sqlite.BuildSnapshot(sqlite.NewSchema(after))); len(again) != 0 {
		t.Errorf("the migration did not converge; a redeploy would emit:\n%s", strings.Join(again, "\n"))
	}
}

// Event sourcing: append, read back in version order, and refuse a
// version that has already been written. The optimistic-concurrency
// guard is the part that only a real database can prove, since it
// depends on a uniqueness constraint being enforced rather than merely
// declared.
func TestEventStoreAgainstTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	evTable := sqlite.NewEventStoreTable("events")
	exec(t, db, sqlite.CreateTable(evTable))
	store := sqlite.NewEventStore(db, "events")

	appendOne := func(version int64, kind string) error {
		return store.Append(db, ctx, "cart", "cart-1", version-1,
			sqlite.EventInput{Type: kind, Payload: []byte(`{"n":1}`)})
	}
	if err := appendOne(1, "added"); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := appendOne(2, "removed"); err != nil {
		t.Fatalf("second append: %v", err)
	}

	got, err := store.Load(ctx, "cart", "cart-1", 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("loaded %d events, want 2", len(got))
	}
	if got[0].Version > got[1].Version {
		t.Errorf("events came back out of order: %d then %d", got[0].Version, got[1].Version)
	}

	// Appending at a version already taken must fail: that is the
	// whole optimistic-concurrency contract.
	if err := appendOne(2, "duplicate"); err == nil {
		t.Error("a stale expected-version was accepted; concurrent writers would interleave silently")
	}
}
