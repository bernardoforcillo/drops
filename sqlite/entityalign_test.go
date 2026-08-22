package sqlite_test

import (
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// sqlite's alignment, pinned on rendered SQL.
//
// The tenant policy block says the alignment that places a row against
// a column list an earlier row fixed is pg's, mysql's and clickhouse's,
// and that sqlite's is a different shape: [Entity.alignBindings] widens
// every row of a batch to the UNION of the columns the batch binds. The
// two differ in what happens to a binding the index does not match. A
// placing alignment discards it — that is the leak the phase closed
// three times, most recently in alignRow, where a binding for the
// tenant axis taken off another table object was dropped and the row
// went out belonging to nobody. A widening has a slot for every column
// any row binds, so there is nothing to discard.
//
// That is why the entity batch is allowed to keep asking the identity
// question, and why the root-level
// TestNoColumnKeyComparisonOnARenderedNamePath exempts it: the bindings
// it indexes were all built by this package from the entity's own
// colFields, sqlite has no Entity.CreateCols for a caller to name a
// column through, and no binding is dropped even when the index matches
// none. The first two are facts about the API and cannot be rendered.
// The third can, and is, here — because it is the one the exemption
// would lose if the widening ever became a placing.
//
// The batch is what makes the shape visible: a single row's INSERT
// names exactly the columns it bound, and any alignment is a no-op.

type eaPost struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
	Kind     string `drop:"kind"`
}

// eaTable declares a batch-visible column: kind carries a DEFAULT, so a
// row that leaves it zero binds nothing for it and the widening has a
// gap to fill.
func eaTable(name string) *sqlite.Table {
	t := sqlite.NewTable(name)
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenantCol := sqlite.Add(t, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(t, sqlite.Text("title"))
	sqlite.Add(t, sqlite.Text("kind").Default("'note'"))
	t.ContextFilter(sqlite.TenantFilter(tenantCol)).ScopeWritesByTenant(tenantCol)
	return t
}

func TestEntityBatchWidensRatherThanDiscards(t *testing.T) {
	cases := []struct {
		name string
		rows []eaPost
		sql  string
		args []any
	}{
		{
			// The row that binds the extra column comes second: the
			// column list is the union, not row zero's.
			name: "gap first",
			rows: []eaPost{{Title: "a"}, {Title: "b", Kind: "link"}},
			sql:  `INSERT INTO "ea1" ("tenantId", "title", "kind") VALUES (?, ?, 'note'), (?, ?, ?)`,
			args: []any{scopeTenant, "a", scopeTenant, "b", "link"},
		},
		{
			// And the other way round, which is the case a placing
			// alignment gets wrong in silence: row zero fixes three
			// columns, row one binds two, and the values must land
			// under the names they were bound to rather than shifting
			// left into "kind".
			name: "gap second",
			rows: []eaPost{{Title: "a", Kind: "link"}, {Title: "b"}},
			sql:  `INSERT INTO "ea2" ("tenantId", "title", "kind") VALUES (?, ?, ?), (?, ?, 'note')`,
			args: []any{scopeTenant, "a", "link", scopeTenant, "b"},
		},
		{
			// Every row binding the same set is the no-op case, kept
			// so a change that only ever widens is visible as one.
			name: "no gap",
			rows: []eaPost{{Title: "a", Kind: "link"}, {Title: "b", Kind: "note"}},
			sql:  `INSERT INTO "ea3" ("tenantId", "title", "kind") VALUES (?, ?, ?), (?, ?, ?)`,
			args: []any{scopeTenant, "a", "link", scopeTenant, "b", "note"},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tbl := eaTable([]string{"ea1", "ea2", "ea3"}[i])
			ent := sqlite.NewEntity[eaPost](tbl)
			drv := dropstest.New()
			db := sqlite.New(drv)

			if _, err := ent.CreateMany(db, scopeCtx(), tc.rows); err != nil {
				t.Fatalf("CreateMany: %v", err)
			}
			last := drv.Last()
			if last.SQL != tc.sql {
				t.Errorf("sql = %v, want %v", last.SQL, tc.sql)
			}
			if !sameArgs(last.Args, tc.args) {
				t.Errorf("args = %v, want %v", last.Args, tc.args)
			}
		})
	}
}
