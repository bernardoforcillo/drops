package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache/memory"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

type topicOrder struct {
	ID         int64 `drop:"id"`
	CustomerID int64 `drop:"customerId"`
	Total      int64 `drop:"total"`
}

// The unit suite proves the topic arithmetic against a fake driver.
// What it cannot show is that a cached query and a live one return the
// same rows — that the entry served from cache is the entry the
// database would have produced, and that after an invalidation the
// query really goes back to the server and picks up the new row.
func TestPGTopicCacheServesAndInvalidates(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "orders"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.BigInt("customerId").NotNull())
	pg.Add(tbl, pg.BigInt("total").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	backend := memory.New(memory.Options{})
	t.Cleanup(func() { _ = backend.Close() })
	idx := pg.NewTopicIndex(backend, time.Hour)

	ent := pg.NewEntity[topicOrder](tbl).
		WithCache(backend, time.Hour).
		WithTopics(idx, "customerId")

	insert := func(id, customer, total int64) {
		t.Helper()
		if _, err := db.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %q ("id", "customerId", "total") VALUES ($1,$2,$3)`, tbl.Name()),
			id, customer, total); err != nil {
			t.Fatal(err)
		}
	}
	ordersOf := func(customer int64) []topicOrder {
		t.Helper()
		got, err := ent.Query(db).
			Where(pg.Eq(tbl.Col("customerId"), customer)).
			DependsOn(
				pg.UnscopedTopic(tbl.Name()),
				pg.ValueTopic(tbl.Name(), "customerId", customer),
			).
			All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	insert(1, 7, 100)
	if got := ordersOf(7); len(got) != 1 || got[0].Total != 100 {
		t.Fatalf("first read = %+v", got)
	}

	// A row inserted behind the cache's back is not visible: the entry
	// is being served, which is the point.
	insert(2, 7, 200)
	if got := ordersOf(7); len(got) != 1 {
		t.Fatalf("the cached entry was not served: %+v", got)
	}

	// A change to another customer must not evict it.
	if err := idx.InvalidateRow(ctx, tbl.Name(), nil,
		map[string]any{"id": 3, "customerId": 9}, "customerId"); err != nil {
		t.Fatal(err)
	}
	if got := ordersOf(7); len(got) != 1 {
		t.Fatalf("an unrelated customer's change evicted the entry: %+v", got)
	}

	// A change to this one must, and the re-read has to come from the
	// database with both rows in it.
	if err := idx.InvalidateRow(ctx, tbl.Name(), nil,
		map[string]any{"id": 2, "customerId": 7}, "customerId"); err != nil {
		t.Fatal(err)
	}
	got := ordersOf(7)
	if len(got) != 2 {
		t.Fatalf("after invalidation the query returned %d rows, want 2", len(got))
	}
	if got[0].Total+got[1].Total != 300 {
		t.Errorf("re-read did not come from the database: %+v", got)
	}
}

// The wide invalidation has to reach a query however it was scoped,
// because it is what every statement bypassing the entity layer owes
// the cache.
func TestPGTopicInvalidateTableReachesScopedQueries(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "orders"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.BigInt("customerId").NotNull())
	pg.Add(tbl, pg.BigInt("total").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	backend := memory.New(memory.Options{})
	t.Cleanup(func() { _ = backend.Close() })
	idx := pg.NewTopicIndex(backend, time.Hour)
	ent := pg.NewEntity[topicOrder](tbl).
		WithCache(backend, time.Hour).
		WithTopics(idx, "customerId")

	if _, err := db.Exec(ctx,
		fmt.Sprintf(`INSERT INTO %q VALUES (1, 7, 100)`, tbl.Name())); err != nil {
		t.Fatal(err)
	}
	scoped := func() []topicOrder {
		t.Helper()
		got, err := ent.Query(db).
			Where(pg.Eq(tbl.Col("customerId"), 7)).
			DependsOn(
				pg.UnscopedTopic(tbl.Name()),
				pg.ValueTopic(tbl.Name(), "customerId", 7),
			).All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := scoped(); len(got) != 1 {
		t.Fatalf("first read = %+v", got)
	}

	// A bulk statement the entity layer never saw.
	if _, err := db.Exec(ctx,
		fmt.Sprintf(`UPDATE %q SET "total" = 999 WHERE "customerId" = 7`, tbl.Name())); err != nil {
		t.Fatal(err)
	}
	if err := idx.InvalidateTable(ctx, tbl.Name()); err != nil {
		t.Fatal(err)
	}
	got := scoped()
	if len(got) != 1 || got[0].Total != 999 {
		t.Errorf("the scoped query survived a table invalidation: %+v", got)
	}
}

// Create is the one write path that can name what it touched, and the
// entity has to actually announce it.
func TestPGTopicCreateInvalidatesItsOwnScope(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "orders"))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.BigInt("customerId").NotNull())
	pg.Add(tbl, pg.BigInt("total").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	backend := memory.New(memory.Options{})
	t.Cleanup(func() { _ = backend.Close() })
	idx := pg.NewTopicIndex(backend, time.Hour)
	ent := pg.NewEntity[topicOrder](tbl).
		WithCache(backend, time.Hour).
		WithTopics(idx, "customerId")

	read := func(customer int64) int {
		t.Helper()
		got, err := ent.Query(db).
			Where(pg.Eq(tbl.Col("customerId"), customer)).
			DependsOn(
				pg.UnscopedTopic(tbl.Name()),
				pg.ValueTopic(tbl.Name(), "customerId", customer),
			).All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}

	if n := read(7); n != 0 {
		t.Fatalf("read %d rows from an empty table", n)
	}
	if n := read(9); n != 0 {
		t.Fatal("read from an empty table")
	}

	row := topicOrder{ID: 1, CustomerID: 7, Total: 100}
	if err := ent.Create(db, ctx, &row); err != nil {
		t.Fatal(err)
	}
	if n := read(7); n != 1 {
		t.Errorf("Create did not invalidate its own scope: got %d rows", n)
	}
	// And it was precise: customer 9's empty answer is still cached
	// and still correct.
	if n := read(9); n != 0 {
		t.Errorf("Create invalidated an unrelated scope: got %d rows", n)
	}
}
