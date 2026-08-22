package mysql_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// alignRow is the write path that decides which bindings render at
// all, and it indexed the row by [mysql.Column.key] while the INSERT
// column list writes the bare name.
//
// So a binding that NAMES the axis by the name it renders was not
// recognised as naming it, and was DISCARDED rather than checked. The
// row did not leave the axis out — drops neither compared the binding
// nor honoured it, and wrote DEFAULT in its place. On a nullable axis
// under Unscoped that is a row belonging to nobody, reported as
// written. Scoped, it is the same discard with the stamp landing on
// top: a binding naming ANOTHER tenant was silently replaced by the
// ctx tenant's value instead of being refused.
//
// The column list is fixed by the first Row, so the discard needs a
// batch: the first row names the axis with the table's own handle and
// the second names it with another handle for the same rendered
// column. On this dialect "the same rendered column" includes a
// different spelling of its case, which the server folds and
// [mysql.identKey] therefore folds too.
//
// No MySQL server was reachable while this was written: every
// assertion here is on the statement drops renders and the arguments
// it binds.

// alignAxisSchema declares a table whose tenant axis is NULLABLE.
// Nullable is what makes the discard silent — a NOT NULL axis turns
// the DEFAULT fill into a server error, which is fail-closed by luck
// of the schema rather than by anything drops did.
func alignAxisSchema() (tbl *mysql.Table, tenant *mysql.Col[int64], title *mysql.Col[string]) {
	t := mysql.NewTable("sprockets")
	mysql.Add(t, mysql.BigInt("id").PrimaryKey())
	tenantCol := mysql.Add(t, mysql.BigInt("tenantId"))
	titleCol := mysql.Add(t, mysql.Text("title"))
	t.ContextFilter(mysql.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)
	return t, tenantCol, titleCol
}

// alignForeignAxis returns handles for a "tenantId" and a "TENANTID"
// column of another table object: the same rendered column to this
// server, a different [mysql.Column.key] to Go.
func alignForeignAxis() (exact, cased *mysql.Col[int64]) {
	other := mysql.NewTable("cogs")
	mysql.Add(other, mysql.BigInt("id").PrimaryKey())
	return mysql.Add(other, mysql.BigInt("tenantId")),
		mysql.Add(other, mysql.BigInt("TENANTID"))
}

