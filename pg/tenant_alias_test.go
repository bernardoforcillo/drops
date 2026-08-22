package pg_test

import (
	"context"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
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
//
// This dialect already stored the entity's own handle when sqlite and
// clickhouse did not. The tests are here so that all four are asked the
// question rather than three of them, and they assert the whole
// statement where [TestAliasScopeByTenantAcceptsAliasHandle] asserts a
// fragment of it.

type aliasTenantedRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// aliasTenantedEntity builds the fixture fresh per test: ScopeByTenant
// registers a context filter on the table, so a shared table would
// stack one per call.
func aliasTenantedEntity(t *testing.T, name string) *pg.Entity[aliasTenantedRow] {
	t.Helper()
	tbl := pg.NewTable(name)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("title"))
	return pg.NewEntity[aliasTenantedRow](tbl).ScopeByTenant(tbl.As("u").Col("tenantId"))
}

func aliasTenantCtx() context.Context {
	return pg.WithTenant(context.Background(), int64(77))
}

func TestScopeByTenantStoresTheEntitysOwnHandle(t *testing.T) {
	ent := aliasTenantedEntity(t, "zz_rows")
	drv := dropstest.New()
	db := pg.New(drv)

	// Get renders the read half. The row does not have to come back:
	// the defect is in the statement, and a fixture holding one tenant
	// answers a phantom-qualified query and a correct one alike.
	_, _ = ent.Get(db, aliasTenantCtx(), int64(1))
	st := drv.Last()
	wantSQL := `SELECT * FROM "zz_rows" ` +
		`WHERE ("zz_rows"."id" = $1) AND ("zz_rows"."tenantId" = $2) LIMIT $3`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	if !sameArgs(st.Args, []any{int64(1), int64(77), int64(1)}) {
		t.Errorf("args:\n got = %#v\nwant = %#v", st.Args, []any{int64(1), int64(77), int64(1)})
	}
}

// The write half reads the same handle, through Table.tenantAxis, so
// the stamp is stored from the same place the predicate is.
func TestScopeByTenantStampsThroughTheEntitysOwnHandle(t *testing.T) {
	ent := aliasTenantedEntity(t, "zz_write_rows")
	drv := dropstest.New().AlwaysRows([]string{"id"}, []any{int64(1)})
	db := pg.New(drv)

	row := aliasTenantedRow{ID: 1, Title: "x"}
	if err := ent.Create(db, aliasTenantCtx(), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st := drv.Last()
	wantSQL := `INSERT INTO "zz_write_rows" ("id", "tenantId", "title") VALUES ($1, $2, $3) ` +
		`RETURNING "zz_write_rows"."id", "zz_write_rows"."tenantId", "zz_write_rows"."title"`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	if !sameArgs(st.Args, []any{int64(1), int64(77), "x"}) {
		t.Errorf("args:\n got = %#v\nwant = %#v", st.Args, []any{int64(1), int64(77), "x"})
	}
}
