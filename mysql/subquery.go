package mysql

import "github.com/bernardoforcillo/drops"

// Subquery and quantified-comparison helpers, ported from drops/pg's
// subquery.go.
//
// MySQL takes the same EXISTS / scalar-subquery / ANY / ALL grammar
// PostgreSQL does. The one place the port cannot follow is that
// PostgreSQL's ANY also accepts an *array* on the right — "id = ANY
// ($1::int[])" — which is how pg code passes a slice as one bind. MySQL
// has no array type, so [AnySub] and [AllSub] take a subquery and only
// a subquery; the slice case is [In], which expands to a placeholder
// list.

// Exists renders EXISTS (<subquery>).
func Exists(q drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("EXISTS (")
		b.Append(q)
		b.WriteByte(')')
	})
}

// NotExists renders NOT EXISTS (<subquery>).
func NotExists(q drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("NOT EXISTS (")
		b.Append(q)
		b.WriteByte(')')
	})
}

// Subquery wraps an expression (typically a SELECT) in parentheses for
// use as a scalar subquery.
//
// MySQL raises error 1242, "Subquery returns more than 1 row", at
// execution time if it does not stay scalar — the planner does not
// know in advance, so this is a runtime failure rather than a parse
// one. Bound it with LIMIT 1 unless the shape guarantees a single row.
func Subquery(q drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		b.Append(q)
		b.WriteByte(')')
	})
}

// AnySub renders <value> = ANY (<subquery>) — true when the value
// equals at least one row the subquery returns.
func AnySub(value, sub any) drops.Expression { return quantified(value, "=", "ANY", sub) }

// AllSub renders <value> = ALL (<subquery>).
func AllSub(value, sub any) drops.Expression { return quantified(value, "=", "ALL", sub) }

// CmpAny renders <value> <op> ANY (<subquery>) for an arbitrary
// comparison operator — "> ANY" being "greater than the smallest".
// PostgreSQL spells this the same way; it is broken out here because
// MySQL's ANY has no array form to fall back on.
func CmpAny(value any, op string, sub any) drops.Expression {
	return quantified(value, op, "ANY", sub)
}

// CmpAll renders <value> <op> ALL (<subquery>).
func CmpAll(value any, op string, sub any) drops.Expression {
	return quantified(value, op, "ALL", sub)
}

func quantified(value any, op, quant string, sub any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, value)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteByte(' ')
		b.WriteString(quant)
		b.WriteString(" (")
		writeOperand(b, sub)
		b.WriteString("))")
	})
}

// InSub renders <value> IN (<subquery>). It is spelled apart from [In]
// because In expands a Go slice into placeholders, and a subquery must
// not be wrapped in a second set of parentheses.
func InSub(value, sub any) drops.Expression { return inSub(value, "IN", sub) }

// NotInSub renders <value> NOT IN (<subquery>).
//
// Worth remembering before reaching for it: NOT IN over a subquery
// that can produce NULL is never true. That is standard three-valued
// logic rather than a MySQL quirk, but MySQL will not warn about it.
// NOT EXISTS is the form that behaves the way people expect.
func NotInSub(value, sub any) drops.Expression { return inSub(value, "NOT IN", sub) }

func inSub(value any, op string, sub any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, value)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteString(" (")
		writeOperand(b, sub)
		b.WriteString("))")
	})
}