func TestInsertAlignsALaterRowByRenderedName(t *testing.T) {
	tests := []struct {
		name     string
		rows     func(tenant, exact, cased *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue
		unscoped bool
		ctx      context.Context
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The row belonging to nobody. Before the fix this
			// rendered VALUES (?, ?), (DEFAULT, ?) with args
			// [77, "a", "b"] — the second row's tenant discarded, and
			// on a nullable axis stored as NULL.
			name: "unscoped: a later row names the axis through another table's handle",
			rows: func(tenant, exact, _ *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{
					{tenant.Val(scopeTenant), title.Val("a")},
					{exact.Val(scopeTenant), title.Val("b")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  "INSERT INTO `sprockets` (`tenantId`, `title`) VALUES (?, ?), (?, ?)",
			wantArgs: []any{scopeTenant, "a", scopeTenant, "b"},
		},
		{
			// The same through a handle spelled in another case.
			// MySQL folds a column name whatever the configuration,
			// so `TENANTID` IS this row's tenant column.
			name: "unscoped: a later row names the axis through a case-variant handle",
			rows: func(tenant, _, cased *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{
					{tenant.Val(scopeTenant), title.Val("a")},
					{cased.Val(scopeTenant), title.Val("b")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  "INSERT INTO `sprockets` (`tenantId`, `title`) VALUES (?, ?), (?, ?)",
			wantArgs: []any{scopeTenant, "a", scopeTenant, "b"},
		},
		{
			// Scoped, the same discard with the stamp on top. Before
			// the fix this rendered VALUES (?, ?), (?, ?) with args
			// [77, "a", 77, "b"]: the caller said tenant 999 and
			// drops wrote tenant 77 without a word.
			name: "scoped: a later row binds another tenant through another table's handle",
			rows: func(tenant, exact, _ *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{
					{tenant.Val(scopeTenant), title.Val("a")},
					{exact.Val(999), title.Val("b")},
				}
			},
			ctx:     scopeCtx(),
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// And through the case-variant handle, which the byte
			// comparison would have called a stranger twice over.
			name: "scoped: a later row binds another tenant through a case-variant handle",
			rows: func(tenant, _, cased *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue {
				return [][]mysql.ColumnValue{
					{tenant.Val(scopeTenant), title.Val("a")},
					{cased.Val(999), title.Val("b")},
				}
			},
			ctx:     scopeCtx(),
			wantErr: mysql.ErrTenantMismatch,
		},
		{
			// The same shape one column further out: a later row
			// naming an ordinary column through another handle used
			// to lose the value it bound.
			name: "unscoped: a later row names an ordinary column through another table's handle",
			rows: func(tenant, _, _ *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue {
				other := mysql.NewTable("cogs2")
				foreignTitle := mysql.Add(other, mysql.Text("title"))
				return [][]mysql.ColumnValue{
					{tenant.Val(scopeTenant), title.Val("a")},
					{tenant.Val(scopeTenant), foreignTitle.Val("b")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  "INSERT INTO `sprockets` (`tenantId`, `title`) VALUES (?, ?), (?, ?)",
			wantArgs: []any{scopeTenant, "a", scopeTenant, "b"},
		},
		{
			// The case alignRow's doc comment was written about, and
			// which the rendered-name index has to keep fixed: an
			// alias copy renders the same name and still lands in its
			// column's slot.
			name: "a later row bound through a table alias",
			rows: func(tenant, _, _ *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue {
				alias := tenant.Table().As("s")
				return [][]mysql.ColumnValue{
					{tenant.Val(scopeTenant), title.Val("a")},
					{mysql.Bind(alias.Col("tenantId"), scopeTenant),
						mysql.Bind(alias.Col("title"), "b")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  "INSERT INTO `sprockets` (`tenantId`, `title`) VALUES (?, ?), (?, ?)",
			wantArgs: []any{scopeTenant, "a", scopeTenant, "b"},
		},
		{
			// A binding for a column the fixed list does not name has
			// nowhere to render, and that is not the handle's doing:
			// the same binding through the table's own handle is
			// dropped in the same place. Pinned so the next reader
			// sees which residue is the column list's and which was
			// the key index's.
			name: "unscoped: a later row binds a column the fixed list does not name",
			rows: func(tenant, _, _ *mysql.Col[int64], title *mysql.Col[string]) [][]mysql.ColumnValue {
				other := mysql.NewTable("cogs3")
				colour := mysql.Add(other, mysql.Text("colour"))
				return [][]mysql.ColumnValue{
					{tenant.Val(scopeTenant), title.Val("a")},
					{tenant.Val(scopeTenant), title.Val("b"), colour.Val("red")},
				}
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  "INSERT INTO `sprockets` (`tenantId`, `title`) VALUES (?, ?), (?, ?)",
			wantArgs: []any{scopeTenant, "a", scopeTenant, "b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, tenant, title := alignAxisSchema()
			exact, cased := alignForeignAxis()
			db := mysql.New(nil)

			ins := db.Insert(tbl).Rows(tt.rows(tenant, exact, cased, title)...)
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
	tbl, tenant, title := alignAxisSchema()
	exact, _ := alignForeignAxis()
	db := mysql.New(nil)

	sql, args, err := db.Insert(tbl).
		Row(tenant.Val(scopeTenant), title.Val("a")).
		Row(tenant.Val(7), exact.Val(9), title.Val("b")).
		Unscoped().
		ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	want := "INSERT INTO `sprockets` (`tenantId`, `title`) VALUES (?, ?), (?, ?)"
	if sql != want {
		t.Errorf("rendered SQL:\n got = %v\nwant = %v", sql, want)
	}
	if got, want := args, []any{scopeTenant, "a", int64(9), "b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", got, want)
	}
}
