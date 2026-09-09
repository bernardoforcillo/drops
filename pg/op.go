package pg

import (
	"context"
	"reflect"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// writeOperand writes v as either an existing Expression or a bound
// parameter. It is the bridge that lets every operator accept raw Go
// values alongside columns and other expressions.
func writeOperand(b *drops.Builder, v any) {
	if e, ok := v.(drops.Expression); ok {
		e.WriteSQL(b)
		return
	}
	b.AddArg(v)
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
func binOp(left any, op string, right any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteByte(' ')
		writeOperand(b, right)
		b.WriteByte(')')
	})
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

// And joins the predicates with AND, ignoring the nil ones. With no
// arguments — or with nothing but nils — it renders TRUE, the identity
// of a conjunction.
func And(preds ...drops.Expression) drops.Expression {
	return joinPreds(" AND ", "TRUE", preds)
}

// Or joins the predicates with OR, ignoring the nil ones. With no
// arguments — or with nothing but nils — it renders FALSE, the
// identity of a disjunction.
func Or(preds ...drops.Expression) drops.Expression {
	return joinPreds(" OR ", "FALSE", preds)
}

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

func joinPreds(sep, empty string, preds []drops.Expression) drops.Expression {
	preds = dropNilPreds(preds)
	return drops.ExprFunc(func(b *drops.Builder) {
		if len(preds) == 0 {
			b.WriteString(empty)
			return
		}
		if len(preds) == 1 {
			preds[0].WriteSQL(b)
			return
		}
		b.WriteByte('(')
		for i, p := range preds {
			if i > 0 {
				b.WriteString(sep)
			}
			p.WriteSQL(b)
		}
		b.WriteByte(')')
	})
}

// Not negates a predicate. A nil predicate is the empty conjunction,
// so Not(nil) renders "(NOT TRUE)".
func Not(p drops.Expression) drops.Expression {
	if p == nil {
		p = And()
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("(NOT ")
		p.WriteSQL(b)
		b.WriteByte(')')
	})
}

// Set membership -------------------------------------------------------

// In renders "left IN (...)". A single slice argument is expanded so
// In(col, []int{1, 2, 3}) is equivalent to In(col, 1, 2, 3).
func In(left any, values ...any) drops.Expression {
	values = expandSlice(values)
	return inExpr(left, "IN", values)
}

// NotIn renders "left NOT IN (...)".
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
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteByte(' ')
		b.WriteString(op)
		b.WriteString(" (")
		for i, v := range values {
			if i > 0 {
				b.WriteString(", ")
			}
			writeOperand(b, v)
		}
		b.WriteString("))")
	})
}

// Null tests -----------------------------------------------------------

func IsNull(e any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, e)
		b.WriteString(" IS NULL)")
	})
}

func IsNotNull(e any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, e)
		b.WriteString(" IS NOT NULL)")
	})
}

// Between renders "left BETWEEN low AND high".
func Between(left, low, high any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, left)
		b.WriteString(" BETWEEN ")
		writeOperand(b, low)
		b.WriteString(" AND ")
		writeOperand(b, high)
		b.WriteByte(')')
	})
}

// --- Nodes -------------------------------------------------------------
//
// Everything above builds its expression out of a closure: the operands
// are captured in a func literal, and a func literal is opaque. That is
// fine for an operand that can only ever be a value, and wrong for one
// a caller may hand a statement to — a subquery captured in a closure
// is a statement resolveExpr cannot see, so it renders through WriteSQL
// with no ctx and reads every tenant's rows to answer one tenant's
// question. See resolve.go for the invariant that names.
//
// So the composite expressions in this package are nodes: literal SQL
// and operands in fields, laid out when the expression is BUILT rather
// than while it renders, which is what keeps the operands somewhere the
// resolver can walk to them.

// opExpr is literal SQL interleaved with operands: parts[0], then
// operands[0], then parts[1], and so on. A "(", " = ANY(", "))" with
// two operands is <value> = ANY(<array>), and the two operands are
// reachable.
type opExpr struct {
	parts    []string
	operands []drops.Expression

	// alias, when set, renders "<expr> AS <alias>" after the parts.
	// [As] is the one constructor that sets it, and an empty alias
	// renders no AS clause at all: `AS ""` is a zero-length delimited
	// identifier PostgreSQL rejects outright.
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

// operandExpr is writeOperand's node form: an Expression stands for
// itself, and anything else becomes a bound parameter. The value is
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
// listOp("ARRAY[", ", ", "]", ...) is an array literal, and
// listOp("f(", ", ", ")", ...) is a call.
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
// array_agg((SELECT ...)), coalesce((SELECT ...), 0) — is walked and
// scoped rather than rendered blind.
func funcExpr(name string, args []any) drops.Expression {
	return listOp(name+"(", ", ", ")", operandExprs(args))
}

// parens wraps e in parentheses, holding it.
func parens(e drops.Expression) drops.Expression {
	return &opExpr{parts: []string{"(", ")"}, operands: []drops.Expression{e}}
}

// opBuilder lays out an [opExpr] a piece at a time, for the expressions
// whose shape is not fixed: a CASE has a variable number of branches, a
// substring may or may not carry its FOR, a window may name any of five
// clauses. Deciding that text while rendering is exactly what would put
// the operands back inside a closure, so it is decided here instead and
// the result is still a node.
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

// done returns the expression built so far.
func (o *opBuilder) done() drops.Expression {
	parts := make([]string, len(o.parts), len(o.parts)+1)
	copy(parts, o.parts)
	return &opExpr{parts: append(parts, o.pending.String()), operands: o.operands}
}
