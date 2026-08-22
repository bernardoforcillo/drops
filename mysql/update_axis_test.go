package mysql_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// The entity paths all refused to move a row between tenants — Create
// and Update stamp the axis from ctx, Patch refuses an op that names
// it, the INSERT stamps every row and the ON DUPLICATE KEY UPDATE
// branch gates its assignments — and db.Update() refused nothing:
//
//	UPDATE `posts` SET `tenantId` = ? WHERE (`posts`.`id` = ?)
//	    AND (`posts`.`tenantId` = ?)
//	args: [999 7 77]
//
// That is the shape the normative policy block describes in section 2:
// the WHERE clause is correctly confined to the caller's own rows and
// the SET list gives one of them away, so the half a review checks is
// the correct half. It needs no foreign handle and no unusual import,
// only the table's own column and the obvious spelling — and in this
// dialect the predicates are the whole of the boundary, so nothing
// underneath refuses it either.
//
// No MySQL server was reachable while this was written: every
// assertion here is on the statement drops renders and the arguments
// it binds, which is where the defect is.

// caseVariantAxisHandle returns a handle for the same column spelled in
// another case, taken off another table — what a schema assembled from
// two codegen runs, or from a hand-written declaration beside a
// generated one, produces.
func caseVariantAxisHandle() *mysql.Col[int64] {
	other := mysql.NewTable("widgets")
	mysql.Add(other, mysql.BigInt("id").PrimaryKey())
	return mysql.Add(other, mysql.BigInt("TENANTID").NotNull())
}

func TestUpdateRefusesToAssignTheTenantAxis(t *testing.T) {
	tests := []struct {
		name     string
		build    func(u *mysql.UpdateBuilder, tenant *mysql.Column, foreign, odd *mysql.Col[int64], title *mysql.Column) *mysql.UpdateBuilder
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The leak, in the spelling any handler would reach for.
			name: "the table's own handle carrying another tenant",
			build: func(u *mysql.UpdateBuilder, tenant *mysql.Column, _, _ *mysql.Col[int64], _ *mysql.Column) *mysql.UpdateBuilder {
				return u.Set(mysql.Bind(tenant, int64(999)))
			},
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// The same assignment made through another table's handle
			// for a column of the same name. It renders identically,
			// so it is refused identically.
			name: "another table's handle for the same column name",
			build: func(u *mysql.UpdateBuilder, _ *mysql.Column, foreign, _ *mysql.Col[int64], _ *mysql.Column) *mysql.UpdateBuilder {
				return u.Set(foreign.Val(999))
			},
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// MySQL resolves a column name case-insensitively, quoted
			// or not, so `TENANTID` assigns the axis as surely as
			// `tenantId` does. A guard comparing rendered names byte
			// for byte answered "not the axis" for a handle the server
			// answers "the axis" for.
			name: "a differently-cased handle for the same column",
			build: func(u *mysql.UpdateBuilder, _ *mysql.Column, _, odd *mysql.Col[int64], _ *mysql.Column) *mysql.UpdateBuilder {
				return u.Set(odd.Val(999))
			},
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// SetExpr is the same statement written the other way, and
			// an expression is refused rather than trusted: what it
			// evaluates to is the server's answer, and a transfer
			// written as arithmetic is still a transfer.
			name: "SetExpr assigning an expression drops cannot compare",
			build: func(u *mysql.UpdateBuilder, tenant *mysql.Column, _, _ *mysql.Col[int64], _ *mysql.Column) *mysql.UpdateBuilder {
				return u.SetExpr(tenant, drops.Raw("`tenantId` + 1"))
			},
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// An assignment that restates where the row already is
			// renders. This is the shape Entity.Update composes,
			// having stamped the field from ctx one call earlier.
			name: "the ctx tenant's own value is a restatement",
			build: func(u *mysql.UpdateBuilder, tenant *mysql.Column, _, _ *mysql.Col[int64], title *mysql.Column) *mysql.UpdateBuilder {
				return u.Set(mysql.Bind(tenant, scopeTenant), mysql.Bind(title, "x"))
			},
			wantSQL: "UPDATE `posts` SET `tenantId` = ?, `title` = ? " +
				"WHERE (`posts`.`id` = ?) AND (`posts`.`tenantId` = ?)",
			wantArgs: []any{scopeTenant, "x", int64(7), scopeTenant},
		},
		{
			// Unscoped is the opt-out, and it is the whole statement's
			// authority that changes: the WHERE clause loses the
			// tenant predicate in the same breath.
			name: "Unscoped says the statement is not the ctx tenant's",
			build: func(u *mysql.UpdateBuilder, tenant *mysql.Column, _, _ *mysql.Col[int64], _ *mysql.Column) *mysql.UpdateBuilder {
				return u.Unscoped().Set(mysql.Bind(tenant, int64(999)))
			},
			wantSQL:  "UPDATE `posts` SET `tenantId` = ? WHERE (`posts`.`id` = ?)",
			wantArgs: []any{int64(999), int64(7)},
		},
		{
			// A column that is not the axis is nobody's business here.
			name: "an ordinary column is untouched",
			build: func(u *mysql.UpdateBuilder, _ *mysql.Column, _, _ *mysql.Col[int64], title *mysql.Column) *mysql.UpdateBuilder {
				return u.Set(mysql.Bind(title, "x"))
			},
			wantSQL: "UPDATE `posts` SET `title` = ? " +
				"WHERE (`posts`.`id` = ?) AND (`posts`.`tenantId` = ?)",
			wantArgs: []any{"x", int64(7), scopeTenant},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, id, tenant := scopedTable("posts")
			foreign := foreignAxisTable()
			odd := caseVariantAxisHandle()
			db := mysql.New(dropstest.New())

			u := tt.build(db.Update(posts), tenant, foreign, odd, posts.Col("title")).
				Where(mysql.Eq(id, int64(7)))
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
	posts, id, tenant := scopedTable("posts")
	drv := dropstest.New()
	db := mysql.New(drv)

	_, err := db.Update(posts).Set(mysql.Bind(tenant, int64(999))).
		Where(mysql.Eq(id, int64(7))).Exec(scopeCtx())
	if !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Fatalf("Exec = %v, want %v", err, mysql.ErrTenantMismatch)
	}
	if got := drv.LastSQL(); got != "" {
		t.Errorf("a refused UPDATE reached the driver: %s", got)
	}
}

