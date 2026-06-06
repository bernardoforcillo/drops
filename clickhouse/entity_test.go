package clickhouse_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// entFakeDriver is a richer driver for entity tests — records both
// SQL and args, and lets us script a Query response.
type entFakeDriver struct {
	queries []string
	args    [][]any
	handler func(string, []any) (drops.Rows, error)
}

func (f *entFakeDriver) Close() error { return nil }
func (f *entFakeDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
	return entFakeResult{}, nil
}
func (f *entFakeDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
	if f.handler == nil {
		return &entFakeRows{}, nil
	}
	return f.handler(sql, args)
}
func (f *entFakeDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, nil }

type entFakeResult struct{}

func (entFakeResult) RowsAffected() (int64, error) { return 0, nil }

type entFakeRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *entFakeRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}
func (r *entFakeRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		rv := reflect.ValueOf(d).Elem()
		val := reflect.ValueOf(row[i])
		if val.IsValid() {
			rv.Set(val)
		}
	}
	return nil
}
func (r *entFakeRows) Columns() ([]string, error) { return r.cols, nil }
func (r *entFakeRows) Close() error               { return nil }
func (r *entFakeRows) Err() error                 { return nil }

type entEvent struct {
	ID     string `drop:"id"`
	UserID uint64 `drop:"userId"`
	Kind   string `drop:"kind"`
}

func entEventsSchema() (*clickhouse.Table, *clickhouse.Entity[entEvent]) {
	tbl := clickhouse.NewTable("events").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	clickhouse.Add(tbl, clickhouse.UInt64("userId"))
	clickhouse.Add(tbl, clickhouse.String("kind"))
	tbl.OrderBy(id)
	return tbl, clickhouse.NewEntity[entEvent](tbl)
}

func TestClickhouseEntityCreate(t *testing.T) {
	_, ent := entEventsSchema()
	fd := &entFakeDriver{}
	db := clickhouse.New(fd)
	ev := entEvent{ID: "u1", UserID: 7, Kind: "click"}
	if _, err := ent.Create(db, context.Background(), &ev); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(fd.queries) != 1 {
		t.Fatalf("expected 1 query, got %d", len(fd.queries))
	}
	if !strings.HasPrefix(fd.queries[0], "INSERT INTO") {
		t.Errorf("expected INSERT, got: %s", fd.queries[0])
	}
	if len(fd.args[0]) != 3 {
		t.Errorf("expected 3 args, got %d: %v", len(fd.args[0]), fd.args[0])
	}
}

func TestClickhouseEntityCreateMany(t *testing.T) {
	_, ent := entEventsSchema()
	fd := &entFakeDriver{}
	db := clickhouse.New(fd)
	evs := []entEvent{
		{ID: "u1", UserID: 1, Kind: "click"},
		{ID: "u2", UserID: 2, Kind: "view"},
	}
	if _, err := ent.CreateMany(db, context.Background(), evs); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	if !strings.Contains(fd.queries[0], "VALUES") || strings.Count(fd.queries[0], "(?, ?, ?)") != 2 {
		t.Errorf("expected 2-row batch, got: %s", fd.queries[0])
	}
}

func TestClickhouseEntityQueryAll(t *testing.T) {
	_, ent := entEventsSchema()
	fd := &entFakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &entFakeRows{
			cols: []string{"id", "userId", "kind"},
			data: [][]any{
				{"u1", uint64(1), "click"},
				{"u2", uint64(2), "view"},
			},
		}, nil
	}}
	db := clickhouse.New(fd)
	out, err := ent.Query(db).Limit(10).All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(out) != 2 || out[0].ID != "u1" || out[1].UserID != 2 {
		t.Errorf("unexpected rows: %+v", out)
	}
}

func TestClickhouseEntityValidatorBlocksCreate(t *testing.T) {
	_, ent := entEventsSchema()
	errBad := errors.New("kind required")
	ent.Validate(func(e *entEvent) error {
		if e.Kind == "" {
			return errBad
		}
		return nil
	})
	fd := &entFakeDriver{}
	db := clickhouse.New(fd)
	ev := entEvent{ID: "u1", UserID: 7}
	if _, err := ent.Create(db, context.Background(), &ev); !errors.Is(err, errBad) {
		t.Errorf("expected validator error, got %v", err)
	}
	if len(fd.queries) != 0 {
		t.Errorf("validator must abort before SQL")
	}
}

func TestClickhouseEntityValidatorAbortsBatch(t *testing.T) {
	_, ent := entEventsSchema()
	errBad := errors.New("invalid")
	ent.Validate(func(e *entEvent) error {
		if e.Kind == "bad" {
			return errBad
		}
		return nil
	})
	fd := &entFakeDriver{}
	db := clickhouse.New(fd)
	evs := []entEvent{
		{ID: "u1", UserID: 1, Kind: "click"},
		{ID: "u2", UserID: 2, Kind: "bad"},
		{ID: "u3", UserID: 3, Kind: "view"},
	}
	if _, err := ent.CreateMany(db, context.Background(), evs); !errors.Is(err, errBad) {
		t.Errorf("expected validator error, got %v", err)
	}
	if len(fd.queries) != 0 {
		t.Errorf("validator failure on any row must abort the whole batch")
	}
}

func TestClickhouseEntityRespectsDefaultFilter(t *testing.T) {
	tbl := clickhouse.NewTable("events").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	clickhouse.Add(tbl, clickhouse.UInt64("userId"))
	clickhouse.Add(tbl, clickhouse.String("kind"))
	deleted := clickhouse.Add(tbl, clickhouse.DateTime("deletedAt", "").Nullable())
	tbl.OrderBy(id)
	tbl.DefaultFilter(deleted.IsNull())

	type ev struct {
		ID     string `drop:"id"`
		UserID uint64 `drop:"userId"`
		Kind   string `drop:"kind"`
	}
	ent := clickhouse.NewEntity[ev](tbl)
	fd := &entFakeDriver{}
	db := clickhouse.New(fd)
	_, _ = ent.Query(db).All(context.Background())
	if !strings.Contains(fd.queries[0], "deletedAt") {
		t.Errorf("default filter missing on entity query: %s", fd.queries[0])
	}

	fd.queries = nil
	_, _ = ent.Query(db).Unscoped().All(context.Background())
	if strings.Contains(fd.queries[0], "deletedAt") {
		t.Errorf("Unscoped must drop default filter: %s", fd.queries[0])
	}
}
