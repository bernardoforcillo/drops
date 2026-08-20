package pg

import "github.com/bernardoforcillo/drops"

// Aggregate and scalar function helpers. Each returns a drops.Expression
// that can be used in a SELECT list, GROUP BY, ORDER BY or WHERE clause.

// Count renders count(<e>).
func Count(e drops.Expression) drops.Expression { return funcExpr("count", []any{e}) }

// CountDistinct renders count(DISTINCT <e>).
func CountDistinct(e drops.Expression) drops.Expression { return distinctAgg("count", e) }

// CountAll renders count(*).
func CountAll() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("count(*)") })
}

// SumDistinct / AvgDistinct apply DISTINCT to the aggregate.
func SumDistinct(e drops.Expression) drops.Expression { return distinctAgg("sum", e) }
func AvgDistinct(e drops.Expression) drops.Expression { return distinctAgg("avg", e) }

// Filter wraps an aggregate with a FILTER (WHERE ...) clause:
//
//	pg.Filter(pg.Count(UserID), pg.Eq(UserStatus, "active"))
//
// Both operands are held rather than closed over, so either may be a
// statement and is scoped as one: count(*) FILTER (WHERE EXISTS (SELECT
// ... FROM posts)) carries posts' context filters, where it used to
// render the subquery through WriteSQL and read every tenant's rows.
func Filter(agg, pred drops.Expression) drops.Expression {
	return &opExpr{
		parts:    []string{"", " FILTER (WHERE ", ")"},
		operands: []drops.Expression{agg, pred},
	}
}

// StringAgg renders string_agg(<e>, <sep>).
func StringAgg(e, sep any) drops.Expression { return funcExpr("string_agg", []any{e, sep}) }

// BoolAnd / BoolOr aggregates.
func BoolAnd(e any) drops.Expression { return funcExpr("bool_and", []any{e}) }
func BoolOr(e any) drops.Expression  { return funcExpr("bool_or", []any{e}) }

// Every is the standard-SQL alias for bool_and.
func Every(e any) drops.Expression { return funcExpr("every", []any{e}) }

// Sum / Avg / Min / Max aggregates.
func Sum(e drops.Expression) drops.Expression { return funcExpr("sum", []any{e}) }
func Avg(e drops.Expression) drops.Expression { return funcExpr("avg", []any{e}) }
func Min(e drops.Expression) drops.Expression { return funcExpr("min", []any{e}) }
func Max(e drops.Expression) drops.Expression { return funcExpr("max", []any{e}) }

// Lower / Upper case-folding helpers.
func Lower(e drops.Expression) drops.Expression { return funcExpr("lower", []any{e}) }
func Upper(e drops.Expression) drops.Expression { return funcExpr("upper", []any{e}) }

// Coalesce renders coalesce(<args...>). Arguments may be Expressions or
// Go values (bound as parameters).
func Coalesce(args ...any) drops.Expression { return funcExpr("coalesce", args) }

// Now renders now().
func Now() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("now()") })
}

// Func renders an arbitrary function call <name>(<args...>). Use it as
// an escape hatch when a built-in helper isn't provided.
func Func(name string, args ...any) drops.Expression { return funcExpr(name, args) }

// As renames an arbitrary expression: "<expr> AS <alias>".
//
// The expression is held rather than closed over, so As(Subquery(sel),
// "n") is scoped like the statement it is.
//
// An empty alias renders no AS clause at all, which is what
// [SelectBuilder.AsSubquery] has always done with one: the alternative
// is `AS ""`, a zero-length delimited identifier PostgreSQL rejects
// with a syntax error.
func As(e drops.Expression, alias string) drops.Expression {
	return &opExpr{parts: []string{"", ""}, operands: []drops.Expression{e}, alias: alias}
}

// distinctAgg renders <name>(DISTINCT <e>) — used by Count/Sum/AvgDistinct.
// The operand is held for the reason funcExpr's is.
func distinctAgg(name string, e drops.Expression) drops.Expression {
	return &opExpr{parts: []string{name + "(DISTINCT ", ")"}, operands: []drops.Expression{e}}
}
