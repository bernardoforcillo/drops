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

	// version is the version this append writes; expectedVersion is
	// the one before it, so a fresh stream's first event is version 0
	// written at -1 — which is what a caller who asked LatestVersion
	// would have in hand.
	appendOne := func(version int64, kind string) error {
		return store.Append(db, ctx, "cart", "cart-1", version-1,
			sqlite.EventInput{Type: kind, Payload: []byte(`{"n":1}`)})
	}
	if err := appendOne(0, "added"); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := appendOne(1, "removed"); err != nil {
		t.Fatalf("second append: %v", err)
	}

	got, err := store.Load(ctx, "cart", "cart-1", -1)
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
	if err := appendOne(1, "duplicate"); err == nil {
		t.Error("a stale expected-version was accepted; concurrent writers would interleave silently")
	}
}

// Load's fromVersion is exclusive, and the number it takes is the same
// number Append's expectedVersion and LatestVersion speak: the version
// you already hold, -1 when you hold none. The doc used to say 0 read
// from the beginning, and 0 is the first event's own version, so every
// caller who believed it lost event 0 of every stream — silently, since
// what came back was still an ordered run of real events, just missing
// the one that created the aggregate.
//
// Three events on a fresh stream leave no room for that: each starting
// value has exactly one right answer and the test names all of them.
func TestEventStoreLoadIsExclusiveOnFromVersion(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	exec(t, db, sqlite.CreateTable(sqlite.NewEventStoreTable("events")))
	store := sqlite.NewEventStore(db, "events")

	if err := store.Append(db, ctx, "cart", "cart-1", -1,
		sqlite.EventInput{Type: "a"},
		sqlite.EventInput{Type: "b"},
		sqlite.EventInput{Type: "c"},
	); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, tc := range []struct {
		from int64
		want string
	}{
		{-1, "a,b,c"},
		{0, "b,c"},
		{1, "c"},
		{2, ""},
	} {
		got, err := store.Load(ctx, "cart", "cart-1", tc.from)
		if err != nil {
			t.Fatalf("Load(%d): %v", tc.from, err)
		}
		var types []string
		for _, ev := range got {
			types = append(types, ev.Type)
		}
		if list := strings.Join(types, ","); list != tc.want {
			t.Errorf("Load(%d) = [%s], want [%s]", tc.from, list, tc.want)
		}
		// The version of the first row returned has to be the one
		// after fromVersion, or "exclusive" means something else.
		if len(got) > 0 && got[0].Version != tc.from+1 {
			t.Errorf("Load(%d) starts at version %d, want %d", tc.from, got[0].Version, tc.from+1)
		}
	}

	// The two ends of the convention: LatestVersion is a resume cursor
	// that returns nothing left to apply, and it is also the expected
	// version the next Append wants.
	head, err := store.LatestVersion(ctx, "cart", "cart-1")
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if head != 2 {
		t.Fatalf("LatestVersion after three events = %d, want 2", head)
	}
	if rest, err := store.Load(ctx, "cart", "cart-1", head); err != nil || len(rest) != 0 {
		t.Errorf("Load(LatestVersion) = %+v (err %v), want nothing left to apply", rest, err)
	}
	if err := store.Append(db, ctx, "cart", "cart-1", head,
		sqlite.EventInput{Type: "d"}); err != nil {
		t.Errorf("Append at LatestVersion: %v", err)
	}
	if tail, err := store.Load(ctx, "cart", "cart-1", head); err != nil || len(tail) != 1 || tail[0].Type != "d" {
		t.Errorf("Load(%d) after appending = %+v (err %v), want only d", head, tail, err)
	}
}

// SQLite takes any default in a CREATE TABLE but only a constant in an
// ALTER TABLE ADD COLUMN: CURRENT_TIMESTAMP and parenthesised
// expressions are rejected with "Cannot add a column with non-constant
// default". A column carrying one has to reach the table through a
// rebuild.
func TestAddColumnWithANonConstantDefaultIsApplicable(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("notes")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(before, sqlite.Text("body").NotNull())
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `INSERT INTO "notes" ("body") VALUES ('kept')`); err != nil {
		t.Fatal(err)
	}

	after := sqlite.NewTable("notes")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(after, sqlite.Text("body").NotNull())
	sqlite.Add(after, sqlite.Timestamp("created_at", false).NotNull().Default("CURRENT_TIMESTAMP"))

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

	// The rebuild has to carry the existing rows across, and the new
	// column has to be populated — a NOT NULL column left NULL would
	// have failed the copy.
	var kept []struct {
		Body      string `drop:"body"`
		CreatedAt string `drop:"created_at"`
	}
	if err := db.Select().From(after).All(ctx, &kept); err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if len(kept) != 1 || kept[0].Body != "kept" || kept[0].CreatedAt == "" {
		t.Errorf("the rebuild lost data: %+v", kept)
	}

	live2, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if again := sqlite.Diff(live2, sqlite.BuildSnapshot(sqlite.NewSchema(after))); len(again) != 0 {
		t.Errorf("the migration did not converge; a redeploy would emit:\n%s", strings.Join(again, "\n"))
	}
}

// A SQLite file is frequently shared — another migration tool's
// bookkeeping table, an unrelated service's table, a drops history
// table under a name Migrator.WithTable chose. None of those are in the
// Schema, and Push must not read "absent from the Schema" as "deleted".
func TestPushLeavesTablesItWasNeverToldAboutAlone(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	users := sqlite.NewTable("users")
	sqlite.Add(users, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(users, sqlite.Text("name").NotNull())
	exec(t, db, sqlite.CreateTable(users))

	foreign := []string{
		`CREATE TABLE "__drizzle_migrations" ("id" INTEGER PRIMARY KEY, "hash" TEXT)`,
		`CREATE TABLE "_house_migrations" ("id" INTEGER PRIMARY KEY)`,
	}
	for _, s := range foreign {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO "__drizzle_migrations" ("id","hash") VALUES (1,'abc')`); err != nil {
		t.Fatal(err)
	}

	// A schema that has moved ahead, so the push has real work to do.
	posts := sqlite.NewTable("posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(users, posts))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, s := range res.Statements {
		if strings.Contains(s, "DROP TABLE") {
			t.Errorf("Push wanted to drop a table it does not own:\n%s", strings.Join(res.Statements, "\n"))
			break
		}
	}

	var rows []struct {
		Hash string `drop:"hash"`
	}
	if err := db.Select().From(sqlite.NewTable("__drizzle_migrations")).All(ctx, &rows); err != nil {
		t.Fatalf("another tool's table did not survive the push: %v", err)
	}
	if len(rows) != 1 || rows[0].Hash != "abc" {
		t.Errorf("another tool's rows did not survive the push: %+v", rows)
	}
	if _, err := db.Exec(ctx, `SELECT 1 FROM "_house_migrations"`); err != nil {
		t.Errorf("a house-keeping table did not survive the push: %v", err)
	}
	// The work the push was actually asked to do still has to happen.
	if _, err := db.Exec(ctx, `SELECT 1 FROM "posts"`); err != nil {
		t.Errorf("Push did not create the declared table: %v", err)
	}
}
