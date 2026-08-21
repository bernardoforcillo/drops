package mysql

import "github.com/bernardoforcillo/drops"

// Window-function support, ported from drops/pg's window.go. Each
// helper returns a function expression; chain [Over] to attach a
// window specification.
//
// # Server support
//
// Window functions arrived in MySQL 8.0 and MariaDB 10.2. Older
// servers do not parse OVER at all, so there is nothing to fall back
// to and drops does not pretend otherwise — ask
// [ServerInfo.SupportsWindowFunctions] when the target version is in
// doubt.
//
// # Where the port stops
//
// PostgreSQL's three-argument lag(expr, offset, default) has no
// portable spelling. MySQL 8.0 takes it; MariaDB 10.11 rejects it with
// a syntax error at the third argument. [Lag] and [Lead] accept the
// offset and pass a default through if you give one, and their docs
// say what that costs. IFNULL around the two-argument call is the form
// that works on both.
//
// PostgreSQL's named WINDOW clause and its GROUPS / EXCLUDE frame
// options are likewise absent: MySQL has WINDOW but not EXCLUDE, and
// MariaDB's frame support stops at ROWS and RANGE.

// Over wraps a function with an OVER clause.
//
//	mysql.Over(mysql.RowNumber(),
//	    mysql.WindowSpec().PartitionBy(PostUserID).OrderBy(PostCreatedAt.Desc()))
//
// A nil window renders OVER (), the whole result set as one partition,
// which is what both servers mean by an empty specification.
func Over(fn drops.Expression, win *Window) drops.Expression {
	var o opBuilder
	o.operand(fn)
	o.text(" OVER (")
	if win != nil {
		win.appendBody(&o)
	}
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
//
// Only ROWS and RANGE frames are portable. MySQL also accepts GROUPS
// and MariaDB does not, and neither takes PostgreSQL's EXCLUDE clause.
func (w *Window) Frame(spec string) *Window { w.frame = spec; return w }

// appendBody writes the window specification into the node being built,
// holding the PARTITION BY and ORDER BY expressions as operands.
//
// They are operands rather than text because a partition key is a place
// a caller can write a scalar subquery — PARTITION BY (SELECT …) is
// ordinary — and an expression closed over in a drops.ExprFunc is one
// the resolver cannot walk. The frame is a raw string the caller wrote,
// with nothing inside it for drops to resolve.
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

// Ntile renders ntile(<n>).
func Ntile(n any) drops.Expression { return funcCall("ntile", []any{n}) }

// Lag renders lag(<expr> [, <offset> [, <default>]]).
//
// The two-argument form is portable. The third argument — the value
// for rows with no predecessor — is MySQL 8.0 only: MariaDB 10.11
// answers a syntax error at it. Wrap the two-argument call in
// [IfNull] for a default that works on both.
func Lag(expr any, args ...any) drops.Expression {
	return funcCall("lag", append([]any{expr}, args...))
}

// Lead renders lead(<expr> [, <offset> [, <default>]]). Same caveat as
// [Lag] about the third argument.
func Lead(expr any, args ...any) drops.Expression {
	return funcCall("lead", append([]any{expr}, args...))
}

// FirstValue / LastValue / NthValue read a value from a position in
// the frame.
//
// LastValue in particular is only useful with an explicit [Window.Frame]:
// the default frame ends at the current row, so without one it answers
// the current row's own value — on both servers, and for the same
// standard reason, but it surprises people on each of them separately.
func FirstValue(expr any) drops.Expression { return funcCall("first_value", []any{expr}) }
func LastValue(expr any) drops.Expression  { return funcCall("last_value", []any{expr}) }

func NthValue(expr, n any) drops.Expression { return funcCall("nth_value", []any{expr, n}) }

// SupportsWindowFunctions reports whether the server parses an OVER
// clause: MySQL 8.0 and MariaDB 10.2 onwards.
func (s ServerInfo) SupportsWindowFunctions() bool {
	if s.MariaDB {
		return s.AtLeast(10, 2, 0)
	}
	return s.AtLeast(8, 0, 0)
}
