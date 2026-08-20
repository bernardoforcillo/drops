package pg

import (
	"context"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// opExpr is an operator and its operands, with the operands held in a
// field rather than closed over in a drops.ExprFunc.
//
// It is this package's only expression node, and holding the operands
// is the whole of its part in the promise in tenant.go: every statement
// that reads or writes a scoped table takes the tenant from ctx and
// carries the predicate.
//
// A closure cannot keep that promise. An operand a caller can hand a
// *SelectBuilder to — In's value list, Not's predicate, And's and Or's
// conjuncts, a comparison's right-hand side, BETWEEN's bounds, an
// array operator's array — was swallowed by a drops.ExprFunc, and a
// swallowed *SelectBuilder can only be rendered. Rendering is WriteSQL,
// which has no ctx, so the body wrote its DefaultFilters and none of
// its ContextFilters: In(col, db.Select().From(posts)) read every
// tenant's posts, with no error, through the exported API, while the
// outer statement carried its own tenant predicate. With no tenant on
// ctx at all it still rendered and still sent — a fail-open in the one
// feature sold as fail-closed.
//
// Rounds 1 to 3 fixed that shape by shape, and each list was one
// structural layer shallower than the defect: NotExists(sub) was scoped
// and the identical Not(Exists(sub)) was not, AnySub(col, sub) was and
// Eq(col, Subquery(sub)) was not. The boundary was where someone had
// looked, which no caller can predict. Holding the operands instead
// moves the boundary to where it belongs — anything drops built is
// walked, anything the caller assembled out of raw fragments is not,
// and there is nothing inside a drops.Raw to walk.
//
// The layout is one text part per operand plus a trailing one, so
// len(parts) == len(operands)+1 and rendering interleaves them:
// parts[0] operands[0] parts[1] ... operands[n-1] parts[n]. Eq is
// {"(", " = ", ")"}; In is {"(", " IN (", ", ", ..., "))"}; Exists is
// {"EXISTS (", ")"}. Constructors in this package build both slices
// together and nothing else may reach them, so the invariant holds by
// construction.
type opExpr struct {
	parts    []string
	operands []drops.Expression

	// alias appends ` AS "<alias>"` after the last part, for the one
	// caller that needs it — SelectBuilder.AsSubquery.
	alias string
}

// WriteSQL implements drops.Expression.
func (e *opExpr) WriteSQL(b *drops.Builder) {
	for i, o := range e.operands {
		b.WriteString(e.parts[i])
		b.Append(o)
	}
	b.WriteString(e.parts[len(e.operands)])
	if e.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(e.alias)
	}
}

// resolveSubqueries implements subqueryResolver: it resolves every
// operand against ctx and returns a copy carrying the resolved ones, or
// itself when there was nothing to resolve.
//
// The recursion is the point. resolveExpr resolves a *SelectBuilder as
// the statement it is and hands anything else that implements this
// interface back to itself, so a tree of these nodes is walked to
// whatever depth the caller built it — And(Or(Not(Exists(sub))), ...)
// included, and every combination nobody has written down yet.
//
// The copy is what keeps the expression reusable, for the reason
// SelectBuilder.resolveCtx copies: a predicate is a value a caller may
// build once and use in a second statement, possibly for a second
// request, and a resolved body written back into it would pin the first
// request's tenant into every later statement that embeds it.
func (e *opExpr) resolveSubqueries(ctx context.Context) (drops.Expression, bool, error) {
	resolved, err := resolveExprs(ctx, e.operands)
	if err != nil {
		return nil, false, err
	}
	if resolved == nil {
		return e, false, nil
	}
	cp := *e
	cp.operands = resolved
	return &cp, true, nil
}

// parens wraps e in a pair of parentheses. It is what Subquery is made
// of, and what And, Or and Not wrap an operand in when leaving it bare
// would let it reassociate its neighbours.
func parens(e drops.Expression) *opExpr {
	return &opExpr{parts: []string{"(", ")"}, operands: []drops.Expression{e}}
}

