package mysql

import (
	"context"
	"errors"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder composes an UPDATE statement.
type UpdateBuilder struct {
	db       *DB
	table    *Table
	sets     []ColumnValue
	wheres   []drops.Expression
	orderBys []drops.Expression
	limit    *int64
	unscoped bool
}

// Set appends column assignments.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, values...)
	return u
}

// SetExpr assigns a raw SQL expression to a column.
func (u *UpdateBuilder) SetExpr(col ColRef, e drops.Expression) *UpdateBuilder {
	u.sets = append(u.sets, exprValue{col: col.col(), expr: e})
	return u
}

// Where appends predicates joined by AND.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// OrderBy and Limit bound which rows an UPDATE touches — a MySQL
// extension with no PostgreSQL equivalent, and the safe way to update
// a large table in batches.
func (u *UpdateBuilder) OrderBy(exprs ...drops.Expression) *UpdateBuilder {
	u.orderBys = append(u.orderBys, exprs...)
	return u
}

func (u *UpdateBuilder) Limit(n int64) *UpdateBuilder { u.limit = &n; return u }

// Unscoped opts out of the table's DefaultFilter predicates.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder { u.unscoped = true; return u }

// ErrNoAssignments is returned when an UPDATE has nothing to set.
var ErrNoAssignments = errors.New("drops/mysql: UPDATE has no assignments")

// WriteSQL renders the UPDATE.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("UPDATE ")
	u.table.writeFrom(b)
	b.WriteString(" SET ")
	for i, s := range u.sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(s.column().name)
		b.WriteString(" = ")
		s.writeValue(b)
	}
	wheres := u.wheres
	if !u.unscoped && len(u.table.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), u.table.defaultFilters...), wheres...)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		writeAnd(b, wheres)
	}
	if len(u.orderBys) > 0 {
		b.WriteString(" ORDER BY ")
		b.AppendList(", ", u.orderBys)
	}
	if u.limit != nil {
		b.WriteString(" LIMIT ")
		b.AddArg(*u.limit)
	}
}

// ToSQL renders the statement and its arguments.
func (u *UpdateBuilder) ToSQL() (string, []any) { return render(u) }

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	if len(u.sets) == 0 {
		return nil, ErrNoAssignments
	}
	sql, args := u.ToSQL()
	return u.db.Exec(ctx, sql, args...)
}
