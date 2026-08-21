package pg

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
// place a caller can write a SELECT that goes out unscoped, and that is
// how every one of the defects this design answers was reachable.
//
// This file is the reference the three ports are diffed against. It
// carries no PostgreSQL in it beyond the spellings in its examples, and
// that is the point: drops/sqlite, drops/mysql and drops/clickhouse
// each hold the same file, and a change made here that is not made
// there is a divergence somebody can see in a diff rather than one that
// has to be re-derived from the SQL.

// ctxStatement is a statement that renders for one particular request:
// the four builders in this package, whose ToSQLCtx resolves the
// context filters of every table the statement names before rendering.
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
// VALUE to render for a ctx, rather than only its rendered text. Every
// statement builder in this package implements it, and resolveExpr
// dispatches on it.
//
// It is an interface rather than a list of type names because a list of
// type names is the defect this round exists to end. resolveExpr used
// to name *SelectBuilder and nothing else, so the other three builders
// — which satisfy drops.Expression exactly as it does, and are what a
// CTE body or a subquery operand is typed as — were invisible to it:
// WITH moved AS (DELETE FROM ax_rows RETURNING name) rendered with no
// WHERE clause on a tenant-scoped table and refused nothing on a ctx
// with no tenant. A cross-tenant write, through the exported API,
// because the resolver's idea of what a statement is had gone one
// builder out of date. Dispatching on a method means the next builder
// is resolved on the day it is written or fails the check in
// TestEveryStatementBearingExpressionIsReachableByTheResolver.
//
// It carries a second method rather than resting on ToSQLCtx alone
// because a nested statement has to render INTO the statement around
// it: a Builder assigns placeholder numbers in write order, so the
// finished text ToSQLCtx returns — numbered from $1, quoted for its own
// dialect — cannot be spliced into a statement that already has
// arguments without renumbering it by parsing SQL. resolveExpr
// therefore asks for the resolved statement itself and lets the
// surrounding Builder render it.
//
// The signature is resolveSubqueries' — the resolved expression,
// whether it differs, and an error — rather than a predicate producer's
// (ctx) (drops.Expression, error), which it would otherwise be mistaken
// for by the census in TestEveryPredicateProducerIsCensused. Reporting
// the change is also what saves resolveExpr from comparing two
// Expressions itself; see there for why that is not safe in general.
type ctxResolvable interface {
	ctxStatement
	resolveStatement(ctx context.Context) (drops.Expression, bool, error)
}

// subqueryResolver is implemented by the expressions this package wraps
// around another statement. It is unexported on purpose: an expression
// a caller assembled themselves cannot implement it, which is the
// honest statement of what drops can and cannot reach into.
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
// nobody will remember to update; see
// TestNoResolutionEntryPointNamesAStatementType.
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
// the slice it already had.
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

// resolveSets resolves the statements reachable from a SET list,
// returning a rebuilt list — or nil when nothing needed resolving, so
// the caller keeps the list it already had.
//
// The assigned value is an operand position like any other, and the one
// that decides what gets written rather than which rows do:
// SET "ownerId" = (SELECT ... FROM "accounts") used to render its body
// through WriteSQL and so read every tenant's accounts to compute a
// value stored in this tenant's row. Handing the held expression to
// resolveExpr is what makes the whole tree under it resolve — a
// subquery three combinators down as much as one written directly.
//
// Only a binding that holds an expression has anything to walk, which
// is what [exprValue] is: [ColumnValue] is closed to this package — its
// methods are unexported, so no caller can implement it — and every
// other implementation binds a Go value that becomes a parameter. The
// test is the interface rather than the name *exprBinding so that a
// binding kind added later is walked by having the methods, instead of
// by somebody remembering this line.
func resolveSets(ctx context.Context, sets []ColumnValue) ([]ColumnValue, error) {
	var out []ColumnValue
	for i, s := range sets {
		eb, ok := s.(exprValue)
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
			out = append([]ColumnValue(nil), sets...)
		}
		// withBoundExpr copies for the reason the builder is copied: a
		// caller may hold the binding and use it in a second statement,
		// and a resolved body written back into it would pin the first
		// request's tenant into every later use.
		out[i] = eb.withBoundExpr(resolved)
	}
	return out, nil
}

// exprValue is a ColumnValue that binds an expression rather than a Go
// value — the one kind of binding a statement can hide in, and so the
// one kind resolveSets has anything to walk.
//
// It is an interface for the reason [ctxResolvable] is one: resolveSets
// used to type-assert *exprBinding, and a named type in a resolution
// path is a list of one that nobody will remember to extend. A binding
// kind added later that holds a caller-supplied expression implements
// these two methods and is walked, or implements neither and is a
// parameter — there is no third answer where a statement renders
// unresolved because the walk had never heard of the type holding it.
type exprValue interface {
	ColumnValue
	boundExpr() drops.Expression
	withBoundExpr(e drops.Expression) ColumnValue
}

// mayHoldStatements reports whether any expression in list could
// resolve to something different — that is, whether walking it can do
// anything at all.
//
// It is resolveExpr's type switch asked in advance and without
// building anything, and every arm of it is therefore an INTERFACE, for
// the reason [ctxResolvable] gives. Written as a list of type names it
// went stale the same way the switch itself did: it admitted
// *SelectBuilder and subqueryResolver and knew nothing of the
// ctxStatement arm, so a default filter whose predicate was a statement
// drops did not build was never offered to the resolver at all — and
// that arm is what keeps a foreign statement FAIL-CLOSED, by asking it
// for its ctx form so a filter refusing for want of a tenant refuses
// the statement around it. It rendered, and was sent, with the guard's
// inner statement carrying none of its context filters.
//
// ctxStatement covers every builder in this package, since
// ctxResolvable embeds it, and covers a caller's own statement type on
// the same terms. The two must agree: an arm resolveExpr learns to
// enter has to be admitted here as well, or the walk is skipped for the
// one shape it was extended to reach.
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

// renderForCtx renders e as the statement ctx names, and is where
// ExecExpr's promise is kept. Split out from ExecExpr so the three
// cases can be read as three cases, and they are three cases rather
// than one because they answer three different questions:
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
	// A SELECT can carry a deferred error — a cursor that failed to
	// decode — which Rows/All/One report and rendering does not. It is
	// checked in SelectBuilder.resolveCtx, which both branches below
	// reach, rather than here: this function used to assert
	// *SelectBuilder for it, and that assertion saw the statement handed
	// to ExecExpr and no statement inside it, so a corrupt cursor in a
	// CTE body still rendered — as the false predicate AfterCursor fails
	// closed with, which matches nothing and reports nothing.
	if st, ok := e.(ctxStatement); ok {
		return st.ToSQLCtx(ctx)
	}
	// Not a statement, but possibly an expression with one inside:
	// resolveExpr walks to any statement this package wrapped and
	// returns e unchanged when there is none, which is what keeps an
	// opaque expression rendering byte-identically.
	resolved, _, err := resolveExpr(ctx, e)
	if err != nil {
		return "", nil, err
	}
	sql, args := drops.String(resolved)
	return sql, args, nil
}
