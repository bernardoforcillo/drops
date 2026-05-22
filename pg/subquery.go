package pg

import "github.com/bernardoforcillo/drops"

// Subquery / predicate helpers.

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
func Subquery(q drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		b.Append(q)
		b.WriteByte(')')
	})
}

// AnySub renders <value> = ANY(<subquery>). Use a subquery expression
// (typically SelectBuilder) as the right-hand side.
func AnySub(value, sub any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, value)
		b.WriteString(" = ANY (")
		writeOperand(b, sub)
		b.WriteString("))")
	})
}

// AllSub renders <value> = ALL(<subquery>).
func AllSub(value, sub any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, value)
		b.WriteString(" = ALL (")
		writeOperand(b, sub)
		b.WriteString("))")
	})
}
