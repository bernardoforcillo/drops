package pg_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/memory"
	"github.com/bernardoforcillo/drops/pg"
)

func newIndex(t *testing.T) (*pg.TopicIndex, cache.Cache) {
	t.Helper()
	c := memory.New(memory.Options{})
	t.Cleanup(func() { _ = c.Close() })
	return pg.NewTopicIndex(c, time.Hour), c
}

func stamp(t *testing.T, idx *pg.TopicIndex, topics ...pg.Topic) string {
	t.Helper()
	s, err := idx.Stamp(context.Background(), topics...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStampIsStableUntilInvalidated(t *testing.T) {
	idx, _ := newIndex(t)
	topic := pg.ValueTopic("orders", "customer_id", 7)

	first := stamp(t, idx, topic)
	if first == "" {
		t.Fatal("empty stamp")
	}
	if second := stamp(t, idx, topic); second != first {
		t.Errorf("stamp changed without an invalidation: %q then %q", first, second)
	}
	if err := idx.Invalidate(context.Background(), topic); err != nil {
		t.Fatal(err)
	}
	if after := stamp(t, idx, topic); after == first {
		t.Error("stamp survived an invalidation")
	}
}

// The declaration order of topics must not change the key.
func TestStampIgnoresOrderAndDuplicates(t *testing.T) {
	idx, _ := newIndex(t)
	a := pg.UnscopedTopic("orders")
	b := pg.ValueTopic("orders", "customer_id", 7)

	one := stamp(t, idx, a, b)
	two := stamp(t, idx, b, a)
	three := stamp(t, idx, b, a, b, a)
	if one != two || one != three {
		t.Errorf("stamps differ by order or repetition: %q %q %q", one, two, three)
	}
}

func TestStampOfNothingIsEmpty(t *testing.T) {
	idx, _ := newIndex(t)
	if got := stamp(t, idx); got != "" {
		t.Errorf("stamp with no topics = %q", got)
	}
}

// The heart of the design: a row change reaches unfiltered queries
// and the matching scoped ones, and leaves the other scopes alone.
func TestInvalidateRowIsScoped(t *testing.T) {
	idx, _ := newIndex(t)
	ctx := context.Background()

	anyRow := pg.TableTopic("orders")
	unscoped := pg.UnscopedTopic("orders")
	cust7 := pg.ValueTopic("orders", "customer_id", 7)
	cust9 := pg.ValueTopic("orders", "customer_id", 9)

	before := map[string]string{}
	for _, tp := range []pg.Topic{anyRow, unscoped, cust7, cust9} {
		before[tp.String()] = stamp(t, idx, tp)
	}

	// An order is created for customer 7.
	if err := idx.InvalidateRow(ctx, "orders", nil,
		map[string]any{"id": 1, "customer_id": 7}, "customer_id"); err != nil {
		t.Fatal(err)
	}

	if stamp(t, idx, anyRow) == before[anyRow.String()] {
		t.Error("an unfiltered query was not invalidated by a row change")
	}
	if stamp(t, idx, cust7) == before[cust7.String()] {
		t.Error("the matching scoped query was not invalidated")
	}
	if got := stamp(t, idx, cust9); got != before[cust9.String()] {
		t.Error("an unrelated scope was invalidated; the whole point is that it is not")
	}
	if got := stamp(t, idx, unscoped); got != before[unscoped.String()] {
		t.Error("a row change touched the unscoped topic, which would evict every scoped query")
	}
}

// Moving a row between scopes has to invalidate both, or the scope it
// left keeps a row that is no longer in it.
func TestInvalidateRowTouchesBothSides(t *testing.T) {
	idx, _ := newIndex(t)
	ctx := context.Background()
	cust7 := pg.ValueTopic("orders", "customer_id", 7)
	cust9 := pg.ValueTopic("orders", "customer_id", 9)

	was7 := stamp(t, idx, cust7)
	was9 := stamp(t, idx, cust9)

	if err := idx.InvalidateRow(ctx, "orders",
		map[string]any{"id": 1, "customer_id": 7},
		map[string]any{"id": 1, "customer_id": 9},
		"customer_id"); err != nil {
		t.Fatal(err)
	}
	if stamp(t, idx, cust7) == was7 {
		t.Error("the scope the row left was not invalidated")
	}
	if stamp(t, idx, cust9) == was9 {
		t.Error("the scope the row joined was not invalidated")
	}
}

// A column the change feed did not report must not be read as NULL.
func TestInvalidateRowSkipsAbsentColumns(t *testing.T) {
	idx, _ := newIndex(t)
	ctx := context.Background()
	nullish := pg.ValueTopic("orders", "customer_id", nil)
	was := stamp(t, idx, nullish)

	if err := idx.InvalidateRow(ctx, "orders", nil,
		map[string]any{"id": 1}, "customer_id"); err != nil {
		t.Fatal(err)
	}
	if got := stamp(t, idx, nullish); got != was {
		t.Error("an absent column was invalidated as if it were NULL")
	}
}

// The conservative call reaches every query on the table however it
// was scoped.
func TestInvalidateTableReachesEveryScope(t *testing.T) {
	idx, _ := newIndex(t)
	ctx := context.Background()
	anyRow := pg.TableTopic("orders")
	unscoped := pg.UnscopedTopic("orders")
	other := pg.TableTopic("customers")

	wasAny := stamp(t, idx, anyRow)
	wasUnscoped := stamp(t, idx, unscoped)
	wasOther := stamp(t, idx, other)

	if err := idx.InvalidateTable(ctx, "orders"); err != nil {
		t.Fatal(err)
	}
	if stamp(t, idx, anyRow) == wasAny {
		t.Error("unfiltered queries survived a table invalidation")
	}
	if stamp(t, idx, unscoped) == wasUnscoped {
		t.Error("scoped queries survived a table invalidation")
	}
	if stamp(t, idx, other) != wasOther {
		t.Error("another table was invalidated")
	}
}

func TestTopicStringsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for _, tp := range []pg.Topic{
		pg.TableTopic("orders"),
		pg.UnscopedTopic("orders"),
		pg.ValueTopic("orders", "customer_id", 7),
		pg.ValueTopic("orders", "customer_id", 9),
		pg.ValueTopic("orders", "tenant_id", 7),
		pg.TableTopic("customers"),
	} {
		if prev, dup := seen[tp.String()]; dup {
			t.Errorf("%s collides with %s", tp.String(), prev)
		}
		seen[tp.String()] = tp.String()
	}
	// A value carrying the key separators must not be able to forge
	// another topic's key.
	forged := pg.ValueTopic("orders", "a", "b=c").String()
	real := pg.ValueTopic("orders", "a=b", "c").String()
	if forged == real {
		t.Error("a value containing '=' forged another topic's key")
	}
}

// A backend that cannot answer must not produce a stamp: serving from
// a key whose freshness is unknown is the one unsafe outcome.
func TestStampFailsOnABrokenBackend(t *testing.T) {
	idx := pg.NewTopicIndex(brokenCache{}, time.Hour)
	if _, err := idx.Stamp(context.Background(), pg.TableTopic("orders")); err == nil {
		t.Error("expected an error from an unreachable backend")
	}
}

func TestNewTopicIndexNilBackendPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected a panic on a nil backend")
		}
	}()
	pg.NewTopicIndex(nil, 0)
}

