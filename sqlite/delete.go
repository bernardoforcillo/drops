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
}

// Where AND-s the given predicates onto the statement. A DELETE with no
// WHERE removes every row — that is intentional but rarely desired.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// Unscoped opts out of the table's DefaultFilter predicates for this
// statement (e.g. to hard-delete rows already hidden by a soft-delete
// guard).
func (d *DeleteBuilder) Unscoped() *DeleteBuilder { d.unscoped = true; return d }

// WriteSQL implements drops.Expression.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("DELETE FROM ")
	d.table.writeName(b)
	wheres := d.wheres
	if !d.unscoped && len(d.table.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), d.table.defaultFilters...), d.wheres...)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.AppendList(" AND ", wheres)
	}
}

// ToSQL renders the statement with SQLite placeholders.
func (d *DeleteBuilder) ToSQL() (sql string, args []any) { return ToSQL(d) }

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args := d.ToSQL()
	return d.db.Exec(ctx, sql, args...)
}
