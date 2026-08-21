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
	scope    filterScope
}

// Set appends column assignments.
//
// Each assignment is restated against the handle this builder's table
// hands out for the column it names. The left-hand side of a SET is
// written bare and never needed it, but a [PatchOp] names its column
// on the right as well — "SET age = age + ?" — and qualified, so an op
// built from another handle on the same table would name a relation
// the UPDATE does not: MySQL answers 1054 whichever of the two
// handles is the odd one out. An assignment naming a column of some
// *other* table is left alone, being a deliberate cross-table
// reference rather than a second handle.
func (u *UpdateBuilder) Set(values ...ColumnValue) *UpdateBuilder {
	for _, v := range values {
		u.sets = append(u.sets, rebindValue(u.table, v))
	}
	return u
}

// SetExpr assigns a raw SQL expression to a column. The expression is
// the caller's and is emitted as given — build it from the same handle
// the statement's table uses.
func (u *UpdateBuilder) SetExpr(col ColRef, e drops.Expression) *UpdateBuilder {
	return u.Set(exprValue{col: col.col(), expr: e})
}

// Where appends predicates joined by AND. Nil predicates are ignored,
// so a filter that is only sometimes present can be passed straight in
// — but an UPDATE all of whose predicates were nil is an UPDATE with no
// WHERE, and rewrites every row the table's filters still admit.
func (u *UpdateBuilder) Where(preds ...drops.Expression) *UpdateBuilder {
	u.wheres = append(u.wheres, dropNilPreds(preds)...)
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

// Unscoped opts out of every global filter on the table — the blunt
// instrument; see [SelectBuilder.Unscoped].
func (u *UpdateBuilder) Unscoped() *UpdateBuilder { u.scope.unscoped = true; return u }

// IgnoreFilters bypasses the named global filters on the table and
// leaves every other one standing — see [SelectBuilder.IgnoreFilters].
func (u *UpdateBuilder) IgnoreFilters(names ...string) *UpdateBuilder {
	u.scope.ignore(names...)
	return u
}

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
	wheres := u.scope.apply(u.table, u.wheres)
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
