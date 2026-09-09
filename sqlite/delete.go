package sqlite

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// DeleteBuilder builds a DELETE statement. Create one via DB.Delete.
type DeleteBuilder struct {
	db     *DB
	table  *Table
	wheres []drops.Expression
	scope  filterScope

	// defaults is what this execution resolved for the table's
	// render-time filters, and nil when none of them held a statement.
	defaults resolvedDefaults

	// resolved marks the copy resolveCtx produced, so a statement
	// nested in another is not resolved twice.
	resolved bool
}

// Table returns the target table.
func (d *DeleteBuilder) Table() *Table { return d.table }

// DB returns the executing DB — used by DeleteHooks that build a
// replacement statement (an UPDATE for soft-delete).
func (d *DeleteBuilder) DB() *DB { return d.db }

// Wheres returns a copy of the predicate slice, so a DeleteHook can read
// the original WHERE clauses when synthesising replacement SQL.
func (d *DeleteBuilder) Wheres() []drops.Expression {
	return append([]drops.Expression(nil), d.wheres...)
}

// IsUnscoped reports whether the caller opted out of every default
// scope via Unscoped. A DeleteHook reads it to tell a hard DELETE from
// the soft one it would otherwise rewrite; IgnoreFilters does not set
// it, because naming a filter drops a predicate and not the rewrite.
func (d *DeleteBuilder) IsUnscoped() bool { return d.scope.unscoped }

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one standing — see [SelectBuilder.IgnoreFilters].
// The DeleteHooks stay, so on a soft-deleted table the statement is
// still rewritten into an UPDATE.
func (d *DeleteBuilder) IgnoreFilters(names ...string) *DeleteBuilder {
	d.scope.ignore(names...)
	return d
}

// Where AND-s the given predicates onto the statement. A DELETE with no
// WHERE removes every row — that is intentional but rarely desired, and
// a Where whose predicates were all nil is one of them: nil means "no
// restriction", so nothing is left to restrict.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, dropNilPreds(preds)...)
	return d
}

// Unscoped opts out of both DeleteHooks and every global filter on the
// table. On a soft-deleted table it forces a real, hard DELETE — and,
// being the blunt instrument, drops the table's other scoping with it.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder {
	d.scope.unscoped = true
	return d
}

// WriteSQL implements drops.Expression. If the table has DeleteHooks and
// the caller has not opted out via Unscoped, a hook may replace the
// statement entirely — used by SoftDelete to flip DELETE into UPDATE.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	if !d.scope.unscoped {
		for _, h := range d.table.deleteHookList() {
			if rep := h.BeforeDelete(d); rep != nil {
				rep.WriteSQL(b)
				return
			}
		}
	}
	wheres := d.scope.apply(d.table, d.wheres, d.defaults)
	b.WriteString("DELETE FROM ")
	d.table.writeName(b)
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
}

// ToSQL renders the statement with SQLite placeholders.
//
// It carries the table's DefaultFilters and none of its
// ContextFilters, because a render has no ctx to build one from — so
// on a tenant-scoped table it is not the statement that would be sent.
// Prefer [DeleteBuilder.ToSQLCtx].
func (d *DeleteBuilder) ToSQL() (sql string, args []any) { return ToSQL(d) }

// ToSQLCtx renders the statement ctx would send: the table's context
// filters built and AND-ed into the WHERE clause, and every statement
// written inside the predicates resolved on the same terms.
//
// A filter that cannot decide what the request may see refuses, and
// the refusal is returned instead of a statement. Nothing walks a
// DELETE back.
func (d *DeleteBuilder) ToSQLCtx(ctx context.Context) (sql string, args []any, err error) {
	r, err := d.resolveCtx(ctx)
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
// The context predicates are appended to the WHERE list rather than
// held apart, because a DeleteHook reads that list to build the UPDATE
// it replaces the statement with — see [DeleteBuilder.Wheres]. A soft
// delete that dropped them would rewrite every tenant's rows.
func (d *DeleteBuilder) resolveCtx(ctx context.Context) (*DeleteBuilder, error) {
	if d.resolved {
		return d, nil
	}
	// The named filters this statement bypasses, for the length of
	// this resolution: a nested statement installs its own at the top
	// of its own resolveCtx, so IgnoreFilters reaches no further than
	// the statement that said it.
	ctx = withIgnoredFilters(ctx, d.scope)

	cp := *d
	changed := false

	wheres := d.wheres
	if r, err := resolveExprs(ctx, d.wheres); err != nil {
		return nil, err
	} else if r != nil {
		wheres, changed = r, true
	}

	if !d.scope.dropsContextFilters() {
		preds, err := d.table.resolveContextFilters(ctx)
		if err != nil {
			return nil, err
		}
		if len(preds) > 0 {
			all := make([]drops.Expression, 0, len(wheres)+len(preds))
			all = append(all, wheres...)
			wheres, changed = append(all, preds...), true
		}
		defaults, err := resolveTableDefaults(ctx, d.table)
		if err != nil {
			return nil, err
		}
		if defaults != nil {
			cp.defaults, changed = defaults, true
		}
	}

	if !changed {
		return d, nil
	}
	cp.wheres = wheres
	cp.resolved = true
	return &cp, nil
}

// resolveStatement implements [ctxResolvable], so a DELETE written as a
// CTE body is scoped like a bare one.
func (d *DeleteBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := d.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != d, nil
}

// Exec runs the DELETE, with the table's context filters resolved
// against ctx first: a DELETE that loses them removes another tenant's
// rows and reports success.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return d.db.Exec(ctx, sql, args...)
}
