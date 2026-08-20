package pg

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// Subquery / predicate helpers.

// subExpr is a statement in expression position: EXISTS (<select>), a
// scalar (<select>), <value> = ANY (<select>), an aliased FROM source.
//
// It is a struct with the inner statement in a field rather than the
// closure these helpers used to return, and that is the whole of this
// file's part in the promise in tenant.go. A drops.ExprFunc is opaque:
// a *SelectBuilder captured inside one can only be rendered, and it is
// rendered by WriteSQL, which has no ctx and so writes the body's
// DefaultFilters and none of its context filters. EXISTS (SELECT ...
// FROM posts) then read every tenant's posts, with no error and through
// the exported API, while the outer statement carried its own tenant
// predicate. Keeping the body reachable lets the same recursion that
// scopes a CTE body — see resolveCTEs — reach a subquery body too.
//
// A body the caller assembled by hand out of raw fragments is still
// theirs to scope: there is nothing to resolve inside a Raw. That is
// said at each constructor rather than only here.
type subExpr struct {
	open  string           // text before the operand, e.g. "EXISTS ("
	left  drops.Expression // operand written before mid; nil when there is none
	mid   string           // text between left and inner, e.g. " = ANY ("
	inner drops.Expression // the subquery itself
	close string           // text after inner
	alias string           // when set, ` AS "<alias>"` follows close
}

// WriteSQL implements drops.Expression.
func (e *subExpr) WriteSQL(b *drops.Builder) {
	b.WriteString(e.open)
	if e.left != nil {
		b.Append(e.left)
		b.WriteString(e.mid)
	}
	b.Append(e.inner)
	b.WriteString(e.close)
	if e.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(e.alias)
	}
}

// resolveSubqueries implements subqueryResolver: it resolves the inner
// statement against ctx and returns a copy carrying the resolved body,
// or itself when there was nothing to resolve.
//
// The copy is what keeps the expression reusable, for the reason
// SelectBuilder.resolveCtx copies: an expression is a value a caller
// may hold and use in a second statement, and a resolved body written
// back into it would pin one request's tenant into every later
// statement that embeds it.
func (e *subExpr) resolveSubqueries(ctx context.Context) (drops.Expression, bool, error) {
	left, leftChanged, err := resolveExpr(ctx, e.left)
	if err != nil {
		return nil, false, err
	}
	inner, innerChanged, err := resolveExpr(ctx, e.inner)
	if err != nil {
		return nil, false, err
	}
	if !leftChanged && !innerChanged {
		return e, false, nil
	}
	cp := *e
	cp.left, cp.inner = left, inner
	return &cp, true, nil
}

// operandExpr adapts a value accepted as `any` to an Expression, so a
// helper that takes either can still keep it in a field. It is
// writeOperand's decision made once, at construction, instead of at
// every render.
func operandExpr(v any) drops.Expression {
	if e, ok := v.(drops.Expression); ok {
		return e
	}
	return drops.Param{Value: v}
}

// Exists renders EXISTS (<subquery>).
//
// When q is a *SelectBuilder its body is scoped like the statement it
// is: the executor resolves the subquery's own FROM table context
// filters — the tenant axis, an authz guard — before rendering, so an
// EXISTS over a scoped table restricts the same rows a bare SELECT from
// it would. A body built from raw fragments cannot be resolved and
// stays the caller's to scope.
func Exists(q drops.Expression) drops.Expression {
	return &subExpr{open: "EXISTS (", inner: q, close: ")"}
}

// NotExists renders NOT EXISTS (<subquery>). The body is scoped exactly
// as [Exists]'s is.
func NotExists(q drops.Expression) drops.Expression {
	return &subExpr{open: "NOT EXISTS (", inner: q, close: ")"}
}

// Subquery wraps an expression (typically a SELECT) in parentheses for
// use as a scalar subquery. The body is scoped exactly as [Exists]'s is.
func Subquery(q drops.Expression) drops.Expression {
	return &subExpr{open: "(", inner: q, close: ")"}
}

// AnySub renders <value> = ANY(<subquery>). Use a subquery expression
// (typically SelectBuilder) as the right-hand side. The body is scoped
// exactly as [Exists]'s is.
func AnySub(value, sub any) drops.Expression {
	return &subExpr{open: "(", left: operandExpr(value), mid: " = ANY (", inner: operandExpr(sub), close: "))"}
}

// AllSub renders <value> = ALL(<subquery>). The body is scoped exactly
// as [Exists]'s is.
func AllSub(value, sub any) drops.Expression {
	return &subExpr{open: "(", left: operandExpr(value), mid: " = ALL (", inner: operandExpr(sub), close: "))"}
}
