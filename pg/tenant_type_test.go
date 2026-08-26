package pg_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// The tenant axis is a type before it is a value, and every defect in
// this file is a conversion that answered "is this the same tenant?"
// without looking at what the conversion did to the value.
//
// Two of them produce the one outcome a scoped write exists to rule
// out: a statement that ASSIGNS one tenant while ADDRESSING another,
// which hands the row to whoever owns the value in the SET clause. Go
// converts an integer to a string as a rune, so a ctx tenant of 65
// stamped into a TEXT column writes "A"; a conversion between integer
// widths truncates, so a value and the ctx tenant can be made to agree
// by throwing away the bits that distinguish them.
//
// The assertions are on the rendered statement and its arguments, and
// on the refusals sending no statement at all. A fixture holding one
// tenant's rows cannot see any of this.

// strAxisRow's tenant column is TEXT. The ctx tenants below are not.
type strAxisRow struct {
	ID       int64  `drop:"id"`
	TenantID string `drop:"tenantId"`
	Title    string `drop:"title"`
}

func strAxisEntity() *pg.Entity[strAxisRow] {
	tbl := pg.NewTable("str_rows")
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.Text("tenantId").NotNull())
	pg.Add(tbl, pg.Text("title").NotNull())
	return pg.NewEntity[strAxisRow](tbl).ScopeByTenant(tenant)
}

// numAxisRow's tenant column is a 64-bit integer, the width a caller
// whose tenant arrives as a plain int does not have.
type numAxisRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

func numAxisEntity() *pg.Entity[numAxisRow] {
	tbl := pg.NewTable("num_rows")
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("title").NotNull())
	return pg.NewEntity[numAxisRow](tbl).ScopeByTenant(tenant)
}

// wideAxisTable and narrowAxisTable differ only in the Go type of the
// tenant column, which is what makes a truncating pair expressible
// from both sides.
func wideAxisTable(name string) (tbl *pg.Table, tenant *pg.Col[int64], title *pg.Col[string]) {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigInt("id").PrimaryKey())
	tenantCol := pg.Add(t, pg.BigInt("tenantId").NotNull())
	titleCol := pg.Add(t, pg.Text("title"))
	t.ContextFilter(pg.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)
	return t, tenantCol, titleCol
}

func narrowAxisTable(name string) (tbl *pg.Table, tenant *pg.Col[int32], title *pg.Col[string]) {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigInt("id").PrimaryKey())
	tenantCol := pg.Add(t, pg.Integer("tenantId").NotNull())
	titleCol := pg.Add(t, pg.Text("title"))
	t.ContextFilter(pg.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)
	return t, tenantCol, titleCol
}

// A ctx tenant of 65 and a TEXT tenant column are not the same kind of
// tenant, and the conversion between them is a rune. Stamping it wrote
// "A" into the SET clause while the WHERE clause still addressed 65 —
// one statement naming two tenants, reported as a success.
func TestUpdateRefusesANumericCtxTenantOnATextAxis(t *testing.T) {
	ent := strAxisEntity()
	drv := dropstest.New()
	db := pg.New(drv)

	row := strAxisRow{ID: 7, Title: "x"}
	err := ent.Update(db, tenantCtx(65), &row)
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Errorf("Update = %v, want ErrTenantMismatch", err)
	}
	if row.TenantID != "" {
		t.Errorf("ctx tenant 65 reached the TEXT axis as %q", row.TenantID)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused Update still sent %d statement(s): %v", len(got), got)
	}
}

// The same refusal on the way in, so Create and Update agree about
// which tenants a TEXT axis can hold.
func TestCreateRefusesANumericCtxTenantOnATextAxis(t *testing.T) {
	ent := strAxisEntity()
	drv := dropstest.New()
	db := pg.New(drv)

	row := strAxisRow{ID: 7, Title: "x"}
	err := ent.Create(db, tenantCtx(65), &row)
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Errorf("Create = %v, want ErrTenantMismatch", err)
	}
	if row.TenantID != "" {
		t.Errorf("ctx tenant 65 reached the TEXT axis as %q", row.TenantID)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused Create still sent %d statement(s): %v", len(got), got)
	}
}

// int64(77) on the column and int(77) on ctx name one tenant. Compared
// with reflect.DeepEqual they are a mismatch, so the refusal fired on a
// match and Update was unusable for every caller whose tenant does not
// round-trip through its transport as the column's exact type.
func TestUpdateAcceptsACtxTenantOfAnotherIntegerType(t *testing.T) {
	ent := numAxisEntity()
	drv := dropstest.New().Rows(
		[]string{"id", "tenantId", "title"},
		[]any{int64(7), int64(77), "x"})
	db := pg.New(drv)

	row := numAxisRow{ID: 7, TenantID: 77, Title: "x"}
	if err := ent.Update(db, tenantCtx(77), &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st := drv.Last()
	wantSQL := `UPDATE "num_rows" SET "tenantId" = $1, "title" = $2 ` +
		`WHERE ("num_rows"."id" = $3) AND ("num_rows"."tenantId" = $4) ` +
		`RETURNING "num_rows"."id", "num_rows"."tenantId", "num_rows"."title"`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	wantArgs := []any{int64(77), "x", int64(7), 77}
	if !reflect.DeepEqual(st.Args, wantArgs) {
		t.Errorf("bound arguments: got = %v, want %v", st.Args, wantArgs)
	}
	if row.TenantID != 77 {
		t.Errorf("Update left the row's tenant at %d, want 77", row.TenantID)
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
		tbl  *pg.Table
		row  []pg.ColumnValue
		ctx  context.Context
	}{
		{
			name: "bound value wider than the ctx tenant",
			tbl:  wide,
			row:  []pg.ColumnValue{wideTitle.Val("x"), wideTen.Val(big)},
			ctx:  tenantCtx(int32(77)),
		},
		{
			name: "ctx tenant wider than the bound value",
			tbl:  narrow,
			row:  []pg.ColumnValue{narrowTitle.Val("x"), narrowTen.Val(77)},
			ctx:  tenantCtx(big),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)
			_, err := db.Insert(tt.tbl).Row(tt.row...).Exec(tt.ctx)
			if !errors.Is(err, pg.ErrTenantMismatch) {
				t.Errorf("Exec = %v, want ErrTenantMismatch", err)
			}
			if got := drv.Statements(); len(got) != 0 {
				t.Errorf("a refused INSERT still sent %d statement(s): %v", len(got), got)
			}
		})
	}
}
