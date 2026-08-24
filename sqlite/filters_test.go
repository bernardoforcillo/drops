package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// A table wearing a soft-delete guard and a tenancy guard at once —
// the pairing that makes an all-or-nothing Unscoped dangerous.
func mkFilteredTable() (*sqlite.Table, *sqlite.Col[int64], sqlite.SoftDeleteCols) {
	posts := sqlite.NewTable("posts")
	id := sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	tenantID := sqlite.Add(posts, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(posts, sqlite.Text("title").NotNull())
	sd := sqlite.SoftDelete(posts)
	posts.AddFilter(sqlite.FilterTenant, tenantID.Eq(7))
	return posts, id, sd
}

func TestIgnoreFiltersDropsOnlyTheNamedOne(t *testing.T) {
	posts, id, _ := mkFilteredTable()
	db := sqlite.New(&fakeDriver{})

	sel, _ := db.Select(id).From(posts).IgnoreFilters(sqlite.FilterSoftDelete).ToSQL()
	if strings.Contains(sel, "deletedAt") {
		t.Errorf("SELECT must drop the soft-delete guard: %s", sel)
	}
	if !strings.Contains(sel, `"tenantId"`) {
		t.Errorf("SELECT must keep the tenancy guard: %s", sel)
	}

	upd, _ := db.Update(posts).Set(id.Val(1)).IgnoreFilters(sqlite.FilterSoftDelete).ToSQL()
	if strings.Contains(upd, "deletedAt IS NULL") {
		t.Errorf("UPDATE must drop the soft-delete guard: %s", upd)
	}
	if !strings.Contains(upd, `"tenantId"`) {
		t.Errorf("UPDATE must keep the tenancy guard: %s", upd)
	}

	del, _ := db.Delete(posts).IgnoreFilters(sqlite.FilterSoftDelete).Where(id.Eq(1)).ToSQL()
	if !strings.Contains(del, `"tenantId"`) {
		t.Errorf("DELETE must keep the tenancy guard: %s", del)
	}
}

func TestUnscopedStillDropsEveryFilter(t *testing.T) {
	posts, id, _ := mkFilteredTable()
	db := sqlite.New(&fakeDriver{})
	sql, _ := db.Select(id).From(posts).Unscoped().ToSQL()
	for _, frag := range []string{"deletedAt", "tenantId"} {
		if strings.Contains(sql, frag) {
			t.Errorf("Unscoped must drop %s: %s", frag, sql)
		}
	}
}

func TestSoftDeleteRegistersNamedFilterOnce(t *testing.T) {
	posts := sqlite.NewTable("posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	sqlite.ApplyMixins(posts, &sqlite.SoftDeleteMixin{})
	if names := posts.FilterNames(); len(names) != 1 || names[0] != sqlite.FilterSoftDelete {
		t.Fatalf("SoftDeleteMixin must register exactly one %q filter, got %v",
			sqlite.FilterSoftDelete, names)
	}
}

// SoftDeleteByID and Restore have to reach the hidden row without
// abandoning the rest of the table's scoping — a soft delete that
// steps around a tenancy guard writes to somebody else's row.
func TestSoftDeleteWritesStayInsideTheOtherFilters(t *testing.T) {
	posts, _, sd := mkFilteredTable()
	ent := sqlite.NewEntity[filteredPost](posts)
	rec := &fakeDriver{}
	db := sqlite.New(rec)
	ctx := context.Background()

	if _, err := ent.SoftDeleteByID(db, ctx, int64(1), sd); err != nil {
		t.Fatalf("SoftDeleteByID: %v", err)
	}
	if _, err := ent.Restore(db, ctx, int64(1), sd); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, sql := range rec.queries {
		if strings.Contains(sql, "deletedAt IS NULL") {
			t.Errorf("must step around the soft-delete guard: %s", sql)
		}
		if !strings.Contains(sql, `"tenantId"`) {
			t.Errorf("must stay inside the tenancy guard: %s", sql)
		}
	}
}

type filteredPost struct {
	ID       int64 `drop:"id,primaryKey"`
	TenantID int64 `drop:"tenantId,notNull"`
	Title    string
}

// The entity's ctx tenant guard is not a table filter, so Unscoped
// must not reach it; only naming it does.
func TestEntityTenantGuardSurvivesUnscoped(t *testing.T) {
	posts := sqlite.NewTable("posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	tenantID := sqlite.Add(posts, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(posts, sqlite.Text("title").NotNull())
	sqlite.SoftDelete(posts)
	ent := sqlite.NewEntity[filteredPost](posts).ScopeByTenant(tenantID)

	rec := &fakeDriver{}
	db := sqlite.New(rec)
	ctx := sqlite.WithTenant(context.Background(), int64(7))

	if _, err := ent.Query(db).Unscoped().All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	if sql := rec.queries[0]; !strings.Contains(sql, `"tenantId" = ?`) {
		t.Errorf("Unscoped must not reach the ctx tenant guard: %s", sql)
	}

	rec2 := &fakeDriver{}
	db2 := sqlite.New(rec2)
	if _, err := ent.Query(db2).IgnoreFilters(sqlite.FilterSoftDelete).All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	sql := rec2.queries[0]
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("soft-delete guard must be gone: %s", sql)
	}
	if !strings.Contains(sql, `"tenantId" = ?`) {
		t.Errorf("tenancy predicate must survive: %s", sql)
	}
}

// Crossing tenants is legitimate; it just has to be spelled out, and
// must not then trip over the missing-tenant error.
func TestEntityIgnoreFilterTenantDropsTheGuard(t *testing.T) {
	posts := sqlite.NewTable("posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	tenantID := sqlite.Add(posts, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(posts, sqlite.Text("title").NotNull())
	ent := sqlite.NewEntity[filteredPost](posts).ScopeByTenant(tenantID)

	rec := &fakeDriver{}
	db := sqlite.New(rec)
	if _, err := ent.Query(db).IgnoreFilters(sqlite.FilterTenant).All(context.Background()); err != nil {
		t.Fatalf("All: %v", err)
	}
	if sql := rec.queries[0]; strings.Contains(sql, `"tenantId" = ?`) {
		t.Errorf("IgnoreFilters(FilterTenant) must drop the guard: %s", sql)
	}
}
