package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/memory"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// The caches, against a table-declared axis.
//
// Round 3 closed two holes here: the query key is a full digest over
// type-qualified arguments, and the PK cache is not written by a scoped
// entity. Moving the axis onto the table reopens both from a new
// direction, and these tests pin the answers.
//
//   - The query namespace is safe for a scoped entity ONLY because the
//     tenant is bound into the statement and hashed into the key. The
//     tenant is now added by the RESOLVER, so a key built from ToSQL
//     rather than from ToSQLCtx does not contain it, and two tenants
//     asking one question share one entry.
//   - The PK namespace has room for the id and nothing else. An entity
//     that declares no axis of its own, over a table that does, is
//     scoped — and used to answer "no" to the question that gates the
//     namespace.

type ccRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`
}

// ccTable declares the axis on the TABLE and builds an entity that
// knows nothing about it. That is the arrangement the third term of
// hasRowScope exists for, and the one an entity-only check misses.
func ccTable(name string) (*sqlite.Table, *sqlite.Entity[ccRow]) {
	t := sqlite.NewTable(name)
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(t, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(t, sqlite.Text("name"))
	t.ContextFilter(sqlite.TenantFilter(tenant))
	return t, sqlite.NewEntity[ccRow](t)
}

func ccCache(t *testing.T) *memory.Cache {
	c := memory.New(memory.Options{MaxEntries: 256})
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Two tenants asking the same question get two cache entries and two
// queries. One entry would mean the second tenant is served the first
// tenant's rows, without a statement being sent and so without the
// tenant predicate ever running.
func TestTheQueryCacheKeyCarriesTheResolvedTenant(t *testing.T) {
	tbl, ent := ccTable("cq_rows")
	ent.WithCache(ccCache(t), time.Minute)

	drv := dropstest.New().
		Rows([]string{"id", "tenantId", "name"}, []any{int64(1), scopeTenant, "mine"}).
		Rows([]string{"id", "tenantId", "name"}, []any{int64(2), int64(5), "theirs"})
	db := sqlite.New(drv)
	_ = tbl

	mine, err := ent.Query(db).All(scopeCtx())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	theirs, err := ent.Query(db).All(sqlite.WithTenant(context.Background(), int64(5)))
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(drv.SQL()) != 2 {
		t.Fatalf("statements = %v, want one per tenant", drv.SQL())
	}
	if len(mine) != 1 || mine[0].Name != "mine" {
		t.Errorf("first tenant got %v", mine)
	}
	if len(theirs) != 1 || theirs[0].Name != "theirs" {
		t.Errorf("second tenant was served the first tenant's rows: %v", theirs)
	}

	// And the same tenant asking again is a hit, so the key is stable
	// rather than merely unique.
	if _, err := ent.Query(db).All(scopeCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(drv.SQL()) != 2 {
		t.Errorf("statements = %v, want the third read to hit the cache", drv.SQL())
	}
}

// A ctx that cannot name a tenant is refused before the cache is
// consulted, rather than after it answers.
func TestTheQueryCacheRefusesWithoutATenant(t *testing.T) {
	_, ent := ccTable("cr_rows")
	ent.WithCache(ccCache(t), time.Minute)
	drv := dropstest.New()
	db := sqlite.New(drv)

	if _, err := ent.Query(db).All(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// The PK namespace is keyed by the primary key alone, so a scoped
// entity must never read from it and never write to it — including an
// entity that declares no axis itself and takes one from its table.
func TestATableScopedEntityNeitherReadsNorWritesThePKCache(t *testing.T) {
	_, ent := ccTable("cp_rows")
	backing := ccCache(t)
	ent.WithCache(backing, time.Minute)

	drv := dropstest.New().
		AlwaysRows([]string{"id", "tenantId", "name"}, []any{int64(1), scopeTenant, "mine"})
	db := sqlite.New(drv)

	if _, err := ent.Get(db, scopeCtx(), int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := ent.Get(db, scopeCtx(), int64(1)); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Two Gets, two statements: the second did not answer from the PK
	// namespace.
	if got := drv.SQL(); len(got) != 2 {
		t.Fatalf("statements = %v, want one per Get", got)
	}
	// And every statement carried the axis, which is the reason the
	// cached path had to be skipped rather than merely warmed.
	for _, s := range drv.SQL() {
		if !strings.Contains(s, `("cp_rows"."tenantId" = ?)`) {
			t.Errorf("statement is unscoped:\n%s", s)
		}
	}
	if _, err := backing.Get(context.Background(), "drops:cp_rows:pk:1"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("a scoped row was written into the PK namespace")
	}
}

// A Create on a table-scoped entity does not populate the PK namespace
// either, and clears whatever is under the key — an entry left by an
// older build, or by a sibling entity that is not scoped, is never
// refreshed by the writes it should have followed and would answer with
// pre-scope values for as long as the backend kept it.
func TestATableScopedCreateClearsThePKCacheEntry(t *testing.T) {
	tbl, ent := ccTable("cw_rows")
	tbl.ScopeWritesByTenant(tbl.Col("tenantId"))
	backing := ccCache(t)
	ent.WithCache(backing, time.Minute)

	// Something else left an entry under this id.
	if err := backing.Set(context.Background(), "drops:cw_rows:pk:1", []byte("stale"), time.Minute); err != nil {
		t.Fatalf("Set: %v", err)
	}

	drv := dropstest.New().AlwaysRows([]string{"id"}, []any{int64(1)})
	db := sqlite.New(drv)
	row := ccRow{ID: 1, Name: "x"}
	if err := ent.Create(db, scopeCtx(), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := backing.Get(context.Background(), "drops:cw_rows:pk:1"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("the stale PK entry survived a scoped write")
	}
}
