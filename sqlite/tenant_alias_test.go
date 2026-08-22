package sqlite_test

import (
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// ScopeByTenant takes a ColRef, so a handle taken off Table.As enters
// legally — and in a codegen'd schema, where the alias is how a query
// spells the table, it is the handle in scope at the call site.
//
// What is stored has to be the entity's OWN handle rather than the one
// passed in. The predicate is rendered into statements that name the
// declared relation, so an alias handle qualifies with an alias no such
// query has a FROM entry for, and the server cannot resolve it. That
// fails closed, but it fails at the server on a statement Go was happy
// with, which is why these assert on the rendered SQL rather than on
// an error.

type aliasTenanted struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// aliasTenantedEntity builds the fixture fresh per test: ScopeByTenant
// registers a context filter on the table, so a shared table would
// stack one per call.
func aliasTenantedEntity(t *testing.T, name string) *sqlite.Entity[aliasTenanted] {
	t.Helper()
	tbl := sqlite.NewTable(name)
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(tbl, sqlite.Text("title"))
	return sqlite.NewEntity[aliasTenanted](tbl).ScopeByTenant(tbl.As("u").Col("tenantId"))
}

func TestScopeByTenantStoresTheEntitysOwnHandle(t *testing.T) {
	ent := aliasTenantedEntity(t, "zz_rows")
	drv := dropstest.New()
	db := sqlite.New(drv)

	// Get renders the read half. The row does not have to come back:
	// the defect is in the statement, and a fixture holding one tenant
	// answers a phantom-qualified query and a correct one alike.
	_, _ = ent.Get(db, scopeCtx(), int64(1))
	st := drv.Last()
	wantSQL := `SELECT "zz_rows"."id", "zz_rows"."tenantId", "zz_rows"."title" ` +
		`FROM "zz_rows" WHERE ("zz_rows"."id" = ?) AND ("zz_rows"."tenantId" = ?)`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	if !sameArgs(st.Args, []any{int64(1), scopeTenant}) {
		t.Errorf("args:\n got = %#v\nwant = %#v", st.Args, []any{int64(1), scopeTenant})
	}
}

// The write half reads the same handle, through Table.tenantAxis, so
// the stamp is stored from the same place the predicate is.
func TestScopeByTenantStampsThroughTheEntitysOwnHandle(t *testing.T) {
	ent := aliasTenantedEntity(t, "zz_write_rows")
	drv := dropstest.New()
	db := sqlite.New(drv)

	row := aliasTenanted{ID: 1, Title: "x"}
	if err := ent.Create(db, scopeCtx(), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st := drv.Last()
	wantSQL := `INSERT INTO "zz_write_rows" ("id", "tenantId", "title") VALUES (?, ?, ?)`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	if !sameArgs(st.Args, []any{int64(1), scopeTenant, "x"}) {
		t.Errorf("args:\n got = %#v\nwant = %#v", st.Args, []any{int64(1), scopeTenant, "x"})
	}
}
