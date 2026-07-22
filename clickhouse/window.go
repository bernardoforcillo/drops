package clickhouse

import "github.com/bernardoforcillo/drops"

// Window-function support. ClickHouse has supported window functions
// with the standard OVER (...) syntax since v21.9. The frame-navigation
// functions differ from standard SQL: ClickHouse spells lag/lead as
// lagInFrame / leadInFrame, which the Lag / Lead helpers emit.

// Over wraps an aggregate or window function with an OVER clause.
func Over(fn drops.Expression, win *Window) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(fn)
		b.WriteString(" OVER (")
		win.writeBody(b)
		b.WriteByte(')')
	})
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

func (w *Window) writeBody(b *drops.Builder) {
	first := true
	if len(w.partition) > 0 {
		b.WriteString("PARTITION BY ")
		b.AppendList(", ", w.partition)
		first = false
	}
	if len(w.order) > 0 {
		if !first {
			b.WriteByte(' ')
		}
		b.WriteString("ORDER BY ")
		b.AppendList(", ", w.order)
		first = false
	}
	if w.frame != "" {
		if !first {
			b.WriteByte(' ')
		}
		b.WriteString(w.frame)
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
