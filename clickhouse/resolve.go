package clickhouse

import (
	"context"

	"github.com/bernardoforcillo/drops"
)

// The resolver: the one place in this package that decides what a
// statement is, and the walk that reaches every statement written
// inside an expression.
//
// A table's automatic predicates come in two kinds and the difference
// is which half of the package can see them. A [Table.DefaultFilter] is
// fixed at declaration time, so WriteSQL can render it. A
// [Table.ContextFilter] cannot be built until a request is in hand, so
// it is resolved HERE, by the executors, and handed to the renderer
// already built. Everything in this file exists to make that second
// kind reach every statement drops composed rather than only the ones
// somebody remembered to build with it.
//
// The invariant, stated once so the rest of the package can refer to
// it: no expression the renderer reads may be invisible to the
// resolver. A list a builder renders and the resolver never walks is a
// place a caller can write a SELECT that goes out unscoped.

// ctxStatement is a statement that renders for one particular request:
// the two builders in this package, whose ToSQLCtx resolves the context
// filters of every table the statement names before rendering.
//
// It is satisfied structurally rather than declared, which is the point
// of stating it as an interface at all: a caller's own statement type
// that exposes the same method is rendered for its ctx on the same
// terms, instead of being rendered blind because it is not one of ours.
type ctxStatement interface {
	drops.Expression
	ToSQLCtx(ctx context.Context) (sql string, args []any, err error)
}

// ctxResolvable is a [ctxStatement] that can hand back the statement
// VALUE to render for a ctx, rather than only its rendered text. Both
// statement builders in this package implement it, and resolveExpr
// dispatches on it.
//
// It is an interface rather than a list of type names because a list of
// type names is precisely the thing that goes stale. A resolver that
// named *SelectBuilder and nothing else would be blind to
// *InsertBuilder — which satisfies drops.Expression exactly as it does,
// and is what a subquery operand or a CTE body may be typed as — so an
// INSERT written into an expression would render with no tenant stamped
// and refuse nothing on a ctx with no tenant. Dispatching on a method
// means the next builder is resolved on the day it is written.
//
// It carries a second method rather than resting on ToSQLCtx alone
// because a nested statement has to render INTO the statement around
// it: the finished text ToSQLCtx returns carries its own bound
// arguments, and splicing that into a statement that already has some
// would mean re-numbering placeholders by parsing SQL. resolveExpr
// therefore asks for the resolved statement itself and lets the
// surrounding Builder render it.
type ctxResolvable interface {
	ctxStatement
	resolveStatement(ctx context.Context) (drops.Expression, bool, error)
}

// subqueryResolver is implemented by the expressions this package wraps
// around another statement — which since [opExpr] is every operator,
// function and connective it builds. It is unexported on purpose: an
// expression a caller assembled themselves cannot implement it, which
// is the honest statement of what drops can and cannot reach into.
type subqueryResolver interface {
	resolveSubqueries(ctx context.Context) (drops.Expression, bool, error)
}

// resolveExpr resolves the statements reachable from e against ctx,
// returning the expression to render and whether it differs from e.
//
// It reports the change rather than leaving the caller to compare,
// because an Expression is not reliably comparable: the closures this
// package and its callers build are func values, and == on two of those
// panics at run time.
//
// Every arm is an interface. That is the invariant — this function is
// the one place that decides what a statement is, and it decides by
// what a value can DO, never by what it is called. A type assertion on
// a builder name anywhere else is a second copy of this decision that
// nobody will remember to update.
func resolveExpr(ctx context.Context, e drops.Expression) (drops.Expression, bool, error) {
	switch v := e.(type) {
	case nil:
		return nil, false, nil
	case ctxResolvable:
		return v.resolveStatement(ctx)
	case ctxStatement:
		// A statement drops did not build. Its ToSQLCtx is the only way
		// in, and its output cannot be spliced into the statement being
		// built — see ctxResolvable — so it is still rendered by its own
		// WriteSQL. Asking it for its ctx form anyway is what keeps the
		// nesting fail-closed: a filter that refuses for want of a
		// tenant reports there, and the whole statement is refused
		// rather than sent with a foreign statement inside it that
		// nobody scoped.
		if _, _, err := v.ToSQLCtx(ctx); err != nil {
			return nil, false, err
		}
		return e, false, nil
	case subqueryResolver:
		return v.resolveSubqueries(ctx)
	}
	return e, false, nil
}

