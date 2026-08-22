package sqlite_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// The entity paths all refused to move a row between tenants — Create
// and Update stamp the axis from ctx, Patch refuses an op that names
// it, the INSERT stamps every row and the upsert branch drops the
// assignment — and db.Update() refused nothing:
//
//	UPDATE "posts" SET "tenantId" = ? WHERE ("posts"."id" = ?)
//	    AND ("posts"."tenantId" = ?)
//	args: [999 7 77]
//
// That is the shape the normative policy block describes in section 2:
// the WHERE clause is correctly confined to the caller's own rows and
// the SET list gives one of them away, so the half a review checks is
// the correct half. It needs no foreign handle and no unusual import,
// only the table's own column and the obvious spelling — and in this
// dialect the predicates are the whole of the boundary, so nothing
// underneath refuses it either.

// updateAxisSchema is axisSchema's table plus the id handle an UPDATE
// addresses a row with.
func updateAxisSchema() (tbl *sqlite.Table, id, tenant, foreign *sqlite.Col[int64], title *sqlite.Col[string]) {
	t := sqlite.NewTable("posts")
	idCol := sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenantCol := sqlite.Add(t, sqlite.BigInt("tenantId").NotNull())
	titleCol := sqlite.Add(t, sqlite.Text("title"))
	t.ContextFilter(sqlite.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)

	other := sqlite.NewTable("widgets")
	sqlite.Add(other, sqlite.BigInt("id").PrimaryKey())
	foreignCol := sqlite.Add(other, sqlite.BigInt("tenantId").NotNull())
	return t, idCol, tenantCol, foreignCol, titleCol
}

func TestUpdateRefusesToAssignTheTenantAxis(t *testing.T) {
	tests := []struct {
		name     string
		build    func(u *sqlite.UpdateBuilder, tenant, foreign *sqlite.Col[int64], title *sqlite.Col[string]) *sqlite.UpdateBuilder
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The leak, in the spelling any handler would reach for.
			name: "the table's own handle carrying another tenant",
			build: func(u *sqlite.UpdateBuilder, tenant, _ *sqlite.Col[int64], _ *sqlite.Col[string]) *sqlite.UpdateBuilder {
				return u.Set(tenant.Val(999))
			},
			wantErr: sqlite.ErrTenantMismatch,
		},
		{
			// The same assignment made through another table's handle
			// for a column of the same name. It renders identically,
			// so it is refused identically.
			name: "another table's handle for the same column name",
			build: func(u *sqlite.UpdateBuilder, _, foreign *sqlite.Col[int64], _ *sqlite.Col[string]) *sqlite.UpdateBuilder {
				return u.Set(foreign.Val(999))
			},
			wantErr: sqlite.ErrTenantMismatch,
		},
		{
			// SetExpr is the same statement written the other way, and
			// an expression is refused rather than trusted: what it
			// evaluates to is the server's answer, and a transfer
			// written as arithmetic is still a transfer.
			name: "SetExpr assigning an expression drops cannot compare",
			build: func(u *sqlite.UpdateBuilder, tenant, _ *sqlite.Col[int64], _ *sqlite.Col[string]) *sqlite.UpdateBuilder {
				return u.SetExpr(tenant.Column, drops.Raw(`"tenantId" + 1`))
			},
			wantErr: sqlite.ErrTenantMismatch,
		},
		{
			// An assignment that restates where the row already is
			// renders. This is the shape Entity.Update composes,
			// having stamped the field from ctx one call earlier.
			name: "the ctx tenant's own value is a restatement",
			build: func(u *sqlite.UpdateBuilder, tenant, _ *sqlite.Col[int64], title *sqlite.Col[string]) *sqlite.UpdateBuilder {
				return u.Set(tenant.Val(scopeTenant), title.Val("x"))
			},
			wantSQL: `UPDATE "posts" SET "tenantId" = ?, "title" = ? ` +
				`WHERE ("posts"."id" = ?) AND ("posts"."tenantId" = ?)`,
			wantArgs: []any{scopeTenant, "x", int64(7), scopeTenant},
		},
		{
			// Unscoped is the opt-out, and it is the whole statement's
			// authority that changes: the WHERE clause loses the
			// tenant predicate in the same breath.
			name: "Unscoped says the statement is not the ctx tenant's",
			build: func(u *sqlite.UpdateBuilder, tenant, _ *sqlite.Col[int64], _ *sqlite.Col[string]) *sqlite.UpdateBuilder {
				return u.Unscoped().Set(tenant.Val(999))
			},
			wantSQL:  `UPDATE "posts" SET "tenantId" = ? WHERE ("posts"."id" = ?)`,
			wantArgs: []any{int64(999), int64(7)},
		},
		{
			// A column that is not the axis is nobody's business here.
			name: "an ordinary column is untouched",
			build: func(u *sqlite.UpdateBuilder, _, _ *sqlite.Col[int64], title *sqlite.Col[string]) *sqlite.UpdateBuilder {
				return u.Set(title.Val("x"))
			},
			wantSQL: `UPDATE "posts" SET "title" = ? ` +
				`WHERE ("posts"."id" = ?) AND ("posts"."tenantId" = ?)`,
			wantArgs: []any{"x", int64(7), scopeTenant},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, id, tenant, foreign, title := updateAxisSchema()
			db := sqlite.New(dropstest.New())

			u := tt.build(db.Update(tbl), tenant, foreign, title).Where(sqlite.Eq(id, int64(7)))
			sql, args, err := u.ToSQLCtx(scopeCtx())
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
	tbl, id, tenant, _, _ := updateAxisSchema()
	drv := dropstest.New()
	db := sqlite.New(drv)

	_, err := db.Update(tbl).Set(tenant.Val(999)).
		Where(sqlite.Eq(id, int64(7))).Exec(scopeCtx())
	if !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Fatalf("Exec = %v, want %v", err, sqlite.ErrTenantMismatch)
	}
	if got := drv.LastSQL(); got != "" {
		t.Errorf("a refused UPDATE reached the driver: %s", got)
	}
}

