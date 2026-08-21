package clickhouse

import "github.com/bernardoforcillo/drops"

// Lifecycle hooks for ClickHouse mirror the pg package — but only
// InsertHook is meaningful here, because ClickHouse mutations
// (UPDATE / DELETE) are async ALTERs and not exposed via builders.
// SelectBuilder honours the table's [Table.DefaultFilter] list and its
// [Table.ContextFilter] list unless the caller opts out with Unscoped().
//
// Hooks are opt-in: a table with no hooks renders SQL unchanged.

// InsertHook is invoked once per INSERT statement, before it is
// resolved for a ctx — see [InsertBuilder.resolveCtx] for why that
// order matters, and [InsertHookCtx] for what a hook may bind.
type InsertHook interface {
	BeforeInsert(ctx *InsertHookCtx)
}

// InsertHookFunc adapts a plain function to the InsertHook interface.
type InsertHookFunc func(*InsertHookCtx)

// BeforeInsert implements InsertHook.
func (f InsertHookFunc) BeforeInsert(ctx *InsertHookCtx) { f(ctx) }

// InsertHookCtx exposes which columns the caller already bound and
// lets the hook append additional bindings that apply to every row.
//
// What a hook binds is held as a [ColumnValue] rather than as an
// already-rendered expression, and the difference is the one the
// resolver turns on: a hook is an operand position no call site shows,
// so a hook that binds a scalar subquery — SetExpr(col, Subquery(sel))
// — writes a statement into every INSERT into the table. Held as a
// binding it goes through the same walk and the same tenant check as a
// value the caller bound; flattened to an expression at registration
// time it was invisible to both.
type InsertHookCtx struct {
	bound map[*Column]bool
	added []ColumnValue
}

// Has reports whether col is already bound on the INSERT. An alias
// copy of a column counts as the column itself.
func (c *InsertHookCtx) Has(col *Column) bool { return c.bound[col.key()] }

// SetExpr binds expr to col across every row, unless col is already
// bound — typical for DB-evaluated defaults such as drops.Raw("now()").
func (c *InsertHookCtx) SetExpr(col *Column, expr drops.Expression) {
	c.Set(&exprBinding{col: col, expr: expr})
}

// Set binds a typed ColumnValue (e.g. the result of (*Col[T]).Val(v)).
func (c *InsertHookCtx) Set(v ColumnValue) {
	if c.bound[v.column().key()] {
		return
	}
	c.bound[v.column().key()] = true
	c.added = append(c.added, v)
}
