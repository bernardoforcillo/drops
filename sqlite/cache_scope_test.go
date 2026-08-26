package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/memory"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// countingCache wraps an in-memory cache and counts the operations, so
// a test can say which half of the PK-cache contract broke: a scoped
// row that was written, or a stale entry that was left alone.
type countingCache struct {
	inner   cache.Cache
	sets    int64
	deletes int64
}

func newCountingCache(t *testing.T) *countingCache {
	c := memory.New(memory.Options{MaxEntries: 1024})
	t.Cleanup(func() { _ = c.Close() })
	return &countingCache{inner: c}
}

func (c *countingCache) Get(ctx context.Context, key string) ([]byte, error) {
	return c.inner.Get(ctx, key)
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

// scopedProject is the fixture row for the tests below.
type scopedProject struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`
}

// scopedProjectsTable declares the table and returns it with its tenant
// column, so each test builds the entities it needs over one table —
// the shape that matters here is two entities, one scoped and one not,
// sharing a table and a cache.
func scopedProjectsTable() (*sqlite.Table, sqlite.ColRef) {
	tbl := sqlite.NewTable("projects")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())
	return tbl, tenant.Column
}

// scopedProjectsDriver answers every query with one row of the fixture
// and records the statements, so a read served from the cache is
// visible as a statement that never happened.
func scopedProjectsDriver() *dropstest.Driver {
	return dropstest.New().AlwaysRows(
		[]string{"id", "tenantId", "name"},
		[]any{int64(7), int64(1), "Apollo"},
	)
}

// pkCacheKey is the documented PK-namespace key for the fixture row.
// It holds the table and the id and has room for nothing else — which
// is the whole reason a scoped row may not be stored under it.
const pkCacheKey = "drops:projects:pk:7"

// TestPKCacheHoldsNoScopedRow is the write half of the isolation
// invariant, ported from pg after the same two writes were found
// ungated here.
//
// The PK key carries the primary key and no scope at all, so a row a
// tenant-scoped entity stores under it is a row waiting to be handed to
// whoever asks for that id next — another tenant, or a caller with no
// tenant at all — without a statement ever being sent, and therefore
// without the tenant predicate ever running or ErrTenantMissing ever
// firing. sqlite gated the read in Get and left both writes open, which
// is the arrangement the pg fix rejected: the namespace is poisoned by
// every Create and Update, and the leak is one refactor of Get away.
//
// The second assertion is the other half: the entry a scoped write
// finds under that key must be deleted, not merely left unwritten.
// Here the entry is left by an unscoped sibling entity over the same
// table — the ordinary read-path/write-path pair, and one of the two
// cases invalidatePK is written for. Nothing else will ever correct it.
func TestPKCacheHoldsNoScopedRow(t *testing.T) {
	tests := []struct {
		name string
		op   func(*sqlite.DB, context.Context, *sqlite.Entity[scopedProject]) error
	}{
		{"create", func(db *sqlite.DB, ctx context.Context, e *sqlite.Entity[scopedProject]) error {
			r := scopedProject{ID: 7, TenantID: 1, Name: "Apollo"}
			return e.Create(db, ctx, &r)
		}},
		{"update", func(db *sqlite.DB, ctx context.Context, e *sqlite.Entity[scopedProject]) error {
			r := scopedProject{ID: 7, TenantID: 1, Name: "Apollo"}
			return e.Update(db, ctx, &r)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, tenantCol := scopedProjectsTable()
			cc := newCountingCache(t)
			db := sqlite.New(scopedProjectsDriver())
			ctx := context.Background()

			// The unscoped sibling fills the PK entry, as it is
			// entitled to: nothing restricts which rows it may see.
			sibling := sqlite.NewEntity[scopedProject](tbl).WithCache(cc, time.Minute)
			if _, err := sibling.Get(db, ctx, int64(7)); err != nil {
				t.Fatalf("sibling Get: %v", err)
			}
			if exists, err := cc.Exists(ctx, pkCacheKey); err != nil || !exists {
				t.Fatalf("sibling Get must fill %q: exists = %v, err = %v", pkCacheKey, exists, err)
			}
			setsBeforeOp := atomic.LoadInt64(&cc.sets)

			scoped := sqlite.NewEntity[scopedProject](tbl).
				ScopeByTenant(tenantCol).
				WithCache(cc, time.Minute)
			if err := tt.op(db, sqlite.WithTenant(ctx, int64(1)), scoped); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}

			if got, want := atomic.LoadInt64(&cc.sets)-setsBeforeOp, int64(0); got != want {
				t.Errorf("cache writes by %s on a tenant-scoped entity: got = %v, want %v", tt.name, got, want)
			}
			exists, err := cc.Exists(ctx, pkCacheKey)
			if err != nil {
				t.Fatalf("Exists(%q): %v", pkCacheKey, err)
			}
			if got, want := exists, false; got != want {
				t.Errorf("PK entry present after a scoped %s: got = %v, want %v", tt.name, got, want)
			}
		})
	}
}

// TestScopedGetNeverReadsPKCache is the read half, and it is written
// around the guard rather than the tenant on purpose.
//
// The gate used to be "did the tenant and the guard resolve to a
// predicate on THIS ctx", which is a different question from "is this
// entity scoped". A guard that returns no predicate for the current
// subject — the ordinary spelling of "this subject is unrestricted" —
// resolved to nil and put the entity back on the cached path, so the
// next request from a subject the guard does restrict was served
// whatever the PK namespace happened to hold. The gate asks about the
// entity's configuration, so it answers the same for every ctx.
func TestScopedGetNeverReadsPKCache(t *testing.T) {
	tbl, _ := scopedProjectsTable()
	cc := newCountingCache(t)
	drv := scopedProjectsDriver()
	db := sqlite.New(drv)
	ctx := context.Background()

	sibling := sqlite.NewEntity[scopedProject](tbl).WithCache(cc, time.Minute)
	if _, err := sibling.Get(db, ctx, int64(7)); err != nil {
		t.Fatalf("sibling Get: %v", err)
	}
	if got, want := len(drv.Statements()), 1; got != want {
		t.Fatalf("statements after the sibling's Get: got = %v, want %v", got, want)
	}

	guarded := sqlite.NewEntity[scopedProject](tbl).
		AuthorizeWith(sqlite.CustomGuard(func(context.Context) (drops.Expression, error) {
			// "No restriction for this subject" — the shape that used
			// to re-open the cached path for a guarded entity.
			return nil, nil
		})).
		WithCache(cc, time.Minute)
	if _, err := guarded.Get(db, ctx, int64(7)); err != nil {
		t.Fatalf("guarded Get: %v", err)
	}
	if got, want := len(drv.Statements()), 2; got != want {
		t.Fatalf("statements after the guarded Get: got = %v, want %v (it was served the sibling's cached row)", got, want)
	}
	if got, want := drv.Last().SQL, `SELECT "projects"."id", "projects"."tenantId", "projects"."name" FROM "projects" WHERE ("projects"."id" = ?)`; !strings.HasPrefix(got, want) {
		t.Errorf("statement sent by the guarded Get: got = %v, want prefix %v", got, want)
	}
}

// TestScopedGetWithoutTenantFailsClosed pins the failure the PK cache
// can silently take away. A tenant-scoped Get with no tenant on ctx
// must refuse; served from a namespace keyed by the id alone it would
// have returned a row instead, and the refusal is the only signal the
// caller gets that they forgot the tenant.
func TestScopedGetWithoutTenantFailsClosed(t *testing.T) {
	tbl, tenantCol := scopedProjectsTable()
	cc := newCountingCache(t)
	db := sqlite.New(scopedProjectsDriver())
	ctx := context.Background()

	sibling := sqlite.NewEntity[scopedProject](tbl).WithCache(cc, time.Minute)
	if _, err := sibling.Get(db, ctx, int64(7)); err != nil {
		t.Fatalf("sibling Get: %v", err)
	}

	scoped := sqlite.NewEntity[scopedProject](tbl).
		ScopeByTenant(tenantCol).
		WithCache(cc, time.Minute)
	if _, err := scoped.Get(db, ctx, int64(7)); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("Get with no tenant on ctx: got = %v, want %v", err, sqlite.ErrTenantMissing)
	}
}

// TestQueryCacheKeySeparatesTenantsOfDifferentTypes covers the other
// spelling of the same leak. The query cache key is allowed to hold a
// scoped result set — the tenant is bound into the statement, so it is
// part of the key — but only if the key preserves what the argument
// was. An application reading the tenant off an HTTP header on one path
// and out of a database column on another holds "7" and int64(7) for
// the same customer, and a key built with %v cannot tell them apart:
// the two render identically, so the second tenant is served the
// first's rows.
//
// The assertion on the SQL is the point of the test rather than
// decoration: the two statements are byte-identical, which is why the
// key is the only thing separating the two result sets.
func TestQueryCacheKeySeparatesTenantsOfDifferentTypes(t *testing.T) {
	tbl, tenantCol := scopedProjectsTable()
	drv := scopedProjectsDriver()
	db := sqlite.New(drv)
	ent := sqlite.NewEntity[scopedProject](tbl).
		ScopeByTenant(tenantCol).
		WithCache(newCountingCache(t), time.Minute)

	if _, err := ent.Query(db).All(sqlite.WithTenant(context.Background(), "7")); err != nil {
		t.Fatalf(`All for tenant "7": %v`, err)
	}
	if _, err := ent.Query(db).All(sqlite.WithTenant(context.Background(), int64(7))); err != nil {
		t.Fatalf("All for tenant int64(7): %v", err)
	}

	stmts := drv.Statements()
	if got, want := len(stmts), 2; got != want {
		t.Fatalf("statements for two differently typed tenant values: got = %v, want %v", got, want)
	}
	if got, want := stmts[1].SQL, stmts[0].SQL; got != want {
		t.Fatalf("the two tenants' statements: got = %v, want %v", got, want)
	}
	if got, want := stmts[0].Args[0], any("7"); got != want {
		t.Errorf("tenant bound by the first statement: got = %v, want %v", got, want)
	}
	if got, want := stmts[1].Args[0], any(int64(7)); got != want {
		t.Errorf("tenant bound by the second statement: got = %v, want %v", got, want)
	}
}
