package pg_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// The entity paths all refused to move a row between tenants — Create
// and Update stamp the axis from ctx, Patch refuses an op that names
// it, the INSERT stamps every row and the ON CONFLICT branch drops the
// assignment — and db.Update(), which the changelog shipped as a
// first-class scoped surface, refused nothing:
//
//	UPDATE "gadgets" SET "tenantId" = $1 WHERE "id" = $2 AND "tenantId" = $3
//	args: [999 7 3]
//
// That is the shape the normative policy block describes in section 2:
// the WHERE clause is correctly confined to the caller's own rows and
// the SET list gives one of them away, so the half a review checks is
// the correct half. It needs no foreign handle and no unusual import,
// only the table's own column and the obvious spelling.
//
// The assertions are on the rendered statement and its arguments,
// because the defect is what the statement assigns rather than what
// comes back from it.

// updateAxisSchema returns a table whose write axis is declared through
// its entity, so both halves — the WHERE clause's predicate and the SET
// list's refusal — are in play.
func updateAxisSchema() (tbl *pg.Table, id, tenant *pg.Col[int64], name *pg.Col[string]) {
	return gadgetSchema()
}

func TestUpdateRefusesToAssignTheTenantAxis(t *testing.T) {
	tests := []struct {
		name     string
		build    func(u *pg.UpdateBuilder, tenant, foreign *pg.Col[int64], name *pg.Col[string]) *pg.UpdateBuilder
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The leak, in the spelling any handler would reach for.
			name: "the table's own handle carrying another tenant",
			build: func(u *pg.UpdateBuilder, tenant, _ *pg.Col[int64], _ *pg.Col[string]) *pg.UpdateBuilder {
				return u.Set(tenant.Val(999))
			},
			wantErr: pg.ErrTenantMismatch,
		},
		{
			// The same assignment made through another table's handle
			// for a column of the same name. It renders identically,
			// so it is refused identically.
			name: "another table's handle for the same column name",
			build: func(u *pg.UpdateBuilder, _, foreign *pg.Col[int64], _ *pg.Col[string]) *pg.UpdateBuilder {
				return u.Set(foreign.Val(999))
			},
			wantErr: pg.ErrTenantMismatch,
		},
		{
			// An expression is refused rather than trusted: what it
			// evaluates to is the server's answer, and a transfer
			// written as arithmetic is still a transfer.
			name: "an expression drops cannot compare with the ctx tenant",
			build: func(u *pg.UpdateBuilder, tenant, _ *pg.Col[int64], _ *pg.Col[string]) *pg.UpdateBuilder {
				return u.Set(tenant.Expr(drops.Raw(`"tenantId" + 1`)))
			},
			wantErr: pg.ErrTenantMismatch,
		},
		{
			// An assignment that restates where the row already is
			// renders. This is the shape [Entity.Update] composes,
			// having stamped the field from ctx one call earlier.
			name: "the ctx tenant's own value is a restatement",
			build: func(u *pg.UpdateBuilder, tenant, _ *pg.Col[int64], name *pg.Col[string]) *pg.UpdateBuilder {
				return u.Set(tenant.Val(3), name.Val("a"))
			},
			wantSQL: `UPDATE "gadgets" SET "tenantId" = $1, "name" = $2 ` +
				`WHERE ("gadgets"."id" = $3) AND ("gadgets"."tenantId" = $4)`,
			wantArgs: []any{int64(3), "a", int64(7), int64(3)},
		},
		{
			// And the value is compared with sameTenant, not with
			// DeepEqual: an int ctx tenant names the same tenant as
			// the int64 column it is bound to.
			name: "a restatement whose Go type differs from the ctx tenant's",
			build: func(u *pg.UpdateBuilder, tenant, _ *pg.Col[int64], _ *pg.Col[string]) *pg.UpdateBuilder {
				return u.Set(tenant.Val(3))
			},
			wantSQL: `UPDATE "gadgets" SET "tenantId" = $1 ` +
				`WHERE ("gadgets"."id" = $2) AND ("gadgets"."tenantId" = $3)`,
			wantArgs: []any{int64(3), int64(7), int64(3)},
		},
		{
			// Unscoped is the opt-out, and it is the whole statement's
			// authority that changes: the WHERE clause loses the
			// tenant predicate in the same breath.
			name: "Unscoped says the statement is not the ctx tenant's",
			build: func(u *pg.UpdateBuilder, tenant, _ *pg.Col[int64], _ *pg.Col[string]) *pg.UpdateBuilder {
				return u.Unscoped().Set(tenant.Val(999))
			},
			wantSQL:  `UPDATE "gadgets" SET "tenantId" = $1 WHERE ("gadgets"."id" = $2)`,
			wantArgs: []any{int64(999), int64(7)},
		},
		{
			// A column that is not the axis is nobody's business here.
			name: "an ordinary column is untouched",
			build: func(u *pg.UpdateBuilder, _, _ *pg.Col[int64], name *pg.Col[string]) *pg.UpdateBuilder {
				return u.Set(name.Val("a"))
			},
			wantSQL: `UPDATE "gadgets" SET "name" = $1 ` +
				`WHERE ("gadgets"."id" = $2) AND ("gadgets"."tenantId" = $3)`,
			wantArgs: []any{"a", int64(7), int64(3)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, id, tenant, name := updateAxisSchema()
			foreign, _ := foreignAxisTable()
			db := pg.New(dropstest.New())

			u := tt.build(db.Update(tbl), tenant, foreign, name).Where(pg.Eq(id, int64(7)))
			sql, args, err := u.ToSQLCtx(tenantCtx(int64(3)))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ToSQLCtx = %v, want %v", err, tt.wantErr)
				}
				if sql != "" {
					t.Errorf("a refused UPDATE still rendered: %s", sql)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if sql != tt.wantSQL {
				t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, tt.wantSQL)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("bound arguments:\n got = %#v\nwant = %#v", args, tt.wantArgs)
			}
		})
	}
}

