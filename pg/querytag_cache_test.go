package pg_test

import (
	"context"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Tagging happens at the driver boundary, below the entity cache,
// which is what keeps the two independent: a per-request tag changes
// the statement the server sees and must not change the cache key, or
// every request would miss and the cache would be a pure cost. Moving
// the tag up into the render path is the edit this refuses.
func TestQueryTagsDoNotSplitTheEntityCache(t *testing.T) {
	_, ent := entUsersSchema()
	cc := newCountingCache(t)
	ent.WithCache(cc, time.Minute)

	drv := &fakeQueryDriver{}
	db := pg.New(drv)

	ctx1 := drops.WithQueryTag(context.Background(), "request_id", "aaaa")
	ctx2 := drops.WithQueryTag(context.Background(), "request_id", "bbbb")

	if _, err := ent.Query(db).All(ctx1); err != nil {
		t.Fatalf("All #1: %v", err)
	}
	first := drv.queries.Load()
	if first != 1 {
		t.Fatalf("first All issued %d queries, want 1", first)
	}
	if _, err := ent.Query(db).All(ctx2); err != nil {
		t.Fatalf("All #2: %v", err)
	}
	if got := drv.queries.Load(); got != first {
		t.Fatalf("a different request_id tag missed the cache: %d queries, want %d", got, first)
	}
}
