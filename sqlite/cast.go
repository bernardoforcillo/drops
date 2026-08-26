package sqlite

import "github.com/bernardoforcillo/drops"

// CastAs renders CAST(<e> AS <type>) — the standard-SQL type conversion.
// SQLite has no "::type" shorthand; the CAST form is the only spelling.
// typeSQL is a SQLite type name such as "INTEGER", "REAL" or "TEXT".
func CastAs(e any, typeSQL string) drops.Expression {
	return &opExpr{
		parts:    []string{"CAST(", " AS " + typeSQL + ")"},
		operands: []drops.Expression{operandExpr(e)},
	}
}

// Cast is an alias for CastAs, provided for source-level parity with
// drops/pg (whose Cast renders the PostgreSQL "::type" shorthand).
func Cast(e any, typeSQL string) drops.Expression { return CastAs(e, typeSQL) }

// Case begins a searched CASE expression. Chain When / Else / End:
//
//	sqlite.Case().
//	    When(UserAge.Lt(18), "minor").
//	    When(UserAge.Lt(65), "adult").
//	    Else("senior").
//	    End()
func Case() *CaseExpr { return &CaseExpr{} }

// CaseOn begins a simple CASE expression on a value:
//
//	sqlite.CaseOn(UserStatus).
//	    When("active", 1).
//	    When("pending", 2).
//	    Else(0).
//	    End()
func CaseOn(value any) *CaseExpr {
	return &CaseExpr{value: value, hasValue: true}
}

// CaseExpr is the in-progress CASE expression.
type CaseExpr struct {
	value     any
	hasValue  bool
	whens     []caseWhen
	elseValue any
	hasElse   bool
}

type caseWhen struct {
	cond  any // predicate (searched) or value (simple)
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
// Every branch is an operand held in the node rather than closed over,
// so a condition or a result may be a statement and is scoped as one:
// CASE WHEN EXISTS (SELECT … FROM posts) THEN … is a shape a caller
// writes, and a closure here rendered that EXISTS with none of the
// posts table's context filters. See opExpr, and opBuilder for why a
// variable-length shape needs a builder to stay a held node.
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
