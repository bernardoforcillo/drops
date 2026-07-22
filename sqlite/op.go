package sqlite

import (
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Free-standing predicate and operator helpers for SQLite. They mirror
// drops/pg's op.go so query code ports between the two dialects, with
// the SQLite-flavoured differences folded in (no ILIKE keyword — LIKE is
// ASCII-case-insensitive by default; GLOB provides case-sensitive
// wildcard matching instead).
//
// Every operator accepts either a drops.Expression (a column or nested
// fragment) or a raw Go value, which is bound as a "?" parameter. The
// column-method forms in column.go (Col.Eq, Col.In, …) stay available;
// these package-level functions are the untyped counterparts used when
// composing expressions over heterogeneous operands.

// writeOperand writes v as either an existing Expression or a bound
// parameter. It is the bridge that lets every operator accept raw Go
// values alongside columns and other expressions.
func writeOperand(b *drops.Builder, v any) {
	if e, ok := v.(drops.Expression); ok {
		e.WriteSQL(b)
		return
	}
	b.AddArg(v)
}

// expandSlice unwraps a single slice argument into its elements so that
// In(col, []int{1,2,3}) works without manual spreading.
func expandSlice(values []any) []any {
	if len(values) != 1 {
		return values
	}
	if _, isExpr := values[0].(drops.Expression); isExpr {
		return values
	}
	rv := reflect.ValueOf(values[0])
	if !rv.IsValid() || rv.Kind() != reflect.Slice {
		return values
	}
	out := make([]any, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		out[i] = rv.Index(i).Interface()
	}
	return out
}

// binOp builds a parenthesised binary infix expression "(left OP right)".
func binOp(left any, op string, right any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteByte(' ')
		writeOperand(b, right)
		b.WriteByte(')')
	})
}

// Comparison operators ---------------------------------------------------

func Eq(left, right any) drops.Expression  { return binOp(left, "=", right) }
func Ne(left, right any) drops.Expression  { return binOp(left, "<>", right) }
func Gt(left, right any) drops.Expression  { return binOp(left, ">", right) }
func Gte(left, right any) drops.Expression { return binOp(left, ">=", right) }
func Lt(left, right any) drops.Expression  { return binOp(left, "<", right) }
func Lte(left, right any) drops.Expression { return binOp(left, "<=", right) }

// IsDistinctFrom / IsNotDistinctFrom render NULL-safe comparison via
// SQLite's IS / IS NOT operators (SQLite's IS treats NULLs as equal).
func IsDistinctFrom(left, right any) drops.Expression    { return binOp(left, "IS NOT", right) }
func IsNotDistinctFrom(left, right any) drops.Expression { return binOp(left, "IS", right) }

// Pattern matching ------------------------------------------------------

// Like renders "<left> LIKE <pattern>". SQLite's LIKE is
// case-insensitive for ASCII by default.
func Like(left, pattern any) drops.Expression { return binOp(left, "LIKE", pattern) }

// NotLike renders "<left> NOT LIKE <pattern>".
func NotLike(left, pattern any) drops.Expression { return binOp(left, "NOT LIKE", pattern) }

// LikeEscape renders "<left> LIKE <pattern> ESCAPE <esc>".
func LikeEscape(left, pattern, esc any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteString(" LIKE ")
		writeOperand(b, pattern)
		b.WriteString(" ESCAPE ")
		writeOperand(b, esc)
		b.WriteByte(')')
	})
}

// Glob renders "<left> GLOB <pattern>" — SQLite's case-sensitive,
// Unix-shell-style wildcard match.
func Glob(left, pattern any) drops.Expression { return binOp(left, "GLOB", pattern) }

// Regexp renders "<left> REGEXP <pattern>". SQLite has no built-in
// REGEXP implementation; a user-defined function must be registered on
// the connection for this to execute.
func Regexp(left, pattern any) drops.Expression { return binOp(left, "REGEXP", pattern) }

// Logical connectives ---------------------------------------------------
//
// And / Or live in column.go. Not is defined here for parity with pg.

// Not negates a predicate.
func Not(p drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("(NOT ")
		p.WriteSQL(b)
		b.WriteByte(')')
	})
}

// Set membership -------------------------------------------------------

// In renders "left IN (...)". A single slice argument is expanded so
// In(col, []int{1, 2, 3}) is equivalent to In(col, 1, 2, 3). A subquery
// Expression is rendered inline: In(col, subSelect).
func In(left any, values ...any) drops.Expression {
	return inExpr(left, "IN", expandSlice(values))
}

// NotIn renders "left NOT IN (...)".
func NotIn(left any, values ...any) drops.Expression {
	return inExpr(left, "NOT IN", expandSlice(values))
}

func inExpr(left any, op string, values []any) drops.Expression {
	// An empty IN/NOT IN list would be a syntax error; emit a static
	// boolean whose semantics match the operator on the empty set:
	// nothing is "in" it, everything is "not in" it.
	if len(values) == 0 {
		if op == "IN" {
			return drops.Raw("(0)")
		}
		return drops.Raw("(1)")
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteString(" (")
		for i, v := range values {
			if i > 0 {
				b.WriteString(", ")
			}
			writeOperand(b, v)
		}
		b.WriteString("))")
	})
}

// Null tests -----------------------------------------------------------

func IsNull(e any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, e)
		b.WriteString(" IS NULL)")
	})
}

func IsNotNull(e any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, e)
		b.WriteString(" IS NOT NULL)")
	})
}

// Between renders "left BETWEEN low AND high".
func Between(left, low, high any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteString(" BETWEEN ")
		writeOperand(b, low)
		b.WriteString(" AND ")
		writeOperand(b, high)
		b.WriteByte(')')
	})
}

// NotBetween renders "left NOT BETWEEN low AND high".
func NotBetween(left, low, high any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteString(" NOT BETWEEN ")
		writeOperand(b, low)
		b.WriteString(" AND ")
		writeOperand(b, high)
		b.WriteByte(')')
	})
}
