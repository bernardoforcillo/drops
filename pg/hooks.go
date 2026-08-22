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
//
// bound is keyed by [boundKey] — the name the column RENDERS as — and
// not by the handle. A hook closes over the handle it was declared
// with, every built-in mixin closes over the package-level one, and
// the caller may have bound the same column through a table alias or
// through another table's handle for the same name. Asked by handle,
// Has answered false for a column that is already in the statement and
// the hook added it a second time: PostgreSQL then rejects the INSERT
// with 42701, and the documented rule that user-supplied values win is
// inverted.
type InsertHookCtx struct {
	bound    map[string]bool
	addCols  []*Column
	addExprs []drops.Expression
}

// Has reports whether c is already bound on the INSERT — either by
// the user or by an earlier hook.
func (c *InsertHookCtx) Has(col *Column) bool { return c.bound[boundKey(col)] }

// SetExpr binds expr to col across every row, unless col is already
// bound. Use this for DB-evaluated defaults (e.g. drops.Raw("now()")).
func (c *InsertHookCtx) SetExpr(col *Column, expr drops.Expression) {
	if c.bound[boundKey(col)] {
		return
	}
	c.bound[boundKey(col)] = true
	c.addCols = append(c.addCols, col)
	c.addExprs = append(c.addExprs, expr)
}

// Set binds a typed ColumnValue, e.g. the result of (*Col[T]).Val(v).
// Equivalent to SetExpr with the binding's writer.
func (c *InsertHookCtx) Set(v ColumnValue) {
	if c.bound[boundKey(v.column())] {
		return
	}
	c.bound[boundKey(v.column())] = true
	c.addCols = append(c.addCols, v.column())
	c.addExprs = append(c.addExprs, bindingExpr(v))
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
// adds a second assignment for it — a duplicate column on the INSERT,
// a duplicate assignment on the UPDATE — which PostgreSQL refuses
// (42701, 42601) and a dialect with laxer rules resolves by keeping
// whichever of the two it prefers.
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
//
// bound is keyed by [boundKey], for the reason spelled out on
// InsertHookCtx: the hook and the caller may hold two handles on one
// column, and a duplicate assignment is rejected with 42601. On this
// statement the duplicate is worse than a rejection, because the axis
// can be one of the two. A hook holding another table's handle for
// "tenantId" saw a SET list that already assigned the axis as
// assigning nothing, and appended its own —
//
//	UPDATE "zp" SET "tenantId" = $1, "tenantId" = $2   args [77 999]
//
// which PostgreSQL refuses and MySQL, where a duplicate SET is legal
// and the last one wins, executes as the transfer.
type UpdateHookCtx struct {
	bound map[string]bool
	add   []ColumnValue
}

// Has reports whether col is already bound on the UPDATE.
func (c *UpdateHookCtx) Has(col *Column) bool { return c.bound[boundKey(col)] }

// Set appends v to the UPDATE's SET list, unless its column is
// already bound.
func (c *UpdateHookCtx) Set(v ColumnValue) {
	if c.bound[boundKey(v.column())] {
		return
	}
	c.bound[boundKey(v.column())] = true
	c.add = append(c.add, v)
}

// SetExpr is the raw-expression variant of Set — useful for hooks
// that want to assign e.g. drops.Raw("now()") to a column.
//
// What it assigns is checked like the caller's own: a hook is
// registered on the table and reaches every UPDATE against it, so an
// axis assignment made here is the same statement as one made at the
// call site — see checkAxisAssignment.
func (c *UpdateHookCtx) SetExpr(col *Column, expr drops.Expression) {
	if c.bound[boundKey(col)] {
		return
	}
	c.bound[boundKey(col)] = true
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
