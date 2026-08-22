package pg_test

import (
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// Patch is the one write in the package that never reads the row
// first, and its op list comes from the caller — a handler that builds
// ops out of the fields a request named puts whatever the request
// named into it. An op on the tenant column renders SET "tenantId" = ?
// beside a WHERE clause that still addresses the ctx tenant: one
// statement handing the row to somebody else, reported as one row
// affected, with nothing downstream to notice. Create and Update
// refuse that instruction when it arrives on a struct; this asks for
// the same rule where the op list can express it.

func patchAxisSchema() (ent *pg.Entity[patchAxisRow], tenant *pg.Col[int64], likes *pg.Col[int64]) {
	tbl := pg.NewTable("patch_axis_rows")
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	tenantCol := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	likesCol := pg.Add(tbl, pg.BigInt("likes").NotNull().Default("0"))
	return pg.NewEntity[patchAxisRow](tbl).ScopeByTenant(tenantCol), tenantCol, likesCol
}

type patchAxisRow struct {
	ID       int64 `drop:"id"`
	TenantID int64 `drop:"tenantId"`
	Likes    int64 `drop:"likes"`
}

func TestPatchRefusesToAssignTheTenantAxis(t *testing.T) {
	ent, tenant, likes := patchAxisSchema()
	tests := []struct {
		name string
		ops  []pg.PatchOp
	}{
		{
			name: "assigned to another tenant",
			ops:  []pg.PatchOp{pg.Set(tenant, int64(999))},
		},
		{
			name: "assigned to the ctx tenant",
			ops:  []pg.PatchOp{pg.Set(tenant, int64(77))},
		},
		{
			name: "incremented",
			ops:  []pg.PatchOp{pg.Inc(tenant, int64(1))},
		},
		{
			name: "buried in a list of ordinary ops",
			ops: []pg.PatchOp{
				pg.Inc(likes, int64(1)),
				pg.Set(tenant, int64(999)),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drv := dropstest.New()
			db := pg.New(drv)
			_, err := ent.Patch(db, tenantCtx(int64(77)), int64(7), tt.ops...)
			if !errors.Is(err, pg.ErrTenantMismatch) {
				t.Errorf("Patch = %v, want ErrTenantMismatch", err)
			}
			if got := drv.Statements(); len(got) != 0 {
				t.Errorf("a refused Patch still sent %d statement(s): %v", len(got), got)
			}
		})
	}
}

// The ops that do not name the axis are untouched, and the statement
// still carries the tenant predicate that makes the patch safe.
func TestPatchLeavesOrdinaryOpsAlone(t *testing.T) {
	ent, _, likes := patchAxisSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	if _, err := ent.Patch(db, tenantCtx(int64(77)), int64(7), pg.Inc(likes, int64(1))); err != nil {
		t.Fatalf("Patch: %v", err)
	}
	st := drv.Last()
	wantSQL := `UPDATE "patch_axis_rows" SET "likes" = "patch_axis_rows"."likes" + $1 ` +
		`WHERE ("patch_axis_rows"."id" = $2) AND ("patch_axis_rows"."tenantId" = $3)`
	if st.SQL != wantSQL {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", st.SQL, wantSQL)
	}
}
