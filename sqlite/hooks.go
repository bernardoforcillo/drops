package sqlite

import "github.com/bernardoforcillo/drops"

// Lifecycle hooks let mixins (and application code) influence how
// INSERT / UPDATE / DELETE statements render. Hooks are registered on a
// Table via OnInsert / OnUpdate / OnDelete and run during the
// corresponding builder's WriteSQL. They are entirely opt-in — a Table
// with no hooks renders exactly as before.
//
// Conflict rule: user-supplied values always win. A hook's Set is a
// no-op if the caller already bound the same column on the statement.

// exprBinding adapts a raw expression into a ColumnValue so hooks can
// assign DB-evaluated defaults (e.g. drops.Raw("CURRENT_TIMESTAMP")).
type exprBinding struct {
	col  *Column
	expr drops.Expression
}

func (e *exprBinding) column() *Column             { return e.col }
func (e *exprBinding) writeValue(b *drops.Builder) { e.expr.WriteSQL(b) }

// boundExpr and withBoundExpr implement exprBound, which is how the
// resolver reaches the statements a hook's assignment may hold. A hook
// is registered on the table and reaches every write against it, and
// what it binds is an arbitrary expression — so it is an operand
// position a *SelectBuilder can be handed to, and one no call site
// shows.
//
// withBoundExpr copies rather than assigning through the pointer: a
// hook is free to hand back a binding it holds on to, and a resolved
// body written into that binding would pin one request's tenant into
// every later statement the hook contributes to.
func (e *exprBinding) boundExpr() drops.Expression { return e.expr }

func (e *exprBinding) withBoundExpr(x drops.Expression) ColumnValue {
	cp := *e
	cp.expr = x
	return &cp
}

// ----------------------------------------------------------------------
// INSERT hook
// ----------------------------------------------------------------------

// InsertHook is invoked once per INSERT statement, before rendering.
type InsertHook interface {
	BeforeInsert(ctx *InsertHookCtx)
}

// InsertHookFunc adapts a plain function to the InsertHook interface.
type InsertHookFunc func(*InsertHookCtx)

// BeforeInsert implements InsertHook.
func (f InsertHookFunc) BeforeInsert(ctx *InsertHookCtx) { f(ctx) }

// InsertHookCtx is the controlled handle a hook uses to inspect the
// statement and append hook-supplied values, applied uniformly to every
// row in the INSERT.
type InsertHookCtx struct {
	bound map[*Column]bool
	adds  []ColumnValue
}

// Has reports whether col is already bound on the INSERT.
func (c *InsertHookCtx) Has(col *Column) bool { return c.bound[col] }

// SetExpr binds expr to col across every row, unless col is already
// bound. Use for DB-evaluated defaults (drops.Raw("CURRENT_TIMESTAMP")).
func (c *InsertHookCtx) SetExpr(col *Column, expr drops.Expression) {
	if c.bound[col] {
		return
	}
	c.bound[col] = true
	c.adds = append(c.adds, &exprBinding{col: col, expr: expr})
}

// Set binds a typed ColumnValue (e.g. (*Col[T]).Val(v)).
func (c *InsertHookCtx) Set(v ColumnValue) {
	if c.bound[v.column()] {
		return
	}
	c.bound[v.column()] = true
	c.adds = append(c.adds, v)
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

// Set appends v to the UPDATE's SET list, unless its column is already
// bound.
func (c *UpdateHookCtx) Set(v ColumnValue) {
	if c.bound[v.column()] {
		return
	}
	c.bound[v.column()] = true
	c.add = append(c.add, v)
}

// SetExpr is the raw-expression variant of Set.
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

// DeleteHook is invoked by DeleteBuilder.WriteSQL. A non-nil returned
// expression replaces the rendered DELETE entirely — used by SoftDelete
// to translate DELETE into UPDATE. Returning nil lets the DELETE render
// normally. Hooks are tried in registration order; the first non-nil
// expression wins.
type DeleteHook interface {
	BeforeDelete(d *DeleteBuilder) drops.Expression
}

// DeleteHookFunc adapts a function to the DeleteHook interface.
type DeleteHookFunc func(*DeleteBuilder) drops.Expression

// BeforeDelete implements DeleteHook.
func (f DeleteHookFunc) BeforeDelete(d *DeleteBuilder) drops.Expression { return f(d) }
