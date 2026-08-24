package clickhouse

import (
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// writeOperand writes v as either an existing Expression or a bound
// parameter. The single bridge that lets every operator accept raw Go
// values alongside columns and other expressions.
func writeOperand(b *drops.Builder, v any) {
	if e, ok := v.(drops.Expression); ok {
		e.WriteSQL(b)
		return
	}
	b.AddArg(v)
}

// expandSlice unwraps a single slice argument into its elements so
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

// binOp builds a parenthesised "(left OP right)" expression.
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

// Comparison ---------------------------------------------------------

func Eq(left, right any) drops.Expression  { return binOp(left, "=", right) }
func Ne(left, right any) drops.Expression  { return binOp(left, "!=", right) }
func Gt(left, right any) drops.Expression  { return binOp(left, ">", right) }
func Gte(left, right any) drops.Expression { return binOp(left, ">=", right) }
func Lt(left, right any) drops.Expression  { return binOp(left, "<", right) }
func Lte(left, right any) drops.Expression { return binOp(left, "<=", right) }

// Pattern matching.

func Like(left, pattern any) drops.Expression  { return binOp(left, "LIKE", pattern) }
func ILike(left, pattern any) drops.Expression { return binOp(left, "ILIKE", pattern) }

// Logical connectives -----------------------------------------------

// And / Or combine predicates, ignoring the nil ones. With no
// arguments — or with nothing but nils — And renders true and Or
// renders false, the identity of each.
func And(preds ...drops.Expression) drops.Expression { return joinPreds(" AND ", "true", preds) }
func Or(preds ...drops.Expression) drops.Expression  { return joinPreds(" OR ", "false", preds) }

// dropNilPreds removes the nil entries. A nil predicate is how a
// conditional filter says "no restriction" — the shape every caller
// reaches for once a search box can be empty — so it has to mean
// nothing at all rather than a nil dereference or a dangling AND. The
// slice is only copied when there is something to drop, so the usual
// case allocates nothing.
func dropNilPreds(preds []drops.Expression) []drops.Expression {
	nils := 0
	for _, p := range preds {
		if p == nil {
			nils++
		}
	}
	if nils == 0 {
		return preds
	}
	kept := make([]drops.Expression, 0, len(preds)-nils)
	for _, p := range preds {
		if p != nil {
			kept = append(kept, p)
		}
	}
	return kept
}

// orTrue substitutes the empty conjunction for a nil predicate, for
// the places the grammar requires one — a join's ON, where omitting
// the expression is not an option the way omitting a WHERE is.
func orTrue(p drops.Expression) drops.Expression {
	if p == nil {
		return And()
	}
	return p
}

func joinPreds(sep, empty string, preds []drops.Expression) drops.Expression {
	preds = dropNilPreds(preds)
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

// Not negates a predicate. A nil predicate is the empty conjunction,
// so Not(nil) renders "(NOT true)".
func Not(p drops.Expression) drops.Expression {
	p = orTrue(p)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("(NOT ")
		p.WriteSQL(b)
		b.WriteByte(')')
	})
}

// Set membership ----------------------------------------------------

func In(left any, values ...any) drops.Expression {
	return inExpr(left, "IN", expandSlice(values))
}

func NotIn(left any, values ...any) drops.Expression {
	return inExpr(left, "NOT IN", expandSlice(values))
}

func inExpr(left any, op string, values []any) drops.Expression {
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

// Null tests --------------------------------------------------------

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
