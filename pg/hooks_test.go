package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// ----------------------------------------------------------------------
// InsertHook
// ----------------------------------------------------------------------

func TestInsertHookFillsMissingColumn(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	stamp := pg.Add(tbl, pg.Timestamp("createdAt", true).NotNull().Default("now()"))
	_ = id

	tbl.OnInsert(pg.InsertHookFunc(func(ctx *pg.InsertHookCtx) {
		ctx.SetExpr(stamp.Column, drops.Raw("now()"))
	}))

	db := pg.New(nil)
	sql, args := db.Insert(tbl).Row(name.Val("Alice")).ToSQL()

	if !strings.Contains(sql, `"createdAt"`) {
		t.Errorf("hook should append createdAt column: %s", sql)
	}
	if !strings.Contains(sql, "now()") {
		t.Errorf("hook should append now() expression: %s", sql)
	}
	if len(args) != 1 || args[0] != "Alice" {
		t.Errorf("user values must be preserved: args=%v", args)
	}
}

func TestInsertHookYieldsToUserValue(t *testing.T) {
	tbl := pg.NewTable("widgets")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	createdAt := pg.Add(tbl, pg.Timestamp("createdAt", true).NotNull())

	hookCalled := false
	tbl.OnInsert(pg.InsertHookFunc(func(ctx *pg.InsertHookCtx) {
		hookCalled = true
		if !ctx.Has(createdAt.Column) {
			t.Error("hook should observe user binding via Has")
		}
		ctx.SetExpr(createdAt.Column, drops.Raw("now()"))
	}))

	db := pg.New(nil)
	sql, args := db.Insert(tbl).
		Row(name.Val("Alice"), createdAt.Expr(drops.Raw("'2030-01-01'"))).
		ToSQL()

	if !hookCalled {
		t.Error("hook must be invoked")
	}
	if strings.Contains(sql, "now()") {
		t.Errorf("user binding must win over hook: %s", sql)
	}
	if !strings.Contains(sql, "'2030-01-01'") {
		t.Errorf("user expression must be preserved: %s", sql)
	}
	if len(args) != 1 || args[0] != "Alice" {
		t.Errorf("args mismatch: %v", args)
	}
}

func TestInsertHookAppliesUniformlyAcrossRows(t *testing.T) {
	tbl := pg.NewTable("widgets")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	createdAt := pg.Add(tbl, pg.Timestamp("createdAt", true).NotNull())

	tbl.OnInsert(pg.InsertHookFunc(func(ctx *pg.InsertHookCtx) {
		ctx.SetExpr(createdAt.Column, drops.Raw("now()"))
	}))

	db := pg.New(nil)
	sql, _ := db.Insert(tbl).
		Row(name.Val("Alice")).
		Row(name.Val("Bob")).
		ToSQL()

	// Both rows must receive the hook-added value.
	if strings.Count(sql, "now()") != 2 {
		t.Errorf("expected now() in every row, got %s", sql)
	}
}

// ----------------------------------------------------------------------
// UpdateHook
// ----------------------------------------------------------------------

func TestUpdateHookFillsMissingSet(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	updatedAt := pg.Add(tbl, pg.Timestamp("updatedAt", true).NotNull())

	tbl.OnUpdate(pg.UpdateHookFunc(func(ctx *pg.UpdateHookCtx) {
		ctx.SetExpr(updatedAt.Column, drops.Raw("now()"))
	}))

	db := pg.New(nil)
	sql, args := db.Update(tbl).Set(name.Val("Bob")).Where(id.Eq(1)).ToSQL()

	if !strings.Contains(sql, `"updatedAt" = now()`) {
		t.Errorf("hook should bump updatedAt: %s", sql)
	}
	if len(args) != 2 || args[0] != "Bob" {
		t.Errorf("args mismatch: %v", args)
	}
}

