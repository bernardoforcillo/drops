package sqlite

import (
	"context"
	"reflect"

	"github.com/bernardoforcillo/drops"
)

// Free-standing predicate and operator helpers for SQLite. They mirror
// drops/pg's op.go so query code ports between the two dialects, with
// the SQLite-flavoured differences folded in (no ILIKE keyword — LIKE is
// ASCII-case-insensitive by default; GLOB provides case-sensitive
// wildcard matching instead).
//
// Every operator accepts either a drops.Expression (a column or nested
// fragment) or a raw Go value, which is bound as a "?" parameter. The
// column-method forms in column.go (Col.Eq, Col.In, …) stay available;
// these package-level functions are the untyped counterparts used when
// composing expressions over heterogeneous operands.

// opExpr is an operator and its operands, with the operands held in a
// field rather than closed over in a drops.ExprFunc.
//
// It is this package's only expression node, and holding the operands
// is the whole of its part in the promise tenant.go makes: every
// statement that reads or writes a scoped table takes the tenant from
// ctx and carries the predicate.
//
// A closure cannot keep that promise. An operand a caller can hand a
// *SelectBuilder to — In's value list, Not's predicate, And's and Or's
// conjuncts, a comparison's right-hand side, BETWEEN's bounds, a
// function argument — was swallowed by a drops.ExprFunc, and a
// swallowed *SelectBuilder can only be rendered. Rendering is WriteSQL,
// which has no ctx, so the body wrote its DefaultFilters and none of
// its ContextFilters: In(col, db.Select().From(posts)) read every
// tenant's posts, with no error, through the exported API, while the
// outer statement carried its own tenant predicate. With no tenant on
// ctx at all it still rendered and still sent — a fail-open in the one
// feature sold as fail-closed.
//
// Doing this shape by shape is what drops/pg spent four rounds
// discovering it must not do: the boundary lands wherever somebody last
// looked, so NotExists(sub) was scoped and the identical
// Not(Exists(sub)) was not — a distinction no caller can predict.
// Holding the operands moves the boundary to where it belongs. Anything
// drops built is walked; anything the caller assembled out of raw
// fragments is not, and there is nothing inside a [drops.Raw] to walk.
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

	// alias appends ` AS "<alias>"` after the last part, for the
	// constructors that name a projection. It is a field rather than
	// text in parts because the quoting belongs to the Builder's
	// dialect: baked in at construction it would be quoted by whichever
	// dialect the constructor guessed, rather than by the one rendering.
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
// The recursion is the point. resolveExpr resolves a statement builder
// as the statement it is and hands anything else that implements this
// interface back to itself, so a tree of these nodes is walked to
// whatever depth the caller built it — And(Or(Not(Exists(sub))), …)
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
// of, and what a conjunction wraps a conjunct in when leaving it bare
// would let it reassociate its neighbours.
func parens(e drops.Expression) *opExpr {
	return &opExpr{parts: []string{"(", ")"}, operands: []drops.Expression{e}}
}

// listOp builds an opExpr for "<open> a <sep> b <sep> c <close>". With
// no operands there is nothing to separate, so the two fixed texts
// become one part and the node renders them alone — which is what keeps
// And() rendering "()" exactly as the closure it replaces did.
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
// helper that takes either can still keep it in a field. It takes the
// is-this-an-Expression-or-a-value decision once, at construction,
// instead of at every render — which is what lets the operand stay
// reachable to the resolver walk instead of living inside a closure.
//
// It is the replacement for the writeOperand this file used to carry,
// and the difference is exactly the one the round is about: that
// function made the decision while rendering, inside a closure, where
// no walk could follow it.
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

// opBuilder assembles the interleaved parts and operands of an opExpr
// whose text is not a fixed template — a CASE with any number of
// branches, a window spec with optional clauses.
//
// It exists so those shapes can hold their operands as cheaply as the
// fixed ones do. Without it the obvious way to write "CASE WHEN <c>
// THEN <v> ... END" is a drops.ExprFunc that decides the text while
// rendering, and that closure is exactly what hides a statement from
// the resolver walk — see opExpr for what the hidden statement then
// does.
type opBuilder struct {
	parts    []string
	operands []drops.Expression
	pending  string // literal text written since the last operand
}

// text appends literal SQL after the operands added so far.
func (o *opBuilder) text(s string) { o.pending += s }

// operand appends an operand, closing the literal text that precedes it.
func (o *opBuilder) operand(e drops.Expression) {
	o.parts = append(o.parts, o.pending)
	o.pending = ""
	o.operands = append(o.operands, e)
}

// value appends an operand given as `any`: an Expression is held as
// itself, anything else becomes a bound parameter. It is operandExpr's
// decision, taken here at construction instead of at render time.
func (o *opBuilder) value(v any) { o.operand(operandExpr(v)) }

