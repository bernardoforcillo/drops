package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// filterSchema is a table wearing all three flavours of global filter
// at once: a named soft-delete guard, a named tenancy guard, and one
// anonymous predicate registered the old way.
func filterSchema() (*pg.Table, *pg.Col[int64]) {
	t := pg.NewTable("docs")
	id := pg.Add(t, pg.BigSerial("id").PrimaryKey())
	deletedAt := pg.Add(t, pg.Timestamp("deletedAt", true).Nullable())
	tenantID := pg.Add(t, pg.BigInt("tenantId").NotNull())
	archived := pg.Add(t, pg.Boolean("archived").NotNull())

	t.AddFilter(pg.FilterSoftDelete, deletedAt.IsNull())
	t.AddFilter(pg.FilterTenant, tenantID.Eq(7))
	t.DefaultFilter(archived.Eq(false))
	return t, id
}

// The whole point of naming a filter: stepping around one must not
// step around the rest. A report that wants soft-deleted rows keeps
// its tenancy predicate.
func TestIgnoreFiltersDropsOnlyTheNamedOne(t *testing.T) {
	tbl, id := filterSchema()
	db := pg.New(&fakeDriver{})

	sel, _ := db.Select(id).From(tbl).IgnoreFilters(pg.FilterSoftDelete).ToSQL()
	if strings.Contains(sel, "deletedAt") {
		t.Errorf("SELECT must drop the named soft-delete filter: %s", sel)
	}
	if !strings.Contains(sel, `"tenantId"`) {
		t.Errorf("SELECT must keep the tenancy filter: %s", sel)
	}
	if !strings.Contains(sel, `"archived"`) {
		t.Errorf("SELECT must keep the anonymous filter: %s", sel)
	}

	upd, _ := db.Update(tbl).Set(id.Val(1)).IgnoreFilters(pg.FilterSoftDelete).ToSQL()
	if strings.Contains(upd, `"deletedAt" IS NULL`) {
		t.Errorf("UPDATE must drop the named soft-delete filter: %s", upd)
	}
	if !strings.Contains(upd, `"tenantId"`) {
		t.Errorf("UPDATE must keep the tenancy filter: %s", upd)
	}

	del, _ := db.Delete(tbl).IgnoreFilters(pg.FilterSoftDelete).Where(id.Eq(1)).ToSQL()
	if strings.Contains(del, `"deletedAt" IS NULL`) {
		t.Errorf("DELETE must drop the named soft-delete filter: %s", del)
	}
	if !strings.Contains(del, `"tenantId"`) {
		t.Errorf("DELETE must keep the tenancy filter: %s", del)
	}
}

// Several names at once, and every unmentioned filter stands.
func TestIgnoreFiltersAcceptsSeveralNames(t *testing.T) {
	tbl, id := filterSchema()
	db := pg.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).
		IgnoreFilters(pg.FilterSoftDelete, pg.FilterTenant).ToSQL()
	if strings.Contains(sql, "deletedAt") || strings.Contains(sql, "tenantId") {
		t.Errorf("both named filters must go: %s", sql)
	}
	if !strings.Contains(sql, `"archived"`) {
		t.Errorf("the anonymous filter must survive: %s", sql)
	}
}

// Unscoped is the blunt instrument and is documented as such: it takes
// every filter, named or not.
func TestUnscopedStillDropsEveryFilter(t *testing.T) {
	tbl, id := filterSchema()
	db := pg.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).Unscoped().ToSQL()
	for _, frag := range []string{"deletedAt", "tenantId", "archived"} {
		if strings.Contains(sql, frag) {
			t.Errorf("Unscoped must drop %s: %s", frag, sql)
		}
	}
}

// A name no filter carries leaves the query exactly as it was — the
// typo direction that returns too few rows, never too many.
func TestIgnoreFiltersUnknownNameIsInert(t *testing.T) {
	tbl, id := filterSchema()
	db := pg.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).IgnoreFilters("sofDelete").ToSQL()
	if !strings.Contains(sql, "deletedAt") {
		t.Errorf("a misspelt name must leave every filter standing: %s", sql)
	}
}

// An empty name would read as named at the call site and behave as
// anonymous at the query — exactly the confusion the feature removes.
func TestAddFilterRejectsEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("AddFilter with an empty name must panic")
		}
	}()
	tbl := pg.NewTable("docs")
	tbl.AddFilter("", drops.Raw("1 = 1"))
}

// DefaultFilter's predicates stay anonymous: nothing but Unscoped can
// reach them, which is the reason AddFilter exists.
func TestAnonymousFilterCannotBeNamed(t *testing.T) {
	tbl := pg.NewTable("docs")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	archived := pg.Add(tbl, pg.Boolean("archived").NotNull())
	tbl.DefaultFilter(archived.Eq(false))
	if names := tbl.FilterNames(); len(names) != 0 {
		t.Errorf("an anonymous filter must contribute no name: %v", names)
	}
	db := pg.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).IgnoreFilters("archived").ToSQL()
	if !strings.Contains(sql, `"archived"`) {
		t.Errorf("no name can bypass an anonymous filter: %s", sql)
	}
}

