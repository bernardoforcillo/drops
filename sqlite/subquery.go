package sqlite

import "github.com/bernardoforcillo/drops"

// Subquery and predicate helpers. A SelectBuilder is a drops.Expression,
// so it can be passed directly as the subquery argument.
//
// A statement in expression position — EXISTS (<select>), a scalar
// (<select>), <value> IN (<select>) — is built from the same [opExpr]
// every operator in this package is built from, so it is walked by the
// same recursion and needs nothing of its own. That is what makes
// Not(Exists(sub)) carry the tenant axis that NotExists(sub) does: the
// distinction between "a helper that names a subquery" and "a helper
// whose operand happens to be one" has gone, and with it the boundary
// no caller could predict.
//
// A body the caller assembled by hand out of raw fragments is still
// theirs to scope: there is nothing to resolve inside a Raw.

// Exists renders EXISTS (<subquery>).
//
// When q is a statement drops built its body is scoped like the
// statement it is: the executor resolves the subquery's own tables'
// context filters — the tenant axis, an authz guard — before rendering,
// so an EXISTS over a scoped table restricts the same rows a bare
// SELECT from it would, and refuses the whole statement when they
// cannot be resolved.
func Exists(q drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"EXISTS (", ")"}, operands: []drops.Expression{q}}
}

// NotExists renders NOT EXISTS (<subquery>). The body is scoped exactly
// as [Exists]'s is.
func NotExists(q drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"NOT EXISTS (", ")"}, operands: []drops.Expression{q}}
}

// Subquery wraps an expression (typically a SELECT) in parentheses for
// use as a scalar subquery in a SELECT list or predicate. The body is
// scoped exactly as [Exists]'s is.
func Subquery(q drops.Expression) drops.Expression { return parens(q) }

// InSub renders "<value> IN (<subquery>)". The body is scoped exactly
// as [Exists]'s is.
func InSub(value any, sub drops.Expression) drops.Expression {
	return subMembership(value, "IN", sub)
}

// NotInSub renders "<value> NOT IN (<subquery>)". The body is scoped
// exactly as [Exists]'s is.
func NotInSub(value any, sub drops.Expression) drops.Expression {
	return subMembership(value, "NOT IN", sub)
}

func subMembership(value any, op string, sub drops.Expression) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " " + op + " (", "))"},
		operands: []drops.Expression{operandExpr(value), sub},
	}
}
