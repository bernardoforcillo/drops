package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder composes an UPDATE statement.
type UpdateBuilder struct {
	db        *DB
	table     *Table
	sets      []ColumnValue
	from      []*Table
	wheres    []drops.Expression
	returning []drops.Expression
}

// Set adds one or more assignments. Use (*Col[T]).Val(v) to bind a typed
// value or (*Col[T]).Expr(e) to bind an expression.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, values...)
	return u
}

// From adds tables to a PostgreSQL UPDATE ... FROM clause for joins.
func (u *UpdateBuilder) From(tables ...*Table) *UpdateBuilder {
	u.from = append(u.from, tables...)
	return u
}

// Where appends predicates joined by AND.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// Returning sets a RETURNING clause.
func (u *UpdateBuilder) Returning(cols ...drops.Expression) *UpdateBuilder {
	u.returning = append(u.returning, cols...)
	return u
}

// WriteSQL renders the UPDATE.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("UPDATE ")
	u.table.writeFrom(b)
	b.WriteString(" SET ")
	for j, s := range u.sets {
		if j > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(s.column().Name())
		b.WriteString(" = ")
		s.writeValue(b)
	}
	if len(u.from) > 0 {
		b.WriteString(" FROM ")
		for j, t := range u.from {
			if j > 0 {
				b.WriteString(", ")
			}
			t.writeFrom(b)
		}
	}
	if len(u.wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, u.wheres)
	}
	if len(u.returning) > 0 {
		b.WriteString(" RETURNING ")
		b.AppendList(", ", u.returning)
	}
}

// ToSQL renders the statement.
func (u *UpdateBuilder) ToSQL() (string, []any) {
	b := drops.NewBuilder()
	u.WriteSQL(b)
	return b.SQL()
}

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(u.sets) == 0 {
		return nil, ErrNoUpdateAssignments
	}
	sql, args := u.ToSQL()
	return u.db.Exec(ctx, sql, args...)
}

// All executes the UPDATE and scans the RETURNING rows into dest.
func (u *UpdateBuilder) All(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args := u.ToSQL()
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanAll(rows, dest)
}

// One executes the UPDATE and scans the first RETURNING row into dest.
func (u *UpdateBuilder) One(ctx context.Context, dest any) error {
	if len(u.returning) == 0 {
		return ErrReturningRequired
	}
	sql, args := u.ToSQL()
	rows, err := u.db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	return scanOne(rows, dest)
}
