package pg

import "github.com/bernardoforcillo/drops"

// Aggregate and scalar function helpers. Each returns a drops.Expression
// that can be used in a SELECT list, GROUP BY, ORDER BY or WHERE clause.

// Count renders count(<e>).
func Count(e drops.Expression) drops.Expression { return funcCall("count", []any{e}) }

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
func Filter(agg drops.Expression, pred drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(agg)
		b.WriteString(" FILTER (WHERE ")
		b.Append(pred)
		b.WriteByte(')')
	})
}

// StringAgg renders string_agg(<e>, <sep>).
func StringAgg(e, sep any) drops.Expression { return funcCall("string_agg", []any{e, sep}) }

// BoolAnd / BoolOr aggregates.
func BoolAnd(e any) drops.Expression { return funcCall("bool_and", []any{e}) }
func BoolOr(e any) drops.Expression  { return funcCall("bool_or", []any{e}) }

// Every is the standard-SQL alias for bool_and.
func Every(e any) drops.Expression { return funcCall("every", []any{e}) }

// Sum / Avg / Min / Max aggregates.
func Sum(e drops.Expression) drops.Expression { return funcCall("sum", []any{e}) }
func Avg(e drops.Expression) drops.Expression { return funcCall("avg", []any{e}) }
func Min(e drops.Expression) drops.Expression { return funcCall("min", []any{e}) }
func Max(e drops.Expression) drops.Expression { return funcCall("max", []any{e}) }

// Lower / Upper case-folding helpers.
func Lower(e drops.Expression) drops.Expression { return funcCall("lower", []any{e}) }
func Upper(e drops.Expression) drops.Expression { return funcCall("upper", []any{e}) }

// Coalesce renders coalesce(<args...>). Arguments may be Expressions or
// Go values (bound as parameters).
func Coalesce(args ...any) drops.Expression { return funcCall("coalesce", args) }

// Now renders now().
func Now() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("now()") })
}

// Func renders an arbitrary function call <name>(<args...>). Use it as
// an escape hatch when a built-in helper isn't provided.
func Func(name string, args ...any) drops.Expression { return funcCall(name, args) }

// As renames an arbitrary expression: "<expr> AS <alias>".
func As(e drops.Expression, alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(e)
		b.WriteString(" AS ")
		b.WriteIdent(alias)
	})
}

// distinctAgg renders <name>(DISTINCT <e>) — used by Count/Sum/AvgDistinct.
func distinctAgg(name string, e drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString(name)
		b.WriteString("(DISTINCT ")
		b.Append(e)
		b.WriteByte(')')
	})
}
