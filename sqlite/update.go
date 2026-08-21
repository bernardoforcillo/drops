package sqlite

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder builds an UPDATE statement. Create one via DB.Update.
type UpdateBuilder struct {
	db       *DB
	table    *Table
	sets     []ColumnValue
	wheres   []drops.Expression
	unscoped bool

	// defaults carries the DefaultFilters of the target table, resolved
	// for one execution — see resolvedDefaults. Set by resolveCtx on the
	// per-execution copy and read by autoWheres through defaults.of,
	// which falls back to the unresolved list so the ToSQL path renders
	// unchanged.
	defaults resolvedDefaults

	// hooked reports that the UpdateHooks have already contributed their
	// assignments to sets, which resolveCtx does so that what they
	// assign is walked for subqueries like anything else in the SET
	// list. WriteSQL runs them itself when it is false — that is the
	// ToSQL path, which has no ctx to walk against.
	hooked bool

	// resolved marks a builder resolveCtx has already produced.
	// Resolution is not idempotent — the target table still has its
	// context filters afterwards, so a second pass appends the tenant
	// predicate a second time and binds its value twice. Nothing fails;
	// the rows come back right and only an argument limit or a query log
	// shows it.
	resolved bool
}

// Set adds a column assignment.
func (u *UpdateBuilder) Set(vals ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, vals...)
	return u
}

// SetExpr assigns a raw SQL expression to col (e.g. CURRENT_TIMESTAMP,
// NULL, or "count + 1") rather than a bound value.
//
// An expression is an operand a statement can be written into, and
// there it decides what gets stored rather than which rows are touched:
// SET "ownerId" = (SELECT … FROM "accounts") reads as much as any
// SELECT. It is resolved for the executing ctx like every other
// operand — see resolveSets.
func (u *UpdateBuilder) SetExpr(col *Column, expr drops.Expression) *UpdateBuilder {
	u.sets = append(u.sets, exprValue{col: col, expr: expr})
	return u
}

// Where AND-s the given predicates onto the statement.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// Unscoped opts out of the table's automatic predicates for this
// UPDATE — its DefaultFilters and its ContextFilters alike (e.g. an
// admin job bypassing a soft-delete or tenant guard).
//
// It is statement-wide for the reason [SelectBuilder.Unscoped] gives: a
// half-scoped write is the worse of the two answers. An UPDATE that
// legitimately has to reach outside the ctx tenant says so here, and
// then says which rows it means with an explicit predicate.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder {
	u.unscoped = true
	return u
}

// WriteSQL implements drops.Expression.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	sets := u.sets
	if !u.hooked && u.table.hasUpdateHooks() {
		sets = u.applyUpdateHooks()
	}
	wheres := u.wheres
	if !u.unscoped {
		if auto := u.autoWheres(); len(auto) > 0 {
			wheres = append(auto, wheres...)
		}
	}
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

// autoWheres gathers the render-time predicates the statement carries
// on its own account — the DefaultFilters of the target table — ahead
// of the caller's own, so a statement reads scoping first and intent
// second.
//
// The filters are taken through resolvedDefaults.of rather than read
// off the table's own list, which is what walks the statements written
// inside them for the ctx being resolved.
//
// PostgreSQL's twin also gathers the FROM tables' filters, because
// UPDATE … FROM states its join condition in the WHERE clause and an
// unfiltered joined relation there lets another tenant's rows decide
// which of this tenant's rows get written. SQLite has had UPDATE … FROM
// since 3.33, but this package's builder does not expose it, so there
// is no second relation for this statement to name. If it ever gains
// one, its tables belong in this list and in contextPreds below.
func (u *UpdateBuilder) autoWheres() []drops.Expression {
	return u.defaults.of(u.table)
}

// applyUpdateHooks runs every UpdateHook on the table and returns the
// (possibly extended) SET list.
func (u *UpdateBuilder) applyUpdateHooks() []ColumnValue {
	hctx := &UpdateHookCtx{bound: make(map[*Column]bool, len(u.sets))}
	for _, s := range u.sets {
		hctx.bound[s.column().key()] = true
	}
	for _, h := range u.table.updateHookList() {
		h.BeforeUpdate(hctx)
	}
	if len(hctx.add) == 0 {
		return u.sets
	}
	out := append([]ColumnValue(nil), u.sets...)
	out = append(out, hctx.add...)
	return out
}

// ToSQL renders the statement with SQLite placeholders.
//
// A table's context filters are resolved by the executors and do not
// appear here — see [SelectBuilder.ToSQL] for why, and use
// [UpdateBuilder.ToSQLCtx] for the statement a given ctx would send.
func (u *UpdateBuilder) ToSQL() (sql string, args []any) { return ToSQL(u) }

// ToSQLCtx renders the complete statement for ctx, with every context
// filter on the target table resolved into the WHERE clause.
func (u *UpdateBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return "", nil, err
	}
	sql, args = r.ToSQL()
	return sql, args, nil
}

// resolveCtx returns the builder to render for one execution — this one
// when there was nothing to resolve, otherwise a shallow copy carrying
// the resolved predicates and the resolved subqueries.
//
// The copy is what keeps a builder executable twice; see
// [SelectBuilder.resolveCtx].
//
// The UpdateHooks run here rather than at render time so that what they
// assign goes through the same walk. A hook is registered on the table
// and reaches every UPDATE against it, and UpdateHookCtx.SetExpr takes
// any expression — so a hook is an operand position a *SelectBuilder
// can be handed to, and one no call site shows. Applied in WriteSQL,
// its assignment was reachable only through a renderer with no ctx,
// which is the same fail-open one layer in from the caller's own Set.
func (u *UpdateBuilder) resolveCtx(ctx context.Context) (*UpdateBuilder, error) {
	if u.resolved {
		return u, nil
	}
	cp := *u
	changed := false

	resolvedWheres, err := resolveExprs(ctx, u.wheres)
	if err != nil {
		return nil, err
	}
	if resolvedWheres != nil {
		cp.wheres, changed = resolvedWheres, true
	}

	sets := u.sets
	if u.table.hasUpdateHooks() {
		sets, cp.hooked, changed = u.applyUpdateHooks(), true, true
	}
	resolvedSets, err := resolveSets(ctx, sets)
	if err != nil {
		return nil, err
	}
	if resolvedSets != nil {
		sets, changed = resolvedSets, true
	}
	cp.sets = sets

	if !u.unscoped {
		defaults, err := resolveTableDefaults(ctx, u.table)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
		preds, err := u.table.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		if len(preds) > 0 {
			// After the caller's own predicates, so the rendered clause
			// reads defaults, intent, scoping — the order a SELECT
			// renders in too.
			cp.wheres = append(append([]drops.Expression(nil), cp.wheres...), preds...)
			changed = true
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
// CTE body or a subquery operand is resolved as the statement it is
// rather than rendered blind.
func (u *UpdateBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := u.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != u, nil
}

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args, err := u.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return u.db.Exec(ctx, sql, args...)
}