func TestFilterNamesReportsRegistrationOrder(t *testing.T) {
	tbl, _ := filterSchema()
	got := strings.Join(tbl.FilterNames(), ",")
	want := pg.FilterSoftDelete + "," + pg.FilterTenant
	if got != want {
		t.Errorf("FilterNames = %q, want %q", got, want)
	}
	if n := len(tbl.Filters()); n != 3 {
		t.Errorf("Filters must include the anonymous one too, got %d", n)
	}
}

// SoftDeleteMixin's guard has to arrive under the constant, or nobody
// can name it.
func TestSoftDeleteMixinRegistersNamedFilter(t *testing.T) {
	tbl := pg.NewTable("posts")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.ApplyMixins(tbl, &pg.SoftDeleteMixin{})
	if names := tbl.FilterNames(); len(names) != 1 || names[0] != pg.FilterSoftDelete {
		t.Fatalf("SoftDeleteMixin must register %q, got %v", pg.FilterSoftDelete, names)
	}
	db := pg.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).IgnoreFilters(pg.FilterSoftDelete).ToSQL()
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("the mixin's guard must be bypassable by name: %s", sql)
	}
}

// IgnoreFilters drops predicates, not the DELETE→UPDATE rewrite. Only
// Unscoped means "really delete the row".
func TestIgnoreFiltersKeepsTheSoftDeleteRewrite(t *testing.T) {
	tbl := pg.NewTable("posts")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.ApplyMixins(tbl, &pg.SoftDeleteMixin{})
	db := pg.New(&fakeDriver{})

	sql, _ := db.Delete(tbl).IgnoreFilters(pg.FilterSoftDelete).Where(id.Eq(1)).ToSQL()
	if !strings.HasPrefix(sql, "UPDATE") {
		t.Errorf("IgnoreFilters must leave the rewrite in place: %s", sql)
	}
	sql, _ = db.Delete(tbl).Unscoped().Where(id.Eq(1)).ToSQL()
	if !strings.HasPrefix(sql, "DELETE") {
		t.Errorf("Unscoped must produce a real DELETE: %s", sql)
	}
}

// An alias is a snapshot of the table, filters included — and the
// names have to survive the copy or an aliased query cannot bypass
// them.
func TestAliasCarriesNamedFilters(t *testing.T) {
	tbl, _ := filterSchema()
	a := tbl.As("d")
	if names := strings.Join(a.FilterNames(), ","); names != pg.FilterSoftDelete+","+pg.FilterTenant {
		t.Errorf("alias lost the filter names: %v", names)
	}
}

// --- entity-level tenancy ---------------------------------------------

type filterDoc struct {
	ID       int64 `drop:"id,primaryKey,autoIncrement"`
	TenantID int64 `drop:"tenantId,notNull"`
	Name     string
}

func filterEntity(t *testing.T) *pg.Entity[filterDoc] {
	t.Helper()
	tbl := pg.NewTable("docs")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenantID := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("name").NotNull())
	pg.ApplyMixins(tbl, &pg.SoftDeleteMixin{})
	return pg.NewEntity[filterDoc](tbl).ScopeByTenant(tenantID)
}

// The headline case: ask for the soft-deleted rows and keep the
// tenancy predicate. Unscoped is what you must NOT have to reach for.
func TestEntityIgnoreSoftDeleteKeepsTenantPredicate(t *testing.T) {
	ent := filterEntity(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(7))

	if _, err := ent.Query(db).IgnoreFilters(pg.FilterSoftDelete).All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	sql := fd.queries[0]
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("soft-delete guard must be gone: %s", sql)
	}
	if !strings.Contains(sql, `"tenantId" = $`) {
		t.Errorf("tenancy predicate must survive: %s", sql)
	}
}

// Unscoped is blunt about the table's filters and deliberately blunt
// about nothing else: the ctx tenant guard is not a table filter and
// stays put, so an administrative "show me deleted rows" cannot leak
// sideways into another customer's data.
func TestEntityUnscopedStillScopesToTenant(t *testing.T) {
	ent := filterEntity(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(7))

	if _, err := ent.Query(db).Unscoped().All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	sql := fd.queries[0]
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("Unscoped must drop the table's filters: %s", sql)
	}
	if !strings.Contains(sql, `"tenantId" = $`) {
		t.Errorf("Unscoped must not reach the ctx tenant guard: %s", sql)
	}
}

// Crossing tenants is a real need; it just has to be spelled out.
func TestEntityIgnoreFilterTenantDropsTheGuard(t *testing.T) {
	ent := filterEntity(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	ctx := pg.WithTenant(context.Background(), int64(7))

	if _, err := ent.Query(db).IgnoreFilters(pg.FilterTenant).All(ctx); err != nil {
		t.Fatalf("All: %v", err)
	}
	sql := fd.queries[0]
	if strings.Contains(sql, "tenantId") {
		t.Errorf("IgnoreFilters(FilterTenant) must drop the guard: %s", sql)
	}
	if !strings.Contains(sql, "deletedAt") {
		t.Errorf("the soft-delete guard must survive: %s", sql)
	}
}

// A cross-tenant read the caller asked for by name must not then trip
// over the missing-tenant error the guard raises.
func TestEntityIgnoreFilterTenantNeedsNoCtxTenant(t *testing.T) {
	ent := filterEntity(t)
	fd := &fakeDriver{}
	db := pg.New(fd)
	if _, err := ent.Query(db).IgnoreFilters(pg.FilterTenant).All(context.Background()); err != nil {
		t.Fatalf("All: %v", err)
	}
}
