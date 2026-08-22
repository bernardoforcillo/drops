package clickhouse_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/dropstest"
)

// The tenant axis is a type before it is a value, and every defect in
// this file is a conversion that answered "is this the same tenant?"
// without looking at what the conversion did to the value.
//
// Two of them produce the one outcome a scoped write exists to rule
// out: a row ASSIGNED to one tenant by a statement whose ctx named
// another. Go converts an integer to a string as a rune, so a ctx
// tenant of 65 stamped into a String column writes "A"; a conversion
// between integer widths truncates, so a value and the ctx tenant can
// be made to agree by throwing away the bits that distinguish them.
//
// This dialect has no UPDATE, so the whole write side is the INSERT —
// which is also why the caller's struct matters here: Create stamps it
// before the bindings are taken, and a struct that comes back carrying
// a tenant nobody asked for is the next statement's input.

// typeCtx puts a tenant of the caller's own type on ctx — the point of
// every case below is that the type is not the column's.
func typeCtx(v any) context.Context {
	return clickhouse.WithTenant(context.Background(), v)
}

// strAxisRow's tenant column is String. The ctx tenants below are not.
type strAxisRow struct {
	ID       int64  `drop:"id"`
	TenantID string `drop:"tenantId"`
	Title    string `drop:"title"`
}

func strAxisEntity() *clickhouse.Entity[strAxisRow] {
	tbl := clickhouse.NewTable("str_rows")
	id := clickhouse.Add(tbl, clickhouse.Int64("id"))
	tenant := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	clickhouse.Add(tbl, clickhouse.String("title"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant, id)
	return clickhouse.NewEntity[strAxisRow](tbl).ScopeByTenant(tenant)
}

// numAxisRow's tenant column is a 64-bit integer, the width a caller
// whose tenant arrives as a plain int does not have.
type numAxisRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

func numAxisEntity() *clickhouse.Entity[numAxisRow] {
	tbl := clickhouse.NewTable("num_rows")
	id := clickhouse.Add(tbl, clickhouse.Int64("id"))
	tenant := clickhouse.Add(tbl, clickhouse.Int64("tenantId"))
	clickhouse.Add(tbl, clickhouse.String("title"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant, id)
	return clickhouse.NewEntity[numAxisRow](tbl).ScopeByTenant(tenant)
}

// wideAxisTable and narrowAxisTable differ only in the Go type of the
// tenant column, which is what makes a truncating pair expressible
// from both sides.
func wideAxisTable(name string) (tbl *clickhouse.Table, tenant *clickhouse.Col[int64], title *clickhouse.Col[string]) {
	t := clickhouse.NewTable(name)
	tenantCol := clickhouse.Add(t, clickhouse.Int64("tenantId"))
	titleCol := clickhouse.Add(t, clickhouse.String("title"))
	t.Engine(clickhouse.MergeTree()).OrderBy(tenantCol).
		ContextFilter(clickhouse.TenantFilter(tenantCol)).
		ScopeWritesByTenant(tenantCol)
	return t, tenantCol, titleCol
}

func narrowAxisTable(name string) (tbl *clickhouse.Table, tenant *clickhouse.Col[int32], title *clickhouse.Col[string]) {
	t := clickhouse.NewTable(name)
	tenantCol := clickhouse.Add(t, clickhouse.Int32("tenantId"))
	titleCol := clickhouse.Add(t, clickhouse.String("title"))
	t.Engine(clickhouse.MergeTree()).OrderBy(tenantCol).
		ContextFilter(clickhouse.TenantFilter(tenantCol)).
		ScopeWritesByTenant(tenantCol)
	return t, tenantCol, titleCol
}

// A ctx tenant of 65 and a String tenant column are not the same kind
// of tenant, and the conversion between them is a rune. Stamping it
// wrote "A" into the caller's struct — the value the next statement
// built from that struct would have carried.
func TestCreateRefusesANumericCtxTenantOnATextAxis(t *testing.T) {
	ent := strAxisEntity()
	drv := dropstest.New()
	db := clickhouse.New(drv)

	row := strAxisRow{ID: 7, Title: "x"}
	_, err := ent.Create(db, typeCtx(65), &row)
	if !errors.Is(err, clickhouse.ErrTenantMismatch) {
		t.Errorf("Create = %v, want ErrTenantMismatch", err)
	}
	if row.TenantID != "" {
		t.Errorf("ctx tenant 65 reached the String axis as %q", row.TenantID)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused Create still sent %d statement(s): %v", len(got), got)
	}
}

// int64(77) on the column and int(77) on ctx name one tenant, and a
// caller whose tenant does not round-trip through its transport as the
// column's exact type still gets a row written under it.
func TestCreateAcceptsACtxTenantOfAnotherIntegerType(t *testing.T) {
	ent := numAxisEntity()
	drv := dropstest.New()
	db := clickhouse.New(drv)

	row := numAxisRow{ID: 7, TenantID: 77, Title: "x"}
	if _, err := ent.Create(db, typeCtx(77), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	st := drv.Last()
	wantSQL := `INSERT INTO "num_rows" ("id", "tenantId", "title") VALUES (?, ?, ?)`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	wantArgs := []any{int64(7), int64(77), "x"}
	if !reflect.DeepEqual(st.Args, wantArgs) {
		t.Errorf("bound arguments: got = %v, want %v", st.Args, wantArgs)
	}
	if row.TenantID != 77 {
		t.Errorf("Create left the row's tenant at %d, want 77", row.TenantID)
	}
}

// A conversion that loses information does not name the same tenant,
// in either direction. int64(1<<32|77) and int32(77) convert onto each
// other's type and compare equal in whichever direction throws the
// high bits away — so a one-way comparison accepts the pair, and the
// statement carries the value the caller supplied rather than the
// tenant the ctx named.
func TestInsertRefusesATruncatingTenantConversion(t *testing.T) {
	const big = int64(1)<<32 | 77
	wide, wideTen, wideTitle := wideAxisTable("trunc_wide")
	narrow, narrowTen, narrowTitle := narrowAxisTable("trunc_narrow")
	tests := []struct {
		name string
		tbl  *clickhouse.Table
		vals []clickhouse.ColumnValue
		ctx  context.Context
	}{
		{
			name: "bound value wider than the ctx tenant",
			tbl:  wide,
			vals: []clickhouse.ColumnValue{wideTitle.Val("x"), wideTen.Val(big)},
			ctx:  typeCtx(int32(77)),
		},
		{
			name: "ctx tenant wider than the bound value",
			tbl:  narrow,
			vals: []clickhouse.ColumnValue{narrowTitle.Val("x"), narrowTen.Val(77)},
			ctx:  typeCtx(big),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := dropstest.New()
			db := clickhouse.New(drv)
			_, err := db.Insert(tt.tbl).Row(tt.vals...).Exec(tt.ctx)
			if !errors.Is(err, clickhouse.ErrTenantMismatch) {
				t.Errorf("Exec = %v, want ErrTenantMismatch", err)
			}
			if got := drv.Statements(); len(got) != 0 {
				t.Errorf("a refused INSERT still sent %d statement(s): %v", len(got), got)
			}
		})
	}
}