func TestUpdateHookYieldsToUserValue(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	updatedAt := pg.Add(tbl, pg.Timestamp("updatedAt", true).NotNull())

	tbl.OnUpdate(pg.UpdateHookFunc(func(ctx *pg.UpdateHookCtx) {
		ctx.SetExpr(updatedAt.Column, drops.Raw("now()"))
	}))

	db := pg.New(nil)
	sql, _ := db.Update(tbl).
		Set(name.Val("Bob"), updatedAt.Expr(drops.Raw("'2030-01-01'"))).
		Where(id.Eq(1)).
		ToSQL()

	if strings.Contains(sql, "now()") {
		t.Errorf("user binding must win: %s", sql)
	}
	if !strings.Contains(sql, "'2030-01-01'") {
		t.Errorf("user expression preserved: %s", sql)
	}
}

// ----------------------------------------------------------------------
// DeleteHook
// ----------------------------------------------------------------------

func TestDeleteHookRewritesToUpdate(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	deletedAt := pg.Add(tbl, pg.Timestamp("deletedAt", true))

	tbl.OnDelete(pg.DeleteHookFunc(func(d *pg.DeleteBuilder) drops.Expression {
		upd := d.DB().Update(d.Table()).
			Set(deletedAt.Expr(drops.Raw("now()")))
		for _, w := range d.Wheres() {
			upd = upd.Where(w)
		}
		return upd
	}))

	db := pg.New(nil)
	sql, _ := db.Delete(tbl).Where(id.Eq(1)).ToSQL()

	if !strings.HasPrefix(sql, "UPDATE ") {
		t.Errorf("expected rewritten UPDATE, got: %s", sql)
	}
	if !strings.Contains(sql, `"deletedAt" = now()`) {
		t.Errorf("missing SET: %s", sql)
	}
}

func TestDeleteHookUnscopedSkipsRewrite(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	deletedAt := pg.Add(tbl, pg.Timestamp("deletedAt", true))

	tbl.OnDelete(pg.DeleteHookFunc(func(d *pg.DeleteBuilder) drops.Expression {
		return d.DB().Update(d.Table()).Set(deletedAt.Expr(drops.Raw("now()")))
	}))

	db := pg.New(nil)
	sql, _ := db.Delete(tbl).Unscoped().Where(id.Eq(1)).ToSQL()

	if !strings.HasPrefix(sql, "DELETE ") {
		t.Errorf("Unscoped must produce a real DELETE: %s", sql)
	}
}

// ----------------------------------------------------------------------
// Default filter on Select / Update / Delete
// ----------------------------------------------------------------------

func TestDefaultFilterAppliedToSelect(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	deletedAt := pg.Add(tbl, pg.Timestamp("deletedAt", true))
	tbl.DefaultFilter(deletedAt.IsNull())

	db := pg.New(nil)
	sql, _ := db.Select(id).From(tbl).ToSQL()
	if !strings.Contains(sql, `"widgets"."deletedAt" IS NULL`) {
		t.Errorf("default filter missing in SELECT: %s", sql)
	}

	sql, _ = db.Select(id).From(tbl).Unscoped().ToSQL()
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("Unscoped SELECT must skip default filter: %s", sql)
	}
}

func TestDefaultFilterAppliedToUpdate(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	deletedAt := pg.Add(tbl, pg.Timestamp("deletedAt", true))
	tbl.DefaultFilter(deletedAt.IsNull())

	db := pg.New(nil)
	sql, _ := db.Update(tbl).Set(name.Val("Carol")).Where(id.Eq(1)).ToSQL()
	if !strings.Contains(sql, "deletedAt") {
		t.Errorf("default filter missing in UPDATE: %s", sql)
	}

	sql, _ = db.Update(tbl).Set(name.Val("Carol")).Unscoped().Where(id.Eq(1)).ToSQL()
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("Unscoped UPDATE must skip default filter: %s", sql)
	}
}

func TestDefaultFilterAppliedToDelete(t *testing.T) {
	tbl := pg.NewTable("widgets")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	deletedAt := pg.Add(tbl, pg.Timestamp("deletedAt", true))
	tbl.DefaultFilter(deletedAt.IsNull())

	db := pg.New(nil)
	sql, _ := db.Delete(tbl).Where(id.Eq(1)).ToSQL()
	if !strings.Contains(sql, "deletedAt") {
		t.Errorf("default filter missing in DELETE: %s", sql)
	}

	sql, _ = db.Delete(tbl).Unscoped().Where(id.Eq(1)).ToSQL()
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("Unscoped DELETE must skip default filter: %s", sql)
	}
}
