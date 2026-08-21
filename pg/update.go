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
	unscoped  bool

	// defaults carries the DefaultFilters of the target table and of
	// the FROM tables, resolved for one execution — see
	// resolvedDefaults. Set by resolveCtx on the per-execution copy and
	// read by autoWheres through defaults.of, which falls back to the
	// unresolved list so the ToSQL path renders unchanged.
	defaults resolvedDefaults

	// hooked reports that the UpdateHooks have already contributed their
	// assignments to sets, which resolveCtx does so that what they
	// assign is walked for subqueries like anything else in the SET
	// list. WriteSQL runs them itself when it is false — that is the
	// ToSQL path, which has no ctx to walk against.
	hooked bool

	// resolved marks a builder resolveCtx has already produced, and is
	// the same discipline [SelectBuilder.resolved] states: resolution is
	// not idempotent — the target table still has its context filters
	// afterwards, so a second pass appends the tenant predicate a second
	// time and binds its value twice. Nothing fails; the rows come back
	// right and only an argument limit or a query log shows it. A
	// resolved statement became reachable a second time the moment
	// resolveExpr learned to walk into the write builders, since a
	// resolved body is what a resolved CTE or operand holds.
	resolved bool
}

// Set adds one or more assignments. Use (*Col[T]).Val(v) to bind a typed
// value or (*Col[T]).Expr(e) to bind an expression.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, values...)
	return u
}

// From adds tables to a PostgreSQL UPDATE ... FROM clause for joins.
//
// Each table is joined as a scoped table, not as a bare relation name:
// its DefaultFilters and its ContextFilters are carried by the
// statement like the target table's own. There is no ON clause to
// choose between here — UPDATE ... FROM states its join condition in
// the WHERE clause — so they go into the WHERE clause with everything
// else, and none of the placement reasoning a SELECT's outer joins
// need applies.
//
// Carrying them is what stops the join from deciding the write. While
// the FROM tables went in unfiltered, "UPDATE accounts SET ... FROM
// posts WHERE accounts.id = posts.accountId" matched against every
// tenant's posts and every soft-deleted one, so whose rows got written
// depended on another tenant's data. Say Unscoped() to opt the whole
// statement out.
func (u *UpdateBuilder) From(tables ...*Table) *UpdateBuilder {
	u.from = append(u.from, tables...)
	return u
}

// Where appends predicates joined by AND.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// Returning sets a RETURNING clause.
func (u *UpdateBuilder) Returning(cols ...drops.Expression) *UpdateBuilder {
	u.returning = append(u.returning, cols...)
	return u
}

// Unscoped opts out of the automatic predicates for this UPDATE — both
// the DefaultFilter list and the ContextFilter list, of the target
// table and of every table named in From alike. Use when an
// administrative job must bypass a soft-delete guard, or write across
// every tenant.
//
// It is statement-wide rather than per table for the reason
// [SelectBuilder.Unscoped] is: a flag that unscoped the target while a
// FROM table kept its tenant axis would answer the administrative
// UPDATE by writing a silently narrowed set of rows — neither the
// caller's intent nor the safe refusal, and invisible in the row count.
//
// On an UPDATE the widening is the dangerous direction: an unscoped
// statement does not read another tenant's rows, it writes them. Prefer
// restating the scope you meant — Unscoped().Where(...) — over dropping
// it wholesale.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder {
	u.unscoped = true
	return u
}

// WriteSQL renders the UPDATE.
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

// autoWheres gathers the render-time predicates the statement carries
// on its own account — the DefaultFilters of the target table and of
// every table named in the FROM clause — in the order they are written,
// ahead of the caller's own. So a statement reads scoping first, intent
// second, and a query log shows at a glance whether a write was scoped
// at all.
//
// Two things about it are load-bearing.
//
// The filters are taken through resolvedDefaults.of rather than read
// off the table's own filter list, which is what restates them against the
// instance of the table the statement actually names, and what walks
// the statements written inside them for the ctx being resolved. A
// filter is an opaque tree of closures over the declared column
// handles, so against UPDATE "notes" AS "n" a raw read renders
// "notes"."deletedAt" — a relation the statement has no FROM entry for,
// and PostgreSQL answers 42P01. That is not a widened write, it is a statement that
// cannot run, which made the tables that most need their scoping the
// ones that could not be written under an alias at all.
//
// The FROM tables are included because UPDATE ... FROM puts its join
// condition in the WHERE clause: there is no ON clause here and so
// none of the placement reasoning joinKind.filterPlacement has to do
// for SELECT. An unfiltered FROM table therefore does not merely widen
// a result — it lets another tenant's rows, or rows a soft delete
// retired, decide which of this tenant's rows get written. Scoping the
// target table alone answered that with a statement that looked
// scoped.
func (u *UpdateBuilder) autoWheres() []drops.Expression {
	var out []drops.Expression
	out = append(out, u.defaults.of(u.table)...)
	for _, t := range u.from {
		out = append(out, u.defaults.of(t)...)
	}
	return out
}

