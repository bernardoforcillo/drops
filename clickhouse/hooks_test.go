package clickhouse_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

func TestClickhouseInsertHookFillsColumn(t *testing.T) {
	tbl := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	created := clickhouse.Add(tbl, clickhouse.DateTime("createdAt", "").Default("now()"))
	tbl.OrderBy(id)

	tbl.OnInsert(clickhouse.InsertHookFunc(func(ctx *clickhouse.InsertHookCtx) {
		ctx.SetExpr(created.Column, drops.Raw("now()"))
	}))

	db := clickhouse.New(nil)
	sql, _ := db.Insert(tbl).Row(id.Val("u1")).ToSQL()
	if !strings.Contains(sql, `"createdAt"`) || !strings.Contains(sql, "now()") {
		t.Errorf("hook should append createdAt = now(): %s", sql)
	}
}

func TestClickhouseInsertHookYieldsToUser(t *testing.T) {
	tbl := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	created := clickhouse.Add(tbl, clickhouse.DateTime("createdAt", ""))
	tbl.OrderBy(id)

	tbl.OnInsert(clickhouse.InsertHookFunc(func(ctx *clickhouse.InsertHookCtx) {
		ctx.SetExpr(created.Column, drops.Raw("now()"))
	}))

	db := clickhouse.New(nil)
	sql, _ := db.Insert(tbl).
		Row(id.Val("u1"), created.Expr(drops.Raw("'2030-01-01 00:00:00'"))).
		ToSQL()
	if strings.Contains(sql, "now()") {
		t.Errorf("user binding must win: %s", sql)
	}
	if !strings.Contains(sql, "'2030-01-01 00:00:00'") {
		t.Errorf("user expression preserved: %s", sql)
	}
}

func TestClickhouseDefaultFilterApplied(t *testing.T) {
	tbl := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	deleted := clickhouse.Add(tbl, clickhouse.DateTime("deletedAt", "").Nullable())
	tbl.OrderBy(id)
	tbl.DefaultFilter(deleted.IsNull())

	db := clickhouse.New(nil)
	sql, _ := db.Select(id).From(tbl).ToSQL()
	if !strings.Contains(sql, "deletedAt") {
		t.Errorf("default filter missing in SELECT: %s", sql)
	}

	sql, _ = db.Select(id).From(tbl).Unscoped().ToSQL()
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("Unscoped SELECT must skip default filter: %s", sql)
	}
}

func TestClickhouseTimestampsMixin(t *testing.T) {
	tbl := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	m := &clickhouse.TimestampsMixin{}
	clickhouse.ApplyMixins(tbl, m)
	tbl.OrderBy(id)

	if m.Cols.CreatedAt == nil || m.Cols.UpdatedAt == nil {
		t.Fatal("TimestampsMixin must populate Cols")
	}
	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(tbl))
	if !strings.Contains(got, `"createdAt" DateTime DEFAULT now()`) {
		t.Errorf("missing createdAt column: %s", got)
	}
}

func TestClickhouseSoftDeleteMixin(t *testing.T) {
	tbl := clickhouse.NewTable("widgets").Engine(clickhouse.MergeTree())
	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	m := &clickhouse.SoftDeleteMixin{}
	clickhouse.ApplyMixins(tbl, m)
	tbl.OrderBy(id)

	db := clickhouse.New(nil)
	sql, _ := db.Select(id).From(tbl).ToSQL()
	if !strings.Contains(sql, `"widgets"."deletedAt" IS NULL`) {
		t.Errorf("default scope missing on SELECT: %s", sql)
	}
	sql, _ = db.Select(id).From(tbl).Unscoped().ToSQL()
	if strings.Contains(sql, "deletedAt") {
		t.Errorf("Unscoped must drop the filter: %s", sql)
	}
}
