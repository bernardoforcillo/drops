package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// --- canned-PRAGMA fake driver ---------------------------------------
//
// Renamed from sqlite_test.go's fakeDriver/fakeRows to inDriver/inRows so
// the two test files coexist in package sqlite_test. This driver returns
// a canned result set chosen by matching a substring of the SQL text,
// which is enough to feed Introspect its sqlite_master / PRAGMA rows.

type inRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *inRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *inRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *any:
			*p = row[i]
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		}
	}
	return nil
}
func (r *inRows) Columns() ([]string, error) { return r.cols, nil }
func (r *inRows) Close() error               { return nil }
func (r *inRows) Err() error                 { return nil }

type inResult struct{}

func (inResult) RowsAffected() (int64, error) { return 0, nil }

// inDriver dispatches each Query to a canned result set. matchers is an
// ordered list of (substring, rows) pairs; the first substring found in
// the SQL wins.
type inDriver struct {
	matchers []inMatch
}

type inMatch struct {
	contains string
	rows     *inRows
}

func (d *inDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return inResult{}, nil
}
func (d *inDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	for _, m := range d.matchers {
		if strings.Contains(sql, m.contains) {
			// return a fresh cursor each call
			cp := *m.rows
			cp.pos = 0
			return &cp, nil
		}
	}
	return &inRows{}, nil
}
func (d *inDriver) Begin(_ context.Context) (drops.Tx, error) { return &inTx{d}, nil }

type inTx struct{ *inDriver }

func (*inTx) Commit(_ context.Context) error   { return nil }
func (*inTx) Rollback(_ context.Context) error { return nil }

// --- test -------------------------------------------------------------

func TestIntrospectAssemblesTable(t *testing.T) {
	drv := &inDriver{matchers: []inMatch{
		{
			contains: "sqlite_master",
			rows: &inRows{
				cols: []string{"name"},
				data: [][]any{{"orders"}},
			},
		},
		{
			contains: "table_info",
			rows: &inRows{
				cols: []string{"cid", "name", "type", "notnull", "dflt_value", "pk"},
				data: [][]any{
					{int64(0), "id", "INTEGER", int64(1), nil, int64(1)},
					{int64(1), "userId", "INTEGER", int64(1), nil, int64(0)},
					{int64(2), "amount", "REAL", int64(1), "0", int64(0)},
				},
			},
		},
		{
			contains: "foreign_key_list",
			rows: &inRows{
				cols: []string{"id", "seq", "table", "from", "to", "on_update", "on_delete", "match"},
				data: [][]any{
					{int64(0), int64(0), "users", "userId", "id", "NO ACTION", "CASCADE", "NONE"},
				},
			},
		},
		{
			contains: "index_list",
			rows: &inRows{
				cols: []string{"seq", "name", "unique", "origin", "partial"},
				data: [][]any{
					{int64(0), "orders_userId_unique", int64(1), "u", int64(0)},
				},
			},
		},
		{
			contains: "index_info",
			rows: &inRows{
				cols: []string{"seqno", "cid", "name"},
				data: [][]any{
					{int64(0), int64(1), "userId"},
				},
			},
		},
	}}

	db := sqlite.New(drv)
	snap, err := sqlite.Introspect(context.Background(), db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}

	ts, ok := snap.Tables["orders"]
	if !ok {
		t.Fatalf("orders table missing; got %v", keysOf(snap.Tables))
	}

	// Columns.
	if len(ts.Columns) != 3 {
		t.Errorf("columns: got %d want 3", len(ts.Columns))
	}
	id, ok := ts.Columns["id"]
	if !ok {
		t.Fatalf("id column missing")
	}
	if id.Type != "INTEGER" {
		t.Errorf("id type: got %q want INTEGER", id.Type)
	}
	if !id.NotNull {
		t.Errorf("id should be NOT NULL")
	}
	if !id.PrimaryKey {
		t.Errorf("id should be PrimaryKey (single-column pk)")
	}

	// Default handling: NULL -> nil, present -> pointer.
	if id.Default != nil {
		t.Errorf("id default: got %q want nil", *id.Default)
	}
	amount, ok := ts.Columns["amount"]
	if !ok {
		t.Fatalf("amount column missing")
	}
	if amount.Default == nil || *amount.Default != "0" {
		t.Errorf("amount default: got %v want \"0\"", amount.Default)
	}

	// Single-column pk means no composite pk.
	if len(ts.CompositePrimaryKeys) != 0 {
		t.Errorf("composite pk: got %d want 0", len(ts.CompositePrimaryKeys))
	}

	// Foreign key.
	if len(ts.ForeignKeys) != 1 {
		t.Fatalf("foreign keys: got %d want 1", len(ts.ForeignKeys))
	}
	var fk *sqlite.ForeignKeySnapshot
	for _, v := range ts.ForeignKeys {
		fk = v
	}
	if fk.TableTo != "users" {
		t.Errorf("fk tableTo: got %q want users", fk.TableTo)
	}
	if len(fk.ColumnsFrom) != 1 || fk.ColumnsFrom[0] != "userId" {
		t.Errorf("fk columnsFrom: got %v want [userId]", fk.ColumnsFrom)
	}
	if len(fk.ColumnsTo) != 1 || fk.ColumnsTo[0] != "id" {
		t.Errorf("fk columnsTo: got %v want [id]", fk.ColumnsTo)
	}
	if fk.OnDelete != "cascade" {
		t.Errorf("fk onDelete: got %q want cascade", fk.OnDelete)
	}
	if fk.OnUpdate != "no action" {
		t.Errorf("fk onUpdate: got %q want \"no action\"", fk.OnUpdate)
	}

	// Unique constraint from index_list/index_info.
	if len(ts.UniqueConstraints) != 1 {
		t.Errorf("unique constraints: got %d want 1", len(ts.UniqueConstraints))
	}
}

func keysOf(m map[string]*sqlite.TableSnapshot) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
