package mysql

import (
	"context"
	"reflect"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// Free-function operators, for building predicates over type-erased
// columns or arbitrary expressions. The typed methods on *Col[T] are
// the ones to prefer where the value type is known — they are what
// makes a mistyped comparison a compile error.

// binOp builds a parenthesised binary infix expression "(left OP right)".
//
// Both operands are held rather than closed over, so either may be a
// statement and is scoped as one: Eq(col, Subquery(sel)) is the
// ordinary spelling of a scalar-subquery comparison, and every
// automatic predicate this package builds — a tenant axis, an
// authorisation guard — is an Eq or an In over whatever the caller
// declared. A closure here was a guard resolveExpr cannot enter, so a
// statement inside it rendered with none of its own scoping.
func binOp(left any, op string, right any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " " + op + " ", ")"},
		operands: []drops.Expression{operandExpr(left), operandExpr(right)},
	}
}

func Eq(left, right any) drops.Expression  { return binOp(left, "=", right) }
func Ne(left, right any) drops.Expression  { return binOp(left, "<>", right) }
func Gt(left, right any) drops.Expression  { return binOp(left, ">", right) }
func Gte(left, right any) drops.Expression { return binOp(left, ">=", right) }
func Lt(left, right any) drops.Expression  { return binOp(left, "<", right) }
func Lte(left, right any) drops.Expression { return binOp(left, "<=", right) }

// Like is MySQL's LIKE. There is no ILike here on purpose: MySQL's
// comparison is case-insensitive whenever the column's collation is
// (which the common utf8mb4_0900_ai_ci and utf8mb4_general_ci both
// are), so case sensitivity is a property of the schema rather than of
// the operator. Force one way or the other with an explicit COLLATE.
func Like(left, pattern any) drops.Expression { return binOp(left, "LIKE", pattern) }

// And / Or combine predicates, ignoring the nil ones. With no
// arguments — or with nothing but nils — And renders TRUE and Or
// renders FALSE, the identity of each.
func And(preds ...drops.Expression) drops.Expression { return joinPreds(" AND ", "TRUE", preds) }
func Or(preds ...drops.Expression) drops.Expression  { return joinPreds(" OR ", "FALSE", preds) }

// dropNilPreds removes the nil entries. A nil predicate is how a
// conditional filter says "no restriction" — the shape every caller
// reaches for once a search box can be empty — so it has to mean
// nothing at all rather than a nil dereference or a dangling AND. The
// slice is only copied when there is something to drop, so the usual
// case allocates nothing.
func dropNilPreds(preds []drops.Expression) []drops.Expression {
	nils := 0
	for _, p := range preds {
		if p == nil {
			nils++
		}
	}
	if nils == 0 {
		return preds
	}
	kept := make([]drops.Expression, 0, len(preds)-nils)
	for _, p := range preds {
		if p != nil {
			kept = append(kept, p)
		}
	}
	return kept
}

// orTrue substitutes the empty conjunction for a nil predicate, for
// the places the grammar requires one — a join's ON, where omitting
// the expression is not an option the way omitting a WHERE is.
func orTrue(p drops.Expression) drops.Expression {
	if p == nil {
		return And()
	}
	return p
}

// joinPreds joins the predicates with sep, or renders empty when there
// are none.
//
// It is a node rather than a closure because a conjunction is where the
// automatic predicates END UP: a tenant axis AND-ed with a caller's
// WHERE clause, a guard AND-ed with both. A closure here holds every
// one of them where resolveExpr cannot walk to it, so a subquery in any
// of them renders unscoped.
func joinPreds(sep, empty string, preds []drops.Expression) drops.Expression {
	preds = dropNilPreds(preds)
	if len(preds) == 0 {
		return drops.Raw(empty)
	}
	if len(preds) == 1 {
		// One predicate is itself, not a conjunction of one: And over
		// a single term has always rendered the term, and a caller who
		// passes a statement gets it back with its own resolution
		// intact.
		return preds[0]
	}
	bracketed := make([]drops.Expression, len(preds))
	for i, p := range preds {
		bracketed[i] = bracketOperand(p)
	}
	return listOp("(", sep, ")", bracketed)
}

// Not negates a predicate. A nil predicate is the empty conjunction,
// so Not(nil) renders "(NOT TRUE)".
func Not(p drops.Expression) drops.Expression {
	return &opExpr{
		parts:    []string{"(NOT ", ")"},
		operands: []drops.Expression{bracketOperand(orTrue(p))},
	}
}

// In renders "left IN (…)". A lone slice argument is expanded, so
// In(col, ids) reads the same as In(col, ids...).
func In(left any, values ...any) drops.Expression {
	return inExpr(left, "IN", expandSlice(values))
}

// NotIn renders "left NOT IN (…)".
func NotIn(left any, values ...any) drops.Expression {
	return inExpr(left, "NOT IN", expandSlice(values))
}

