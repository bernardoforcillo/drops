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
	bound map[string]bool
	added []ColumnValue
}

// Has reports whether col is already bound on the INSERT. An alias
// copy of a column counts as the column itself, and so does any other
// handle that renders as the same name — see [boundKey].
func (c *InsertHookCtx) Has(col *Column) bool { return c.bound[boundKey(col)] }

// SetExpr binds expr to col across every row, unless col is already
// bound — typical for DB-evaluated defaults such as drops.Raw("now()").
func (c *InsertHookCtx) SetExpr(col *Column, expr drops.Expression) {
	c.Set(&exprBinding{col: col, expr: expr})
}

// Set binds a typed ColumnValue (e.g. the result of (*Col[T]).Val(v)).
func (c *InsertHookCtx) Set(v ColumnValue) {
	if c.bound[boundKey(v.column())] {
		return
	}
	c.bound[boundKey(v.column())] = true
	c.added = append(c.added, v)
}

// boundKey returns the key a hook context records a column under: the
// form its rendered name shares with every other handle the server
// resolves to the same column — see [identKey].
//
// Keying the set by what the statement WRITES rather than by the
// handle that wrote it is the same rule [namesAxis] states for the
// axis, applied to every column a hook may touch. The two handles a
// hook and a caller hold for one column need not be the same pointer:
// an alias copy is one case, a codegen'd OtherTable.TenantID naming a
// column of this table's name is the other, and both render the one
// name the server reads. A hook that cannot see the caller's binding
// adds a second binding for it, and this dialect's INSERT derives its
// column list from those bindings — so the statement names one column
// twice and ClickHouse answers with a duplicate-column error rather
// than writing the row the caller and the hook each described.
//
// Nil-safe, because Has takes a handle from the caller and a nil one
// is a declaration mistake this should report as "not bound" rather
// than panic inside a hook.
func boundKey(col *Column) string {
	if col == nil {
		return ""
	}
	return identKey(col.Name())
}