// listOp builds an opExpr for "<open> a <sep> b <sep> c <close>". With
// no operands there is nothing to separate, so the two fixed texts
// become one part and the node renders them alone.
func listOp(open, sep, close string, operands []drops.Expression) *opExpr {
	if len(operands) == 0 {
		return &opExpr{parts: []string{open + close}}
	}
	parts := make([]string, len(operands)+1)
	parts[0] = open
	for i := 1; i < len(operands); i++ {
		parts[i] = sep
	}
	parts[len(operands)] = close
	return &opExpr{parts: parts, operands: operands}
}

// operandExpr adapts a value accepted as `any` to an Expression, so a
// helper that takes either can still keep it in a field. It is
// writeOperand's decision made once, at construction, instead of at
// every render — which is what lets the operand stay reachable to the
// resolver walk instead of living inside a closure.
func operandExpr(v any) drops.Expression {
	if e, ok := v.(drops.Expression); ok {
		return e
	}
	return drops.Param{Value: v}
}

// operandExprs adapts a whole argument list. The slice is new, so a
// caller's variadic backing array cannot be aliased into a node that
// outlives the call.
func operandExprs(values []any) []drops.Expression {
	out := make([]drops.Expression, len(values))
	for i, v := range values {
		out[i] = operandExpr(v)
	}
	return out
}

// writeOperand writes v as either an existing Expression or a bound
// parameter, at render time.
//
// It is the older half of the bridge operandExpr now replaces, kept for
// the helpers in this package that still build a drops.ExprFunc —
// funcCall in strings.go and the ones in cast.go, datetime.go and
// ddl.go. Every one of those is an operand position a caller can hand a
// *SelectBuilder to, and every one of them therefore has the leak
// described on opExpr: the statement is reachable only through a
// closure, so the resolver walk stops at it and the body renders
// without its context filters. Converting them is a matter of building
// an opExpr — see funcExpr, which renders byte-identically to funcCall
// and holds its arguments.
func writeOperand(b *drops.Builder, v any) {
	if e, ok := v.(drops.Expression); ok {
		e.WriteSQL(b)
		return
	}
	b.AddArg(v)
}

// funcExpr renders "<name>(<args>)" with its arguments held rather than
// closed over. It is funcCall's resolvable twin: identical output, so a
// helper can move across without changing one byte of rendered SQL.
func funcExpr(name string, args []any) drops.Expression {
	return listOp(name+"(", ", ", ")", operandExprs(args))
}

// expandSlice unwraps a single slice argument into its elements so that
// In(col, []int{1,2,3}) works without manual spreading.
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

// binOp builds a parenthesised binary infix expression "(left OP right)".
//
// Both operands are held, so either may be a statement: Eq(col,
// Subquery(sel)) and ArrayContains(col, Subquery(sel)) and every
// operator in json.go are scoped by the same walk, without any of them
// naming a subquery.
func binOp(left any, op string, right any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " " + op + " ", ")"},
		operands: []drops.Expression{operandExpr(left), operandExpr(right)},
	}
}

// Comparison operators ---------------------------------------------------

func Eq(left, right any) drops.Expression  { return binOp(left, "=", right) }
func Ne(left, right any) drops.Expression  { return binOp(left, "<>", right) }
func Gt(left, right any) drops.Expression  { return binOp(left, ">", right) }
func Gte(left, right any) drops.Expression { return binOp(left, ">=", right) }
func Lt(left, right any) drops.Expression  { return binOp(left, "<", right) }
func Lte(left, right any) drops.Expression { return binOp(left, "<=", right) }

// Pattern matching ------------------------------------------------------

func Like(left, pattern any) drops.Expression  { return binOp(left, "LIKE", pattern) }
func ILike(left, pattern any) drops.Expression { return binOp(left, "ILIKE", pattern) }

// Logical connectives ---------------------------------------------------

