package clickhouse

import "github.com/bernardoforcillo/drops"

// CastAs renders CAST(<e> AS <type>) — ClickHouse's standard-SQL type
// conversion. typeSQL is a ClickHouse type name such as "UInt32",
// "String" or "DateTime". ClickHouse also accepts the toType() function
// family (toInt32, toString, …); use Func for those.
func CastAs(e any, typeSQL string) drops.Expression {
	return &opExpr{
		parts:    []string{"CAST(", " AS " + typeSQL + ")"},
		operands: []drops.Expression{operandExpr(e)},
	}
}

// Cast is an alias for CastAs, provided for source-level parity with the
// other dialects.
func Cast(e any, typeSQL string) drops.Expression { return CastAs(e, typeSQL) }

// Case begins a searched CASE expression. Chain When / Else / End.
// ClickHouse supports the standard CASE form (and multiIf as a
// function-style alternative).
func Case() *CaseExpr { return &CaseExpr{} }

// CaseOn begins a simple CASE expression on a value.
func CaseOn(value any) *CaseExpr { return &CaseExpr{value: value, hasValue: true} }

// CaseExpr is the in-progress CASE expression.
type CaseExpr struct {
	value     any
	hasValue  bool
	whens     []caseWhen
	elseValue any
	hasElse   bool
}

type caseWhen struct {
	cond  any
	value any
}

// When adds a WHEN <cond> THEN <value> branch.
func (c *CaseExpr) When(cond, value any) *CaseExpr {
	c.whens = append(c.whens, caseWhen{cond: cond, value: value})
	return c
}

// Else sets the ELSE value.
func (c *CaseExpr) Else(value any) *CaseExpr { c.elseValue = value; c.hasElse = true; return c }

// End finalises the CASE expression.
//
// It assembles a node holding every branch rather than a closure that
// decides the text while rendering, so a subquery written as a
// condition, a THEN value or the ELSE is reachable to the resolver —
// see opBuilder, which exists for exactly the shapes whose text is not
// a fixed template.
func (c *CaseExpr) End() drops.Expression {
	var o opBuilder
	o.text("CASE")
	if c.hasValue {
		o.text(" ")
		o.value(c.value)
	}
	for _, w := range c.whens {
		o.text(" WHEN ")
		o.value(w.cond)
		o.text(" THEN ")
		o.value(w.value)
	}
	if c.hasElse {
		o.text(" ELSE ")
		o.value(c.elseValue)
	}
	o.text(" END")
	return o.done()
}
