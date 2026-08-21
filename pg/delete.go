package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// DeleteBuilder composes a DELETE statement.
type DeleteBuilder struct {
	db        *DB
	table     *Table
	using     []*Table
	wheres    []drops.Expression
	returning []drops.Expression
	unscoped  bool

	// defaults carries the DefaultFilters of the target table and of
	// the USING tables, resolved for one execution — see
	// resolvedDefaults. Set by resolveCtx on the per-execution copy and
	// read by autoWheres through defaults.of, which falls back to the
	// unresolved list so the ToSQL path renders unchanged.
	defaults resolvedDefaults

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

// Table returns the target table.
func (d *DeleteBuilder) Table() *Table { return d.table }

// Wheres returns a copy of the predicate slice — exposed so custom
// DeleteHooks (e.g. soft-delete rewrites) can read the original WHERE
// clauses when synthesising replacement SQL.
func (d *DeleteBuilder) Wheres() []drops.Expression {
	return append([]drops.Expression(nil), d.wheres...)
}

// ReturningClauses returns a copy of the RETURNING projection list.
func (d *DeleteBuilder) ReturningClauses() []drops.Expression {
	return append([]drops.Expression(nil), d.returning...)
}

// IsUnscoped reports whether the caller opted out of default scopes.
func (d *DeleteBuilder) IsUnscoped() bool { return d.unscoped }

// DB returns the executing DB. Hooks that need to build a replacement
// statement (an UPDATE for soft-delete, for instance) use it.
func (d *DeleteBuilder) DB() *DB { return d.db }

// Using adds tables to a PostgreSQL DELETE ... USING clause for joins.
//
// Each table is joined as a scoped table, not as a bare relation name:
// its DefaultFilters and its ContextFilters are carried by the
// statement like the target table's own, in the WHERE clause — which is
// where DELETE ... USING states its join condition, so there is no ON
// clause to choose between and none of the placement reasoning a
// SELECT's outer joins need.
//
// Carrying them is what stops the join from deciding the delete. While
// the USING tables went in unfiltered, "DELETE FROM accounts USING
// posts WHERE accounts.id = posts.accountId" matched against every
// tenant's posts, so whose rows were removed depended on another
// tenant's data — and a delete is the one write nothing walks back. Say
// Unscoped() to opt the whole statement out.
func (d *DeleteBuilder) Using(tables ...*Table) *DeleteBuilder {
	d.using = append(d.using, tables...)
	return d
}

// Where appends predicates joined by AND.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// Returning sets a RETURNING clause.
func (d *DeleteBuilder) Returning(cols ...drops.Expression) *DeleteBuilder {
	d.returning = append(d.returning, cols...)
	return d
}

// Unscoped opts out of the DeleteHooks, the DefaultFilters and the
// ContextFilters for this statement — for the target table and for
// every table named in Using alike. On a soft-deleted table it forces a
// real, hard DELETE that bypasses the rewrite-to-UPDATE behaviour.
//
// It is statement-wide rather than per table for the reason
// [SelectBuilder.Unscoped] is: leaving a USING table's tenant axis in
// place would answer the administrative DELETE by removing a silently
// narrowed set of rows, which is neither the caller's intent nor the
// safe refusal and shows up only as a row count nobody checks.
//
// It is the widest opt-out drops has: an unscoped DELETE against a
// tenant-scoped table removes every tenant's rows and cannot be undone
// by a soft-delete column that is no longer being consulted. Restate
// the scope you meant in a Where.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder {
	d.unscoped = true
	return d
}

