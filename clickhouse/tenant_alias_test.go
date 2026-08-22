package clickhouse_test

import (
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/dropstest"
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
	ID       uint64 `drop:"id"`
	TenantID string `drop:"tenantId"`
	Path     string `drop:"path"`
}

// aliasTenantedEntity builds the fixture fresh per test: ScopeByTenant
// registers a context filter on the table, so a shared table would
// stack one per call.
func aliasTenantedEntity(t *testing.T, name string) *clickhouse.Entity[aliasTenanted] {
	t.Helper()
	tbl := clickhouse.NewTable(name)
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	clickhouse.Add(tbl, clickhouse.String("path"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ten, id)
	return clickhouse.NewEntity[aliasTenanted](tbl).ScopeByTenant(tbl.As("u").Col("tenantId"))
}

func TestScopeByTenantStoresTheEntitysOwnHandle(t *testing.T) {
	ent := aliasTenantedEntity(t, "zz_rows")
	db := clickhouse.New(nil)

	sql, args, err := ent.Query(db).ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `SELECT * FROM "zz_rows" WHERE ("zz_rows"."tenantId" = ?)`)
	checkArgs(t, args, "acme")
}

// The write half reads the same handle, through Table.tenantAxis, so
// the stamp is stored from the same place the predicate is.
func TestScopeByTenantStampsThroughTheEntitysOwnHandle(t *testing.T) {
	ent := aliasTenantedEntity(t, "zz_write_rows")
	drv := dropstest.New()
	db := clickhouse.New(drv)

	row := aliasTenanted{ID: 1, Path: "/"}
	if _, err := ent.Create(db, tenantCtx("acme"), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st := drv.Last()
	wantSQL := `INSERT INTO "zz_write_rows" ("id", "tenantId", "path") VALUES (?, ?, ?)`
	checkSQL(t, st.SQL, wantSQL)
	checkArgs(t, st.Args, uint64(1), "acme", "/")
}
