package pg

import "github.com/bernardoforcillo/drops"

// Lifecycle hooks let templates (and application code) influence how
// INSERT / UPDATE / DELETE statements are rendered. Hooks are
// registered on a Table via OnInsert / OnUpdate / OnDelete and run
// during the corresponding builder's WriteSQL. They are entirely
// opt-in: a Table with no hooks behaves exactly as it always has.
//
// The conflict-resolution rule is consistent across hooks:
//
//   - User-supplied values always win. A hook's Set call is a no-op
//     if the user already bound the same column on the statement.
//
// This keeps backfills, imports, tests, and ad-hoc overrides
// predictable.

// ----------------------------------------------------------------------
// INSERT hook
// ----------------------------------------------------------------------

// InsertHook is invoked once per INSERT statement, before rendering.
// It receives an InsertHookCtx that exposes which columns the caller
// already bound and lets the hook append further bindings that apply
// to every row.
type InsertHook interface {
	BeforeInsert(ctx *InsertHookCtx)
}

// InsertHookFunc adapts a plain function to the InsertHook interface.
type InsertHookFunc func(*InsertHookCtx)

// BeforeInsert implements InsertHook.
func (f InsertHookFunc) BeforeInsert(ctx *InsertHookCtx) { f(ctx) }

// InsertHookCtx is the controlled handle a hook uses to inspect the
// statement and append hook-supplied values. Hook-added expressions
// apply uniformly to every row in the INSERT.
type InsertHookCtx struct {
	bound    map[*Column]bool
	addCols  []*Column
	addExprs []drops.Expression
}

// Has reports whether c is already bound on the INSERT — either by
// the user or by an earlier hook.
func (c *InsertHookCtx) Has(col *Column) bool { return c.bound[col] }

// SetExpr binds expr to col across every row, unless col is already
// bound. Use this for DB-evaluated defaults (e.g. drops.Raw("now()")).
func (c *InsertHookCtx) SetExpr(col *Column, expr drops.Expression) {
	if c.bound[col] {
		return
	}
	c.bound[col] = true
	c.addCols = append(c.addCols, col)
	c.addExprs = append(c.addExprs, expr)
}

// Set binds a typed ColumnValue, e.g. the result of (*Col[T]).Val(v).
// Equivalent to SetExpr with the binding's writer.
func (c *InsertHookCtx) Set(v ColumnValue) {
	if c.bound[v.column()] {
		return
	}
	c.bound[v.column()] = true
	c.addCols = append(c.addCols, v.column())
	c.addExprs = append(c.addExprs, bindingExpr(v))
}

// ----------------------------------------------------------------------
// UPDATE hook
// ----------------------------------------------------------------------

// UpdateHook is invoked once per UPDATE statement, before rendering.
type UpdateHook interface {
	BeforeUpdate(ctx *UpdateHookCtx)
}

// UpdateHookFunc adapts a plain function to the UpdateHook interface.
type UpdateHookFunc func(*UpdateHookCtx)

// BeforeUpdate implements UpdateHook.
func (f UpdateHookFunc) BeforeUpdate(ctx *UpdateHookCtx) { f(ctx) }

// UpdateHookCtx is the controlled handle a hook uses to add SET
// assignments without clobbering user-supplied values.
type UpdateHookCtx struct {
	bound map[*Column]bool
	add   []ColumnValue
}

// Has reports whether col is already bound on the UPDATE.
func (c *UpdateHookCtx) Has(col *Column) bool { return c.bound[col] }

// Set appends v to the UPDATE's SET list, unless its column is
// already bound.
func (c *UpdateHookCtx) Set(v ColumnValue) {
	if c.bound[v.column()] {
		return
	}
	c.bound[v.column()] = true
	c.add = append(c.add, v)
}

// SetExpr is the raw-expression variant of Set — useful for hooks
// that want to assign e.g. drops.Raw("now()") to a column.
func (c *UpdateHookCtx) SetExpr(col *Column, expr drops.Expression) {
	if c.bound[col] {
		return
	}
	c.bound[col] = true
	c.add = append(c.add, &exprBinding{col: col, expr: expr})
}

// ----------------------------------------------------------------------
// DELETE hook
// ----------------------------------------------------------------------

// DeleteHook is invoked by DeleteBuilder.WriteSQL. If it returns a
// non-nil expression, that expression replaces the rendered DELETE
// entirely — used by SoftDelete to translate DELETE into an UPDATE.
// Returning nil lets the DELETE render normally.
//
// Hooks are tried in registration order; the first non-nil expression
// wins.
type DeleteHook interface {
	BeforeDelete(d *DeleteBuilder) drops.Expression
}

// DeleteHookFunc adapts a function to the DeleteHook interface.
type DeleteHookFunc func(*DeleteBuilder) drops.Expression

// BeforeDelete implements DeleteHook.
func (f DeleteHookFunc) BeforeDelete(d *DeleteBuilder) drops.Expression { return f(d) }