// list appends a comma-separated run of operands.
func (o *opBuilder) list(exprs []drops.Expression) {
	for i, e := range exprs {
		if i > 0 {
			o.text(", ")
		}
		o.operand(e)
	}
}

// done closes the trailing text and returns the node. The invariant
// opExpr documents — one part per operand plus a trailing one — holds by
// construction, since every operand appended exactly one part.
func (o *opBuilder) done() *opExpr {
	return &opExpr{parts: append(o.parts, o.pending), operands: o.operands}
}

// funcExpr renders "<name>(<args>)" with its arguments held rather than
// closed over. Nearly every helper in funcs.go, strings.go, math.go,
// json.go, datetime.go and window.go is one call to it, so the operand
// position a caller hands a scalar subquery to is walkable in all of
// them at once.
func funcExpr(name string, args []drops.Expression) drops.Expression {
	return listOp(name+"(", ", ", ")", args)
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
// Subquery(sel)) and every operator in json.go are scoped by the same
// walk, without any of them naming a subquery.
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

// IsDistinctFrom / IsNotDistinctFrom render NULL-safe comparison via
// SQLite's IS / IS NOT operators (SQLite's IS treats NULLs as equal).
func IsDistinctFrom(left, right any) drops.Expression    { return binOp(left, "IS NOT", right) }
func IsNotDistinctFrom(left, right any) drops.Expression { return binOp(left, "IS", right) }

// Pattern matching ------------------------------------------------------

// Like renders "<left> LIKE <pattern>". SQLite's LIKE is
// case-insensitive for ASCII by default.
func Like(left, pattern any) drops.Expression { return binOp(left, "LIKE", pattern) }

// NotLike renders "<left> NOT LIKE <pattern>".
func NotLike(left, pattern any) drops.Expression { return binOp(left, "NOT LIKE", pattern) }

// LikeEscape renders "<left> LIKE <pattern> ESCAPE <esc>".
func LikeEscape(left, pattern, esc any) drops.Expression {
	return &opExpr{
		parts: []string{"(", " LIKE ", " ESCAPE ", ")"},
		operands: []drops.Expression{
			operandExpr(left), operandExpr(pattern), operandExpr(esc),
		},
	}
}

// Glob renders "<left> GLOB <pattern>" — SQLite's case-sensitive,
// Unix-shell-style wildcard match.
func Glob(left, pattern any) drops.Expression { return binOp(left, "GLOB", pattern) }

// Regexp renders "<left> REGEXP <pattern>". SQLite has no built-in
// REGEXP implementation; a user-defined function must be registered on
// the connection for this to execute.
func Regexp(left, pattern any) drops.Expression { return binOp(left, "REGEXP", pattern) }

// Logical connectives ---------------------------------------------------
//
// And / Or live in column.go. Not is defined here for parity with pg.

// Not negates a predicate.
//
// The operand is bracketed when it could otherwise reach past itself,
// because NOT binds tighter than every connective a caller can put
// inside it: Not(drops.Raw("a OR b")) rendered "(NOT a OR b)", which is
// "((NOT a) OR b)" — a predicate that is true for every row matching b,
// tenant guard or no tenant guard. See bracketConjunct.
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
	return inExpr(left, "IN", expandSlice(values))
}

// NotIn renders "left NOT IN (...)". Its values are scoped exactly as
// [In]'s are.
func NotIn(left any, values ...any) drops.Expression {
	return inExpr(left, "NOT IN", expandSlice(values))
}

func inExpr(left any, op string, values []any) drops.Expression {
	// An empty IN/NOT IN list would be a syntax error; emit a static
	// boolean whose semantics match the operator on the empty set:
	// nothing is "in" it, everything is "not in" it.
	if len(values) == 0 {
		if op == "IN" {
			return drops.Raw("(0)")
		}
		return drops.Raw("(1)")
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
		parts: []string{"(", " BETWEEN ", " AND ", ")"},
		operands: []drops.Expression{
			operandExpr(left), operandExpr(low), operandExpr(high),
		},
	}
}

// NotBetween renders "left NOT BETWEEN low AND high". Its bounds are
// held exactly as [Between]'s are.
func NotBetween(left, low, high any) drops.Expression {
	return &opExpr{
		parts: []string{"(", " NOT BETWEEN ", " AND ", ")"},
		operands: []drops.Expression{
			operandExpr(left), operandExpr(low), operandExpr(high),
		},
	}
}

// aliasExpr renders "<e> AS <alias>", holding e.
//
// It is what [Column.As] and the package-level [As] are made of, and it
// holds its operand for the same reason every other node does:
// As(Subquery(sel), "recent") in a projection is an ordinary
// scalar-subquery idiom, and a closure there hid the statement from the
// walk.
func aliasExpr(e drops.Expression, alias string) drops.Expression {
	return &opExpr{parts: []string{"", ""}, operands: []drops.Expression{e}, alias: alias}
}
