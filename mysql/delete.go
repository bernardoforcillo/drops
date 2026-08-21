package mysql

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// DeleteBuilder composes a DELETE statement.
type DeleteBuilder struct {
	db       *DB
	table    *Table
	wheres   []drops.Expression
	orderBys []drops.Expression
	limit    *int64
	unscoped bool

	// defaults carries the DefaultFilters of the target table, resolved
	// for one execution — see resolvedDefaults. Set by resolveCtx on the
	// per-execution copy and read by WriteSQL through defaults.of, which
	// falls back to the unresolved list so the ToSQL path renders
	// unchanged.
	defaults resolvedDefaults

	// resolved marks a builder resolveCtx has already produced; see
	// [UpdateBuilder] for why resolving twice is not harmless.
	resolved bool
}

// Where appends predicates joined by AND.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// OrderBy and Limit bound which rows a DELETE removes — a MySQL
// extension, and how you delete a large backlog in batches without
// holding one enormous transaction.
//
// Neither survives an alias. An aliased DELETE has to be written in the
// multi-table form (see [DeleteBuilder.WriteSQL]), and that form
// accepts no ORDER BY and no LIMIT on either server — the statement
// comes back as error 1064. There is nothing to render that would
// work, so Exec refuses with [ErrAliasedDeleteBounded] rather than
// posting a statement the server is certain to reject. Batch through
// the un-aliased table handle.
func (d *DeleteBuilder) OrderBy(exprs ...drops.Expression) *DeleteBuilder {
	d.orderBys = append(d.orderBys, exprs...)
	return d
}

func (d *DeleteBuilder) Limit(n int64) *DeleteBuilder { d.limit = &n; return d }

// Unscoped opts out of the table's automatic predicates — its
// DefaultFilter list and its ContextFilter list alike. On a
// tenant-scoped table this is the difference between deleting one
// tenant's rows and deleting everybody's, so it says so at the call
// site; see [SelectBuilder.Unscoped] for why it is both lists or
// neither.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder { d.unscoped = true; return d }

// WriteSQL renders the DELETE.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	if d.table.alias != "" {
		// An aliased DELETE has to name the alias twice: once as the
		// target and once in the FROM. MariaDB rejects the shorter
		// "DELETE FROM t AS a" outright — error 1064 — while both it
		// and MySQL accept the multi-table spelling against a single
		// table, so drops emits the form the whole family takes.
		b.WriteString("DELETE ")
		b.WriteIdent(d.table.alias)
		b.WriteString(" FROM ")
		d.table.writeFrom(b)
	} else {
		b.WriteString("DELETE FROM ")
		d.table.writeName(b)
	}
	wheres := d.wheres
	if dfs := d.autoWheres(); len(dfs) > 0 {
		wheres = append(append([]drops.Expression(nil), dfs...), wheres...)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(d.orderBys) > 0 {
		b.WriteString(" ORDER BY ")
		b.AppendList(", ", d.orderBys)
	}
	if d.limit != nil {
		b.WriteString(" LIMIT ")
		b.AddArg(*d.limit)
	}
}

// ErrAliasedDeleteBounded is returned when an aliased DELETE also
// carries ORDER BY or LIMIT, a combination no server accepts — see
// [DeleteBuilder.OrderBy].
var ErrAliasedDeleteBounded = errors.New(
	"drops/mysql: an aliased DELETE takes no ORDER BY or LIMIT; batch through the un-aliased table")

// ToSQL renders the statement and its arguments. It renders what was
// asked for, including the bounded aliased form Exec refuses: a
// builder that quietly dropped the LIMIT would turn a batch of a
// thousand rows into the whole table.
func (d *DeleteBuilder) ToSQL() (string, []any) { return render(d) }

// autoWheres gathers the render-time predicates the statement carries
// on its own account — the DefaultFilters of the target table — ahead
// of the caller's own, so a statement reads scoping first and intent
// second.
//
// PostgreSQL's twin also gathers the USING tables' filters, because
// DELETE … USING states its join condition in the WHERE clause and an
// unfiltered joined relation there lets another tenant's rows decide
// which of this tenant's rows are destroyed. MySQL has that shape too,
// as DELETE t FROM t JOIN u ON …, but this builder does not expose it:
// a DELETE here names exactly one relation, which the multi-table form
// below names twice. If it ever gains a second, its tables belong in
// this list and in resolveCtx.
func (d *DeleteBuilder) autoWheres() []drops.Expression {
	if d.unscoped {
		return nil
	}
	return d.defaults.of(d.table)
}

// ToSQLCtx renders the complete statement for ctx, with every context
// filter on the target table resolved into the WHERE clause.
//
// The alias interaction is this dialect's own. An aliased DELETE is
// written in the multi-table form, which names the alias as the target
// and again in the FROM — there is no un-aliased spelling to fall back
// on — so a tenant axis built from the declared column handles would
// name a relation the statement does not have, and MariaDB and MySQL
// both answer 1054. Table.resolveFilterExprs renames the relation for
// the length of each predicate, which is what keeps a scoped table
// deletable under an alias at all.
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
// the resolved predicates and the resolved subqueries. The copy is what
// keeps a builder executable twice; see [SelectBuilder.resolveCtx].
func (d *DeleteBuilder) resolveCtx(ctx context.Context) (*DeleteBuilder, error) {
	if d.resolved {
		return d, nil
	}
	cp := *d
	changed := false

	lists := []struct {
		src []drops.Expression
		dst *[]drops.Expression
	}{
		{d.wheres, &cp.wheres},
		{d.orderBys, &cp.orderBys},
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
// the interface resolveExpr dispatches on, so a DELETE written as a
// CTE body or a subquery operand is resolved as the statement it is
// rather than rendered blind — which for a DELETE is the difference
// between removing one tenant's rows and removing everybody's.
func (d *DeleteBuilder) resolveStatement(ctx context.Context) (drops.Expression, bool, error) {
	r, err := d.resolveCtx(ctx)
	if err != nil {
		return nil, false, err
	}
	return r, r != d, nil
}

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if d.table.alias != "" && (len(d.orderBys) > 0 || d.limit != nil) {
		return nil, ErrAliasedDeleteBounded
	}
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return d.db.Exec(ctx, sql, args...)
}
