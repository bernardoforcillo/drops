package clickhouse

import "github.com/bernardoforcillo/drops"

// ColumnValue pairs a target column with the value or expression to
// bind for it in an INSERT row. Construct one with (*Col[T]).Val or
// (*Col[T]).Expr.
type ColumnValue interface {
	column() *Column
	writeValue(b *drops.Builder)
}

// valueBinding holds a typed Go value to bind as a parameter.
type valueBinding[T any] struct {
	col *Column
	val T
}

func (v *valueBinding[T]) column() *Column             { return v.col }
func (v *valueBinding[T]) writeValue(b *drops.Builder) { b.AddArg(v.val) }

// exprBinding holds an arbitrary SQL expression for the column.
type exprBinding struct {
	col  *Column
	expr drops.Expression
}

func (e *exprBinding) column() *Column             { return e.col }
func (e *exprBinding) writeValue(b *drops.Builder) { e.expr.WriteSQL(b) }

// Bind pairs a column with an untyped value.
//
// It is the type-erased counterpart of (*Col[T]).Val, for callers
// that hold a *Column and a value whose Go type is only known at
// runtime — a generic row copier, a CDC consumer, anything driving
// an INSERT from a map. Prefer Val wherever the type is known: it is
// what makes a mistyped binding a compile error rather than a driver
// error.
func Bind(col ColRef, v any) ColumnValue {
	return &valueBinding[any]{col: col.col(), val: v}
}

// boundExpr and withBoundExpr implement exprBound, so the resolver can
// walk to a statement written into a column's value and hand back a
// binding carrying the scoped copy.
//
// withBoundExpr copies rather than assigning through the receiver: a
// caller may hold the binding and use it in a second statement, and a
// resolved body written back into it would pin the first request's
// tenant into every later use.
func (e *exprBinding) boundExpr() drops.Expression { return e.expr }

func (e *exprBinding) withBoundExpr(next drops.Expression) ColumnValue {
	cp := *e
	cp.expr = next
	return &cp
}
