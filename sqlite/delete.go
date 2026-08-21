package sqlite

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// DeleteBuilder builds a DELETE statement. Create one via DB.Delete.
type DeleteBuilder struct {
	db       *DB
	table    *Table
	wheres   []drops.Expression
	unscoped bool

	// defaults carries the DefaultFilters of the target table, resolved
	// for one execution — see resolvedDefaults and
	// [UpdateBuilder.defaults], which this mirrors.
	defaults resolvedDefaults

	// resolved marks a builder resolveCtx has already produced; see
	// [UpdateBuilder.resolved] for why resolving twice is not harmless.
	resolved bool
}

// Table returns the target table.
func (d *DeleteBuilder) Table() *Table { return d.table }

// DB returns the executing DB — used by DeleteHooks that build a
// replacement statement (an UPDATE for soft-delete).
func (d *DeleteBuilder) DB() *DB { return d.db }

// Wheres returns a copy of the predicate slice, so a DeleteHook can read
// the original WHERE clauses when synthesising replacement SQL.
//
// On the executed path the copy a hook sees is the RESOLVED one: the
// hook runs during WriteSQL, and by then resolveCtx has produced a
// builder whose wheres already carry the target table's context
// filters. That ordering is the whole reason a soft delete stays
// scoped — a rewrite built from an unresolved predicate list would drop
// the tenant guard on exactly the statement that changes rows.
func (d *DeleteBuilder) Wheres() []drops.Expression {
	return append([]drops.Expression(nil), d.wheres...)
}

// IsUnscoped reports whether the caller opted out of default scopes.
func (d *DeleteBuilder) IsUnscoped() bool { return d.unscoped }

// Where AND-s the given predicates onto the statement. A DELETE with no
// WHERE removes every row — that is intentional but rarely desired.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// Unscoped opts out of DeleteHooks and of the table's automatic
// predicates — its DefaultFilters and its ContextFilters alike. On a
// soft-deleted table it forces a real, hard DELETE.
//
// It is statement-wide for the reason [SelectBuilder.Unscoped] gives,
// and the consequence is sharper here than anywhere else in the
// package: an unscoped DELETE on a tenant-scoped table can remove
// another tenant's rows, and nothing walks a DELETE back. Pair it with
// the predicate that says which rows are meant.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder {
	d.unscoped = true
	return d
}

// WriteSQL implements drops.Expression. If the table has DeleteHooks and
// the caller has not opted out via Unscoped, a hook may replace the
// statement entirely — used by SoftDelete to flip DELETE into UPDATE.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	if !d.unscoped {
		for _, h := range d.table.deleteHookList() {
			if rep := h.BeforeDelete(d); rep != nil {
				d.carryDefaults(rep).WriteSQL(b)
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
	d.table.writeName(b)
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
}

// autoWheres gathers the render-time predicates the statement carries
// on its own account — the DefaultFilters of the target table — ahead
// of the caller's own.
//
// It mirrors [UpdateBuilder.autoWheres], and the entry that mirror is
// missing is deliberate: PostgreSQL's DELETE … USING names a second
// relation whose filters have to travel with it, and SQLite's DELETE
// has no USING clause at all. There is no joined relation here for an
// unfiltered read to smuggle in.
func (d *DeleteBuilder) autoWheres() []drops.Expression {
	return d.defaults.of(d.table)
}

// carryDefaults hands this execution's resolved DefaultFilters to a
// hook's replacement statement when the replacement is an UPDATE that
// has none of its own.
//
// The rewrite a soft delete performs builds a fresh UpdateBuilder from
// d.Wheres(), and a fresh builder has an empty resolvedDefaults map — so
// it falls back to the table's RENDER-TIME filter list, which is the
// list with the statements inside it unwalked. A default filter that
// embeds a scoped read then went out unresolved on exactly the
// statement that mutates rows, while the DELETE it replaced had
// resolved it correctly. The context filters need no such handover:
// they are already in the predicates the hook copied across, and
// re-adding them would bind the tenant twice.
//
// The replacement is copied rather than mutated because a hook is free
// to hand back a builder it holds on to, and writing one execution's
// resolution into that builder would answer the next execution with it.
func (d *DeleteBuilder) carryDefaults(rep drops.Expression) drops.Expression {
	up, ok := rep.(*UpdateBuilder)
	if !ok || d.defaults == nil || up.defaults != nil {
		return rep
	}
	cp := *up
	cp.defaults = d.defaults
	return &cp
}

// ToSQL renders the statement with SQLite placeholders.
//
// A table's context filters are resolved by the executors and do not
// appear here — use [DeleteBuilder.ToSQLCtx] for the statement a given
// ctx would send.
func (d *DeleteBuilder) ToSQL() (sql string, args []any) { return ToSQL(d) }

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
// statement into an UPDATE built from d.Wheres(), and anything added
// after the hook ran would be missing from exactly the statement that
// mutates rows. The hook sees the copy, so it sees the tenant — the
// target table's, and the tenant of every statement written inside a
// predicate it copies over.
func (d *DeleteBuilder) resolveCtx(ctx context.Context) (*DeleteBuilder, error) {
	if d.resolved {
		return d, nil
	}
	cp := *d
	changed := false

	resolvedWheres, err := resolveExprs(ctx, d.wheres)
	if err != nil {
		return nil, err
	}
	if resolvedWheres != nil {
		cp.wheres, changed = resolvedWheres, true
	}

	if !d.unscoped {
		defaults, err := resolveTableDefaults(ctx, d.table)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
		preds, err := d.table.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		if len(preds) > 0 {
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
// body or a subquery operand is resolved as the statement it is.
func (d *DeleteBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := d.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != d, nil
}

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return d.db.Exec(ctx, sql, args...)
}
