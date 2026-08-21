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

// exprValue is a ColumnValue that binds an expression rather than a Go
// value — the one kind of binding a statement can hide in, and so the
// one kind resolveSets has anything to walk.
//
// It is an interface for the reason [ctxResolvable] is one: resolveSets
// used to type-assert *exprBinding, and a named type in a resolution
// path is a list of one that nobody will remember to extend. A binding
// kind added later that holds a caller-supplied expression implements
// these two methods and is walked, or implements neither and is a
// parameter — there is no third answer where a statement renders
// unresolved because the walk had never heard of the type holding it.
type exprValue interface {
	ColumnValue
	boundExpr() drops.Expression
	withBoundExpr(e drops.Expression) ColumnValue
}

// exprBinding holds an arbitrary SQL expression for the column.
type exprBinding struct {
	col  *Column
	expr drops.Expression
}

func (e *exprBinding) column() *Column             { return e.col }
func (e *exprBinding) writeValue(b *drops.Builder) { e.expr.WriteSQL(b) }
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

// sqlDefault renders the literal token DEFAULT — used for omitted
// columns in INSERT batches and via (*Col[T]).SetDefault.
type sqlDefault struct{}

func (sqlDefault) WriteSQL(b *drops.Builder) { b.WriteString("DEFAULT") }