// Exec refuses too, and refuses before the statement reaches the
// driver: a check that only ToSQLCtx made would be a check every
// caller in the readme walks past.
func TestUpdateExecRefusesToAssignTheTenantAxis(t *testing.T) {
	tbl, id, tenant, _ := updateAxisSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	_, err := db.Update(tbl).Set(tenant.Val(999)).Where(pg.Eq(id, int64(7))).Exec(tenantCtx(int64(3)))
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Fatalf("Exec = %v, want %v", err, pg.ErrTenantMismatch)
	}
	if got := drv.LastSQL(); got != "" {
		t.Errorf("a refused UPDATE reached the driver: %s", got)
	}
}

// A table that names its write axis with ScopeWritesByTenant and scopes
// its reads some other way has no context filter to refuse a ctx with
// no tenant, so the SET list is where the refusal has to happen.
func TestUpdateRefusesAnAxisAssignmentWithNoTenantOnCtx(t *testing.T) {
	tbl := pg.NewTable("notes")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	tbl.ScopeWritesByTenant(tenant)
	db := pg.New(dropstest.New())

	sql, _, err := db.Update(tbl).Set(tenant.Val(999)).
		Where(pg.Eq(id, int64(7))).ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, pg.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("a refused UPDATE still rendered: %s", sql)
	}
}

// An UpdateHook is registered on the table and reaches every UPDATE
// against it, so an axis assignment made there is the same statement as
// one made at the call site.
func TestUpdateHookCannotAssignTheTenantAxis(t *testing.T) {
	tbl, id, _, name := updateAxisSchema()
	foreign, _ := foreignAxisTable()
	tbl.OnUpdate(pg.UpdateHookFunc(func(hc *pg.UpdateHookCtx) {
		hc.Set(foreign.Val(999))
	}))
	db := pg.New(dropstest.New())

	sql, _, err := db.Update(tbl).Set(name.Val("a")).
		Where(pg.Eq(id, int64(7))).ToSQLCtx(tenantCtx(int64(3)))
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, pg.ErrTenantMismatch)
	}
	if sql != "" {
		t.Errorf("a refused UPDATE still rendered: %s", sql)
	}
}

// The hook context's bound set is keyed by the name a column renders
// as, so a hook holding another table's handle for a column the caller
// already assigned sees it as bound and adds nothing.
//
// Keyed by the handle, the hook added a second assignment for one
// column: UPDATE "gadgets" SET "tenantId" = $1, "tenantId" = $2 with
// args [3 999]. PostgreSQL answers 42601, but MySQL accepts a
// duplicate SET and keeps the last, which makes the same builder a
// transfer in a dialect whose predicates are the whole boundary.
func TestUpdateHookSeesABindingMadeThroughAnotherTablesHandle(t *testing.T) {
	tbl, id, tenant, _ := updateAxisSchema()
	foreign, _ := foreignAxisTable()
	saw := false
	tbl.OnUpdate(pg.UpdateHookFunc(func(hc *pg.UpdateHookCtx) {
		saw = hc.Has(foreign.Column)
		hc.Set(foreign.Val(999))
	}))
	db := pg.New(dropstest.New())

	sql, args, err := db.Update(tbl).Set(tenant.Val(3)).
		Where(pg.Eq(id, int64(7))).ToSQLCtx(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if !saw {
		t.Error("Has answered false for a column the statement already assigns")
	}
	want := `UPDATE "gadgets" SET "tenantId" = $1 ` +
		`WHERE ("gadgets"."id" = $2) AND ("gadgets"."tenantId" = $3)`
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if wantArgs := []any{int64(3), int64(7), int64(3)}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", args, wantArgs)
	}
}