// applyUpdateHooks runs every UpdateHook registered on the table and
// returns the (possibly extended) SET list.
func (u *UpdateBuilder) applyUpdateHooks() []ColumnValue {
	ctx := &UpdateHookCtx{bound: make(map[*Column]bool, len(u.sets))}
	for _, s := range u.sets {
		ctx.bound[s.column().key()] = true
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
//
// A table's context filters are resolved by the executors and do not
// appear here — see [SelectBuilder.ToSQL] for why, and use
// [UpdateBuilder.ToSQLCtx] for the statement a given ctx would send.
func (u *UpdateBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	u.WriteSQL(b)
	return b.SQL()
}

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
// the resolved predicates and the resolved subqueries. The copy is what
// keeps the same builder executable twice without accumulating a
// predicate per run; see [SelectBuilder.resolveCtx].
//
// Two kinds of thing are resolved, and the second was missing until
// this round.
//
// Every table the statement names, the FROM tables included, because a
// context filter is a property of the table and the promise in
// tenant.go is about statements rather than about clauses. The FROM
// table of an UPDATE ... FROM is joined through the WHERE clause, so
// leaving its tenant axis off does not narrow a result set — it lets
// another tenant's rows select which of this tenant's rows are written,
// which is the read defect one grade worse.
//
// And every statement written inside this one, wherever it is written:
// a WHERE predicate, an assigned value, a RETURNING term. Resolving the
// tables alone left an UPDATE whose own tenant predicate was present
// and whose EXISTS (SELECT ... FROM posts) saw every tenant's posts,
// because the inner statement was reached only through WriteSQL, which
// has no ctx and so writes the DefaultFilters and none of the
// ContextFilters. The walk is resolveExprs — the same one
// [SelectBuilder.resolveCtx] uses, over the same node type — so it
// reaches an operand at any depth and through any combinator, not the
// shapes anybody happened to list.
//
// A filter that refuses — [TenantFilter] with no tenant on ctx — aborts
// the whole resolution: for the FROM tables as for the target, and for
// a subquery as for either. A write whose row set is chosen by a read
// that cannot say which tenant it is reading must not be sent.
//
// Unscoped skips the statement's own scoping and not the scoping of the
// statements written inside it, exactly as [SelectBuilder.resolveCtx]
// does: a subquery is a statement of its own, keeps its own tenant
// axis, and is how to widen one relation of a write and no other.
//
// The UpdateHooks run here rather than at render time so that what they
// assign goes through the same walk — see the comment on the call, and
// [UpdateBuilder.WriteSQL], which still runs them itself on the ToSQL
// path where there is no ctx to walk against.
func (u *UpdateBuilder) resolveCtx(ctx context.Context) (*UpdateBuilder, error) {
	if u.resolved {
		return u, nil
	}
	cp := *u
	changed := false

	// Every expression list the statement carries, because a subquery is
	// a statement wherever it is written.
	lists := []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{u.wheres, &cp.wheres},
		{u.returning, &cp.returning},
	}
	for _, l := range lists {
		resolved, err := resolveExprs(ctx, l.src)
		if err != nil {
			return nil, err
		}
		if resolved != nil {
			*l.dst, changed = resolved, true
		}
	}

	// The hooks run here rather than at render time, so that what they
	// assign is walked with the rest of the SET list. An UpdateHook is
	// registered on the table and reaches every UPDATE against it, and
	// UpdateHookCtx.SetExpr takes any expression — so a hook is an
	// operand position a *SelectBuilder can be handed to, and one no
	// call site shows. Applied in WriteSQL, its assignment was reachable
	// only through a renderer with no ctx, which is the same fail-open
	// one layer in from the caller's own Set.
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
		// The DefaultFilters of the target and of the FROM tables,
		// walked for the statements written inside them — the same
		// lists autoWheres renders, resolved for the same reason the
		// context filters below are.
		defaults, err := resolveTableDefaults(ctx, append([]*Table{u.table}, u.from...)...)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
		preds, err := u.contextPreds(ctx)
		if err != nil {
			return nil, err
		}
		if len(preds) > 0 {
			// After the caller's own predicates and after the resolution
			// above, so the rendered clause reads the same as it did
			// before subqueries were walked: defaults, intent, scoping.
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

// contextPreds resolves the context filters of every table the
// statement names — the target and the FROM tables — into the
// predicates the WHERE clause carries for this ctx.
func (u *UpdateBuilder) contextPreds(ctx context.Context) ([]drops.Expression, error) {
	preds, err := u.table.resolveContextFilters(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range u.from {
		fp, err := t.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		preds = append(preds, fp...)
	}
	return preds, nil
}

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(u.sets) == 0 && !u.table.hasUpdateHooks() {
		return nil, ErrNoUpdateAssignments
	}
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
