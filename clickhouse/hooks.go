package clickhouse

import "github.com/bernardoforcillo/drops"

// Lifecycle hooks for ClickHouse mirror the pg package — but only
// InsertHook is meaningful here, because ClickHouse mutations
// (UPDATE / DELETE) are async ALTERs and not exposed via builders.
// SelectBuilder honours the table's DefaultFilter list unless the
// caller opts out with Unscoped().
//
// Hooks are opt-in: a table with no hooks renders SQL unchanged.

// InsertHook is invoked once per INSERT statement, before rendering.
type InsertHook interface {
	BeforeInsert(ctx *InsertHookCtx)
}

// InsertHookFunc adapts a plain function to the InsertHook interface.
type InsertHookFunc func(*InsertHookCtx)

// BeforeInsert implements InsertHook.
func (f InsertHookFunc) BeforeInsert(ctx *InsertHookCtx) { f(ctx) }

// InsertHookCtx exposes which columns the caller already bound and
// lets the hook append additional bindings that apply to every row.
type InsertHookCtx struct {
	bound    map[*Column]bool
	addCols  []*Column
	addExprs []drops.Expression
}

// Has reports whether col is already bound on the INSERT.
func (c *InsertHookCtx) Has(col *Column) bool { return c.bound[col] }

// SetExpr binds expr to col across every row, unless col is already
// bound — typical for DB-evaluated defaults such as drops.Raw("now()").
func (c *InsertHookCtx) SetExpr(col *Column, expr drops.Expression) {
	if c.bound[col] {
		return
	}
	c.bound[col] = true
	c.addCols = append(c.addCols, col)
	c.addExprs = append(c.addExprs, expr)
}

// Set binds a typed ColumnValue (e.g. the result of (*Col[T]).Val(v)).
func (c *InsertHookCtx) Set(v ColumnValue) {
	if c.bound[v.column()] {
		return
	}
	c.bound[v.column()] = true
	c.addCols = append(c.addCols, v.column())
	c.addExprs = append(c.addExprs, bindingExpr(v))
}