func inExpr(left any, op string, values []any) drops.Expression {
	// MySQL rejects an empty IN list, so the empty case renders the
	// boolean the operator means: nothing is in the empty set,
	// everything is not in it.
	if len(values) == 0 {
		if op == "IN" {
			return drops.Raw("(FALSE)")
		}
		return drops.Raw("(TRUE)")
	}
	return &opExpr{
		parts: []string{"(", " " + op + " ", ")"},
		operands: []drops.Expression{
			operandExpr(left),
			listOp("(", ", ", ")", operandExprs(values)),
		},
	}
}

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

func IsNull(e any) drops.Expression {
	return &opExpr{parts: []string{"(", " IS NULL)"}, operands: []drops.Expression{operandExpr(e)}}
}

func IsNotNull(e any) drops.Expression {
	return &opExpr{parts: []string{"(", " IS NOT NULL)"}, operands: []drops.Expression{operandExpr(e)}}
}

// Between renders "left BETWEEN low AND high".
func Between(left, low, high any) drops.Expression {
	return &opExpr{
		parts: []string{"(", " BETWEEN ", " AND ", ")"},
		operands: []drops.Expression{
			operandExpr(left), operandExpr(low), operandExpr(high),
		},
	}
}

// Func renders an arbitrary function call — the escape hatch for
// anything the helpers do not cover.
func Func(name string, args ...any) drops.Expression {
	return funcExpr(name, operandExprs(args))
}

// Count / Sum / Avg / Min / Max are the aggregates worth naming.
func Count(e drops.Expression) drops.Expression { return Func("count", e) }
func CountAll() drops.Expression                { return drops.Raw("count(*)") }
func Sum(e drops.Expression) drops.Expression   { return Func("sum", e) }
func Avg(e drops.Expression) drops.Expression   { return Func("avg", e) }
func Min(e drops.Expression) drops.Expression   { return Func("min", e) }
func Max(e drops.Expression) drops.Expression   { return Func("max", e) }

// Now renders NOW(6), matching the microsecond precision Timestamp
// declares.
func Now() drops.Expression { return drops.Raw("NOW(6)") }

// --- Nodes -------------------------------------------------------------
//
// The composite expressions above used to be built out of closures: the
// operands were captured in a func literal, and a func literal is
// opaque. That is fine for an operand that can only ever be a value,
// and wrong for one a caller may hand a statement to — a subquery
// captured in a closure is a statement resolveExpr cannot see, so it
// renders through WriteSQL with no ctx and reads every tenant's rows to
// answer one tenant's question. See resolve.go for the invariant that
// names it.
//
// So they are nodes: literal SQL and operands in fields, laid out when
// the expression is BUILT rather than while it renders, which is what
// keeps the operands somewhere the resolver can walk to them.

// opExpr is literal SQL interleaved with operands: parts[0], then
// operands[0], then parts[1], and so on. A "(", " IN (", "))" with two
// operands is <left> IN (<right>), and both operands are reachable.
type opExpr struct {
	parts    []string
	operands []drops.Expression

	// alias, when set, renders "<expr> AS <alias>" after the parts.
	// [As] is the constructor that sets it, and an empty alias renders
	// no AS clause at all: `AS ""` is a zero-length delimited
	// identifier.
	alias string
}

// WriteSQL implements drops.Expression.
func (o *opExpr) WriteSQL(b *drops.Builder) {
	for i := 0; i < len(o.parts) || i < len(o.operands); i++ {
		if i < len(o.parts) {
			b.WriteString(o.parts[i])
		}
		if i < len(o.operands) {
			o.operands[i].WriteSQL(b)
		}
	}
	if o.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(o.alias)
	}
}

// resolveSubqueries implements subqueryResolver: it is the arm of
// resolveExpr that reaches a statement written into an operand.
//
// A change is returned as a new node rather than written back into this
// one, for the reason the builders copy: an expression is a value a
// caller may hold and use in a second statement, and resolving in place
// would pin the first request's ctx into every later use of it.
func (o *opExpr) resolveSubqueries(ctx context.Context) (drops.Expression, bool, error) {
	resolved, err := resolveExprs(ctx, o.operands)
	if err != nil {
		return nil, false, err
	}
	if resolved == nil {
		return o, false, nil
	}
	return &opExpr{parts: o.parts, operands: resolved, alias: o.alias}, true, nil
}

// operandExpr is the old writeOperand's node form: an Expression stands
// for itself, and anything else becomes a bound parameter. The value is
// returned rather than written, which is the whole difference — it can
// be held in a field.
func operandExpr(v any) drops.Expression {
	if e, ok := v.(drops.Expression); ok {
		return e
	}
	return drops.Param{Value: v}
}

// operandExprs is operandExpr over a list.
func operandExprs(values []any) []drops.Expression {
	if len(values) == 0 {
		return nil
	}
	out := make([]drops.Expression, len(values))
	for i, v := range values {
		out[i] = operandExpr(v)
	}
	return out
}

// listOp renders items between open and closing, separated by sep:
// listOp("(", ", ", ")", ...) is a tuple, and listOp("f(", ", ", ")",
// ...) is a call.
func listOp(open, sep, closing string, items []drops.Expression) drops.Expression {
	parts := make([]string, 0, len(items)+1)
	parts = append(parts, open)
	for i := 1; i < len(items); i++ {
		parts = append(parts, sep)
	}
	parts = append(parts, closing)
	return &opExpr{parts: parts, operands: items}
}

