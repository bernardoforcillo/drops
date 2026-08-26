package pg

import (
	"github.com/bernardoforcillo/drops"
)

// Subquery / predicate helpers.
//
// A statement in expression position — EXISTS (<select>), a scalar
// (<select>), <value> = ANY (<select>) — is built from the same opExpr
// every operator in this package is built from, so it is walked by the
// same recursion and needs nothing of its own. Round 3 gave these five
// constructors a struct of their own for exactly this reason and the
// rest of the package kept its closures; there is now one node type and
// the distinction between "a helper that names a subquery" and "a
// helper whose operand happens to be one" has gone, which is what makes
// Not(Exists(sub)) and Eq(col, Subquery(sub)) carry the tenant axis
// that NotExists(sub) and AnySub(col, sub) always did. See opExpr for
// the failure mode.
//
// A body the caller assembled by hand out of raw fragments is still
// theirs to scope: there is nothing to resolve inside a Raw. That is
// said at each constructor rather than only here.

// Exists renders EXISTS (<subquery>).
//
// When q is a *SelectBuilder its body is scoped like the statement it
// is: the executor resolves the subquery's own FROM table context
// filters — the tenant axis, an authz guard — before rendering, so an
// EXISTS over a scoped table restricts the same rows a bare SELECT from
// it would. A body built from raw fragments cannot be resolved and
// stays the caller's to scope.
func Exists(q drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"EXISTS (", ")"}, operands: []drops.Expression{q}}
}

// NotExists renders NOT EXISTS (<subquery>). The body is scoped exactly
// as [Exists]'s is.
func NotExists(q drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"NOT EXISTS (", ")"}, operands: []drops.Expression{q}}
}

// Subquery wraps an expression (typically a SELECT) in parentheses for
// use as a scalar subquery. The body is scoped exactly as [Exists]'s is.
//
// It is also the wrapper to reach for when embedding a SELECT as an
// operand: [SelectBuilder.WriteSQL] renders the statement bare, so
// Eq(col, sel) is a syntax error and Eq(col, Subquery(sel)) is not.
func Subquery(q drops.Expression) drops.Expression {
	return parens(q)
}

// AnySub renders <value> = ANY(<subquery>). Use a subquery expression
// (typically SelectBuilder) as the right-hand side. The body is scoped
// exactly as [Exists]'s is.
func AnySub(value, sub any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " = ANY (", "))"},
		operands: []drops.Expression{operandExpr(value), operandExpr(sub)},
	}
}

// AllSub renders <value> = ALL(<subquery>). The body is scoped exactly
// as [Exists]'s is.
func AllSub(value, sub any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " = ALL (", "))"},
		operands: []drops.Expression{operandExpr(value), operandExpr(sub)},
	}
}
