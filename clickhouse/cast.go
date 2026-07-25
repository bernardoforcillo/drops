package clickhouse

import "github.com/bernardoforcillo/drops"

// CastAs renders CAST(<e> AS <type>) — ClickHouse's standard-SQL type
// conversion. typeSQL is a ClickHouse type name such as "UInt32",
// "String" or "DateTime". ClickHouse also accepts the toType() function
// family (toInt32, toString, …); use Func for those.
func CastAs(e any, typeSQL string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CAST(")
		writeOperand(b, e)
		b.WriteString(" AS ")
		b.WriteString(typeSQL)
		b.WriteByte(')')
	})
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
func (c *CaseExpr) End() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CASE")
		if c.hasValue {
			b.WriteByte(' ')
			writeOperand(b, c.value)
		}
		for _, w := range c.whens {
			b.WriteString(" WHEN ")
			writeOperand(b, w.cond)
			b.WriteString(" THEN ")
			writeOperand(b, w.value)
		}
		if c.hasElse {
			b.WriteString(" ELSE ")
			writeOperand(b, c.elseValue)
		}
		b.WriteString(" END")
	})
}
