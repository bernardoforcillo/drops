package mysql

import (
	"context"

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
}

// Where appends predicates joined by AND.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// OrderBy and Limit bound which rows a DELETE removes — a MySQL
// extension, and how you delete a large backlog in batches without
// holding one enormous transaction.
func (d *DeleteBuilder) OrderBy(exprs ...drops.Expression) *DeleteBuilder {
	d.orderBys = append(d.orderBys, exprs...)
	return d
}

func (d *DeleteBuilder) Limit(n int64) *DeleteBuilder { d.limit = &n; return d }

// Unscoped opts out of the table's DefaultFilter predicates.
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
	if !d.unscoped && len(d.table.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), d.table.defaultFilters...), wheres...)
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

// ToSQL renders the statement and its arguments.
func (d *DeleteBuilder) ToSQL() (string, []any) { return render(d) }

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args := d.ToSQL()
	return d.db.Exec(ctx, sql, args...)
}
