package clickhouse_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// alignRow is the write path that decides which bindings render at
// all, and it indexed the row by [clickhouse.Column.key] while the
// INSERT column list writes the bare name.
//
// So a binding that NAMES the axis by the name it renders was not
// recognised as naming it, and was DISCARDED. This dialect fills the
// gap with NULL rather than DEFAULT, so the discarded tenant became a
// row belonging to nobody outright — invisible to every later request
// including the one that wrote it, and reported as written. It
// happened SCOPED as well as unscoped: stampTenantColumn reads the raw
// row and did find the caller's binding there, so it compared the
// value, passed it, appended no stamp — and alignRow then dropped the
// binding it had just approved. The check and the renderer were asking
// two different questions, and the renderer's was the one that
// reached the server.
//
// The column list is fixed by the first Row, so the discard needs a
// batch: the first row names the axis with the table's own handle and
// the second names it with another handle for the same rendered
// column.
//
// No ClickHouse server was reachable while this was written: every
// assertion here is on the statement drops renders and the arguments
// it binds.

// alignHits is a table whose axis is declared on the table, with no
// merging engine for [clickhouse.ErrTenantNotInSortingKey] to be about
// — this file is about which bindings render, not about what a merge
// would later fold.
var (
	alignHits   = clickhouse.NewTable("sprockets")
	alignHitID  = clickhouse.Add(alignHits, clickhouse.UInt64("id"))
	alignHitTen = clickhouse.Add(alignHits, clickhouse.String("tenantId"))
)

func init() {
	alignHits.ContextFilter(clickhouse.TenantFilter(alignHitTen)).
		ScopeWritesByTenant(alignHitTen)
}

// alignForeign holds handles for columns of ANOTHER table object that
// render as the same names: the same columns to the server, strangers
// to [clickhouse.Column.key].
var (
	alignForeign    = clickhouse.NewTable("cogs")
	alignForeignTen = clickhouse.Add(alignForeign, clickhouse.String("tenantId"))
	alignForeignID  = clickhouse.Add(alignForeign, clickhouse.UInt64("id"))
)

func TestInsertAlignsALaterRowByRenderedName(t *testing.T) {
	tests := []struct {
		name     string
		rows     [][]clickhouse.ColumnValue
		unscoped bool
		ctx      context.Context
		wantErr  error
		wantSQL  string
		wantArgs []any
	}{
		{
			// The row belonging to nobody, and it needed no Unscoped
			// to get there. Before the fix this rendered
			// VALUES (?, ?), (?, NULL) with args [1, "acme", 2]: the
			// tenant check had already approved the second row's
			// binding.
			name: "scoped: a later row names the axis through another table's handle",
			rows: [][]clickhouse.ColumnValue{
				{alignHitTen.Val("acme"), alignHitID.Val(1)},
				{alignForeignTen.Val("acme"), alignHitID.Val(2)},
			},
			ctx:      tenantCtx("acme"),
			wantSQL:  `INSERT INTO "sprockets" ("id", "tenantId") VALUES (?, ?), (?, ?)`,
			wantArgs: []any{uint64(1), "acme", uint64(2), "acme"},
		},
		{
			// The same statement with the axis opted out of: nothing
			// stamps and nothing checks, so the discard is the whole
			// of what happens.
			name: "unscoped: a later row names the axis through another table's handle",
			rows: [][]clickhouse.ColumnValue{
				{alignHitTen.Val("acme"), alignHitID.Val(1)},
				{alignForeignTen.Val("acme"), alignHitID.Val(2)},
			},
			unscoped: true,
			ctx:      context.Background(),
			wantSQL:  `INSERT INTO "sprockets" ("id", "tenantId") VALUES (?, ?), (?, ?)`,
			wantArgs: []any{uint64(1), "acme", uint64(2), "acme"},
		},
		{
			// The negative control the check already passed: a
			// foreign handle carrying somebody else's tenant is
			// refused, because stampTenantColumn reads the raw row
			// and asks [namesAxis]. It stays refused.
			name: "scoped: a later row binds another tenant through another table's handle",
			rows: [][]clickhouse.ColumnValue{
				{alignHitTen.Val("acme"), alignHitID.Val(1)},
				{alignForeignTen.Val("globex"), alignHitID.Val(2)},
			},
			ctx:     tenantCtx("acme"),
			wantErr: clickhouse.ErrTenantMismatch,
		},
		{
			// The same shape one column further out: a later row
			// naming an ordinary column through another handle used
			// to lose the value it bound, and nothing in the tenant
			// path was ever going to notice.
			name: "scoped: a later row names an ordinary column through another table's handle",
			rows: [][]clickhouse.ColumnValue{
				{alignHitTen.Val("acme"), alignHitID.Val(1)},
				{alignHitTen.Val("acme"), alignForeignID.Val(2)},
			},
			ctx:      tenantCtx("acme"),
			wantSQL:  `INSERT INTO "sprockets" ("id", "tenantId") VALUES (?, ?), (?, ?)`,
			wantArgs: []any{uint64(1), "acme", uint64(2), "acme"},
		},
		{
			// The case alignRow was already right about, and which
			// the rendered-name index has to keep right: an alias
			// copy renders the same name and still lands in its
			// column's slot.
			name: "scoped: a later row bound through a table alias",
			rows: [][]clickhouse.ColumnValue{
				{alignHitTen.Val("acme"), alignHitID.Val(1)},
				{clickhouse.Bind(alignHits.As("s").Col("tenantId"), "acme"),
					clickhouse.Bind(alignHits.As("s").Col("id"), uint64(2))},
			},
			ctx:      tenantCtx("acme"),
			wantSQL:  `INSERT INTO "sprockets" ("id", "tenantId") VALUES (?, ?), (?, ?)`,
			wantArgs: []any{uint64(1), "acme", uint64(2), "acme"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := clickhouse.New(nil)
			ins := db.Insert(alignHits).Rows(tt.rows...)
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
			checkSQL(t, sql, tt.wantSQL)
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
	db := clickhouse.New(nil)
	sql, args, err := db.Insert(alignHits).
		Row(alignHitTen.Val("acme"), alignHitID.Val(1)).
		Row(alignHitTen.Val("globex"), alignForeignTen.Val("initech"), alignHitID.Val(2)).
		Unscoped().
		ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `INSERT INTO "sprockets" ("id", "tenantId") VALUES (?, ?), (?, ?)`)
	if got, want := args, []any{uint64(1), "acme", uint64(2), "initech"}; !reflect.DeepEqual(got, want) {
		t.Errorf("bound arguments:\n got = %#v\nwant = %#v", got, want)
	}
}
