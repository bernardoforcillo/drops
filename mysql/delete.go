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
	wheres := d.scope.apply(d.table, d.wheres)
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

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if d.table.alias != "" && (len(d.orderBys) > 0 || d.limit != nil) {
		return nil, ErrAliasedDeleteBounded
	}
	sql, args := d.ToSQL()
	return d.db.Exec(ctx, sql, args...)
}
