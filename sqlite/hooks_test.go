package sqlite_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

func softDeleteTable() (*sqlite.Table, *sqlite.Col[int64], *sqlite.Col[string]) {
	posts := sqlite.NewTable("posts")
	id := sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	title := sqlite.Add(posts, sqlite.Text("title"))
	sqlite.ApplyMixins(posts, &sqlite.TimestampsMixin{}, &sqlite.SoftDeleteMixin{})
	return posts, id, title
}

func TestSoftDeleteSelectScope(t *testing.T) {
	posts, _, _ := softDeleteTable()
	db := sqlite.New(&entDriver{})

	sql, _ := db.Select().From(posts).ToSQL()
	if !strings.Contains(sql, `WHERE ("posts"."deletedAt" IS NULL)`) {
		t.Errorf("default scope absent:\n%s", sql)
	}
	// Unscoped bypasses the filter.
	usql, _ := db.Select().From(posts).Unscoped().ToSQL()
	if strings.Contains(usql, "deletedAt") {
		t.Errorf("unscoped should drop the filter:\n%s", usql)
	}
}

func TestSoftDeleteRewritesDelete(t *testing.T) {
	posts, id, _ := softDeleteTable()
	db := sqlite.New(&entDriver{})

	sql, _ := db.Delete(posts).Where(id.Eq(1)).ToSQL()
	if !strings.HasPrefix(sql, `UPDATE "posts" SET "deletedAt" = CURRENT_TIMESTAMP`) {
		t.Errorf("delete not rewritten to soft-delete UPDATE:\n%s", sql)
	}
	if !strings.Contains(sql, `("posts"."id" = ?)`) {
		t.Errorf("original WHERE lost:\n%s", sql)
	}
	// Unscoped forces a real DELETE.
	hard, _ := db.Delete(posts).Where(id.Eq(1)).Unscoped().ToSQL()
	if !strings.HasPrefix(hard, `DELETE FROM "posts"`) {
		t.Errorf("unscoped should hard-delete:\n%s", hard)
	}
}

func TestTimestampsUpdateHook(t *testing.T) {
	posts, id, title := softDeleteTable()
	db := sqlite.New(&entDriver{})

	sql, _ := db.Update(posts).Set(title.Val("x")).Where(id.Eq(1)).ToSQL()
	if !strings.Contains(sql, `"updatedAt" = CURRENT_TIMESTAMP`) {
		t.Errorf("updatedAt not bumped:\n%s", sql)
	}
	// A caller-supplied updatedAt wins (hook is a no-op then). Not
	// asserted here since the column handle isn't exported from the
	// mixin; the no-clobber path is covered by the hook's Has() guard.
}

func TestInsertHookAddsColumn(t *testing.T) {
	users := sqlite.NewTable("users")
	uid := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	source := sqlite.Add(users, sqlite.Text("source"))
	users.OnInsert(sqlite.InsertHookFunc(func(c *sqlite.InsertHookCtx) {
		c.Set(source.Val("api"))
	}))
	db := sqlite.New(&entDriver{})

	sql, args := db.Insert(users).Values(uid.Val(int64(1))).ToSQL()
	if !strings.Contains(sql, `("id", "source")`) {
		t.Errorf("hook column not added:\n%s", sql)
	}
	if len(args) != 2 || args[1] != "api" {
		t.Errorf("hook value not bound: %v", args)
	}
	// A user-bound column is not clobbered by the hook.
	sql2, args2 := db.Insert(users).Values(uid.Val(int64(2)), source.Val("manual")).ToSQL()
	if strings.Count(sql2, "source") != 1 || args2[1] != "manual" {
		t.Errorf("hook clobbered user value: %s %v", sql2, args2)
	}
}

func TestUUIDPrimaryKeyMixin(t *testing.T) {
	things := sqlite.NewTable("things")
	m := &sqlite.UUIDPrimaryKeyMixin{}
	sqlite.ApplyMixins(things, m)
	sql, _ := sqlite.ToSQL(sqlite.CreateTable(things))
	if !strings.Contains(sql, `"id" TEXT`) || !strings.Contains(sql, "randomblob") {
		t.Errorf("uuid pk default absent:\n%s", sql)
	}
}