// A table that names its write axis with ScopeWritesByTenant and scopes
// its reads some other way has no context filter to refuse a ctx with
// no tenant, so the SET list is where the refusal has to happen.
func TestUpdateRefusesAnAxisAssignmentWithNoTenantOnCtx(t *testing.T) {
	tbl := sqlite.NewTable("notes")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	tbl.ScopeWritesByTenant(tenant)
	db := sqlite.New(dropstest.New())

	sql, _, err := db.Update(tbl).Set(tenant.Val(999)).
		Where(sqlite.Eq(id, int64(7))).ToSQLCtx(context.Background())
	if !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("a refused UPDATE still rendered: %s", sql)
	}
}

// An UpdateHook is registered on the table and reaches every UPDATE
// against it, so an axis assignment made there is the same statement as
// one made at the call site.
func TestUpdateHookCannotAssignTheTenantAxis(t *testing.T) {
	tbl, id, _, foreign, title := updateAxisSchema()
	tbl.OnUpdate(sqlite.UpdateHookFunc(func(hc *sqlite.UpdateHookCtx) {
		hc.Set(foreign.Val(999))
	}))
	db := sqlite.New(dropstest.New())

	sql, _, err := db.Update(tbl).Set(title.Val("x")).
		Where(sqlite.Eq(id, int64(7))).ToSQLCtx(scopeCtx())
	if !errors.Is(err, sqlite.ErrTenantMismatch) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, sqlite.ErrTenantMismatch)
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
// column: SET "tenantId" = ?, "tenantId" = ? with args [77 999]. SQLite
// executes that and the last assignment wins, so the hook's value is
// what lands.
func TestUpdateHookSeesABindingMadeThroughAnotherTablesHandle(t *testing.T) {
	tbl, id, tenant, foreign, _ := updateAxisSchema()
	saw := false
	tbl.OnUpdate(sqlite.UpdateHookFunc(func(hc *sqlite.UpdateHookCtx) {
		saw = hc.Has(foreign.Column)
		hc.Set(foreign.Val(999))
	}))
	db := sqlite.New(dropstest.New())

	sql, args, err := db.Update(tbl).Set(tenant.Val(scopeTenant)).
		Where(sqlite.Eq(id, int64(7))).ToSQLCtx(scopeCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if !saw {
		t.Error("Has answered false for a column the statement already assigns")
	}
	want := `UPDATE "posts" SET "tenantId" = ? ` +
		`WHERE ("posts"."id" = ?) AND ("posts"."tenantId" = ?)`
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if wantArgs := []any{scopeTenant, int64(7), scopeTenant}; !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", args, wantArgs)
	}
}
