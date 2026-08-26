package clickhouse

import "github.com/bernardoforcillo/drops"

// Window-function support. ClickHouse has supported window functions
// with the standard OVER (...) syntax since v21.9. The frame-navigation
// functions differ from standard SQL: ClickHouse spells lag/lead as
// lagInFrame / leadInFrame, which the Lag / Lead helpers emit.

// Over wraps an aggregate or window function with an OVER clause.
//
// The function and every term of the window spec are held as operands
// rather than closed over, so a scalar subquery written as a PARTITION
// BY or ORDER BY term is reachable to the resolver. A window spec is an
// easy place to forget: it is not a predicate, so it does not look like
// somewhere a statement goes, and PARTITION BY (SELECT …) is perfectly
// legal.
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

// appendBody writes the window spec into an opBuilder, so each term
// stays an operand. It renders exactly the text the closure it replaces
// rendered.
func (w *Window) appendBody(o *opBuilder) {
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

func RowNumber() drops.Expression { return drops.Raw("row_number()") }
func Rank() drops.Expression      { return drops.Raw("rank()") }
func DenseRank() drops.Expression { return drops.Raw("dense_rank()") }

// Lag renders lagInFrame(expr [, offset [, default]]) — ClickHouse's
// spelling of the lag window function.
func Lag(expr any, args ...any) drops.Expression {
	return funcCall("lagInFrame", append([]any{expr}, args...))
}

// Lead renders leadInFrame(expr [, offset [, default]]).
func Lead(expr any, args ...any) drops.Expression {
	return funcCall("leadInFrame", append([]any{expr}, args...))
}

func FirstValue(expr any) drops.Expression { return funcCall("first_value", []any{expr}) }
func LastValue(expr any) drops.Expression  { return funcCall("last_value", []any{expr}) }
func NthValue(expr, n any) drops.Expression {
	return funcCall("nth_value", []any{expr, n})
}
