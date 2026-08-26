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
	"github.com/bernardoforcillo/drops/dropstest"
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

// ----------------------------------------------------------------------
// Row scope and the PK cache
// ----------------------------------------------------------------------

// scopedUser is the fixture for the tenant-scoped half of these tests.
type scopedUser struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`
}

// scopedUsersSchema declares the tenant axis on the TABLE — the
// spelling pg/tenant.go recommends and the one every fixture in
// scoping_test.go uses — rather than through Entity.ScopeByTenant.
// That is the whole point of the fixture: the entity's tenantCol is
// nil, so an "is this entity scoped?" test that only asks the Entity
// answers no for a table nobody may query without a tenant.
func scopedUsersSchema() *pg.Entity[scopedUser] {
	tbl := pg.NewTable("users")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("name").NotNull())
	tbl.ContextFilter(pg.TenantFilter(tenant))
	return pg.NewEntity[scopedUser](tbl)
}

// scopedDriver answers every query with one row of the fixture and
// records the statements, so a cached read is visible as a statement
// that never happened.
func scopedDriver() *dropstest.Driver {
	return dropstest.New().AlwaysRows(
		[]string{"id", "tenantId", "name"},
		[]any{int64(7), int64(1), "Alice"},
	)
}

// TestPKCacheNotReadForTableScopedEntity is the read half of the
// isolation invariant: a Get on a table carrying a context filter must
// reach the database, because the PK cache key holds the id and
// nothing else. Answering from it returns the row the previous caller
// cached — to a different tenant, and to a caller with no tenant at
// all, without the filter ever running.
func TestPKCacheNotReadForTableScopedEntity(t *testing.T) {
	ent := scopedUsersSchema().WithCache(newCountingCache(t), time.Minute)
	drv := scopedDriver()
	db := pg.New(drv)

	if _, err := ent.Get(db, pg.WithTenant(context.Background(), int64(1)), int64(7)); err != nil {
		t.Fatalf("Get for tenant 1: %v", err)
	}
	if got, want := len(drv.Statements()), 1; got != want {
		t.Fatalf("statements after tenant 1's Get: got = %v, want %v", got, want)
	}

	if _, err := ent.Get(db, pg.WithTenant(context.Background(), int64(2)), int64(7)); err != nil {
		t.Fatalf("Get for tenant 2: %v", err)
	}
	if got, want := len(drv.Statements()), 2; got != want {
		t.Fatalf("statements after tenant 2 asked for the same id: got = %v, want %v (tenant 2 was served tenant 1's cached row)", got, want)
	}
	// args are (id, tenant, limit) — the tenant predicate is appended
	// by the executor, after the caller's own predicate.
	if got, want := drv.Last().Args[1], any(int64(2)); got != want {
		t.Errorf("tenant bound by tenant 2's statement: got = %v, want %v", got, want)
	}

	if _, err := ent.Get(db, context.Background(), int64(7)); !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("Get with no tenant on ctx: got = %v, want %v", err, pg.ErrTenantMissing)
	}
}

// TestPKCacheHoldsNoScopedRow is the write half, and states the
// invariant the read half depends on: the PK namespace never holds a
// scoped row in the first place. Gating only the read leaves every
// Create and Update poisoning it, one refactor away from a leak.
func TestPKCacheHoldsNoScopedRow(t *testing.T) {
	tests := []struct {
		name string
		op   func(*pg.DB, context.Context, *pg.Entity[scopedUser]) error
	}{
		{"create", func(db *pg.DB, ctx context.Context, e *pg.Entity[scopedUser]) error {
			r := scopedUser{TenantID: 1, Name: "Alice"}
			return e.Create(db, ctx, &r)
		}},
		{"update", func(db *pg.DB, ctx context.Context, e *pg.Entity[scopedUser]) error {
			r := scopedUser{ID: 7, TenantID: 1, Name: "Alice"}
			return e.Update(db, ctx, &r)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := newCountingCache(t)
			ent := scopedUsersSchema().WithCache(cc, time.Minute)
			drv := scopedDriver()
			db := pg.New(drv)

			if err := tt.op(db, pg.WithTenant(context.Background(), int64(1)), ent); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if got, want := atomic.LoadInt64(&cc.sets), int64(0); got != want {
				t.Errorf("cache writes by %s on a scoped entity: got = %v, want %v", tt.name, got, want)
			}
		})
	}
}

// TestQueryCacheKeySeparatesTenantsOfDifferentTypes covers the other
// spelling of the same leak. The query cache key is allowed to hold a
// scoped result set — the tenant is bound into the statement, so it is
// part of the key — but only if the key preserves what the argument
// was. An application reading the tenant off an HTTP header on one
// path and out of a database column on another holds "7" and int64(7)
// for the same customer, and a key built with %v cannot tell them
// apart.
func TestQueryCacheKeySeparatesTenantsOfDifferentTypes(t *testing.T) {
	ent := scopedUsersSchema().WithCache(newCountingCache(t), time.Minute)
	drv := scopedDriver()
	db := pg.New(drv)

	if _, err := ent.Query(db).All(pg.WithTenant(context.Background(), "7")); err != nil {
		t.Fatalf(`All for tenant "7": %v`, err)
	}
	if _, err := ent.Query(db).All(pg.WithTenant(context.Background(), int64(7))); err != nil {
		t.Fatalf("All for tenant int64(7): %v", err)
	}
	if got, want := len(drv.Statements()), 2; got != want {
		t.Errorf("statements for two differently typed tenant values: got = %v, want %v", got, want)
	}
}

// TestScopedWriteInvalidatesStalePKEntry is the other half of the
// invariant TestPKCacheHoldsNoScopedRow states. Refusing to *write* the
// PK entry keeps a scoped row out of a namespace that cannot hold the
// scope; it does nothing about an entry that is already there, and
// something always is eventually — left by a build from before the
// table was scoped, or by a sibling entity over the same table that is
// not (invalidatePK's doc comment names both).
//
// Left alone, that entry is never written and never invalidated by the
// scoped entity, so it survives every subsequent write to the row and
// answers with pre-scope values for as long as the backend keeps it:
// stale for ever, for whoever still reads that namespace. Gates belong
// on the paths that put rows in, never on the path that takes them out,
// so a scoped write takes the entry out.
func TestScopedWriteInvalidatesStalePKEntry(t *testing.T) {
	// The entry an unscoped writer left behind. Its contents never
	// matter — nothing decodes it before it is deleted — only that the
	// key is occupied.
	const stalePKKey = "drops:users:pk:7"

	tests := []struct {
		name string
		op   func(*pg.DB, context.Context, *pg.Entity[scopedUser]) error
	}{
		{"create", func(db *pg.DB, ctx context.Context, e *pg.Entity[scopedUser]) error {
			r := scopedUser{TenantID: 1, Name: "Alice"}
			return e.Create(db, ctx, &r)
		}},
		{"update", func(db *pg.DB, ctx context.Context, e *pg.Entity[scopedUser]) error {
			r := scopedUser{ID: 7, TenantID: 1, Name: "Alice"}
			return e.Update(db, ctx, &r)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc := newCountingCache(t)
			ent := scopedUsersSchema().WithCache(cc, time.Minute)
			db := pg.New(scopedDriver())
			ctx := pg.WithTenant(context.Background(), int64(1))

			if err := cc.Set(ctx, stalePKKey, []byte("row cached before the table was scoped"), time.Minute); err != nil {
				t.Fatalf("seeding the stale entry: %v", err)
			}
			setsBeforeOp := atomic.LoadInt64(&cc.sets)
			if err := tt.op(db, ctx, ent); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}

			exists, err := cc.Exists(ctx, stalePKKey)
			if err != nil {
				t.Fatalf("Exists(%q): %v", stalePKKey, err)
			}
			if got, want := exists, false; got != want {
				t.Errorf("stale PK entry still present after a scoped %s: got = %v, want %v", tt.name, got, want)
			}
			// The gate on the write half still holds: invalidating is
			// not an excuse to start filling the namespace again.
			if got, want := atomic.LoadInt64(&cc.sets)-setsBeforeOp, int64(0); got != want {
				t.Errorf("cache writes by %s on a scoped entity: got = %v, want %v", tt.name, got, want)
			}
		})
	}
}
