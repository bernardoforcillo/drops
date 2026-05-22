package pg

import (
	"reflect"

	"github.com/bernardoforcillo/drops"
)

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

// Pattern matching ------------------------------------------------------

func Like(left, pattern any) drops.Expression  { return binOp(left, "LIKE", pattern) }
func ILike(left, pattern any) drops.Expression { return binOp(left, "ILIKE", pattern) }

// Logical connectives ---------------------------------------------------

// And joins the predicates with AND. With no arguments it renders TRUE.
func And(preds ...drops.Expression) drops.Expression {
	return joinPreds(" AND ", "TRUE", preds)
}

// Or joins the predicates with OR. With no arguments it renders FALSE.
func Or(preds ...drops.Expression) drops.Expression {
	return joinPreds(" OR ", "FALSE", preds)
}

func joinPreds(sep, empty string, preds []drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		if len(preds) == 0 {
			b.WriteString(empty)
			return
		}
		if len(preds) == 1 {
			preds[0].WriteSQL(b)
			return
		}
		b.WriteByte('(')
		for i, p := range preds {
			if i > 0 {
				b.WriteString(sep)
			}
			p.WriteSQL(b)
		}
		b.WriteByte(')')
	})
}

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
// In(col, []int{1, 2, 3}) is equivalent to In(col, 1, 2, 3).
func In(left any, values ...any) drops.Expression {
	values = expandSlice(values)
	return inExpr(left, "IN", values)
}

// NotIn renders "left NOT IN (...)".
func NotIn(left any, values ...any) drops.Expression {
	values = expandSlice(values)
	return inExpr(left, "NOT IN", values)
}

func inExpr(left any, op string, values []any) drops.Expression {
	// PostgreSQL disallows an empty IN/NOT IN list, so emit a static
	// boolean instead of `<col> IN ()`. The semantics match the operator:
	// nothing is "in" an empty set; everything is "not in" an empty set.
	if len(values) == 0 {
		if op == "IN" {
			return drops.Raw("(false)")
		}
		return drops.Raw("(true)")
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