// brokenCache fails every operation the way an unreachable backend
// does — not with ErrNotFound, which means "no entry" and is an
// answer.
type brokenCache struct{}

var errBroken = errors.New("backend unreachable")

func (brokenCache) Get(context.Context, string) ([]byte, error) { return nil, errBroken }
func (brokenCache) Set(context.Context, string, []byte, time.Duration) error {
	return errBroken
}
func (brokenCache) Delete(context.Context, ...string) (int, error) { return 0, errBroken }
func (brokenCache) Exists(context.Context, string) (bool, error)   { return false, errBroken }
func (brokenCache) TTL(context.Context, string) (time.Duration, error) {
	return 0, errBroken
}
func (brokenCache) Ping(context.Context) error { return errBroken }
func (brokenCache) Close() error               { return nil }

// countingRowsDriver answers every query with an empty cursor and
// counts how many reached it — which is how a cache hit is observed
// from outside.
type countingRowsDriver struct{ queries int }

func (d *countingRowsDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}
func (d *countingRowsDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	d.queries++
	return &stubRows{}, nil
}
func (d *countingRowsDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

type topicOrder struct {
	ID         int64 `drop:"id"`
	CustomerID int64 `drop:"customer_id"`
}

// End to end: a declared dependency turns the query cache from
// TTL-only into something a write can evict.
func TestEntityQueryDependsOnInvalidation(t *testing.T) {
	idx, backend := newIndex(t)
	drv := &countingRowsDriver{}
	db := pg.New(drv)
	ctx := context.Background()

	orders := pg.NewTable("orders")
	pg.Add(orders, pg.BigSerial("id").PrimaryKey())
	pg.Add(orders, pg.BigInt("customer_id").NotNull())

	ent := pg.NewEntity[topicOrder](orders).
		WithCache(backend, time.Hour).
		WithTopics(idx, "customer_id")

	run := func() {
		if _, err := ent.Query(db).
			Where(pg.Eq(orders.Col("customer_id"), 7)).
			DependsOn(
				pg.UnscopedTopic("orders"),
				pg.ValueTopic("orders", "customer_id", 7),
			).All(ctx); err != nil {
			t.Fatal(err)
		}
	}

	run()
	if drv.queries != 1 {
		t.Fatalf("first query hit the driver %d times, want 1", drv.queries)
	}
	run()
	if drv.queries != 1 {
		t.Fatalf("second query hit the driver %d times, want 1 (served from cache)", drv.queries)
	}

	// A change to another customer must not evict it.
	if err := idx.InvalidateRow(ctx, "orders", nil,
		map[string]any{"id": 2, "customer_id": 9}, "customer_id"); err != nil {
		t.Fatal(err)
	}
	run()
	if drv.queries != 1 {
		t.Fatalf("an unrelated customer's change evicted the entry (driver hits: %d)", drv.queries)
	}

	// A change to this customer must.
	if err := idx.InvalidateRow(ctx, "orders", nil,
		map[string]any{"id": 3, "customer_id": 7}, "customer_id"); err != nil {
		t.Fatal(err)
	}
	run()
	if drv.queries != 2 {
		t.Fatalf("the matching change did not evict the entry (driver hits: %d)", drv.queries)
	}
}

// Without DependsOn the behaviour is what it always was: TTL only.
func TestEntityQueryWithoutDependsOnIsTTLOnly(t *testing.T) {
	idx, backend := newIndex(t)
	drv := &countingRowsDriver{}
	db := pg.New(drv)
	ctx := context.Background()

	orders := pg.NewTable("orders")
	pg.Add(orders, pg.BigSerial("id").PrimaryKey())
	pg.Add(orders, pg.BigInt("customer_id").NotNull())

	ent := pg.NewEntity[topicOrder](orders).
		WithCache(backend, time.Hour).
		WithTopics(idx, "customer_id")

	for i := 0; i < 2; i++ {
		if _, err := ent.Query(db).All(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if drv.queries != 1 {
		t.Fatalf("driver hits: %d, want 1", drv.queries)
	}
	if err := idx.InvalidateTable(ctx, "orders"); err != nil {
		t.Fatal(err)
	}
	if _, err := ent.Query(db).All(ctx); err != nil {
		t.Fatal(err)
	}
	if drv.queries != 1 {
		t.Errorf("a query that declared nothing was evicted anyway (driver hits: %d)", drv.queries)
	}
}
