package mysql_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mysql"
)

// A table wearing a soft-delete guard and a tenancy guard at once —
// the pairing that makes an all-or-nothing Unscoped dangerous.
func mkFilteredTable() (*mysql.Table, *mysql.Col[int64]) {
	tbl := mysql.NewTable("posts")
	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false).Nullable())
	tenantID := mysql.Add(tbl, mysql.BigInt("tenantId").NotNull())
	tbl.AddFilter(mysql.FilterSoftDelete, deleted.IsNull())
	tbl.AddFilter(mysql.FilterTenant, tenantID.Eq(7))
	return tbl, id
}

func TestIgnoreFiltersDropsOnlyTheNamedOne(t *testing.T) {
	tbl, id := mkFilteredTable()
	db := mysql.New(&fakeDriver{})

	sel, _ := db.Select(id).From(tbl).IgnoreFilters(mysql.FilterSoftDelete).ToSQL()
	if strings.Contains(sel, "deletedAt") {
		t.Errorf("SELECT must drop the soft-delete guard: %s", sel)
	}
	if !strings.Contains(sel, "`tenantId`") {
		t.Errorf("SELECT must keep the tenancy guard: %s", sel)
	}

	upd, _ := db.Update(tbl).Set(id.Val(1)).IgnoreFilters(mysql.FilterSoftDelete).ToSQL()
	if strings.Contains(upd, "deletedAt` IS NULL") {
		t.Errorf("UPDATE must drop the soft-delete guard: %s", upd)
	}
	if !strings.Contains(upd, "`tenantId`") {
		t.Errorf("UPDATE must keep the tenancy guard: %s", upd)
	}

	del, _ := db.Delete(tbl).IgnoreFilters(mysql.FilterSoftDelete).Where(id.Eq(1)).ToSQL()
	if strings.Contains(del, "deletedAt` IS NULL") {
		t.Errorf("DELETE must drop the soft-delete guard: %s", del)
	}
	if !strings.Contains(del, "`tenantId`") {
		t.Errorf("DELETE must keep the tenancy guard: %s", del)
	}
}

func TestUnscopedStillDropsEveryFilter(t *testing.T) {
	tbl, id := mkFilteredTable()
	db := mysql.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).Unscoped().ToSQL()
	for _, frag := range []string{"deletedAt", "tenantId"} {
		if strings.Contains(sql, frag) {
			t.Errorf("Unscoped must drop %s: %s", frag, sql)
		}
	}
}

// A misspelt name leaves every filter standing — the direction that
// returns too few rows rather than too many.
func TestIgnoreFiltersUnknownNameIsInert(t *testing.T) {
	tbl, id := mkFilteredTable()
	db := mysql.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).IgnoreFilters("sofDelete").ToSQL()
	if !strings.Contains(sql, "deletedAt") {
		t.Errorf("a misspelt name must leave every filter standing: %s", sql)
	}
}

func TestAddFilterRejectsEmptyName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("AddFilter with an empty name must panic")
		}
	}()
	tbl := mysql.NewTable("posts")
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false).Nullable())
	tbl.AddFilter("", deleted.IsNull())
}

// DefaultFilter stays anonymous: no name reaches it, which is the
// reason AddFilter exists.
func TestAnonymousFilterCannotBeNamed(t *testing.T) {
	tbl := mysql.NewTable("posts")
	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false).Nullable())
	tbl.DefaultFilter(deleted.IsNull())
	if names := tbl.FilterNames(); len(names) != 0 {
		t.Errorf("an anonymous filter must contribute no name: %v", names)
	}
	db := mysql.New(&fakeDriver{})
	sql, _ := db.Select(id).From(tbl).IgnoreFilters(mysql.FilterSoftDelete).ToSQL()
	if !strings.Contains(sql, "deletedAt") {
		t.Errorf("no name can bypass an anonymous filter: %s", sql)
	}
}

// An alias is a snapshot of the table, and the filter names have to
// survive the copy or an aliased query cannot bypass them.
func TestAliasCarriesNamedFilters(t *testing.T) {
	tbl, _ := mkFilteredTable()
	a := tbl.As("p")
	if got := strings.Join(a.FilterNames(), ","); got != mysql.FilterSoftDelete+","+mysql.FilterTenant {
		t.Errorf("alias lost the filter names: %q", got)
	}
}

// The entity layer needs the same escape hatch the builders got. Without
// it, an entity query on a doubly-filtered table has only Unscoped —
// which is exactly the all-or-nothing the named filters exist to
// replace, and it costs the tenancy guard to read a deleted row.
func TestEntityQueryIgnoresOneFilter(t *testing.T) {
	tbl, id := mkFilteredTable()
	db := mysql.New(&fakeDriver{})
	type post struct {
		ID        int64      `drop:"id"`
		DeletedAt *time.Time `drop:"deletedAt"`
		TenantID  int64      `drop:"tenantId"`
	}
	ent := mysql.NewEntity[post](tbl)

	sql, _ := ent.Query(db).IgnoreFilters(mysql.FilterSoftDelete).ToSQL()
	if strings.Contains(sql, "deletedAt` IS NULL") {
		t.Errorf("the named filter must be gone: %s", sql)
	}
	if !strings.Contains(sql, "`tenantId` = ?") {
		t.Errorf("every other filter must stand: %s", sql)
	}

	// And the blunt instrument still drops both, so the two are
	// distinguishable rather than the same call under two names.
	blunt, _ := ent.Query(db).Unscoped().ToSQL()
	if strings.Contains(blunt, "WHERE") {
		t.Errorf("Unscoped must leave no filter behind: %s", blunt)
	}
	_ = id
}
