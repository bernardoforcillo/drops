package mysql_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// Stamping asked "is the tenant value right?" and never "is this
// handle even ours?".
//
// It located the tenant column with Column.key, which collapses alias
// copies onto the column they were declared as and therefore calls a
// handle for the same-named column of a DIFFERENT table object a
// stranger — so a row that plainly named the tenant column was read as
// naming no tenant at all, and the ctx stamp was APPENDED ALONGSIDE
// the caller's binding:
//
//	INSERT INTO `zzi` (`title`, `tenantId`, `tenantId`) VALUES (?, ?, ?)
//	args: {"x", "evil", "acme"}
//
// This server should answer ER_FIELD_SPECIFIED_TWICE, but that is the
// server refusing rather than drops, and the same handle reaches the
// ON DUPLICATE KEY UPDATE branch, where the assignment the axis guard
// is supposed to DROP survived and rendered gated:
// `tenantId` = IF(`tenantId` = VALUES(`tenantId`), ?, `tenantId`) —
// a gate that passes for exactly the rows this tenant owns, wrapped
// around the value that gives one of them away.
//
// So the axis is matched by the name the statement RENDERS, and every
// occurrence is checked rather than the first.

// foreignAxisTable returns a second table declaring the same column
// names. Two tables with a "tenantId" is what any multi-tenant schema
// looks like, and in a codegen'd one every table exports its own
// <Table>Cols.TenantID.
func foreignAxisTable() (foreignTenant *mysql.Col[int64]) {
	other := mysql.NewTable("widgets")
	mysql.Add(other, mysql.BigInt("id").PrimaryKey())
	return mysql.Add(other, mysql.BigInt("tenantId").NotNull())
}

func TestInsertReadsAForeignHandleAsTheAxisItRenders(t *testing.T) {
	tests := []struct {
		name     string
		rows     func(posts *mysql.Table, tenant *mysql.Column, foreign *mysql.Col[int64]) [][]mysql.ColumnValue
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The leak. Before the fix this rendered
			// INSERT INTO `posts` (`title`, `tenantId`, `tenantId`)
			// VALUES (?, ?, ?) with args ["x", 9, 77].
			name: "bound to another tenant through another table's handle",
			rows: func(posts *mysql.Table, _ *mysql.Column, foreign *mysql.Col[int64]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{{mysql.Bind(posts.Col("title"), "x"), foreign.Val(9)}}
			},
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// The same handle carrying the right value is not an
			// error — it names the column it renders as, and the
			// statement that goes out binds the tenant ONCE.
			name: "bound to the ctx tenant through another table's handle",
			rows: func(posts *mysql.Table, _ *mysql.Column, foreign *mysql.Col[int64]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{{mysql.Bind(posts.Col("title"), "x"), foreign.Val(scopeTenant)}}
			},
			wantSQL:  "INSERT INTO `posts` (`title`, `tenantId`) VALUES (?, ?)",
			wantArgs: []any{"x", scopeTenant},
		},
		{
			// Two bindings for one rendered column, the second of
			// them wrong. A check that stopped at the first match
			// passed this.
			name: "the column bound twice, the second binding another tenant's",
			rows: func(posts *mysql.Table, tenant *mysql.Column, foreign *mysql.Col[int64]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{{
					mysql.Bind(tenant, scopeTenant),
					mysql.Bind(posts.Col("title"), "x"),
					foreign.Val(9),
				}}
			},
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// And per row, not per statement: the batch's first row
			// is impeccable.
			name: "a later row of a batch carrying another tenant",
			rows: func(posts *mysql.Table, _ *mysql.Column, foreign *mysql.Col[int64]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{
					{foreign.Val(scopeTenant), mysql.Bind(posts.Col("title"), "x")},
					{foreign.Val(9), mysql.Bind(posts.Col("title"), "y")},
				}
			},
			wantErr: mysql.ErrTenantMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, _, tenant := scopedTable("posts")
			posts.ScopeWritesByTenant(tenant)
			foreign := foreignAxisTable()
			db := mysql.New(dropstest.New())

			ins := db.Insert(posts)
			for _, row := range tt.rows(posts, tenant, foreign) {
				ins.Row(row...)
			}
			sql, args, err := ins.ToSQLCtx(scopeCtx())
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("ToSQLCtx = %v, want %v", err, tt.wantErr)
				}
				if sql != "" {
					t.Errorf("a refused INSERT still rendered: %s", sql)
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

// The ON DUPLICATE KEY UPDATE half. The gate proves the collided row
// belongs to this tenant and the assignment then gives it away, which
// is the shape the normative policy block describes: "the half a review
// checks is correct and the other half is the leak".
func TestUpsertDropsAnAxisAssignmentMadeThroughAForeignHandle(t *testing.T) {
	posts, _, tenant := scopedTable("posts")
	posts.ScopeWritesByTenant(tenant)
	foreign := foreignAxisTable()
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Insert(posts).
		Row(mysql.Bind(posts.Col("title"), "x")).
		OnDuplicateKeyUpdate(
			mysql.Bind(posts.Col("title"), "z"),
			foreign.Val(9)))
	// Before the fix the branch also carried
	// `tenantId` = IF(`tenantId` = VALUES(`tenantId`), ?, `tenantId`)
	// with 9 bound into it.
	wantText(t, sql, "INSERT INTO `posts` (`title`, `tenantId`) VALUES (?, ?) "+
		"ON DUPLICATE KEY UPDATE `title` = IF(`tenantId` = VALUES(`tenantId`), ?, `title`)")
	wantArgs(t, args, "x", scopeTenant, "z")
}
