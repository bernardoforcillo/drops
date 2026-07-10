package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// pageUser is the row type paginated in these tests.
type pageUser struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func mkPageEntity() (*sqlite.Table, *sqlite.Col[int64], *sqlite.Col[string], *sqlite.Entity[pageUser]) {
	users := sqlite.NewTable("users")
	id := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	name := sqlite.Add(users, sqlite.Text("name").NotNull())
	return users, id, name, sqlite.NewEntity[pageUser](users)
}

// pageDriver records each query's SQL+args and returns queued rows.
type pageDriver struct {
	queued  []*fakeRows
	queries []string
	args    [][]any
	n       int
}

func (d *pageDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	return fakeResult{}, nil
}
func (d *pageDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	var r *fakeRows
	if d.n < len(d.queued) {
		r = d.queued[d.n]
	} else {
		r = &fakeRows{}
	}
	d.n++
	return r, nil
}
func (d *pageDriver) Begin(_ context.Context) (drops.Tx, error) { return &fakeTx{&fakeDriver{}}, nil }

func rowsOf(pairs ...any) *fakeRows {
	data := make([][]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		data = append(data, []any{pairs[i], pairs[i+1]})
	}
	return &fakeRows{cols: []string{"id", "name"}, data: data}
}

func TestPageFirstPageHasMoreAndCursor(t *testing.T) {
	users, id, _, ent := mkPageEntity()
	_ = users
	// limit 2 → fetch 3; driver returns 3 rows, so HasMore is true.
	drv := &pageDriver{queued: []*fakeRows{
		rowsOf(int64(1), "a", int64(2), "b", int64(3), "c"),
	}}
	db := sqlite.New(drv)

	p, err := ent.Page(db).OrderBy(sqlite.Asc(id)).Limit(2).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 2 || !p.HasMore || p.NextCursor == "" {
		t.Fatalf("page: items=%d hasMore=%v cursor=%q", len(p.Items), p.HasMore, p.NextCursor)
	}
	if p.Items[1].ID != 2 {
		t.Errorf("trimmed to wrong rows: %+v", p.Items)
	}
	// Query must order and fetch limit+1.
	q := drv.queries[0]
	if !strings.Contains(q, `ORDER BY "users"."id" ASC`) {
		t.Errorf("missing ORDER BY: %s", q)
	}
	if got := drv.args[0][0]; got != int64(3) {
		t.Errorf("LIMIT should be limit+1=3, got %v", got)
	}
}

func TestPageCursorGuardResumesAfter(t *testing.T) {
	_, id, _, ent := mkPageEntity()
	drv := &pageDriver{queued: []*fakeRows{
		rowsOf(int64(1), "a", int64(2), "b", int64(3), "c"), // page 1 (limit 2 → 3 rows)
		rowsOf(int64(3), "c"),                               // page 2
	}}
	db := sqlite.New(drv)

	p1, err := ent.Page(db).OrderBy(sqlite.Asc(id)).Limit(2).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Page 2 uses the cursor.
	p2, err := ent.Page(db).OrderBy(sqlite.Asc(id)).Limit(2).After(p1.NextCursor).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 1 || p2.HasMore {
		t.Errorf("last page wrong: items=%d hasMore=%v", len(p2.Items), p2.HasMore)
	}
	// The second query must carry the row-comparison guard and bind the
	// last id from page 1 (which was 2).
	q2 := drv.queries[1]
	if !strings.Contains(q2, `("users"."id") > (?)`) {
		t.Errorf("missing keyset guard: %s", q2)
	}
	if drv.args[1][0] != int64(2) {
		t.Errorf("cursor arg should be last id 2, got %v", drv.args[1][0])
	}
}

func TestPageLastPageNoCursor(t *testing.T) {
	_, id, _, ent := mkPageEntity()
	drv := &pageDriver{queued: []*fakeRows{
		rowsOf(int64(1), "a", int64(2), "b"), // exactly limit → no more
	}}
	db := sqlite.New(drv)

	p, err := ent.Page(db).OrderBy(sqlite.Asc(id)).Limit(2).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.HasMore || p.NextCursor != "" {
		t.Errorf("should be last page: hasMore=%v cursor=%q", p.HasMore, p.NextCursor)
	}
}

func TestPageMixedDirectionGuard(t *testing.T) {
	_, id, name, ent := mkPageEntity()
	drv := &pageDriver{queued: []*fakeRows{
		rowsOf(int64(1), "a", int64(2), "b"), // 2 rows for limit 1 → hasMore
		rowsOf(int64(2), "b"),
	}}
	db := sqlite.New(drv)

	// name DESC, id ASC — mixed directions force the disjunction form.
	p1, err := ent.Page(db).OrderBy(sqlite.Desc(name), sqlite.Asc(id)).Limit(1).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ent.Page(db).OrderBy(sqlite.Desc(name), sqlite.Asc(id)).Limit(1).
		After(p1.NextCursor).All(context.Background()); err != nil {
		t.Fatal(err)
	}
	q2 := drv.queries[1]
	// Disjunction: name < v OR (name = v AND id > v)
	if !strings.Contains(q2, ` OR `) || !strings.Contains(q2, `"users"."name" < ?`) ||
		!strings.Contains(q2, `"users"."id" > ?`) {
		t.Errorf("mixed-direction guard wrong: %s", q2)
	}
}

func TestPageRequiresOrderBy(t *testing.T) {
	_, _, _, ent := mkPageEntity()
	db := sqlite.New(&pageDriver{})
	if _, err := ent.Page(db).All(context.Background()); err == nil {
		t.Error("expected error without OrderBy")
	}
}

func TestPageInvalidCursor(t *testing.T) {
	_, id, _, ent := mkPageEntity()
	db := sqlite.New(&pageDriver{})
	_, err := ent.Page(db).OrderBy(sqlite.Asc(id)).After("!!!not-base64!!!").All(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid cursor") {
		t.Errorf("want invalid cursor error, got %v", err)
	}
}
