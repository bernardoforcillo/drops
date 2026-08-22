package pg_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// alignRow is the write path that decides which bindings render at
// all, and it indexed the row by [pg.Column.key] while the INSERT
// column list writes the bare name.
//
// So a binding that NAMES the axis by the name it renders was not
// recognised as naming it, and was DISCARDED rather than checked. The
// row did not leave the axis out — drops neither compared the binding
// nor honoured it, and wrote DEFAULT in its place. On a nullable axis
// under Unscoped that is a row belonging to nobody, reported as
// written, which is the failure mode section 1 of the policy block
// opens with. Scoped, it is the same discard with the stamp landing on
// top: a binding naming ANOTHER tenant was silently replaced by the
// ctx tenant's value instead of being refused.
//
// The column list is fixed by the first Row, so the discard needs a
// batch: the first row is impeccable and names the axis with the
// table's own handle, and the second names it with another handle for
// the same rendered column.

// alignAxisSchema declares a table whose tenant axis is NULLABLE.
// Nullable is what makes the discard silent — a NOT NULL axis turns
// the DEFAULT fill into a server error, which is the fail-closed luck
// this test refuses to rely on.
func alignAxisSchema() (tbl *pg.Table, tenant *pg.Col[int64], name *pg.Col[string]) {
	tbl = pg.NewTable("sprockets")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant = pg.Add(tbl, pg.BigInt("tenantId"))
	name = pg.Add(tbl, pg.Text("name").NotNull())
	tbl.ContextFilter(pg.TenantFilter(tenant)).ScopeWritesByTenant(tenant)
	return tbl, tenant, name
}

// alignForeignAxis returns a handle for a "tenantId" column of another
// table object — the same rendered name, a different [pg.Column.key].
func alignForeignAxis() *pg.Col[int64] {
	other := pg.NewTable("cogs")
	pg.Add(other, pg.BigSerial("id").PrimaryKey())
	return pg.Add(other, pg.BigInt("tenantId"))
}

func TestInsertAlignsALaterRowByRenderedName(t *testing.T) {
	tests := []struct {
		name     string
		rows     func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue
		unscoped bool
		ctx      context.Context
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The row belonging to nobody. Before the fix this
			// rendered VALUES ($1, $2), (DEFAULT, $3) with args
			// [3, "a", "b"] — the second row's tenant discarded, and
			// on a nullable axis stored as NULL.
			name: "unscoped: a later row names the axis through another table's handle",
			rows: func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				return [][]pg.ColumnValue{
					{tenant.Val(3), name.Val("a")},
					{foreign.Val(3), name.Val("b")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  `INSERT INTO "sprockets" ("tenantId", "name") VALUES ($1, $2), ($3, $4)`,
			wantArgs: []any{int64(3), "a", int64(3), "b"},
		},
		{
			// Scoped, the same discard with the stamp on top. Before
			// the fix this rendered VALUES ($1, $2), ($3, $4) with
			// args [3, "a", 3, "b"]: the caller said tenant 9 and
			// drops wrote tenant 3 without a word.
			name: "scoped: a later row binds another tenant through another table's handle",
			rows: func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				return [][]pg.ColumnValue{
					{tenant.Val(3), name.Val("a")},
					{foreign.Val(9), name.Val("b")},
				}
			},
			ctx:     tenantCtx(int64(3)),
			wantErr: pg.ErrTenantMismatch,
		},
		{
			// The same shape one column further out: a later row
			// naming an ordinary column through another handle used
			// to lose the value it bound.
			name: "unscoped: a later row names an ordinary column through another table's handle",
			rows: func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				other := pg.NewTable("cogs2")
				foreignName := pg.Add(other, pg.Text("name"))
				return [][]pg.ColumnValue{
					{tenant.Val(3), name.Val("a")},
					{tenant.Val(3), foreignName.Val("b")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  `INSERT INTO "sprockets" ("tenantId", "name") VALUES ($1, $2), ($3, $4)`,
			wantArgs: []any{int64(3), "a", int64(3), "b"},
		},
		{
			// The case alignRow's doc comment was written about, and
			// which the rendered-name index has to keep fixed: an
			// alias copy renders the same name and still lands in its
			// column's slot.
			name: "a later row bound through a table alias",
			rows: func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				alias := tenant.Table().As("s")
				return [][]pg.ColumnValue{
					{tenant.Val(3), name.Val("a")},
					{aliasBind[int64](alias.Col("tenantId")).Val(3),
						aliasBind[string](alias.Col("name")).Val("b")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  `INSERT INTO "sprockets" ("tenantId", "name") VALUES ($1, $2), ($3, $4)`,
			wantArgs: []any{int64(3), "a", int64(3), "b"},
		},
		{
			// A binding for a column the fixed list does not name has
			// nowhere to render, and that is not the handle's doing:
			// the same binding through the table's own handle is
			// dropped in the same place. Pinned so the next reader
			// sees which residue is the column list's and which was
			// the key index's.
			name: "unscoped: a later row binds a column the fixed list does not name",
			rows: func(tenant, foreign *pg.Col[int64], name *pg.Col[string]) [][]pg.ColumnValue {
				other := pg.NewTable("cogs3")
				colour := pg.Add(other, pg.Text("colour"))
				return [][]pg.ColumnValue{
					{tenant.Val(3), name.Val("a")},
					{tenant.Val(3), name.Val("b"), colour.Val("red")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  `INSERT INTO "sprockets" ("tenantId", "name") VALUES ($1, $2), ($3, $4)`,
			wantArgs: []any{int64(3), "a", int64(3), "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, tenant, name := alignAxisSchema()
			foreign := alignForeignAxis()
			db := pg.New(nil)

			ins := db.Insert(tbl).Rows(tt.rows(tenant, foreign, name)...)
			if tt.unscoped {
				ins = ins.Unscoped()
			}
			sql, args, err := ins.ToSQLCtx(tt.ctx)
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

// A row that names one rendered column more often than the fixed
// column list does has more bindings than slots, and the last of them
// wins — which is what the map this index replaced did for a handle
// repeated in one row. Pinned because "the row contradicts itself" is
// resolved here rather than by whichever spelling came first.
func TestInsertKeepsTheLastBindingWhenARowNamesAColumnTwice(t *testing.T) {
	tbl, tenant, name := alignAxisSchema()
	foreign := alignForeignAxis()
	db := pg.New(nil)

	sql, args, err := db.Insert(tbl).
		Row(tenant.Val(3), name.Val("a")).
		Row(tenant.Val(7), foreign.Val(9), name.Val("b")).
		Unscoped().
		ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := `INSERT INTO "sprockets" ("tenantId", "name") VALUES ($1, $2), ($3, $4)`
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if got, want := args, []any{int64(3), "a", int64(9), "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", got, want)
	}
}
