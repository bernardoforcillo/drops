package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// DeleteBuilder composes a DELETE statement.
type DeleteBuilder struct {
	db        *DB
	table     *Table
	using     []*Table
	wheres    []drops.Expression
	returning []drops.Expression
	unscoped  bool
}

// Table returns the target table.
func (d *DeleteBuilder) Table() *Table { return d.table }

// Wheres returns a copy of the predicate slice — exposed so custom
// DeleteHooks (e.g. soft-delete rewrites) can read the original WHERE
// clauses when synthesising replacement SQL.
func (d *DeleteBuilder) Wheres() []drops.Expression {
	return append([]drops.Expression(nil), d.wheres...)
}

// ReturningClauses returns a copy of the RETURNING projection list.
func (d *DeleteBuilder) ReturningClauses() []drops.Expression {
	return append([]drops.Expression(nil), d.returning...)
}

// IsUnscoped reports whether the caller opted out of default scopes.
func (d *DeleteBuilder) IsUnscoped() bool { return d.unscoped }

// DB returns the executing DB. Hooks that need to build a replacement
// statement (an UPDATE for soft-delete, for instance) use it.
func (d *DeleteBuilder) DB() *DB { return d.db }

// Using adds tables to a PostgreSQL DELETE ... USING clause for joins.
func (d *DeleteBuilder) Using(tables ...*Table) *DeleteBuilder {
	d.using = append(d.using, tables...)
	return d
}

// Where appends predicates joined by AND.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, preds...)
	return d
}

// Returning sets a RETURNING clause.
func (d *DeleteBuilder) Returning(cols ...drops.Expression) *DeleteBuilder {
	d.returning = append(d.returning, cols...)
	return d
}

// Unscoped opts out of both DeleteHooks and DefaultFilters for this
// statement. On a soft-deleted table it forces a real, hard DELETE
// that bypasses the rewrite-to-UPDATE behaviour.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder {
	d.unscoped = true
	return d
}

// WriteSQL renders the DELETE. If the table has DeleteHooks and the
// caller has not opted out via Unscoped, hooks may replace the
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
	d.table.writeFrom(b)
	if len(d.using) > 0 {
		b.WriteString(" USING ")
		for j, t := range d.using {
			if j > 0 {
				b.WriteString(", ")
			}
			t.writeFrom(b)
		}
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(d.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", d.returning)
	}
}

// ToSQL renders the statement.
func (d *DeleteBuilder) ToSQL() (string, []any) {
	b := drops.NewBuilder()
	d.WriteSQL(b)
	return b.SQL()
}

// Exec runs the DELETE.
func (d *DeleteBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args := d.ToSQL()
	return d.db.Exec(ctx, sql, args...)
}

// All executes the DELETE and scans the RETURNING rows into dest.
func (d *DeleteBuilder) All(ctx context.Context, dest any) error {
	if len(d.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args := d.ToSQL()
	rows, err := d.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the DELETE and scans the first RETURNING row into dest.
func (d *DeleteBuilder) One(ctx context.Context, dest any) error {
	if len(d.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args := d.ToSQL()
	rows, err := d.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
