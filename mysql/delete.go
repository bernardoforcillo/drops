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
	scope    filterScope

	// defaults is what this execution resolved for the table's
	// render-time filters, and nil when none of them held a statement.
	defaults resolvedDefaults

	// resolved marks the copy resolveCtx produced, so a statement
	// nested in another is not resolved twice.
	resolved bool
}

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a filter that is only sometimes present can be passed straight in
// — but a DELETE all of whose predicates were nil is a DELETE with no
// WHERE, and removes every row the table's filters still admit.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, dropNilPreds(preds)...)
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

// Unscoped opts out of every global filter on the table — the blunt
// instrument; see [SelectBuilder.Unscoped].
func (d *DeleteBuilder) Unscoped() *DeleteBuilder { d.scope.unscoped = true; return d }

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one standing — see [SelectBuilder.IgnoreFilters].
func (d *DeleteBuilder) IgnoreFilters(names ...string) *DeleteBuilder {
	d.scope.ignore(names...)
	return d
}

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
	wheres := d.scope.apply(d.table, d.wheres, d.defaults)
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
func (d *DeleteBuilder) resolveCtx(ctx context.Context) (*DeleteBuilder, error) {
	if d.resolved {
		return d, nil
	}
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
	if d.table.alias != "" && (len(d.orderBys) > 0 || d.limit != nil) {
		return nil, ErrAliasedDeleteBounded
	}
	sql, args, err := d.ToSQLCtx(ctx)
	if err != nil {
		return nil, err
	}
	return d.db.Exec(ctx, sql, args...)
}