// funcExpr renders name(args...). Every "<name>(<args>)" helper in the
// package is built from it, so an argument that is a statement —
// coalesce((SELECT ...), 0) — is walked and scoped rather than rendered
// blind.
func funcExpr(name string, args []drops.Expression) drops.Expression {
	return listOp(name+"(", ", ", ")", args)
}

// parens wraps e in parentheses, holding it.
func parens(e drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"(", ")"}, operands: []drops.Expression{e}}
}

// suffixExpr renders e followed by literal SQL — " ASC", " IS NULL" —
// holding e.
func suffixExpr(e drops.Expression, suffix string) drops.Expression {
	return &opExpr{parts: []string{"", suffix}, operands: []drops.Expression{e}}
}

// opBuilder lays out an [opExpr] a piece at a time, for the expressions
// whose shape is not fixed: a CASE has a variable number of branches, a
// window may name any of several clauses. Deciding that text while
// rendering is exactly what would put the operands back inside a
// closure, so it is decided here instead and the result is still a
// node.
//
// Text accumulates until an operand arrives, which is what keeps the
// parts and the operands in step: parts[i] is everything written before
// operands[i].
type opBuilder struct {
	parts    []string
	operands []drops.Expression
	pending  strings.Builder
}

// text appends literal SQL.
func (o *opBuilder) text(s string) { o.pending.WriteString(s) }

// operand appends an expression as the next operand.
func (o *opBuilder) operand(e drops.Expression) {
	o.parts = append(o.parts, o.pending.String())
	o.pending.Reset()
	o.operands = append(o.operands, e)
}

// value appends v as the next operand, binding it as a parameter when
// it is not an expression.
func (o *opBuilder) value(v any) { o.operand(operandExpr(v)) }

// list appends items as one comma-separated run of operands — a
// PARTITION BY or ORDER BY list, where each key is an operand position
// in its own right.
func (o *opBuilder) list(items []drops.Expression) {
	for i, e := range items {
		if i > 0 {
			o.text(", ")
		}
		o.operand(e)
	}
}

// done returns the expression built so far. It hands back the node
// rather than the interface so a caller that has to finish it off — a
// JSON_TABLE that carries its own alias — can, without a second
// constructor.
func (o *opBuilder) done() *opExpr {
	parts := make([]string, len(o.parts), len(o.parts)+1)
	copy(parts, o.parts)
	return &opExpr{parts: append(parts, o.pending.String()), operands: o.operands}
}

// bracketOperand wraps e in parentheses when its rendering could
// re-associate with what is written beside it.
//
// A conjunction joins its operands with a bare " AND ", and SQL binds
// AND tighter than OR — so a caller's drops.Raw("a OR b") AND-ed with a
// tenant guard rendered "a OR b AND (tenantId = ?)", which is
// "a OR (b AND guard)": every row matching "a" came back, for every
// tenant. NOT was worse, binding tighter than either, so NOT over the
// same operand negated only its first term.
//
// What escapes is decided by rendering the operand and looking at the
// shape, not by its Go type: every predicate this package builds is
// already a bracketed term and renders unchanged, and everything a
// caller can hand in — a Raw, an expression of their own — is bracketed
// on the way in. The render is of the operand alone and its text is
// thrown away; only the shape is read.
func bracketOperand(e drops.Expression) drops.Expression {
	if e == nil || !escapesItsBrackets(e) {
		return e
	}
	return parens(e)
}

// escapesItsBrackets reports whether e's rendering can reach past
// itself: a boolean operator at depth zero, which re-associates with
// whatever is written beside it, or a comment or statement break, which
// swallows it.
//
// Depth zero is the whole question. "(a OR b)" is one term and renders
// unchanged; "a OR b" is two, and AND-ed with a guard it becomes
// "a OR (b AND guard)". An EXISTS or a comparison has no boolean
// operator of its own to re-associate with and is left exactly as it
// was, which is what keeps every statement this package already
// rendered rendering byte for byte.
func escapesItsBrackets(e drops.Expression) bool {
	sql, _ := drops.String(e)
	depth, quote := 0, byte(0)
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
		case ';':
			return true
		case '-':
			if depth == 0 && i+1 < len(sql) && sql[i+1] == '-' {
				return true
			}
		case '/':
			if depth == 0 && i+1 < len(sql) && sql[i+1] == '*' {
				return true
			}
		case ' ':
			if depth != 0 {
				continue
			}
			rest := sql[i:]
			for _, op := range []string{" or ", " and "} {
				if len(rest) >= len(op) && strings.EqualFold(rest[:len(op)], op) {
					return true
				}
			}
		}
	}
	// Quoting or bracketing this could not follow to the end: where
	// the term stops is then a question about escape conventions the
	// server settles and this does not, so it is bracketed. Being
	// wrong in this direction costs a pair of parentheses; being wrong
	// in the other costs the predicate beside it.
	return depth != 0 || quote != 0
}
