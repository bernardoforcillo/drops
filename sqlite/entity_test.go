package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

type entDriver struct {
	queries []string
	args    [][]any
	rows    *entRows
}

func (d *entDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	return entResult{}, nil
}
func (d *entDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if d.rows != nil {
		return d.rows, nil
	}
	return &entRows{}, nil
}
func (d *entDriver) Begin(_ context.Context) (drops.Tx, error) { return &entTx{d}, nil }

type entTx struct{ *entDriver }

func (*entTx) Commit(_ context.Context) error   { return nil }
func (*entTx) Rollback(_ context.Context) error { return nil }

type entResult struct{}

func (entResult) RowsAffected() (int64, error) { return 1, nil }

type entRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *entRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}
func (r *entRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		}
	}
	return nil
}
func (r *entRows) Columns() ([]string, error) { return r.cols, nil }
func (r *entRows) Close() error               { return nil }
func (r *entRows) Err() error                 { return nil }

type entUser struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func entSchema() *sqlite.Table {
	t := sqlite.NewTable("users")
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(t, sqlite.Text("name").NotNull())
	return t
}

func TestEntityCreateAndGet(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)

	drv := &entDriver{}
	db := sqlite.New(drv)

	if err := ent.Create(db, context.Background(), &entUser{ID: 1, Name: "alice"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(drv.queries) != 1 || !strings.HasPrefix(drv.queries[0], `INSERT INTO "users"`) {
		t.Errorf("expected INSERT, got %v", drv.queries)
	}

	// Get: return one canned row.
	drv.rows = &entRows{cols: []string{"id", "name"}, data: [][]any{{int64(7), "bob"}}}
	got, err := ent.Get(db, context.Background(), 7)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != 7 || got.Name != "bob" {
		t.Errorf("Get returned %+v", got)
	}
	last := drv.queries[len(drv.queries)-1]
	if !strings.Contains(last, `WHERE ("users"."id" = ?)`) {
		t.Errorf("Get query missing PK predicate: %s", last)
	}
}

func TestEntityUpdateAndDelete(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)
	drv := &entDriver{}
	db := sqlite.New(drv)

	if err := ent.Update(db, context.Background(), &entUser{ID: 3, Name: "carol"}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	upd := drv.queries[len(drv.queries)-1]
	// The PK column must not be in the SET list, only in WHERE.
	if !strings.HasPrefix(upd, `UPDATE "users" SET "name" = ?`) || !strings.Contains(upd, `WHERE ("users"."id" = ?)`) {
		t.Errorf("update sql: %s", upd)
	}

	if _, err := ent.Delete(db, context.Background(), 3); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	del := drv.queries[len(drv.queries)-1]
	if del != `DELETE FROM "users" WHERE ("users"."id" = ?)` {
		t.Errorf("delete sql: %s", del)
	}
}

func TestEntityQuery(t *testing.T) {
	users := entSchema()
	ent := sqlite.NewEntity[entUser](users)
	drv := &entDriver{rows: &entRows{
		cols: []string{"id", "name"},
		data: [][]any{{int64(1), "a"}, {int64(2), "b"}},
	}}
	db := sqlite.New(drv)

	id := users.Col("id")
	out, err := ent.Query(db).Where(sqlite.And()).OrderBy(id).Limit(10).All(context.Background())
	if err != nil {
		t.Fatalf("Query.All: %v", err)
	}
	if len(out) != 2 || out[1].Name != "b" {
		t.Errorf("query result: %+v", out)
	}
}
