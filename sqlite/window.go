package sqlite

import "github.com/bernardoforcillo/drops"

// Window-function support. SQLite has supported window functions since
// 3.25 (2018); the OVER (...) syntax matches standard SQL, so these
// helpers mirror drops/pg's window.go exactly.

// Over wraps an aggregate or window function with an OVER clause.
//
//	sqlite.Over(sqlite.RowNumber(),
//	    sqlite.WindowSpec().PartitionBy(UserID).OrderBy(PostCreatedAt.Desc()))
func Over(fn drops.Expression, win *Window) drops.Expression {
	var o opBuilder
	o.operand(fn)
	o.text(" OVER (")
	win.build(&o)
	o.text(")")
	return o.done()
}

// Window describes the contents of an OVER (...) clause.
type Window struct {
	partition []drops.Expression
	order     []drops.Expression
	frame     string
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

// build appends the window spec's body to o.
//
// The partition and order lists are held as operands rather than
// rendered inside a closure, because a window key is an operand
// position a caller can hand a statement to:
// row_number() OVER (PARTITION BY (SELECT … FROM posts)) is a shape
// that renders, and while it lived in a drops.ExprFunc the statement in
// it carried none of the posts table's context filters. See opExpr.
func (w *Window) build(o *opBuilder) {
	first := true
	if len(w.partition) > 0 {
		o.text("PARTITION BY ")
		o.list(w.partition)
		first = false
	}
	if len(w.order) > 0 {
		if !first {
			o.text(" ")
		}
		o.text("ORDER BY ")
		o.list(w.order)
		first = false
	}
	if w.frame != "" {
		if !first {
			o.text(" ")
		}
		o.text(w.frame)
	}
}

// Window functions ----------------------------------------------------

// These take no operand at all, so there is nothing for a walk to
// reach: they are fixed text and stay fixed text.
func RowNumber() drops.Expression   { return drops.Raw("row_number()") }
func Rank() drops.Expression        { return drops.Raw("rank()") }
func DenseRank() drops.Expression   { return drops.Raw("dense_rank()") }
func PercentRank() drops.Expression { return drops.Raw("percent_rank()") }
func CumeDist() drops.Expression    { return drops.Raw("cume_dist()") }
func Ntile(n any) drops.Expression  { return funcCall("ntile", []any{n}) }

// Lag renders lag(expr [, offset [, default]]).
func Lag(expr any, args ...any) drops.Expression {
	return funcCall("lag", append([]any{expr}, args...))
}

// Lead renders lead(expr [, offset [, default]]).
func Lead(expr any, args ...any) drops.Expression {
	return funcCall("lead", append([]any{expr}, args...))
}

func FirstValue(expr any) drops.Expression { return funcCall("first_value", []any{expr}) }
func LastValue(expr any) drops.Expression  { return funcCall("last_value", []any{expr}) }
func NthValue(expr, n any) drops.Expression {
	return funcCall("nth_value", []any{expr, n})
}
