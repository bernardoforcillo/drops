package pg_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/memory"
	"github.com/bernardoforcillo/drops/pg"
)

// countingCache wraps an in-memory cache and counts hit / miss / set
// / delete operations so tests can assert on the cache behaviour.
type countingCache struct {
	inner   cache.Cache
	hits    int64
	misses  int64
	sets    int64
	deletes int64
}

func newCountingCache(t *testing.T) *countingCache {
	c := memory.New(memory.Options{MaxEntries: 1024})
	t.Cleanup(func() { _ = c.Close() })
	return &countingCache{inner: c}
}

func (c *countingCache) Get(ctx context.Context, key string) ([]byte, error) {
	v, err := c.inner.Get(ctx, key)
	if err == nil {
		atomic.AddInt64(&c.hits, 1)
	} else if errors.Is(err, cache.ErrNotFound) {
		atomic.AddInt64(&c.misses, 1)
	}
	return v, err
}
func (c *countingCache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	atomic.AddInt64(&c.sets, 1)
	return c.inner.Set(ctx, key, val, ttl)
}
func (c *countingCache) Delete(ctx context.Context, keys ...string) (int, error) {
	atomic.AddInt64(&c.deletes, 1)
	return c.inner.Delete(ctx, keys...)
}
func (c *countingCache) Exists(ctx context.Context, key string) (bool, error) {
	return c.inner.Exists(ctx, key)
}
func (c *countingCache) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.inner.TTL(ctx, key)
}
func (c *countingCache) Ping(ctx context.Context) error { return c.inner.Ping(ctx) }
func (c *countingCache) Close() error                   { return c.inner.Close() }

// fakeQueryDriver responds to every Query with a single row that the
// scanner decodes into an entUser. Counts the number of queries
// issued so tests can verify the cache short-circuits the DB.
type fakeQueryDriver struct {
	queries atomic.Int64
}

func (f *fakeQueryDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, nil
}
func (f *fakeQueryDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	f.queries.Add(1)
	return &fakeRows{
		cols: []string{"id", "name", "email"},
		data: [][]any{{int64(7), "Alice", "a@x"}},
	}, nil
}
func (f *fakeQueryDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }

func TestEntityCacheGetHitsAndMisses(t *testing.T) {
	_, ent := entUsersSchema()
	cc := newCountingCache(t)
	ent.WithCache(cc, time.Minute)

	drv := &fakeQueryDriver{}
	db := pg.New(drv)

	// 1st call: miss → DB query.
	if _, err := ent.Get(db, context.Background(), int64(7)); err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	if drv.queries.Load() != 1 {
		t.Errorf("expected 1 DB query after first Get, got %d", drv.queries.Load())
	}
	if atomic.LoadInt64(&cc.sets) != 1 {
		t.Errorf("expected 1 cache Set after first Get, got %d", cc.sets)
	}

	// 2nd call: hit → no DB query.
	if _, err := ent.Get(db, context.Background(), int64(7)); err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	if drv.queries.Load() != 1 {
		t.Errorf("expected still 1 DB query after cached Get, got %d", drv.queries.Load())
	}
	if atomic.LoadInt64(&cc.hits) == 0 {
		t.Error("expected at least one cache hit")
	}
}

func TestEntityCacheUpdateRefreshesEntry(t *testing.T) {
	_, ent := entUsersSchema()
	cc := newCountingCache(t)
	ent.WithCache(cc, time.Minute)

	drv := &fakeQueryDriver{}
	db := pg.New(drv)

	// Populate the cache.
	u, err := ent.Get(db, context.Background(), int64(7))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Update triggers a write-through: the post-RETURNING values
	// land back into the cache.
	u.Name = "Alice-new"
	if err := ent.Update(db, context.Background(), &u); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Next Get should serve from cache (no further DB query).
	queriesBefore := drv.queries.Load()
	_, _ = ent.Get(db, context.Background(), int64(7))
	if drv.queries.Load() != queriesBefore {
		t.Errorf("Update should refresh the cache, but next Get hit the DB")
	}
}

func TestEntityCacheDeleteInvalidates(t *testing.T) {
	_, ent := entUsersSchema()
	cc := newCountingCache(t)
	ent.WithCache(cc, time.Minute)

	drv := &fakeQueryDriver{}
	db := pg.New(drv)

	// Populate.
	if _, err := ent.Get(db, context.Background(), int64(7)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Delete.
	if _, err := ent.Delete(db, context.Background(), int64(7)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if atomic.LoadInt64(&cc.deletes) == 0 {
		t.Error("Delete must invalidate the cache entry")
	}
	// Next Get is a miss → DB hit.
	queriesBefore := drv.queries.Load()
	if _, err := ent.Get(db, context.Background(), int64(7)); err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if drv.queries.Load() != queriesBefore+1 {
		t.Errorf("Get after Delete must reach DB again")
	}
}

func TestEntityCacheSingleFlight(t *testing.T) {
	_, ent := entUsersSchema()
	cc := newCountingCache(t)
	ent.WithCache(cc, time.Minute)

	// Slow driver: blocks until released so concurrent callers all
	// queue up on the single-flight lease.
	release := make(chan struct{})
	hits := atomic.Int64{}
	drv := slowDriver{
		onQuery: func() (drops.Rows, error) {
			hits.Add(1)
			<-release
			return &fakeRows{
				cols: []string{"id", "name", "email"},
				data: [][]any{{int64(7), "Alice", "a@x"}},
			}, nil
		},
	}
	db := pg.New(drv)

	const N = 25
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = ent.Get(db, context.Background(), int64(7))
		}()
	}
	// Give the goroutines time to fan out and queue on the lease.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := hits.Load(); got != 1 {
		t.Errorf("single-flight must collapse N concurrent misses to 1 DB query, got %d", got)
	}
}

type slowDriver struct {
	onQuery func() (drops.Rows, error)
}

func (slowDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, nil
}
func (s slowDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	return s.onQuery()
}
func (slowDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }

func TestEntityCacheQueryAllCachesByHash(t *testing.T) {
	_, ent := entUsersSchema()
	cc := newCountingCache(t)
	ent.WithCache(cc, time.Minute)

	drv := &fakeQueryDriver{}
	db := pg.New(drv)

	// Same query, executed twice.
	if _, err := ent.Query(db).Limit(10).All(context.Background()); err != nil {
		t.Fatalf("All #1: %v", err)
	}
	if _, err := ent.Query(db).Limit(10).All(context.Background()); err != nil {
		t.Fatalf("All #2: %v", err)
	}
	if drv.queries.Load() != 1 {
		t.Errorf("identical Query.All should hit the DB once, got %d", drv.queries.Load())
	}

	// Different query (different LIMIT) → cache miss → second DB call.
	if _, err := ent.Query(db).Limit(20).All(context.Background()); err != nil {
		t.Fatalf("All #3: %v", err)
	}
	if drv.queries.Load() != 2 {
		t.Errorf("changing the query must produce a fresh DB call, got %d", drv.queries.Load())
	}
}