// WriteSQL renders the DELETE. If the table has DeleteHooks and the
// caller has not opted out via Unscoped, hooks may replace the
// statement entirely — used by SoftDelete to flip DELETE into UPDATE.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	if !d.unscoped {
		for _, h := range d.table.deleteHooks {
			if rep := h.BeforeDelete(d); rep != nil {
				d.carryUsing(rep).WriteSQL(b)
				return
			}
		}
	}
	wheres := d.wheres
	if !d.unscoped {
		if auto := d.autoWheres(); len(auto) > 0 {
			wheres = append(auto, wheres...)
		}
	}
	b.WriteString("DELETE FROM ")
	d.table.writeFrom(b)
	if len(d.using) > 0 {
		b.WriteString(" USING ")
		for j, t := range d.using {
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
	if len(d.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", d.returning)
	}
}

// autoWheres gathers the render-time predicates the statement carries
// on its own account — the DefaultFilters of the target table and of
// every table named in the USING clause — ahead of the caller's own, so
// a statement reads scoping first and intent second.
//
// It mirrors UpdateBuilder.autoWheres exactly, and for the same two
// reasons. The filters go through resolvedDefaults.of so that they are
// restated against the instance the statement names, and so that the
// statements written inside them are walked for the ctx being
// resolved: read raw, a filter built from the declared handles renders
// "notes"."archivedAt" against DELETE FROM "notes" AS "n", which is
// 42P01 rather than a widened delete — the aliased statement cannot run
// at all. And the USING tables are included because DELETE ... USING
// states its join condition in the WHERE clause, so an unfiltered
// USING table lets another tenant's rows, or rows a soft delete
// retired, choose which of this tenant's rows are removed.
func (d *DeleteBuilder) autoWheres() []drops.Expression {
	var out []drops.Expression
	out = append(out, d.defaults.of(d.table)...)
	for _, t := range d.using {
		out = append(out, d.defaults.of(t)...)
	}
	return out
}

// carryUsing hands the USING tables to a hook's replacement statement
// when the replacement is an UPDATE that has no FROM clause of its own.
//
// The rewrite a soft delete performs is built from d.Wheres(), and on a
// DELETE ... USING those predicates name the USING relation — the
// caller's join condition does, and since this round so do that
// relation's own automatic predicates. An UPDATE with no FROM clause
// carrying them is 42P01: the soft delete of a joined DELETE simply did
// not render a runnable statement. UPDATE ... FROM is the exact
// equivalent of DELETE ... USING, so handing the tables over is what
// makes the rewrite mean what the DELETE meant — and routes them
// through UpdateBuilder.autoWheres, so the joined relation is scoped on
// the rewritten statement too rather than only on the one shape that
// never reached a server.
//
// The replacement is copied rather than mutated because a hook is free
// to hand back a builder it holds on to, and appending to that builder's
// FROM list would grow it by one table per render.
func (d *DeleteBuilder) carryUsing(rep drops.Expression) drops.Expression {
	up, ok := rep.(*UpdateBuilder)
	if !ok || len(d.using) == 0 || len(up.from) > 0 {
		return rep
	}
	cp := *up
	cp.from = append([]*Table(nil), d.using...)
	return &cp
}

// ToSQL renders the statement.
//
// A table's context filters are resolved by the executors and do not
// appear here — see [SelectBuilder.ToSQL] for why, and use
// [DeleteBuilder.ToSQLCtx] for the statement a given ctx would send.
func (d *DeleteBuilder) ToSQL() (sql string, args []any) {
	b := drops.NewBuilder()
	d.WriteSQL(b)
	return b.SQL()
}

// ToSQLCtx renders the complete statement for ctx, with every context
// filter on the target table resolved into the WHERE clause.
func (d *DeleteBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := d.resolveCtx(ctx)
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
// Resolving into the copy's clauses rather than at render time is what
// carries the scope through a soft delete: a DeleteHook rewrites the
// statement into an UPDATE built from d.Wheres() and
// d.ReturningClauses(), and anything added after the hook ran would be
// missing from exactly the statement that mutates rows. The hook sees
// the copy, so it sees the tenant — the target table's, and the tenant
// of every statement written inside a predicate it copies over.
//
// Two kinds of thing are resolved, and the second was missing until
// this round.
//
// Every table the statement names, the USING tables included, because a
// context filter is a property of the table and the promise in
// tenant.go is about statements rather than about clauses. A
// DELETE ... USING joins through its WHERE clause, so an unfiltered
// USING table does not narrow a result — it lets another tenant's rows
// choose which of this tenant's rows are removed, and a delete is the
// one write that cannot be walked back.
//
// And every statement written inside this one — a WHERE predicate, a
// RETURNING term — through resolveExprs, the walk
// [SelectBuilder.resolveCtx] uses over the same node type, so it
// reaches an operand at any depth and through any combinator rather
// than the shapes anybody happened to list. Resolving the tables alone
// left DELETE FROM "users" WHERE "id" NOT IN (SELECT ... FROM "posts")
// reading every tenant's posts to decide which of this tenant's rows
// survived, out of a statement carrying a tenant predicate of its own:
// the inner statement was reached only through WriteSQL, which has no
// ctx and so writes the DefaultFilters and none of the ContextFilters.
//
// A filter that refuses — [TenantFilter] with no tenant on ctx — aborts
// the whole resolution: for the USING tables as for the target, and for
// a subquery as for either.
//
// Unscoped skips the statement's own scoping — its DeleteHooks
// included, so the rewrite does not run — and not the scoping of the
// statements written inside it, exactly as [SelectBuilder.resolveCtx]
// does.
//
// One thing this does not reach, stated so the next round does not have
// to find it: what a DeleteHook's replacement statement assigns. The
// hooks run inside WriteSQL, which has no ctx, so an UpdateHook on the
// target table that assigned a subquery would have it rendered
// unresolved on the rewrite path while a direct UPDATE against the same
// table resolves it — see UpdateBuilder.resolveCtx, which runs its own
// hooks during resolution for exactly that reason. Closing it means
// dispatching the DeleteHooks here instead, which is a bigger change to
// the render path than this round is making; nothing a caller writes
// lands there, since a DELETE has no SET list of its own.
func (d *DeleteBuilder) resolveCtx(ctx context.Context) (*DeleteBuilder, error) {
	if d.resolved {
		return d, nil
	}
	cp := *d
	changed := false

	// Every expression list the statement carries, because a subquery is
	// a statement wherever it is written.
	lists := []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{d.wheres, &cp.wheres},
		{d.returning, &cp.returning},
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

	if !d.unscoped {
		// The DefaultFilters of the target and of the USING tables,
		// walked for the statements written inside them — the same
		// lists autoWheres renders, resolved for the same reason the
		// context filters below are.
		defaults, err := resolveTableDefaults(ctx, append([]*Table{d.table}, d.using...)...)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
		preds, err := d.contextPreds(ctx)
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
		return d, nil
	}
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable]: it is resolveCtx behind
// the interface resolveExpr dispatches on, so a DELETE written as a CTE
// body or a subquery operand is resolved as the statement it is rather
// than rendered blind — which for a DELETE means rendered with no WHERE
// clause at all.
func (d *DeleteBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := d.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != d, nil
}

// contextPreds resolves the context filters of every table the
// statement names — the target and the USING tables — into the
// predicates the WHERE clause carries for this ctx.
func (d *DeleteBuilder) contextPreds(ctx context.Context) ([]drops.Expression, error) {
	preds, err := d.table.resolveContextFilters(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range d.using {
		up, err := t.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		preds = append(preds, up...)
	}
	return preds, nil
}

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return d.db.Exec(ctx, sql, args...)
}

// All executes the DELETE and scans the RETURNING rows into dest.
func (d *DeleteBuilder) All(ctx context.Context, dest any) error {
	if len(d.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := d.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the DELETE and scans the first RETURNING row into dest.
func (d *DeleteBuilder) One(ctx context.Context, dest any) error {
	if len(d.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return err
	}
	rows, err := d.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
