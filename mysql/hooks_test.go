package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

func softDeleteTable() (*mysql.Table, *mysql.Col[int64], *mysql.Col[string]) {
	posts := mysql.NewTable("posts")
	id := mysql.Add(posts, mysql.BigInt("id").PrimaryKey())
	title := mysql.Add(posts, mysql.Text("title"))
	mysql.ApplyMixins(posts, &mysql.TimestampsMixin{}, &mysql.SoftDeleteMixin{})
	return posts, id, title
}

func TestSoftDeleteSelectScope(t *testing.T) {
	posts, _, _ := softDeleteTable()
	db := mysql.New(&frDriver{})

	sql, _ := db.Select().From(posts).ToSQL()
	if !strings.Contains(sql, "WHERE (`posts`.`deletedAt` IS NULL)") {
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
	db := mysql.New(&frDriver{})

	sql, _ := db.Delete(posts).Where(id.Eq(1)).ToSQL()
	if !strings.HasPrefix(sql, "UPDATE `posts` SET `deletedAt` = CURRENT_TIMESTAMP") {
		t.Errorf("delete not rewritten to soft-delete UPDATE:\n%s", sql)
	}
	if !strings.Contains(sql, "(`posts`.`id` = ?)") {
		t.Errorf("original WHERE lost:\n%s", sql)
	}
	// Unscoped forces a real DELETE.
	hard, _ := db.Delete(posts).Where(id.Eq(1)).Unscoped().ToSQL()
	if !strings.HasPrefix(hard, "DELETE FROM `posts`") {
		t.Errorf("unscoped should hard-delete:\n%s", hard)
	}
}

func TestTimestampsUpdateHook(t *testing.T) {
	posts, id, title := softDeleteTable()
	db := mysql.New(&frDriver{})

	sql, _ := db.Update(posts).Set(title.Val("x")).Where(id.Eq(1)).ToSQL()
	if !strings.Contains(sql, "`updatedAt` = CURRENT_TIMESTAMP") {
		t.Errorf("updatedAt not bumped:\n%s", sql)
	}
	// A caller-supplied updatedAt wins (hook is a no-op then). Not
	// asserted here since the column handle isn't exported from the
	// mixin; the no-clobber path is covered by the hook's Has() guard.
}

func TestInsertHookAddsColumn(t *testing.T) {
	users := mysql.NewTable("users")
	uid := mysql.Add(users, mysql.BigInt("id").PrimaryKey())
	source := mysql.Add(users, mysql.Text("source"))
	users.OnInsert(mysql.InsertHookFunc(func(c *mysql.InsertHookCtx) {
		c.Set(source.Val("api"))
	}))
	db := mysql.New(&frDriver{})

	sql, args := db.Insert(users).Row(uid.Val(int64(1))).ToSQL()
	if !strings.Contains(sql, "(`id`, `source`)") {
		t.Errorf("hook column not added:\n%s", sql)
	}
	if len(args) != 2 || args[1] != "api" {
		t.Errorf("hook value not bound: %v", args)
	}
	// A user-bound column is not clobbered by the hook.
	sql2, args2 := db.Insert(users).Row(uid.Val(int64(2)), source.Val("manual")).ToSQL()
	if strings.Count(sql2, "source") != 1 || args2[1] != "manual" {
		t.Errorf("hook clobbered user value: %s %v", sql2, args2)
	}
}

func TestUUIDPrimaryKeyMixin(t *testing.T) {
	things := mysql.NewTable("things")
	m := &mysql.UUIDPrimaryKeyMixin{}
	mysql.ApplyMixins(things, m)
	sql, _ := mysql.ToSQL(mysql.CreateTable(things))
	// CHAR(36) rather than TEXT: this package maps UUID to a fixed-width
	// char column, which is what MySQL indexes well. And the default is
	// the server's own UUID() — the expression drops/sqlite builds out
	// of randomblob has no counterpart here, and the parenthesised form
	// is the one MySQL 8.0.13+ and MariaDB both accept in DEFAULT.
	if !strings.Contains(sql, "`id` CHAR(36)") || !strings.Contains(sql, "(UUID())") {
		t.Errorf("uuid pk default absent:\n%s", sql)
	}
}
