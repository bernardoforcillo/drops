package mysql

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder composes an UPDATE statement.
type UpdateBuilder struct {
	db       *DB
	table    *Table
	sets     []ColumnValue
	wheres   []drops.Expression
	orderBys []drops.Expression
	limit    *int64
	scope    filterScope

	// ctxPreds are the table's context filters, built for one
	// execution by resolveCtx. They are held rather than resolved
	// while rendering because WriteSQL has no ctx to build them from.
	ctxPreds []drops.Expression

	// defaults is what this execution resolved for the table's
	// render-time filters, and nil when none of them held a statement.
	defaults resolvedDefaults

	// hooked says the SET list already carries what the UPDATE hooks
	// add, so a builder resolveCtx produced does not run them again at
	// render.
	hooked bool

	// resolved marks the copy resolveCtx produced, so a statement
	// nested in another is not resolved twice.
	resolved bool
}

// Set appends column assignments.
//
// Each assignment is restated against the handle this builder's table
// hands out for the column it names. The left-hand side of a SET is
// written bare and never needed it, but a [PatchOp] names its column
// on the right as well — "SET age = age + ?" — and qualified, so an op
// built from another handle on the same table would name a relation
// the UPDATE does not: MySQL answers 1054 whichever of the two
// handles is the odd one out. An assignment naming a column of some
// *other* table is left alone, being a deliberate cross-table
// reference rather than a second handle.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	for _, v := range values {
		u.sets = append(u.sets, rebindValue(u.table, v))
	}
	return u
}

// SetExpr assigns a raw SQL expression to a column. The expression is
// the caller's and is emitted as given — build it from the same handle
// the statement's table uses.
func (u *UpdateBuilder) SetExpr(col ColRef, e drops.Expression) *UpdateBuilder {
	return u.Set(exprValue{col: col.col(), expr: e})
}

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a filter that is only sometimes present can be passed straight in
// — but an UPDATE all of whose predicates were nil is an UPDATE with no
// WHERE, and rewrites every row the table's filters still admit.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, dropNilPreds(preds)...)
	return u
}

// OrderBy and Limit bound which rows an UPDATE touches — a MySQL
// extension with no PostgreSQL equivalent, and the safe way to update
// a large table in batches.
func (u *UpdateBuilder) OrderBy(exprs ...drops.Expression) *UpdateBuilder {
	u.orderBys = append(u.orderBys, exprs...)
	return u
}

func (u *UpdateBuilder) Limit(n int64) *UpdateBuilder { u.limit = &n; return u }

// Unscoped opts out of every global filter on the table — the blunt
// instrument; see [SelectBuilder.Unscoped].
func (u *UpdateBuilder) Unscoped() *UpdateBuilder { u.scope.unscoped = true; return u }

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one standing — see [SelectBuilder.IgnoreFilters].
func (u *UpdateBuilder) IgnoreFilters(names ...string) *UpdateBuilder {
	u.scope.ignore(names...)
	return u
}

// ErrNoAssignments is returned when an UPDATE has nothing to set.
var ErrNoAssignments = errors.New("drops/mysql: UPDATE has no assignments")

// WriteSQL renders the UPDATE.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	sets := u.sets
	if !u.hooked && u.table.hasUpdateHooks() {
		sets = u.applyUpdateHooks()
	}
	b.WriteString("UPDATE ")
	u.table.writeFrom(b)
	b.WriteString(" SET ")
	for i, s := range sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(s.column().name)
		b.WriteString(" = ")
		s.writeValue(b)
	}
	// Defaults first, then what the caller asked for, then the
	// request-time predicates: the order the WHERE clause has always
	// read in, scope first.
	wheres := u.scope.apply(u.table, u.wheres, u.defaults)
	wheres = append(append([]drops.Expression(nil), wheres...), u.ctxPreds...)
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(u.orderBys) > 0 {
		b.WriteString(" ORDER BY ")
		b.AppendList(", ", u.orderBys)
	}
	if u.limit != nil {
		b.WriteString(" LIMIT ")
		b.AddArg(*u.limit)
	}
}

// ToSQL renders the statement and its arguments.
//
// It carries the table's DefaultFilters and none of its
// ContextFilters, because a render has no ctx to build one from — so
// on a tenant-scoped table it is not the statement that would be sent.
// Prefer [UpdateBuilder.ToSQLCtx].
func (u *UpdateBuilder) ToSQL() (string, []any) { return render(u) }

// ToSQLCtx renders the statement ctx would send: the table's context
// filters built and AND-ed into the WHERE clause, and every statement
// written inside the SET list or the predicates resolved on the same
// terms.
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
// carrying the resolved lists when there was. See
// [SelectBuilder.resolveCtx] for why the identity matters.
// applyUpdateHooks runs every UpdateHook on the table and returns the
// (possibly extended) SET list.
//
// A hook that assigns a column the caller already assigned is ignored:
// the caller's value wins, because a hook is a default. That is what
// UpdateHookCtx.Has answers, and it is keyed by the name the statement
// writes rather than by the handle that wrote it — the two handles a
// hook and a caller hold for one column need not be the same pointer.
func (u *UpdateBuilder) applyUpdateHooks() []ColumnValue {
	hctx := &UpdateHookCtx{bound: make(map[string]bool, len(u.sets))}
	for _, s := range u.sets {
		hctx.bound[boundKey(s.column())] = true
	}
	for _, h := range u.table.updateHookList() {
		h.BeforeUpdate(hctx)
	}
	if len(hctx.add) == 0 {
		return u.sets
	}
	out := append([]ColumnValue(nil), u.sets...)
	return append(out, hctx.add...)
}

func (u *UpdateBuilder) resolveCtx(ctx context.Context) (*UpdateBuilder, error) {
	if u.resolved {
		return u, nil
	}
	ctx = withIgnoredFilters(ctx, u.scope)

	cp := *u
	changed := false

	// The hooks run before everything below, for two reasons. A hook
	// binds an arbitrary expression, so running it after the resolve
	// walk would leave a hook's subquery to render through WriteSQL,
	// which has no ctx. And a hook can assign the tenant column, so its
	// assignment has to reach the axis check like any other — a hook
	// that quietly reassigned the axis would be the one write nobody
	// checked.
	sets := u.sets
	if u.table.hasUpdateHooks() {
		sets = u.applyUpdateHooks()
		cp.sets, cp.hooked, changed = sets, true, true
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
		cp.sets, changed = r, true
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

// resolveStatement implements [ctxResolvable], so an UPDATE written as
// a CTE body carries the same predicates a bare one would.
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
	if len(u.sets) == 0 {
		return nil, ErrNoAssignments
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return u.db.Exec(ctx, sql, args...)
}
