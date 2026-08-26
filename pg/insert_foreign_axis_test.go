package pg_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
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
//	INSERT INTO "zzi" ("title", "tenantId", "tenantId") VALUES ($1, $2, $3)
//	args: {"x", "evil", "acme"}
//
// PostgreSQL rejects that with 42701 — fail-closed, by luck of the
// server rather than by anything drops did. SQLite accepts it and
// keeps the first occurrence, and a row written under ctx tenant
// "acme" landed as "evil" against a real server with no error. The
// same column list also reaches an ON CONFLICT DO UPDATE, where the
// assignment the axis guard is supposed to drop survived and rendered
// under a gate that had just confirmed the collided row was this
// tenant's.
//
// So the axis is matched by the name the statement RENDERS, and every
// occurrence is checked rather than the first.

// foreignAxisTable returns a second table declaring the same column
// names as gadgets. Two tables with a "tenantId" is what any
// multi-tenant schema looks like, and in a codegen'd one every table
// exports its own <Table>Cols.TenantID.
func foreignAxisTable() (foreignTenant *pg.Col[int64], foreignName *pg.Col[string]) {
	other := pg.NewTable("widgets")
	pg.Add(other, pg.BigSerial("id").PrimaryKey())
	ft := pg.Add(other, pg.BigInt("tenantId").NotNull())
	fn := pg.Add(other, pg.Text("name").NotNull())
	return ft, fn
}

func TestInsertReadsAForeignHandleAsTheAxisItRenders(t *testing.T) {
	tests := []struct {
		name     string
		rows     func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The leak. Before the fix this rendered
			// INSERT INTO "gadgets" ("name", "tenantId", "tenantId")
			// VALUES ($1, $2, $3) with args ["a", 9, 3].
			name: "bound to another tenant through another table's handle",
			rows: func(_, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				return [][]pg.ColumnValue{{name.Val("a"), foreign.Val(9)}}
			},
			wantErr: pg.ErrTenantMismatch,
		},
		{
			// The same handle carrying the right value is not an
			// error — it names the column it renders as, and the
			// statement that goes out binds the tenant ONCE.
			name: "bound to the ctx tenant through another table's handle",
			rows: func(_, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				return [][]pg.ColumnValue{{name.Val("a"), foreign.Val(3)}}
			},
			wantSQL:  `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2)`,
			wantArgs: []any{"a", int64(3)},
		},
		{
			// Two bindings for one rendered column, the second of
			// them wrong. A check that stopped at the first match
			// passed this.
			name: "the column bound twice, the second binding another tenant's",
			rows: func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				return [][]pg.ColumnValue{{tenant.Val(3), name.Val("a"), foreign.Val(9)}}
			},
			wantErr: pg.ErrTenantMismatch,
		},
		{
			// And per row, not per statement: the batch's first row
			// is impeccable.
			name: "a later row of a batch carrying another tenant",
			rows: func(_, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				return [][]pg.ColumnValue{
					{foreign.Val(3), name.Val("a")},
					{foreign.Val(9), name.Val("b")},
				}
			},
			wantErr: pg.ErrTenantMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, _, tenant, name := gadgetSchema()
			foreign, _ := foreignAxisTable()
			drv := dropstest.New()
			db := pg.New(drv)

			sql, args, err := db.Insert(tbl).
				Rows(tt.rows(tenant, foreign, name)...).
				ToSQLCtx(tenantCtx(int64(3)))
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

// The ON CONFLICT DO UPDATE half. The gate proves the collided row
// belongs to this tenant and the assignment then gives it away, which
// is the shape the normative policy block describes: "the half a review
// checks is correct and the other half is the leak".
func TestUpsertDropsAnAxisAssignmentMadeThroughAForeignHandle(t *testing.T) {
	tbl, id, _, name := gadgetSchema()
	foreignTenant, _ := foreignAxisTable()
	drv := dropstest.New()
	db := pg.New(drv)

	_, err := db.Insert(tbl).Row(name.Val("a")).
		OnConflictUpdate(id).
		Set(name.Expr(pg.Excluded(name)), foreignTenant.Val(9)).
		Done().Exec(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	// Before the fix: ... DO UPDATE SET "name" = EXCLUDED."name",
	// "tenantId" = $3 WHERE "tenantId" = EXCLUDED."tenantId", args
	// ["a", 3, 9].
	want := `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO UPDATE SET "name" = EXCLUDED."name" ` +
		`WHERE "tenantId" = EXCLUDED."tenantId"`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
	if got, want := drv.Last().Args, []any{"a", int64(3)}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound arguments: got = %v, want %v", got, want)
	}
}

// And when the foreign assignment was the only one, the branch
// degrades to DO NOTHING exactly as it does for the entity's own
// handle — "DO UPDATE SET" with an empty list is a syntax error.
func TestUpsertWithOnlyAForeignAxisAssignmentDoesNothing(t *testing.T) {
	tbl, id, _, name := gadgetSchema()
	foreignTenant, _ := foreignAxisTable()
	drv := dropstest.New()
	db := pg.New(drv)

	_, err := db.Insert(tbl).Row(name.Val("a")).
		OnConflictUpdate(id).Set(foreignTenant.Val(9)).
		Done().Exec(tenantCtx(int64(3)))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := `INSERT INTO "gadgets" ("name", "tenantId") VALUES ($1, $2) ` +
		`ON CONFLICT ("id") DO NOTHING`
	if got := drv.LastSQL(); got != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", got, want)
	}
}
