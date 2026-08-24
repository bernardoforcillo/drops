package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

func mkSoftDeleteTable() (*sqlite.Table, *sqlite.Col[int64], *sqlite.Col[string], sqlite.SoftDeleteCols) {
	posts := sqlite.NewTable("posts")
	id := sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	title := sqlite.Add(posts, sqlite.Text("title").NotNull())
	sd := sqlite.SoftDelete(posts)
	return posts, id, title, sd
}

func TestSoftDeleteScopesSelect(t *testing.T) {
	posts, id, _, _ := mkSoftDeleteTable()
	db := sqlite.New(&fakeDriver{})

	sql, _ := db.Select(id).From(posts).ToSQL()
	if !strings.Contains(sql, `WHERE ("posts"."deletedAt" IS NULL)`) {
		t.Errorf("select not auto-scoped: %s", sql)
	}

	// Explicit predicate ANDs after the default filter.
	sql2, _ := db.Select(id).From(posts).Where(id.Gt(5)).ToSQL()
	if !strings.Contains(sql2, `WHERE ("posts"."deletedAt" IS NULL) AND ("posts"."id" > ?)`) {
		t.Errorf("filter composition wrong: %s", sql2)
	}
}

func TestUnscopedBypassesFilter(t *testing.T) {
	posts, id, _, _ := mkSoftDeleteTable()
	db := sqlite.New(&fakeDriver{})

	sql, _ := db.Select(id).From(posts).Unscoped().ToSQL()
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("Unscoped should drop the filter: %s", sql)
	}
}

func TestSoftDeleteFilterAppliesToUpdateAndDelete(t *testing.T) {
	posts, id, title, _ := mkSoftDeleteTable()
	db := sqlite.New(&fakeDriver{})

	upd, _ := db.Update(posts).Set(title.Val("x")).Where(id.Eq(1)).ToSQL()
	if !strings.Contains(upd, `WHERE ("posts"."deletedAt" IS NULL) AND`) {
		t.Errorf("update not scoped: %s", upd)
	}

	del, _ := db.Delete(posts).Where(id.Eq(1)).ToSQL()
	if !strings.Contains(del, `WHERE ("posts"."deletedAt" IS NULL) AND ("posts"."id" = ?)`) {
		t.Errorf("delete not scoped: %s", del)
	}
	// Unscoped delete hard-deletes without the guard.
	del2, _ := db.Delete(posts).Unscoped().Where(id.Eq(1)).ToSQL()
	if strings.Contains(del2, "deletedAt") {
		t.Errorf("unscoped delete should skip guard: %s", del2)
	}
}

func TestEntitySoftDeleteAndRestore(t *testing.T) {
	posts, _, _, sd := mkSoftDeleteTable()
	type post struct {
		ID    int64  `drop:"id"`
		Title string `drop:"title"`
	}
	ent := sqlite.NewEntity[post](posts)
	drv := &fakeDriver{}
	db := sqlite.New(drv)
	ctx := context.Background()

	if _, err := ent.SoftDeleteByID(db, ctx, int64(7), sd); err != nil {
		t.Fatal(err)
	}
	// SoftDelete is Unscoped (no default filter) and sets CURRENT_TIMESTAMP.
	last := drv.queries[len(drv.queries)-1]
	if last != `UPDATE "posts" SET "deletedAt" = CURRENT_TIMESTAMP WHERE ("posts"."id" = ?)` {
		t.Errorf("soft-delete SQL wrong: %s", last)
	}

	if _, err := ent.Restore(db, ctx, int64(7), sd); err != nil {
		t.Fatal(err)
	}
	// Restore binds the NULL rather than splicing the token, so the
	// statement is the same one every other assignment to this column
	// produces — one entry in a prepared-statement cache, and a value
	// hooks and tracers can see.
	last = drv.queries[len(drv.queries)-1]
	if last != `UPDATE "posts" SET "deletedAt" = ? WHERE ("posts"."id" = ?)` {
		t.Errorf("restore SQL wrong: %s", last)
	}
	if args := drv.args[len(drv.args)-1]; len(args) != 2 || args[0] != nil {
		t.Errorf("restore should bind a NULL parameter, got %#v", args)
	}
}

func TestEntityQueryAutoScopes(t *testing.T) {
	posts, _, _, _ := mkSoftDeleteTable()
	type post struct {
		ID    int64  `drop:"id"`
		Title string `drop:"title"`
	}
	ent := sqlite.NewEntity[post](posts)
	drv := &fakeDriver{}
	db := sqlite.New(drv)

	if _, err := ent.Query(db).All(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.queries[0], `WHERE ("posts"."deletedAt" IS NULL)`) {
		t.Errorf("entity query not scoped: %s", drv.queries[0])
	}
}
