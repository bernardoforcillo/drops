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
}

// Where AND-s the given predicates onto the statement. A DELETE with no
// WHERE removes every row — that is intentional but rarely desired.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// WriteSQL implements drops.Expression.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("DELETE FROM ")
	d.table.writeName(b)
	if len(d.wheres) > 0 {
		b.WriteString(" WHERE ")
		b.AppendList(" AND ", d.wheres)
	}
}

// ToSQL renders the statement with SQLite placeholders.
func (d *DeleteBuilder) ToSQL() (sql string, args []any) { return ToSQL(d) }

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args := d.ToSQL()
	return d.db.Exec(ctx, sql, args...)
}
