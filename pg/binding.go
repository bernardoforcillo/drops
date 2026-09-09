package pg

import "github.com/bernardoforcillo/drops"

// ColumnValue pairs a target column with the value or expression to write
// for it in an INSERT row or UPDATE SET assignment. Construct one via
// (*Col[T]).Val, (*Col[T]).Expr, or (*Col[T]).SetDefault.
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

// boundExpr and withBoundExpr implement [exprValue], which lives in
// resolve.go beside the walk that uses it: this is the one binding
// kind in the package whose value is an expression, so it is the one
// the resolver has anything to walk into. See resolveSets.
func (e *exprBinding) boundExpr() drops.Expression { return e.expr }

// withBoundExpr returns a copy carrying x, never this binding with x
// written into it: a caller may hold the binding and use it in a second
// statement, and a resolved body stored back would pin the first
// request's tenant into every later use.
func (e *exprBinding) withBoundExpr(x drops.Expression) ColumnValue {
	cp := *e
	cp.expr = x
	return &cp
}

// insertBinding binds value to col as a parameter — the shape a
// binding takes when drops supplies the value rather than the caller,
// which today is the ctx tenant an INSERT stamps onto its axis.
//
// It is a valueBinding rather than an exprBinding so that
// classifyBinding reads it back as a literal: the tenant stamp is
// compared with the ctx tenant on every later pass over the row, and a
// binding that renders as an opaque expression is one the comparison
// has to refuse.
func insertBinding(col *Column, value any) ColumnValue {
	return &valueBinding[any]{col: col, val: value}
}

// sqlDefault renders the literal token DEFAULT — used for omitted
// columns in INSERT batches and via (*Col[T]).SetDefault.
type sqlDefault struct{}

func (sqlDefault) WriteSQL(b *drops.Builder) { b.WriteString("DEFAULT") }
