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
// WHERE removes every row — that is intentional but rarely desired.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
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
		for _, h := range d.table.deleteHooks {
			if rep := h.BeforeDelete(d); rep != nil {
				rep.WriteSQL(b)
				return
			}
		}
	}
	wheres := d.scope.apply(d.table, d.wheres)
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
