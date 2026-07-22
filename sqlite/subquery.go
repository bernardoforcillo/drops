package sqlite

import "github.com/bernardoforcillo/drops"

// Subquery and predicate helpers. A SelectBuilder is a drops.Expression,
// so it can be passed directly as the subquery argument.

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
// use as a scalar subquery in a SELECT list or predicate.
func Subquery(q drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		b.Append(q)
		b.WriteByte(')')
	})
}

// InSub renders "<value> IN (<subquery>)".
func InSub(value any, sub drops.Expression) drops.Expression {
	return subMembership(value, "IN", sub)
}

// NotInSub renders "<value> NOT IN (<subquery>)".
func NotInSub(value any, sub drops.Expression) drops.Expression {
	return subMembership(value, "NOT IN", sub)
}

func subMembership(value any, op string, sub drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, value)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteString(" (")
		b.Append(sub)
		b.WriteString("))")
	})
}
