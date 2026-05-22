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
