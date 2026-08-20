package mysql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

// pageUser is the row type these tests paginate.
type pageUser struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func pageEntity() (*mysql.Table, *mysql.Col[int64], *mysql.Col[string], *mysql.Entity[pageUser]) {
	t := mysql.NewTable("users")
	id := mysql.Add(t, mysql.BigSerial("id").PrimaryKey())
	name := mysql.Add(t, mysql.Varchar("name", 255).NotNull())
	return t, id, name, mysql.NewEntity[pageUser](t)
}

// pageDriver records every statement and hands back queued rows, so a
// test can assert on the SQL of page N and the rows of page N+1 at
// once.
type pageDriver struct {
	queued  []*pageRows
	queries []string
	args    [][]any
	n       int
}

func (d *pageDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	return plainResult{}, nil
}

func (d *pageDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	var r *pageRows
	if d.n < len(d.queued) {
		r = d.queued[d.n]
	} else {
		r = &pageRows{}
	}
	d.n++
	return r, nil
}

func (d *pageDriver) Begin(context.Context) (drops.Tx, error) { return &pageTx{d}, nil }

type pageTx struct{ *pageDriver }

func (*pageTx) Commit(context.Context) error   { return nil }
func (*pageTx) Rollback(context.Context) error { return nil }

// pageRows scans for real — the cursor is built from the last row's
// values, so a fake that leaves them zero would prove nothing.
type pageRows struct {
	data [][]any
	pos  int
}

func (r *pageRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *pageRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *string:
			*p = row[i].(string)
		}
	}
	return nil
}

func (r *pageRows) Columns() ([]string, error) { return []string{"id", "name"}, nil }
func (r *pageRows) Close() error               { return nil }
func (r *pageRows) Err() error                 { return nil }

func usersRows(pairs ...any) *pageRows {
	data := make([][]any, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		data = append(data, []any{pairs[i], pairs[i+1]})
	}
	return &pageRows{data: data}
}

func TestPageFetchesOneExtraRowToDetectMore(t *testing.T) {
	_, id, _, ent := pageEntity()
	d := &pageDriver{queued: []*pageRows{usersRows(
		int64(1), "a", int64(2), "b", int64(3), "c",
	)}}
	db := mysql.New(d)

	page, err := ent.Page(db).OrderBy(mysql.Asc(id)).Limit(2).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || !page.HasMore {
		t.Fatalf("items = %d, hasMore = %v", len(page.Items), page.HasMore)
	}
	if page.NextCursor == "" {
		t.Fatal("a page with more rows must hand out a cursor")
	}
	// The limit argument is the page size plus the probe row.
	last := d.args[0][len(d.args[0])-1]
	if last != int64(3) {
		t.Errorf("LIMIT arg = %v, want 3", last)
	}
}

func TestPageWithoutMoreRowsHasNoCursor(t *testing.T) {
	_, id, _, ent := pageEntity()
	db := mysql.New(&pageDriver{queued: []*pageRows{usersRows(int64(1), "a")}})

	page, err := ent.Page(db).OrderBy(mysql.Asc(id)).Limit(2).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if page.HasMore || page.NextCursor != "" {
		t.Errorf("hasMore = %v, cursor = %q", page.HasMore, page.NextCursor)
	}
}

// The cursor from page one must produce the guard for page two, over
// the values of the last row actually scanned.
func TestPageCursorResumesFromTheLastRow(t *testing.T) {
	_, id, name, ent := pageEntity()
	d := &pageDriver{queued: []*pageRows{
		usersRows(int64(1), "a", int64(2), "b", int64(3), "c"),
		usersRows(int64(3), "c"),
	}}
	db := mysql.New(d)

	first, err := ent.Page(db).OrderBy(mysql.Asc(name), mysql.Asc(id)).Limit(2).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ent.Page(db).
		OrderBy(mysql.Asc(name), mysql.Asc(id)).
		Limit(2).
		After(first.NextCursor).
		All(context.Background()); err != nil {
		t.Fatal(err)
	}

	sql := d.queries[1]
	if !strings.Contains(sql, "(`users`.`name` > ?)") ||
		!strings.Contains(sql, "((`users`.`name` = ?) AND (`users`.`id` > ?))") {
		t.Errorf("page 2 guard: %s", sql)
	}
	// Values come from row 2 — the last one on page one, not the probe.
	args := d.args[1]
	if args[0] != "b" || args[1] != "b" || args[2] != int64(2) {
		t.Errorf("guard args = %v, want [b b 2 …]", args)
	}
}

func TestPageRequiresOrderBy(t *testing.T) {
	_, _, _, ent := pageEntity()
	db := mysql.New(&pageDriver{})
	if _, err := ent.Page(db).All(context.Background()); err == nil {
		t.Error("expected an error without OrderBy")
	}
}

// Page returns its refusals as errors — it has somewhere to put them,
// unlike the chained SelectBuilder form.
func TestPageRefusesANonUniqueSortKey(t *testing.T) {
	_, _, name, ent := pageEntity()
	db := mysql.New(&pageDriver{})
	_, err := ent.Page(db).OrderBy(mysql.Asc(name)).All(context.Background())
	if err == nil || !strings.Contains(err.Error(), "users.name") {
		t.Errorf("err = %v, want a refusal naming the column", err)
	}
}

func TestPageRejectsACursorFromAnotherOrdering(t *testing.T) {
	_, id, name, ent := pageEntity()
	d := &pageDriver{queued: []*pageRows{usersRows(int64(1), "a", int64(2), "b")}}
	db := mysql.New(d)

	first, err := ent.Page(db).OrderBy(mysql.Asc(id)).Limit(1).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = ent.Page(db).OrderBy(mysql.Asc(name), mysql.Asc(id)).After(first.NextCursor).All(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different ORDER BY") {
		t.Errorf("err = %v, want a refusal", err)
	}
}

// A Page cursor and a hand-built SELECT share one format, so a token
// from either entry point works in the other.
func TestPageCursorIsTheSameTokenAsEncodeCursor(t *testing.T) {
	tbl, id, _, ent := pageEntity()
	d := &pageDriver{queued: []*pageRows{usersRows(int64(1), "a", int64(2), "b")}}
	db := mysql.New(d)

	page, err := ent.Page(db).OrderBy(mysql.Asc(id)).Limit(1).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	spec := mysql.NewCursorSpec(mysql.OrderKey{Col: id})
	sql, _ := sqlOf(db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, mysql.Cursor(page.NextCursor)))
	if strings.Contains(sql, "FALSE") {
		t.Errorf("Page's cursor must be spendable on a SELECT: %s", sql)
	}
	if !strings.Contains(sql, "(`users`.`id` > ?)") {
		t.Errorf("guard: %s", sql)
	}
}