// resolveExprs resolves every expression in list, returning a rebuilt
// slice — or nil when nothing needed resolving, so the caller can keep
// the slice it already had and render exactly what it always did.
func resolveExprs(ctx context.Context, list []drops.Expression) ([]drops.Expression, error) {
	var out []drops.Expression
	for i, e := range list {
		resolved, changed, err := resolveExpr(ctx, e)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if out == nil {
			out = append([]drops.Expression(nil), list...)
		}
		out[i] = resolved
	}
	return out, nil
}

// resolveRow resolves the statements reachable from one INSERT row's
// bindings, returning a rebuilt list — or nil when nothing needed
// resolving.
//
// A bound value is an operand position like any other, and on an INSERT
// it is the ONLY operand position there is: the statement has no WHERE
// clause and no ON clause, so a read that decides what gets written can
// only be written here.
// INSERT … VALUES ((SELECT id FROM accounts WHERE …)) used to render
// its body through WriteSQL and so read every tenant's accounts to
// compute a value stored in this tenant's row.
//
// Only a binding that holds an expression has anything to walk, which
// is what [exprBound] is: [ColumnValue] is closed to this package — its
// methods are unexported, so no caller can implement it — and every
// other implementation binds a Go value that becomes a parameter. The
// test is an interface rather than the name of one struct so that a
// binding kind added later is walked by having the methods, instead of
// by somebody remembering this line.
func resolveRow(ctx context.Context, row []ColumnValue) ([]ColumnValue, error) {
	var out []ColumnValue
	for i, s := range row {
		eb, ok := s.(exprBound)
		if !ok {
			continue
		}
		resolved, changed, err := resolveExpr(ctx, eb.boundExpr())
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		if out == nil {
			out = append([]ColumnValue(nil), row...)
		}
		// withBoundExpr copies for the reason the builders copy: a
		// caller may hold the binding and use it in a second statement,
		// and a resolved body written back into it would pin the first
		// request's tenant into every later use.
		out[i] = eb.withBoundExpr(resolved)
	}
	return out, nil
}

// exprBound is a [ColumnValue] whose value is an expression rather than
// a bound parameter, and which can hand back a copy of itself carrying
// a different one.
type exprBound interface {
	ColumnValue
	boundExpr() drops.Expression
	withBoundExpr(drops.Expression) ColumnValue
}

// mayHoldStatements reports whether any expression in list could
// resolve to something different — that is, whether walking it can do
// anything at all.
//
// It is resolveExpr's type switch asked in advance and without building
// anything, and every arm of it is therefore an INTERFACE, for the
// reason ctxResolvable gives. Written as a list of concrete type names
// it would go stale the same way the switch would, and the cost of
// going stale here is that a default filter holding a statement is
// never offered to the resolver at all.
//
// It exists so the near-universal case — a soft-delete guard, which
// holds no statement — does not pay for a ctx it has no use for. A
// default filter list is read once per statement per table.
func mayHoldStatements(list []drops.Expression) bool {
	for _, e := range list {
		switch e.(type) {
		case ctxStatement, subqueryResolver:
			return true
		}
	}
	return false
}

// renderForCtx renders e as the statement ctx names.
//
// Three cases, and they are three cases rather than one because they
// answer three different questions:
//
//   - a statement (any [ctxStatement], including a caller's own) is
//     asked for its ctx form;
//   - an expression with a statement inside it — a predicate, a
//     function call, any operand built on [opExpr] — is walked to that
//     statement and resolved there;
//   - anything genuinely opaque — a DDL helper, a [drops.Raw], a
//     caller's own [drops.ExprFunc] — has nothing drops can resolve.
//     It renders exactly as it did before, byte for byte, and stays the
//     caller's to scope.
func renderForCtx(ctx context.Context, e drops.Expression) (string, []any, error) {
	if st, ok := e.(ctxStatement); ok {
		return st.ToSQLCtx(ctx)
	}
	resolved, _, err := resolveExpr(ctx, e)
	if err != nil {
		return "", nil, err
	}
	sql, args := ToSQL(resolved)
	return sql, args, nil
}
