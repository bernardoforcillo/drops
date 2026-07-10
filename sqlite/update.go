package sqlite

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// UpdateBuilder builds an UPDATE statement. Create one via DB.Update.
type UpdateBuilder struct {
	db       *DB
	table    *Table
	sets     []ColumnValue
	wheres   []drops.Expression
	unscoped bool
}

// Set adds a column assignment binding a value.
func (u *UpdateBuilder) Set(vals ...ColumnValue) *UpdateBuilder {
	u.sets = append(u.sets, vals...)
	return u
}

// SetExpr assigns a raw SQL expression to col (e.g. CURRENT_TIMESTAMP,
// NULL, or "count + 1") rather than a bound value.
func (u *UpdateBuilder) SetExpr(col *Column, expr drops.Expression) *UpdateBuilder {
	u.sets = append(u.sets, exprValue{col: col, expr: expr})
	return u
}

// Unscoped opts out of the table's DefaultFilter predicates for this
// statement.
func (u *UpdateBuilder) Unscoped() *UpdateBuilder { u.unscoped = true; return u }

// Where AND-s the given predicates onto the statement.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, preds...)
	return u
}

// WriteSQL implements drops.Expression.
func (u *UpdateBuilder) WriteSQL(b *drops.Builder) {
	b.WriteString("UPDATE ")
	u.table.writeName(b)
	b.WriteString(" SET ")
	for i, cv := range u.sets {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteIdent(cv.column().Name())
		b.WriteString(" = ")
		cv.writeValue(b)
	}
	wheres := u.wheres
	if !u.unscoped && len(u.table.defaultFilters) > 0 {
		wheres = append(append([]drops.Expression(nil), u.table.defaultFilters...), u.wheres...)
	}
	if len(wheres) > 0 {
		b.WriteString(" WHERE ")
		b.AppendList(" AND ", wheres)
	}
}

// ToSQL renders the statement with SQLite placeholders.
func (u *UpdateBuilder) ToSQL() (sql string, args []any) { return ToSQL(u) }

// Exec runs the UPDATE.
func (u *UpdateBuilder) Exec(ctx context.Context) (drops.Result, error) {
	sql, args := u.ToSQL()
	return u.db.Exec(ctx, sql, args...)
}
