package mysql

import (
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Free-function operators, for building predicates over type-erased
// columns or arbitrary expressions. The typed methods on *Col[T] are
// the ones to prefer where the value type is known — they are what
// makes a mistyped comparison a compile error.

func writeOperand(b *drops.Builder, v any) {
	if e, ok := v.(drops.Expression); ok {
		e.WriteSQL(b)
		return
	}
	b.AddArg(v)
}

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

func Eq(left, right any) drops.Expression  { return binOp(left, "=", right) }
func Ne(left, right any) drops.Expression  { return binOp(left, "<>", right) }
func Gt(left, right any) drops.Expression  { return binOp(left, ">", right) }
func Gte(left, right any) drops.Expression { return binOp(left, ">=", right) }
func Lt(left, right any) drops.Expression  { return binOp(left, "<", right) }
func Lte(left, right any) drops.Expression { return binOp(left, "<=", right) }

// Like is MySQL's LIKE. There is no ILike here on purpose: MySQL's
// comparison is case-insensitive whenever the column's collation is
// (which the common utf8mb4_0900_ai_ci and utf8mb4_general_ci both
// are), so case sensitivity is a property of the schema rather than of
// the operator. Force one way or the other with an explicit COLLATE.
func Like(left, pattern any) drops.Expression { return binOp(left, "LIKE", pattern) }

// And / Or combine predicates, ignoring the nil ones. With no
// arguments — or with nothing but nils — And renders TRUE and Or
// renders FALSE, the identity of each.
func And(preds ...drops.Expression) drops.Expression { return joinPreds(" AND ", "TRUE", preds) }
func Or(preds ...drops.Expression) drops.Expression  { return joinPreds(" OR ", "FALSE", preds) }

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
// so Not(nil) renders "(NOT TRUE)".
func Not(p drops.Expression) drops.Expression {
	p = orTrue(p)
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("(NOT ")
		p.WriteSQL(b)
		b.WriteByte(')')
	})
}

// In renders "left IN (…)". A lone slice argument is expanded, so
// In(col, ids) reads the same as In(col, ids...).
func In(left any, values ...any) drops.Expression {
	return inExpr(left, "IN", expandSlice(values))
}

// NotIn renders "left NOT IN (…)".
func NotIn(left any, values ...any) drops.Expression {
	return inExpr(left, "NOT IN", expandSlice(values))
}

func inExpr(left any, op string, values []any) drops.Expression {
	// MySQL rejects an empty IN list, so the empty case renders the
	// boolean the operator means: nothing is in the empty set,
	// everything is not in it.
	if len(values) == 0 {
		if op == "IN" {
			return drops.Raw("(FALSE)")
		}
		return drops.Raw("(TRUE)")
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

// Func renders an arbitrary function call — the escape hatch for
// anything the helpers do not cover.
func Func(name string, args ...any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString(name)
		b.WriteByte('(')
		for i, a := range args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeOperand(b, a)
		}
		b.WriteByte(')')
	})
}

// Count / Sum / Avg / Min / Max are the aggregates worth naming.
func Count(e drops.Expression) drops.Expression { return Func("count", e) }
func CountAll() drops.Expression                { return drops.Raw("count(*)") }
func Sum(e drops.Expression) drops.Expression   { return Func("sum", e) }
func Avg(e drops.Expression) drops.Expression   { return Func("avg", e) }
func Min(e drops.Expression) drops.Expression   { return Func("min", e) }
func Max(e drops.Expression) drops.Expression   { return Func("max", e) }

// Now renders NOW(6), matching the microsecond precision Timestamp
// declares.
func Now() drops.Expression { return drops.Raw("NOW(6)") }