// Entity.Update writes every mapped column of the row, the axis among
// them, having stamped it from ctx one call earlier — so the rule the
// raw builder enforces has to be one the entity path satisfies rather
// than one it is exempt from.
func TestEntityUpdateStillAssignsTheStampedAxis(t *testing.T) {
	tbl := pg.NewTable("gadgets")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("name").NotNull())
	ent := pg.NewEntity[gadget](tbl).ScopeByTenant(tenant)

	drv := dropstest.New().Rows([]string{"id", "tenantId", "name"}, []any{int64(7), int64(3), "a"})
	db := pg.New(drv)

	row := gadget{ID: 7, Name: "a"}
	if err := ent.Update(db, tenantCtx(int64(3)), &row); err != nil {
		t.Fatalf("Update: %v", err)
	}
	want := `UPDATE "gadgets" SET "tenantId" = $1, "name" = $2 ` +
		`WHERE ("gadgets"."id" = $3) AND ("gadgets"."tenantId" = $4) ` +
		`RETURNING "gadgets"."id", "gadgets"."tenantId", "gadgets"."name"`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
	if got, wantArgs := drv.Last().Args, []any{int64(3), "a", int64(7), int64(3)}; !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", got, wantArgs)
	}
}

// PostgreSQL compares a quoted identifier byte for byte, and drops
// quotes every identifier it writes. So a handle spelled "TenantId"
// does NOT render as this table's axis: it names a column the table
// does not have, and the statement is refused by the server (42703)
// rather than by drops.
//
// This is the dialect boundary the other two write axes turn on —
// sqlite and mysql resolve the same two spellings to one column, and
// there the guard has to fold case. Pinning pg's answer is what stops
// that fold from being copied here, where it would refuse a schema
// legitimately declaring both columns.
func TestUpdateTreatsACaseVariantNameAsAnotherColumn(t *testing.T) {
	tbl, id, _, _ := updateAxisSchema()
	other := pg.NewTable("widgets")
	odd := pg.Add(other, pg.BigInt("TenantId").NotNull())
	db := pg.New(dropstest.New())

	sql, args, err := db.Update(tbl).Set(odd.Val(999)).
		Where(pg.Eq(id, int64(7))).ToSQLCtx(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `UPDATE "gadgets" SET "TenantId" = $1 ` +
		`WHERE ("gadgets"."id" = $2) AND ("gadgets"."tenantId" = $3)`
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if wantArgs := []any{int64(999), int64(7), int64(3)}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", args, wantArgs)
	}
}

// A PII-flagged tenant column binds its value wrapped in a redaction
// marker, which is a logging concern and not a tenancy one — so the
// check compares what the driver will receive rather than the wrapper
// a formatter sees. Asking the wrapper would refuse every restatement
// on such a column, Entity.Update's included.
func TestUpdateComparesTheValueBehindThePIIMarker(t *testing.T) {
	tbl := pg.NewTable("secrets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull().AsPII())
	tbl.ContextFilter(pg.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	db := pg.New(dropstest.New())

	sql, args, err := db.Update(tbl).Set(tenant.Val(3)).
		Where(pg.Eq(id, int64(7))).ToSQLCtx(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `UPDATE "secrets" SET "tenantId" = $1 ` +
		`WHERE ("secrets"."id" = $2) AND ("secrets"."tenantId" = $3)`
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if len(args) != 3 {
		t.Fatalf("bound %d arguments, want 3: %#v", len(args), args)
	}

	// And the refusal still fires through the marker.
	_, _, err = db.Update(tbl).Set(tenant.Val(999)).
		Where(pg.Eq(id, int64(7))).ToSQLCtx(tenantCtx(int64(3)))
	if !errors.Is(err, pg.ErrTenantMismatch) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, pg.ErrTenantMismatch)
	}
}