// And joins the predicates with AND. With no arguments it renders TRUE.
//
// A predicate that does not parenthesise itself is parenthesised here
// when leaving it bare would let it reassociate its neighbours — see
// bracketConjunct, which is writeAnd's rule for a WHERE clause applied
// to a connective's own operands. A conjunct that holds a statement
// keeps its own scoping: And is a walkable node, so a subquery under it
// is resolved for the executing ctx like one written on its own.
func And(preds ...drops.Expression) drops.Expression {
	return joinPreds(" AND ", "TRUE", preds)
}

// Or joins the predicates with OR. With no arguments it renders FALSE.
// Its operands are bracketed and walked exactly as [And]'s are.
func Or(preds ...drops.Expression) drops.Expression {
	return joinPreds(" OR ", "FALSE", preds)
}

func joinPreds(sep, empty string, preds []drops.Expression) drops.Expression {
	// Nothing to join, and nothing to reassociate: the identity of the
	// connective, and the caller's lone predicate as they wrote it.
	if len(preds) == 0 {
		return drops.Raw(empty)
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return listOp("(", sep, ")", bracketConjuncts(preds))
}

// Not negates a predicate.
//
// The operand is parenthesised when it could otherwise reach past
// itself, because NOT binds tighter than every connective a caller can
// put inside it: Not(drops.Raw("a OR b")) rendered "(NOT a OR b)",
// which is "((NOT a) OR b)" — a predicate that is true for every row
// matching b, tenant guard or no tenant guard.
func Not(p drops.Expression) drops.Expression {
	return &opExpr{
		parts:    []string{"(NOT ", ")"},
		operands: []drops.Expression{bracketConjunct(p)},
	}
}

// Set membership -------------------------------------------------------

// In renders "left IN (...)". A single slice argument is expanded so
// In(col, []int{1, 2, 3}) is equivalent to In(col, 1, 2, 3).
//
// A single *SelectBuilder is a subquery rather than a list of values,
// and is scoped as the statement it is: In(col, db.Select().From(t))
// renders t's context filters into the inner WHERE clause, and refuses
// the whole statement when they cannot be resolved.
func In(left any, values ...any) drops.Expression {
	values = expandSlice(values)
	return inExpr(left, "IN", values)
}

// NotIn renders "left NOT IN (...)". Its values are scoped exactly as
// [In]'s are.
func NotIn(left any, values ...any) drops.Expression {
	values = expandSlice(values)
	return inExpr(left, "NOT IN", values)
}

func inExpr(left any, op string, values []any) drops.Expression {
	// PostgreSQL disallows an empty IN/NOT IN list, so emit a static
	// boolean instead of `<col> IN ()`. The semantics match the operator:
	// nothing is "in" an empty set; everything is "not in" an empty set.
	if len(values) == 0 {
		if op == "IN" {
			return drops.Raw("(false)")
		}
		return drops.Raw("(true)")
	}
	// One part before the left operand, one opening the list, one comma
	// per further value, and the two closing parentheses.
	parts := make([]string, len(values)+2)
	parts[0] = "("
	parts[1] = " " + op + " ("
	for i := 2; i <= len(values); i++ {
		parts[i] = ", "
	}
	parts[len(values)+1] = "))"
	return &opExpr{
		parts:    parts,
		operands: append([]drops.Expression{operandExpr(left)}, operandExprs(values)...),
	}
}

// Null tests -----------------------------------------------------------

func IsNull(e any) drops.Expression {
	return &opExpr{parts: []string{"(", " IS NULL)"}, operands: []drops.Expression{operandExpr(e)}}
}

func IsNotNull(e any) drops.Expression {
	return &opExpr{parts: []string{"(", " IS NOT NULL)"}, operands: []drops.Expression{operandExpr(e)}}
}

// Between renders "left BETWEEN low AND high". Each of the three
// operands is held, so a bound may be a scalar subquery and is scoped
// as one.
func Between(left, low, high any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " BETWEEN ", " AND ", ")"},
		operands: []drops.Expression{operandExpr(left), operandExpr(low), operandExpr(high)},
	}
}
