package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// pageUsersSchema returns an entity bound to a table whose PK column
// type matches entUser's ID field.
func pageUsersSchema(t *testing.T) (*pg.Table, *pg.Entity[entUser], *pg.Column) {
	t.Helper()
	tbl, ent := entUsersSchema()
	return tbl, ent, tbl.Col("id")
}

func TestPageFirstPageReturnsCursor(t *testing.T) {
	tbl, ent, _ := pageUsersSchema(t)

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		// LIMIT 3 (page=2 + 1) returns 3 rows → HasMore true.
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{
				{int64(1), "A", "a@x"},
				{int64(2), "B", "b@x"},
				{int64(3), "C", "c@x"},
			},
		}, nil
	}}
	db := pg.New(fd)

	page, err := ent.Page(db).OrderBy(pg.Asc(tbl.Col("id"))).Limit(2).All(context.Background())
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if len(page.Items) != 2 {
		t.Errorf("expected 2 items (limit), got %d", len(page.Items))
	}
	if !page.HasMore {
		t.Error("HasMore should be true")
	}
	if page.NextCursor == "" {
		t.Error("NextCursor should be set when HasMore")
	}
	// Verify LIMIT 3 (limit+1) was issued.
	if !strings.Contains(fd.queries[0], "LIMIT $") {
		t.Errorf("expected LIMIT, got: %s", fd.queries[0])
	}
}

func TestPageLastPageHasNoCursor(t *testing.T) {
	tbl, ent, _ := pageUsersSchema(t)

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		// LIMIT 3 returns 2 rows → HasMore false.
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{
				{int64(1), "A", "a@x"},
				{int64(2), "B", "b@x"},
			},
		}, nil
	}}
	db := pg.New(fd)

	page, err := ent.Page(db).OrderBy(pg.Asc(tbl.Col("id"))).Limit(2).All(context.Background())
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if page.HasMore {
		t.Error("HasMore should be false on the last page")
	}
	if page.NextCursor != "" {
		t.Error("NextCursor should be empty on the last page")
	}
}

func TestPageAfterAppliesCursorGuard(t *testing.T) {
	tbl, ent, _ := pageUsersSchema(t)

	calls := 0
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		calls++
		if calls == 1 {
			// First page: LIMIT 3 (limit+1) returns 3 → HasMore true.
			return &fakeRows{
				cols: []string{"id", "name", "email"},
				data: [][]any{
					{int64(1), "A", "a@x"},
					{int64(2), "B", "b@x"},
					{int64(3), "C", "c@x"},
				},
			}, nil
		}
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(4), "D", "d@x"}},
		}, nil
	}}
	db := pg.New(fd)

	first, err := ent.Page(db).OrderBy(pg.Asc(tbl.Col("id"))).Limit(2).All(context.Background())
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatal("first page should expose a cursor for the next request")
	}

	fd.queries = nil
	if _, err := ent.Page(db).
		OrderBy(pg.Asc(tbl.Col("id"))).
		Limit(2).
		After(first.NextCursor).
		All(context.Background()); err != nil {
		t.Fatalf("second page: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, `("users"."id") > (`) {
		t.Errorf("second page must include cursor guard: %s", sql)
	}
}

func TestPageInvalidCursorErrors(t *testing.T) {
	tbl, ent, _ := pageUsersSchema(t)
	db := pg.New(&fakeDriver{})
	_, err := ent.Page(db).
		OrderBy(pg.Asc(tbl.Col("id"))).
		After("not-a-real-cursor").
		All(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid cursor") {
		t.Errorf("expected invalid-cursor error, got: %v", err)
	}
}

func TestPageRequiresOrderBy(t *testing.T) {
	_, ent, _ := pageUsersSchema(t)
	db := pg.New(&fakeDriver{})
	_, err := ent.Page(db).All(context.Background())
	if err == nil || !strings.Contains(err.Error(), "OrderBy") {
		t.Errorf("expected OrderBy-required error, got: %v", err)
	}
}

// ----------------------------------------------------------------------
// Stream
// ----------------------------------------------------------------------

func TestStreamIteratesEveryRow(t *testing.T) {
	_, ent := entUsersSchema()

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{
				{int64(1), "A", "a@x"},
				{int64(2), "B", "b@x"},
				{int64(3), "C", "c@x"},
			},
		}, nil
	}}
	db := pg.New(fd)

	var got []string
	err := ent.Query(db).Stream(context.Background(), func(u *entUser) error {
		got = append(got, u.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 3 || got[0] != "A" || got[2] != "C" {
		t.Errorf("Stream missed rows: %v", got)
	}
}

func TestStreamPropagatesUserError(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "name", "email"},
			data: [][]any{{int64(1), "X", "x@x"}},
		}, nil
	}}
	db := pg.New(fd)
	want := "stop"
	err := ent.Query(db).Stream(context.Background(), func(u *entUser) error {
		return &fakeStopError{msg: want}
	})
	if err == nil || err.Error() != want {
		t.Errorf("Stream must propagate fn error, got %v", err)
	}
}

type fakeStopError struct{ msg string }

func (e *fakeStopError) Error() string { return e.msg }

// ----------------------------------------------------------------------
// CreateMany / UpsertMany
// ----------------------------------------------------------------------

func TestCreateManyBatchesInsert(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{}
	db := pg.New(fd)

	rows := []entUser{
		{Name: "A", Email: "a@x"},
		{Name: "B", Email: "b@x"},
		{Name: "C", Email: "c@x"},
	}
	if _, err := ent.CreateMany(db, context.Background(), rows); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}
	if len(fd.queries) != 1 {
		t.Errorf("expected 1 batched query, got %d", len(fd.queries))
	}
	if !strings.HasPrefix(fd.queries[0], "INSERT INTO") {
		t.Errorf("expected INSERT, got: %s", fd.queries[0])
	}
	if strings.Count(fd.queries[0], "VALUES") != 1 {
		t.Errorf("expected single VALUES clause: %s", fd.queries[0])
	}
}

func TestUpsertManyEmitsOnConflict(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{}
	db := pg.New(fd)

	if _, err := ent.UpsertMany(db, context.Background(), []entUser{
		{ID: 1, Name: "A", Email: "a@x"},
		{ID: 2, Name: "B", Email: "b@x"},
	}); err != nil {
		t.Fatalf("UpsertMany: %v", err)
	}
	sql := fd.queries[0]
	if !strings.Contains(sql, "ON CONFLICT") {
		t.Errorf("UpsertMany must emit ON CONFLICT: %s", sql)
	}
	if !strings.Contains(sql, "EXCLUDED") {
		t.Errorf("UpsertMany must use EXCLUDED to copy values: %s", sql)
	}
}
