package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder composes an UPDATE statement.
type UpdateBuilder struct {
	db        *DB
	table     *Table
	sets      []ColumnValue
	from      []*Table
	wheres    []drops.Expression
	returning []drops.Expression
	scope     filterScope

	// defaults are the default filters of the tables this statement
	// names, resolved for one execution — see SelectBuilder.defaults.
	defaults resolvedDefaults

	// resolved marks a builder resolveCtx has already produced, so a
	// statement is not scoped twice. See SelectBuilder.resolved.
	resolved bool

	// hooked marks a builder whose SET list already carries what the
	// table's UpdateHooks added. resolveCtx runs them, because what a
	// hook assigns is part of the statement the axis check has to see
	// and part of what the resolver has to walk; running them again at
	// render time would assign each of them twice.
	hooked bool
}

// Set adds one or more assignments. Use (*Col[T]).Val(v) to bind a typed
// value or (*Col[T]).Expr(e) to bind an expression.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, values...)
	return u
}

// From adds tables to a PostgreSQL UPDATE ... FROM clause for joins.
func (u *UpdateBuilder) From(tables ...*Table) *UpdateBuilder {
	u.from = append(u.from, tables...)
	return u
}

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a filter that is only sometimes present can be passed straight in
// — but an UPDATE all of whose predicates were nil is an UPDATE with no
// WHERE, and rewrites every row the table's filters still admit.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, dropNilPreds(preds)...)
	return u
}

// Returning sets a RETURNING clause.
func (u *UpdateBuilder) Returning(cols ...drops.Expression) *UpdateBuilder {
	u.returning = append(u.returning, cols...)
	return u
}

// Unscoped opts out of every global filter registered on the table —
// named and anonymous alike. The blunt instrument: an administrative
// job that means "every row, no scoping" wants it, and nothing
// narrower does. To step around one guard while keeping the rest, name
// it with IgnoreFilters.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder {
	u.scope.unscoped = true
	return u
}

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one in place — see [SelectBuilder.IgnoreFilters].
// Writing to a soft-deleted row without abandoning the table's other
// scoping is the usual reason:
//
//	db.Update(Posts).IgnoreFilters(pg.FilterSoftDelete).
//	    Set(Deleted.SetNull()).Where(id.Eq(7))
func (u *UpdateBuilder) IgnoreFilters(names ...string) *UpdateBuilder {
	u.scope.ignore(names...)
	return u
}

// WriteSQL renders the UPDATE.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	sets := u.sets
	if !u.hooked && u.table.hasUpdateHooks() {
		sets = u.applyUpdateHooks()
	}
	wheres := u.scope.applyAll(u.namedTables(), u.wheres, u.defaults)
	b.WriteString("UPDATE ")
	u.table.writeFrom(b)
	b.WriteString(" SET ")
	for j, s := range sets {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(s.column().Name())
		b.WriteString(" = ")
		s.writeValue(b)
	}
	if len(u.from) > 0 {
		b.WriteString(" FROM ")
		for j, t := range u.from {
			if j > 0 {
				b.WriteString(", ")
			}
			t.writeFrom(b)
		}
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(u.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", u.returning)
	}
}

// namedTables is the target and every FROM table, in the order the
// statement names them — the tables whose automatic predicates this
// UPDATE carries. A FROM table's rows choose which of the target's rows
// are rewritten, so its guards are as load-bearing as the target's.
func (u *UpdateBuilder) namedTables() []*Table {
	tables := make([]*Table, 0, len(u.from)+1)
	if u.table != nil {
		tables = append(tables, u.table)
	}
	return append(tables, u.from...)
}

// ToSQLCtx renders the statement ctx would send: the context filters of
// the target table and of every FROM table, resolved and AND-ed in, and
// every statement written inside the SET list, the WHERE clause or the
// RETURNING projection resolved with them.
//
// An UPDATE is the statement where losing the axis is worst. A SELECT
// that loses it reads rows it should not have; an UPDATE that loses it
// REWRITES them, and there is nothing to walk back. So a filter that
// refuses returns the refusal and no statement at all.
func (u *UpdateBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution — the
// receiver when there was nothing to resolve. See
// [SelectBuilder.resolveCtx] for why the identity matters.
//
// The context predicates are appended to the WHERE list of the copy
// rather than kept in a field of their own: they are already resolved
// when they arrive, and resolved marks the builder so the walk does not
// come back to them.
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

	// What a hook assigns is part of this statement: it reaches every
	// UPDATE against the table, so an axis assignment made there is the
	// same statement as one made at the call site, and a subquery
	// assigned there is as much a statement as one written here.
	sets := u.sets
	if u.table.hasUpdateHooks() {
		sets, cp.sets, cp.hooked, changed = u.applyUpdateHooks(), u.applyUpdateHooks(), true, true
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
	wheres := u.wheres
	if r, err := resolveExprs(ctx, u.wheres); err != nil {
		return nil, err
	} else if r != nil {
		wheres, changed = r, true
	}
	if r, err := resolveExprs(ctx, u.returning); err != nil {
		return nil, err
	} else if r != nil {
		cp.returning, changed = r, true
	}

	if !u.scope.dropsContextFilters() {
		tables := u.namedTables()
		var preds []drops.Expression
		for _, t := range tables {
			// A FROM table is joined into the statement and its rows
			// choose which target rows the UPDATE rewrites, so its
			// filters are as load-bearing as the target's.
			p, err := t.resolveContextFilters(ctx)
			if err != nil {
				return nil, err
			}
			preds = append(preds, p...)
		}
		if len(preds) > 0 {
			all := make([]drops.Expression, 0, len(wheres)+len(preds))
			all = append(all, wheres...)
			wheres, changed = append(all, preds...), true
		}
		defaults, err := resolveTableDefaults(ctx, tables...)
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
	cp.wheres = wheres
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable], so an UPDATE written as
// a CTE body carries the same predicates a bare one would — the shape
// that made WITH moved AS (UPDATE scoped RETURNING ...) a cross-tenant
// write.
func (u *UpdateBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != u, nil
}

// applyUpdateHooks runs every UpdateHook registered on the table and
// returns the (possibly extended) SET list.
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

// ToSQL renders the statement.
func (u *UpdateBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	u.WriteSQL(b)
	return b.SQL()
}

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(u.sets) == 0 && !u.table.hasUpdateHooks() {
		return nil, ErrNoUpdateAssignments
	}
	// Through ToSQLCtx: the axis check and the context filters are the
	// statement, and a check only ToSQLCtx made would be a check every
	// caller in the readme walks past.
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return u.db.Exec(ctx, sql, args...)
}

// All executes the UPDATE and scans the RETURNING rows into dest.
func (u *UpdateBuilder) All(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the UPDATE and scans the first RETURNING row into dest.
func (u *UpdateBuilder) One(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
