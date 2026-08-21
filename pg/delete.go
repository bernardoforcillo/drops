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
	scope     filterScope
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

// IsUnscoped reports whether the caller opted out of every default
// scope via Unscoped. A DeleteHook reads it to tell a hard DELETE from
// the soft one it would otherwise rewrite. IgnoreFilters does not set
// it: naming a filter drops a predicate, it does not cancel the
// rewrite that turns DELETE into UPDATE.
func (d *DeleteBuilder) IsUnscoped() bool { return d.scope.unscoped }

// DB returns the executing DB. Hooks that need to build a replacement
// statement (an UPDATE for soft-delete, for instance) use it.
func (d *DeleteBuilder) DB() *DB { return d.db }

// Using adds tables to a PostgreSQL DELETE ... USING clause for joins.
func (d *DeleteBuilder) Using(tables ...*Table) *DeleteBuilder {
	d.using = append(d.using, tables...)
	return d
}

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a filter that is only sometimes present can be passed straight in
// — but a DELETE all of whose predicates were nil is a DELETE with no
// WHERE, and removes every row the table's filters still admit.
func (d *DeleteBuilder) Where(preds ...drops.Expression) *DeleteBuilder {
	d.wheres = append(d.wheres, dropNilPreds(preds)...)
	return d
}

// Returning sets a RETURNING clause.
func (d *DeleteBuilder) Returning(cols ...drops.Expression) *DeleteBuilder {
	d.returning = append(d.returning, cols...)
	return d
}

// Unscoped opts out of both DeleteHooks and every global filter on the
// table. On a soft-deleted table it forces a real, hard DELETE that
// bypasses the rewrite-to-UPDATE behaviour — and, being the blunt
// instrument, drops the table's other scoping with it.
func (d *DeleteBuilder) Unscoped() *DeleteBuilder {
	d.scope.unscoped = true
	return d
}

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one in place — see [SelectBuilder.IgnoreFilters].
// It only drops predicates: the DeleteHooks stay, so on a soft-deleted
// table the statement is still rewritten into an UPDATE. Use Unscoped
// when you want the row gone for good.
func (d *DeleteBuilder) IgnoreFilters(names ...string) *DeleteBuilder {
	d.scope.ignore(names...)
	return d
}

// WriteSQL renders the DELETE. If the table has DeleteHooks and the
// caller has not opted out via Unscoped, hooks may replace the
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
func (d *DeleteBuilder) ToSQL() (sql string, args []any) {
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
