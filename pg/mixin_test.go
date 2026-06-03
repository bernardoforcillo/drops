package pg_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

func timeMustParse(s string) time.Time {
	tt, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return tt
}

// TestTimestampsMixin verifies that the rich mixin variant adds the
// columns AND registers an UpdateHook that bumps updated_at.
func TestTimestampsMixin(t *testing.T) {
	tbl := pg.NewTable("widgets")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	m := &pg.TimestampsMixin{}
	pg.ApplyMixins(tbl, m)

	if m.Cols.CreatedAt == nil || m.Cols.UpdatedAt == nil {
		t.Fatal("TimestampsMixin must populate Cols on Apply")
	}

	db := pg.New(nil)
	id, _ := pg.Add(tbl, pg.Integer("dummy")), 1
	_ = id

	// UPDATE without an explicit updated_at must auto-set it.
	sql, _ := db.Update(tbl).Set(name.Val("Alice")).ToSQL()
	if !strings.Contains(sql, `"updated_at" = now()`) {
		t.Errorf("UpdateHook should bump updated_at: %s", sql)
	}

	// User-supplied value wins.
	sql, _ = db.Update(tbl).
		Set(name.Val("Alice"), m.Cols.UpdatedAt.Val(timeMustParse("2030-01-01"))).
		ToSQL()
	if strings.Contains(sql, "now()") {
		t.Errorf("explicit updated_at must override hook: %s", sql)
	}
}

// TestSoftDeleteMixin verifies that SoftDelete rewrites DELETE into
// UPDATE, adds the default filter, and Unscoped() opts out of both.
func TestSoftDeleteMixin(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	m := &pg.SoftDeleteMixin{}
	pg.ApplyMixins(tbl, m)

	if m.Cols.DeletedAt == nil {
		t.Fatal("SoftDeleteMixin must populate Cols on Apply")
	}

	db := pg.New(nil)

	// SELECT picks up the default filter automatically.
	sql, _ := db.Select(id).From(tbl).ToSQL()
	if !strings.Contains(sql, `"widgets"."deleted_at" IS NULL`) {
		t.Errorf("default filter missing: %s", sql)
	}

	// DELETE is rewritten into UPDATE deleted_at = now().
	sql, _ = db.Delete(tbl).Where(id.Eq(1)).ToSQL()
	if !strings.HasPrefix(sql, "UPDATE ") {
		t.Errorf("DELETE must be rewritten to UPDATE: %s", sql)
	}
	if !strings.Contains(sql, `"deleted_at" = now()`) {
		t.Errorf("rewrite must set deleted_at: %s", sql)
	}
	if !strings.Contains(sql, `"widgets"."deleted_at" IS NULL`) {
		t.Errorf("rewritten UPDATE must keep the deleted_at IS NULL guard: %s", sql)
	}

	// Unscoped DELETE bypasses both the rewrite and the filter.
	sql, _ = db.Delete(tbl).Unscoped().Where(id.Eq(1)).ToSQL()
	if !strings.HasPrefix(sql, "DELETE ") {
		t.Errorf("Unscoped DELETE must produce a real DELETE: %s", sql)
	}
	if strings.Contains(sql, "deleted_at") {
		t.Errorf("Unscoped DELETE must drop the default filter: %s", sql)
	}
}

// TestUUIDPrimaryKeyMixin verifies the trivial rich variant.
func TestUUIDPrimaryKeyMixin(t *testing.T) {
	tbl := pg.NewTable("widgets")
	m := &pg.UUIDPrimaryKeyMixin{}
	pg.ApplyMixins(tbl, m)
	if m.Cols.ID == nil {
		t.Fatal("UUIDPrimaryKeyMixin must populate Cols on Apply")
	}
	if !m.Cols.ID.Column.IsPrimaryKey() {
		t.Error("id must be PRIMARY KEY")
	}
}

// TestAuditMixin verifies that AuditMixin registers FK columns
// referencing the target.
func TestAuditMixin(t *testing.T) {
	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())

	docs := pg.NewTable("docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	m := &pg.AuditMixin[int64]{Target: uid}
	pg.ApplyMixins(docs, m)

	if m.Cols.CreatedBy == nil || m.Cols.UpdatedBy == nil {
		t.Fatal("AuditMixin must populate Cols on Apply")
	}
	if m.Cols.CreatedBy.ForeignKey() == nil {
		t.Error("created_by must reference target")
	}
}

// TestMixinsCompose verifies that several rich mixins applied to the
// same table interact correctly: timestamps + soft-delete + audit.
func TestMixinsCompose(t *testing.T) {
	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())

	docs := pg.NewTable("docs")
	pkm := &pg.UUIDPrimaryKeyMixin{}
	tsm := &pg.TimestampsMixin{}
	sdm := &pg.SoftDeleteMixin{}
	am := &pg.AuditMixin[int64]{Target: uid}
	pg.ApplyMixins(docs, pkm, tsm, sdm, am)
	title := pg.Add(docs, pg.Text("title").NotNull())

	db := pg.New(nil)

	// SELECT: default filter applied (from soft-delete).
	sql, _ := db.Select(pkm.Cols.ID, title).From(docs).ToSQL()
	if !strings.Contains(sql, `"docs"."deleted_at" IS NULL`) {
		t.Errorf("compose: select filter missing: %s", sql)
	}

	// UPDATE: hook bumps updated_at AND default filter applied.
	sql, _ = db.Update(docs).Set(title.Val("hi")).ToSQL()
	if !strings.Contains(sql, `"updated_at" = now()`) {
		t.Errorf("compose: updated_at hook missing: %s", sql)
	}
	if !strings.Contains(sql, "deleted_at") {
		t.Errorf("compose: default filter missing in UPDATE: %s", sql)
	}

	// DELETE: rewritten + default filter still in place.
	sql, _ = db.Delete(docs).Where(pkm.Cols.ID.Eq("uid")).ToSQL()
	if !strings.HasPrefix(sql, "UPDATE ") {
		t.Errorf("compose: DELETE must be rewritten: %s", sql)
	}
	if !strings.Contains(sql, `"deleted_at" = now()`) {
		t.Errorf("compose: rewritten UPDATE must set deleted_at: %s", sql)
	}
	// And the timestamps hook should also bump updated_at on the rewrite.
	if !strings.Contains(sql, `"updated_at" = now()`) {
		t.Errorf("compose: rewrite should also bump updated_at: %s", sql)
	}
}
