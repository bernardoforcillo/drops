package pg

import "github.com/bernardoforcillo/drops"

// Window-function support. Each helper returns an aggregate-style
// expression; chain Over(...) to attach a window specification.

// Over wraps an aggregate or window function with an OVER clause.
//
//	pg.Over(pg.RowNumber(),
//	    pg.WindowSpec().PartitionBy(UserID).OrderBy(PostCreatedAt.Desc()))
//
// The function and every term of the window specification are operands
// and are held rather than closed over, so a statement in any of them
// is scoped as one: row_number() OVER (PARTITION BY (SELECT ... FROM
// posts)) carries posts' context filters, where the whole clause used
// to render through WriteSQL — which has no ctx — and partition this
// tenant's rows by every tenant's.
//
// The spec is flattened here, when the expression is built, rather than
// by a body the renderer walks into: a *Window is mutable up to that
// point, and an operand only reachable through a closure is an operand
// no walk can resolve.
func Over(fn drops.Expression, win *Window) drops.Expression {
	var o opBuilder
	o.operand(fn)
	o.text(" OVER (")
	win.appendBody(&o)
	o.text(")")
	return o.done()
}

// Window describes the contents of an OVER (...) clause.
type Window struct {
	partition []drops.Expression
	order     []drops.Expression
	frame     string // raw frame spec like "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW"
}

// WindowSpec begins building a window specification.
func WindowSpec() *Window { return &Window{} }

// PartitionBy adds PARTITION BY columns.
func (w *Window) PartitionBy(exprs ...drops.Expression) *Window {
	w.partition = append(w.partition, exprs...)
	return w
}

// OrderBy adds ORDER BY entries to the window.
func (w *Window) OrderBy(exprs ...drops.Expression) *Window {
	w.order = append(w.order, exprs...)
	return w
}

// Frame sets the raw frame specification — e.g.
// "ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING".
func (w *Window) Frame(spec string) *Window { w.frame = spec; return w }

// appendBody writes the body of the OVER (...) clause into o. The frame
// is raw SQL the caller supplied as a string, so there is nothing in it
// to hold; the partition and order terms are expressions and are held.
func (w *Window) appendBody(o *opBuilder) {
	first := true
	if len(w.partition) > 0 {
		o.text("PARTITION BY ")
		appendSeparated(o, w.partition)
		first = false
	}
	if len(w.order) > 0 {
		if !first {
			o.text(" ")
		}
		o.text("ORDER BY ")
		appendSeparated(o, w.order)
		first = false
	}
	if w.frame != "" {
		if !first {
			o.text(" ")
		}
		o.text(w.frame)
	}
}

// appendSeparated appends a comma-separated list of operands.
func appendSeparated(o *opBuilder, exprs []drops.Expression) {
	for i, e := range exprs {
		if i > 0 {
			o.text(", ")
		}
		o.operand(e)
	}
}

// Window functions ----------------------------------------------------

func RowNumber() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("row_number()") })
}
func Rank() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("rank()") })
}
func DenseRank() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("dense_rank()") })
}
func PercentRank() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("percent_rank()") })
}
func CumeDist() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("cume_dist()") })
}
func Ntile(n any) drops.Expression { return funcExpr("ntile", []any{n}) }

// Lag renders lag(expr [, offset [, default]]).
func Lag(expr any, args ...any) drops.Expression {
	return funcExpr("lag", append([]any{expr}, args...))
}

// Lead renders lead(expr [, offset [, default]]).
func Lead(expr any, args ...any) drops.Expression {
	return funcExpr("lead", append([]any{expr}, args...))
}

func FirstValue(expr any) drops.Expression { return funcExpr("first_value", []any{expr}) }
func LastValue(expr any) drops.Expression  { return funcExpr("last_value", []any{expr}) }
func NthValue(expr, n any) drops.Expression {
	return funcExpr("nth_value", []any{expr, n})
}
