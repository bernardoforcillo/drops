package pg

import "github.com/bernardoforcillo/drops"

// Cast renders <e>::<type> — the PostgreSQL shorthand for explicit type
// conversion. Equivalent to CAST(<e> AS <type>).
//
// e is held rather than closed over, so Cast(Subquery(sel), "bigint")
// is scoped like the statement it is; typeSQL is raw SQL naming a type,
// which no statement can appear in.
func Cast(e any, typeSQL string) drops.Expression {
	return &opExpr{
		parts:    []string{"(", ")::" + typeSQL},
		operands: []drops.Expression{operandExpr(e)},
	}
}

// CastAs renders CAST(<e> AS <type>) — the standard-SQL form. Its
// operand is held exactly as [Cast]'s is, so the two spellings do not
// disagree about the tenant axis.
func CastAs(e any, typeSQL string) drops.Expression {
	return &opExpr{
		parts:    []string{"CAST(", " AS " + typeSQL + ")"},
		operands: []drops.Expression{operandExpr(e)},
	}
}

// Case begins a CASE expression. Chain When / Else / End to finish.
//
//	pg.Case().
//	    When(UserAge.Lt(18), "minor").
//	    When(UserAge.Lt(65), "adult").
//	    Else("senior").
//	    End()
func Case() *CaseExpr { return &CaseExpr{} }

// CaseOn begins a simple CASE expression on a value:
//
//	pg.CaseOn(UserStatus).
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
// Every branch is an operand and every operand is held, so the subject,
// a WHEN condition, a THEN result and the ELSE may each be a statement
// and each is scoped as one. CASE WHEN EXISTS (SELECT ... FROM posts)
// THEN ... used to render that subquery through WriteSQL, which has no
// ctx, and so read every tenant's posts to decide this tenant's column.
//
// The branches are laid out here rather than while rendering — see
// opBuilder — because a CASE has a variable number of them and deciding
// the text at render time is what puts the operands inside a closure.
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
