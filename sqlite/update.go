package sqlite

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder builds an UPDATE statement. Create one via DB.Update.
type UpdateBuilder struct {
	db     *DB
	table  *Table
	sets   []ColumnValue
	wheres []drops.Expression
	scope  filterScope

	// ctxPreds are the table's context filters, built for one
	// execution by resolveCtx. They are held rather than resolved
	// while rendering because WriteSQL has no ctx to build them from.
	ctxPreds []drops.Expression

	// defaults is what this execution resolved for the table's
	// render-time filters, and nil when none of them held a statement.
	defaults resolvedDefaults

	// hooked says the SET list already carries what the UPDATE hooks
	// added, so WriteSQL does not run them a second time.
	hooked bool

	// resolved marks the copy resolveCtx produced, so a statement
	// nested in another is not resolved twice.
	resolved bool
}

// Set adds a column assignment.
func (u *UpdateBuilder) Set(vals ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, vals...)
	return u
}

// SetExpr assigns a raw SQL expression to col (e.g. CURRENT_TIMESTAMP,
// NULL, or "count + 1") rather than a bound value.
func (u *UpdateBuilder) SetExpr(col *Column, expr drops.Expression) *UpdateBuilder {
	u.sets = append(u.sets, exprValue{col: col, expr: expr})
	return u
}

// Where AND-s the given predicates onto the statement. Nil predicates
// are ignored, so a filter that is only sometimes present can be passed
// straight in — but an UPDATE all of whose predicates were nil is an
// UPDATE with no WHERE, and rewrites every row.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, dropNilPreds(preds)...)
	return u
}

// Unscoped opts out of every global filter on the table for this
// UPDATE — named and anonymous alike; the blunt instrument, for an
// admin job that means "no scoping at all". To step around one guard
// and keep the rest, name it with [UpdateBuilder.IgnoreFilters].
func (u *UpdateBuilder) Unscoped() *UpdateBuilder {
	u.scope.unscoped = true
	return u
}

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one standing — see [SelectBuilder.IgnoreFilters].
func (u *UpdateBuilder) IgnoreFilters(names ...string) *UpdateBuilder {
	u.scope.ignore(names...)
	return u
}

// WriteSQL implements drops.Expression.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	sets := u.sets
	if !u.hooked && u.table.hasUpdateHooks() {
		sets = u.applyUpdateHooks()
	}
	// Defaults first, then what the caller asked for, then the
	// request-time predicates: the order the WHERE clause has always
	// read in, scope first.
	wheres := u.scope.apply(u.table, u.wheres, u.defaults)
	wheres = append(append([]drops.Expression(nil), wheres...), u.ctxPreds...)
	b.WriteString("UPDATE ")
	u.table.writeName(b)
	b.WriteString(" SET ")
	for i, cv := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(cv.column().Name())
		b.WriteString(" = ")
		cv.writeValue(b)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
}

// applyUpdateHooks runs every UpdateHook on the table and returns the
// (possibly extended) SET list.
func (u *UpdateBuilder) applyUpdateHooks() []ColumnValue {
	ctx := &UpdateHookCtx{bound: make(map[string]bool, len(u.sets))}
	for _, s := range u.sets {
		// Keyed by the name the statement writes, not by the handle
		// that wrote it: the two handles a hook and a caller hold for
		// one column need not be the same pointer. See boundKey.
		ctx.bound[boundKey(s.column())] = true
	}
	for _, h := range u.table.updateHookList() {
		h.BeforeUpdate(ctx)
	}
	if len(ctx.add) == 0 {
		return u.sets
	}
	out := append([]ColumnValue(nil), u.sets...)
	out = append(out, ctx.add...)
	return out
}

// ToSQL renders the statement with SQLite placeholders.
//
// It carries the table's DefaultFilters and none of its
// ContextFilters, because a render has no ctx to build one from — so
// on a tenant-scoped table it is not the statement that would be sent.
// Prefer [UpdateBuilder.ToSQLCtx].
func (u *UpdateBuilder) ToSQL() (sql string, args []any) { return ToSQL(u) }

// ToSQLCtx renders the statement ctx would send: the table's context
// filters built and AND-ed into the WHERE clause, the UPDATE hooks run,
// and every statement written inside the SET list or the predicates
// resolved on the same terms.
//
// A filter that cannot decide what the request may see refuses, and
// the refusal is returned instead of a statement: an UPDATE missing
// the predicate that makes it safe rewrites rows that belong to
// somebody else.
func (u *UpdateBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution: the
// receiver when there was nothing to resolve, and a shallow copy
// carrying the resolved lists when there was.
//
// Handing back the receiver is not a micro-optimisation.
// resolveStatement reports "this differs from the original" by
// comparing pointers, so a builder that always answered with a copy
// would tell every statement it is nested in that it had changed.
func (u *UpdateBuilder) resolveCtx(ctx context.Context) (*UpdateBuilder, error) {
	if u.resolved {
		return u, nil
	}
	// The named filters this statement bypasses, for the length of
	// this resolution: a nested statement installs its own at the top
	// of its own resolveCtx, so IgnoreFilters reaches no further than
	// the statement that said it.
	ctx = withIgnoredFilters(ctx, u.scope)

	cp := *u
	changed := false

	// The hooks run here rather than at render time, so what they bind
	// goes through the same walk as a value the caller bound: a hook
	// takes bindings of any shape, and one of them can be a statement.
	sets := u.sets
	if u.table.hasUpdateHooks() {
		sets, cp.hooked, changed = u.applyUpdateHooks(), true, true
	}
	// The SET list is the half of an UPDATE a tenant predicate does not
	// reach: the WHERE clause says which rows may be touched, and the
	// assignment says what they become — including, if nobody checks,
	// somebody else's tenant.
	if !u.scope.dropsContextFilters() {
		if err := checkAxisAssignment(ctx, u.table, sets); err != nil {
			return nil, err
		}
	}

	// The assigned value is an operand position like any other, and the
	// one that decides what gets written rather than which rows do.
	if r, err := resolveSets(ctx, sets); err != nil {
		return nil, err
	} else if r != nil {
		sets, changed = r, true
	}
	if changed {
		cp.sets = sets
	}

	if r, err := resolveExprs(ctx, u.wheres); err != nil {
		return nil, err
	} else if r != nil {
		cp.wheres, changed = r, true
	}

	if !u.scope.dropsContextFilters() {
		preds, err := u.table.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		if len(preds) > 0 {
			cp.ctxPreds, changed = preds, true
		}
		defaults, err := resolveTableDefaults(ctx, u.table)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
	}

	if !changed {
		return u, nil
	}
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable]: it is resolveCtx behind
// the interface resolveExpr dispatches on, so an UPDATE written as a
// CTE body carries the same predicates a bare one would.
func (u *UpdateBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != u, nil
}

// Exec runs the UPDATE, with the table's context filters resolved
// against ctx first.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return u.db.Exec(ctx, sql, args...)
}
