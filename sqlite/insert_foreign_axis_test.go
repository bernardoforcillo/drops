package sqlite_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// Stamping asked "is the tenant value right?" and never "is this
// handle even ours?".
//
// It located the tenant column by handle identity, so a binding made
// through a handle for the same-named column of a DIFFERENT table
// object was read as naming no tenant at all, and the ctx stamp was
// APPENDED ALONGSIDE the caller's:
//
//	INSERT INTO "zzi" ("title", "tenantId", "tenantId") VALUES (?, ?, ?)
//	args: {"x", "evil", "acme"}
//
// This is the dialect where that lands. PostgreSQL rejects a duplicate
// column; SQLite accepts it and keeps the FIRST occurrence, so a row
// written under ctx tenant "acme" was stored as "evil" against a real
// server, with no error, in the dialect whose own tenant.go says the
// predicates are the whole boundary. The end-to-end proof lives in the
// integration suite; these tests pin the statement that carried it.
//
// So the axis is matched by the name the statement RENDERS, and every
// occurrence is checked rather than the first — which is also what
// makes binding this table's own handle twice safe by rule rather than
// by SQLite's choice of which duplicate to keep.

// axisSchema builds the table under test with typed handles, plus a
// SECOND table declaring the same column names. Two tables with a
// "tenantId" is what any multi-tenant schema looks like, and in a
// codegen'd one every table exports its own <Table>Cols.TenantID.
func axisSchema() (tbl *sqlite.Table, tenant, foreign *sqlite.Col[int64], title *sqlite.Col[string]) {
	t := sqlite.NewTable("posts")
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenantCol := sqlite.Add(t, sqlite.BigInt("tenantId").NotNull())
	titleCol := sqlite.Add(t, sqlite.Text("title"))
	t.ContextFilter(sqlite.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)

	other := sqlite.NewTable("widgets")
	sqlite.Add(other, sqlite.BigInt("id").PrimaryKey())
	foreignCol := sqlite.Add(other, sqlite.BigInt("tenantId").NotNull())
	return t, tenantCol, foreignCol, titleCol
}

func TestInsertReadsAForeignHandleAsTheAxisItRenders(t *testing.T) {
	tests := []struct {
		name     string
		rows     func(tenant, foreign *sqlite.Col[int64], title *sqlite.Col[string]) [][]sqlite.ColumnValue
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The leak. Before the fix this rendered
			// INSERT INTO "posts" ("title", "tenantId", "tenantId")
			// VALUES (?, ?, ?) with args ["x", 9, 77], and SQLite
			// stored 9.
			name: "bound to another tenant through another table's handle",
			rows: func(_, foreign *sqlite.Col[int64], title *sqlite.Col[string]) [][]sqlite.ColumnValue {
				return [][]sqlite.ColumnValue{{title.Val("x"), foreign.Val(9)}}
			},
			wantErr: sqlite.ErrTenantMismatch,
		},
		{
			// The same handle carrying the right value is not an
			// error — it names the column it renders as, and the
			// statement that goes out binds the tenant ONCE.
			name: "bound to the ctx tenant through another table's handle",
			rows: func(_, foreign *sqlite.Col[int64], title *sqlite.Col[string]) [][]sqlite.ColumnValue {
				return [][]sqlite.ColumnValue{{title.Val("x"), foreign.Val(scopeTenant)}}
			},
			wantSQL:  `INSERT INTO "posts" ("title", "tenantId") VALUES (?, ?)`,
			wantArgs: []any{"x", scopeTenant},
		},
		{
			// Two bindings for one rendered column, the second of
			// them wrong. A check that stopped at the first match
			// passed this, and SQLite kept the checked one — benign
			// by luck, which is what this stops relying on.
			name: "the column bound twice, the second binding another tenant's",
			rows: func(tenant, _ *sqlite.Col[int64], title *sqlite.Col[string]) [][]sqlite.ColumnValue {
				return [][]sqlite.ColumnValue{{
					tenant.Val(scopeTenant),
					title.Val("x"),
					tenant.Val(9),
				}}
			},
			wantErr: sqlite.ErrTenantMismatch,
		},
		{
			// And per row, not per statement: the batch's first row
			// is impeccable.
			name: "a later row of a batch carrying another tenant",
			rows: func(_, foreign *sqlite.Col[int64], title *sqlite.Col[string]) [][]sqlite.ColumnValue {
				return [][]sqlite.ColumnValue{
					{title.Val("x"), foreign.Val(scopeTenant)},
					{title.Val("y"), foreign.Val(9)},
				}
			},
			wantErr: sqlite.ErrTenantMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			posts, tenant, foreign, title := axisSchema()
			db := sqlite.New(dropstest.New())

			ins := db.Insert(posts)
			for _, row := range tt.rows(tenant, foreign, title) {
				ins.Values(row...)
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
