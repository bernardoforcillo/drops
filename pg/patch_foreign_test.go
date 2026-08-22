package pg_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// Eleven rounds of this phase asked "is the tenant value right?" and
// none asked "is this handle even ours?".
//
// The axis guard compares op columns to the axis with Column.key,
// which collapses alias copies onto the column they were declared as
// and therefore calls a handle for the SAME-NAMED column of a
// DIFFERENT table object a stranger. The renderer disagrees: the SET
// list writes the bare column name, so OtherTable.TenantID renders as
// "tenantId" and the server applies it to the row this UPDATE
// addresses. A patch built from OtherTableCols.TenantID walked past
// the guard and moved a row to another tenant, on a live server, with
// no error — and in a codegen'd schema where every table exports its
// own <Table>Cols.TenantID that is one character away in any file.
//
// The rule these tests ask for is wider than the axis, because the
// mistake is: an op naming another table's column is a bug whatever
// the column holds. The statement it renders addresses a relation the
// query does not name.

// foreignPatchSchema returns the entity under test plus handles taken
// off a SECOND table that declares the same column names. Two tables
// with a "tenantId" and a "likes" is what any multi-tenant schema
// looks like; the handles are what an editor's completion offers.
func foreignPatchSchema() (ent *pg.Entity[patchAxisRow], foreignTenant, foreignLikes *pg.Col[int64]) {
	ent, _, _ = patchAxisSchema()
	other := pg.NewTable("other_axis_rows")
	pg.Add(other, pg.BigInt("id").PrimaryKey())
	ft := pg.Add(other, pg.BigInt("tenantId").NotNull())
	fl := pg.Add(other, pg.BigInt("likes").NotNull().Default("0"))
	return ent, ft, fl
}

func TestPatchRefusesAnOpNamingAnotherTablesColumn(t *testing.T) {
	tests := []struct {
		name string
		ops  func(foreignTenant, foreignLikes *pg.Col[int64]) []pg.PatchOp
	}{
		{
			// The leak the phase closes: this rendered
			// UPDATE "patch_axis_rows" SET "tenantId" = $1
			// WHERE ("patch_axis_rows"."id" = $2)
			//   AND ("patch_axis_rows"."tenantId" = $3)
			// with args [999 7 77] — confined to the caller's own
			// rows and giving one of them away.
			name: "the tenant axis, reached through another table's handle",
			ops: func(ft, _ *pg.Col[int64]) []pg.PatchOp {
				return []pg.PatchOp{pg.Set(ft, int64(999))}
			},
		},
		{
			name: "an ordinary column of another table",
			ops: func(_, fl *pg.Col[int64]) []pg.PatchOp {
				return []pg.PatchOp{pg.Set(fl, int64(5))}
			},
		},
		{
			name: "an increment of another table's column",
			ops: func(_, fl *pg.Col[int64]) []pg.PatchOp {
				return []pg.PatchOp{pg.Inc(fl, int64(1))}
			},
		},
		{
			name: "buried in a list of ops on this table's own columns",
			ops: func(ft, _ *pg.Col[int64]) []pg.PatchOp {
				_, _, likes := patchAxisSchema()
				return []pg.PatchOp{pg.Inc(likes, int64(1)), pg.Set(ft, int64(999))}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ent, ft, fl := foreignPatchSchema()
			drv := dropstest.New()
			db := pg.New(drv)
			_, err := ent.Patch(db, tenantCtx(int64(77)), int64(7), tt.ops(ft, fl)...)
			if !errors.Is(err, pg.ErrForeignColumn) {
				t.Errorf("Patch = %v, want ErrForeignColumn", err)
			}
			// A foreign column is not a tenant mismatch, and saying so
			// sends the caller to their tenancy instead of to the
			// import that handed them the wrong handle.
			if errors.Is(err, pg.ErrTenantMismatch) {
				t.Errorf("Patch = %v, want a foreign-column error rather than a tenant one", err)
			}
			if got := drv.Statements(); len(got) != 0 {
				t.Errorf("a refused Patch still sent %d statement(s): %v", len(got), got)
			}
		})
	}
}

// The refusal names the handle's own table, because "tenantId" alone
// reads as the right column and the whole mistake is which table it
// came from.
func TestPatchForeignColumnErrorNamesBothTables(t *testing.T) {
	ent, ft, _ := foreignPatchSchema()
	db := pg.New(dropstest.New())
	_, err := ent.Patch(db, tenantCtx(int64(77)), int64(7), pg.Set(ft, int64(999)))
	if err == nil {
		t.Fatal("Patch accepted an op naming another table's column")
	}
	for _, want := range []string{"other_axis_rows.tenantId", "patch_axis_rows"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// An alias handle is not foreign. Aliasing is a query-scope rename of
// the same column, so a handle taken off Table.As has to keep
// answering as the column it was declared as — the guard must not
// mistake the two, or every entity built on an alias becomes
// unpatchable.
func TestPatchAcceptsAnAliasHandleForItsOwnColumn(t *testing.T) {
	ent, _, likes := patchAxisSchema()
	drv := dropstest.New()
	db := pg.New(drv)
	alias := likes.Table().As("p")
	aliasLikes := alias.Col("likes")

	if _, err := ent.Patch(db, tenantCtx(int64(77)), int64(7), pg.Inc(&pg.Col[int64]{Column: aliasLikes}, int64(1))); err != nil {
		t.Fatalf("Patch with an alias handle for its own column: %v", err)
	}
	st := drv.Last()
	wantSQL := `UPDATE "patch_axis_rows" SET "likes" = "p"."likes" + $1 ` +
		`WHERE ("patch_axis_rows"."id" = $2) AND ("patch_axis_rows"."tenantId" = $3)`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
	wantArgs := []any{int64(1), int64(7), int64(77)}
	if !sameArgs(st.Args, wantArgs) {
		t.Errorf("args:\n got = %#v\nwant = %#v", st.Args, wantArgs)
	}
}

// The axis handle itself arrives from Table.ScopeWritesByTenant, which
// takes whatever it is given. A schema that declared the write axis
// with another table's handle — the same one-character mistake, made
// one level up — left the axis check comparing its own column against
// a stranger and matching nothing, so the op it exists to refuse went
// through. It is asked by rendered name now, so the declaration
// mistake costs a confusing error message and not a row.
func TestPatchRefusesTheAxisEvenWhenTheAxisWasDeclaredForeign(t *testing.T) {
	tbl := pg.NewTable("misdeclared_rows")
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.BigInt("likes").NotNull().Default("0"))
	ent := pg.NewEntity[patchAxisRow](tbl)

	other := pg.NewTable("misdeclared_source")
	pg.Add(other, pg.BigInt("id").PrimaryKey())
	foreign := pg.Add(other, pg.BigInt("tenantId").NotNull())
	tbl.ContextFilter(pg.TenantFilter(tenant)).ScopeWritesByTenant(foreign)

	drv := dropstest.New()
	db := pg.New(drv)
	_, err := ent.Patch(db, tenantCtx(int64(77)), int64(7), pg.Set(tenant, int64(999)))
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Errorf("Patch = %v, want ErrTenantMismatch", err)
	}
	if got := drv.Statements(); len(got) != 0 {
		t.Errorf("a refused Patch still sent %d statement(s): %v", len(got), got)
	}
}
