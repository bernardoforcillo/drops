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

// IsUnscoped reports whether the caller opted out of default scopes.
func (d *DeleteBuilder) IsUnscoped() bool { return d.unscoped }

// Where AND-s the given predicates onto the statement. A DELETE with no
// WHERE removes every row — that is intentional but rarely desired.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// Unscoped opts out of both DeleteHooks and DefaultFilters for this
// statement. On a soft-deleted table it forces a real, hard DELETE.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder {
	d.unscoped = true
	return d
}

// WriteSQL implements drops.Expression. If the table has DeleteHooks and
// the caller has not opted out via Unscoped, a hook may replace the
// statement entirely — used by SoftDelete to flip DELETE into UPDATE.
func (d *DeleteBuilder) WriteSQL(b *drops.Builder) {
	if !d.unscoped {
		for _, h := range d.table.deleteHooks {
			if rep := h.BeforeDelete(d); rep != nil {
				rep.WriteSQL(b)
				return
			}
		}
	}
	wheres := d.wheres
	if !d.unscoped && len(d.table.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), d.table.defaultFilters...), wheres...)
	}
	b.WriteString("DELETE FROM ")
	d.table.writeName(b)
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
