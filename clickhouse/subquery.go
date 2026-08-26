package clickhouse

import "github.com/bernardoforcillo/drops"

// Subquery and predicate helpers. A SelectBuilder is a drops.Expression,
// so it can be passed directly as the subquery argument.
//
// In / NotIn (the value-list operators) live in op.go; these are the
// subquery-oriented forms.

// Exists renders EXISTS (<subquery>).
//
// The operand is held rather than closed over, so the statement inside
// is scoped exactly as one written at the top level is — and, since
// [Not] holds its operand too, Not(Exists(sub)) is scoped exactly as
// NotExists(sub) is. See opExpr for why that pairing is the whole
// point.
func Exists(q drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"EXISTS (", ")"}, operands: []drops.Expression{q}}
}

// NotExists renders NOT EXISTS (<subquery>).
func NotExists(q drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"NOT EXISTS (", ")"}, operands: []drops.Expression{q}}
}

// Subquery wraps an expression (typically a SELECT) in parentheses for
// use as a scalar subquery.
func Subquery(q drops.Expression) drops.Expression { return parens(q) }

// InSub renders "<value> IN (<subquery>)".
func InSub(value any, sub drops.Expression) drops.Expression {
	return subMembership(value, "IN", sub)
}

// NotInSub renders "<value> NOT IN (<subquery>)".
func NotInSub(value any, sub drops.Expression) drops.Expression {
	return subMembership(value, "NOT IN", sub)
}

func subMembership(value any, op string, sub drops.Expression) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " " + op + " (", "))"},
		operands: []drops.Expression{operandExpr(value), sub},
	}
}