// A table that names its write axis with ScopeWritesByTenant and scopes
// its reads some other way has no context filter to refuse a ctx with
// no tenant, so the SET list is where the refusal has to happen.
func TestUpdateRefusesAnAxisAssignmentWithNoTenantOnCtx(t *testing.T) {
	tbl := mysql.NewTable("notes")
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	tenant := mysql.Add(tbl, mysql.BigInt("tenantId").NotNull())
	tbl.ScopeWritesByTenant(tenant)
	db := mysql.New(dropstest.New())

	sql, _, err := db.Update(tbl).Set(tenant.Val(999)).
		Where(mysql.Eq(id, int64(7))).ToSQLCtx(context.Background())
	if !errors.Is(err, mysql.ErrTenantMissing) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, mysql.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("a refused UPDATE still rendered: %s", sql)
	}
}

// The INSERT half of the same fold: `TENANTID` names the axis, so a
// row binding it through that spelling is checked rather than having
// the ctx stamp appended beside it.
func TestInsertReadsACaseVariantHandleAsTheAxisItRenders(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	odd := caseVariantAxisHandle()
	db := mysql.New(dropstest.New())

	sql, _, err := db.Insert(posts).
		Row(mysql.Bind(posts.Col("title"), "x"), odd.Val(9)).
		ToSQLCtx(scopeCtx())
	if !errors.Is(err, mysql.ErrTenantMismatch) {
		t.Fatalf("ToSQLCtx = %v, want %v", err, mysql.ErrTenantMismatch)
	}
	if sql != "" {
		t.Errorf("a refused INSERT still rendered: %s", sql)
	}
}

// And the ON DUPLICATE KEY UPDATE branch drops an assignment made
// through the differently-cased handle, exactly as it drops one made
// through the table's own.
func TestUpsertDropsACaseVariantAxisAssignment(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	odd := caseVariantAxisHandle()
	drv := dropstest.New()
	db := mysql.New(drv)

	_, err := db.Insert(posts).Row(mysql.Bind(posts.Col("title"), "x")).
		OnDuplicateKeyUpdate(mysql.Bind(posts.Col("title"), "y"), odd.Val(999)).
		Exec(scopeCtx())
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// Before the fold, the branch kept the assignment and rendered it
	// gated: `TENANTID` = IF(`tenantId` = VALUES(`tenantId`), ?,
	// `TENANTID`) — a gate that passes for exactly the rows this
	// tenant owns, wrapped around the value that gives one away.
	want := "INSERT INTO `posts` (`title`, `tenantId`) VALUES (?, ?) " +
		"ON DUPLICATE KEY UPDATE `title` = IF(`tenantId` = VALUES(`tenantId`), ?, `title`)"
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
	if got, wantArgs := drv.Last().Args, []any{"x", scopeTenant, "y"}; !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", got, wantArgs)
	}
}
