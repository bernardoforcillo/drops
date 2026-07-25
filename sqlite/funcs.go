package sqlite

import "github.com/bernardoforcillo/drops"

// Aggregate and scalar function helpers for SQLite. Each returns a
// drops.Expression usable in a SELECT list, GROUP BY, ORDER BY or WHERE
// clause. Names mirror drops/pg where SQLite has an equivalent; SQLite's
// own spellings (total, group_concat, ifnull) are exposed alongside.

// funcCall is the shared renderer for "<name>(<args>)" expressions. args
// is a pre-flattened slice so callers avoid variadic-of-variadic
// awkwardness. Each argument may be a drops.Expression or a Go value
// (bound as a parameter).
func funcCall(name string, args []any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString(name)
		b.WriteByte('(')
		for i, a := range args {
			if i > 0 {
				b.WriteString(", ")
			}
			writeOperand(b, a)
		}
		b.WriteByte(')')
	})
}

// Func renders an arbitrary function call <name>(<args...>). Escape
// hatch for SQLite functions without a dedicated helper.
func Func(name string, args ...any) drops.Expression { return funcCall(name, args) }

// Aggregates -----------------------------------------------------------

// Count renders count(<e>).
func Count(e drops.Expression) drops.Expression { return funcCall("count", []any{e}) }

// CountAll renders count(*).
func CountAll() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("count(*)") })
}

// CountDistinct renders count(DISTINCT <e>).
func CountDistinct(e drops.Expression) drops.Expression { return distinctAgg("count", e) }

// Sum / Avg / Min / Max aggregates.
func Sum(e drops.Expression) drops.Expression { return funcCall("sum", []any{e}) }
func Avg(e drops.Expression) drops.Expression { return funcCall("avg", []any{e}) }
func Min(e drops.Expression) drops.Expression { return funcCall("min", []any{e}) }
func Max(e drops.Expression) drops.Expression { return funcCall("max", []any{e}) }

// SumDistinct / AvgDistinct apply DISTINCT to the aggregate.
func SumDistinct(e drops.Expression) drops.Expression { return distinctAgg("sum", e) }
func AvgDistinct(e drops.Expression) drops.Expression { return distinctAgg("avg", e) }

// Total is SQLite's sum() variant that returns 0.0 (never NULL) over an
// empty or all-NULL set, always as a floating-point value.
func Total(e drops.Expression) drops.Expression { return funcCall("total", []any{e}) }

// GroupConcat renders group_concat(<e>) or group_concat(<e>, <sep>) —
// SQLite's equivalent of PostgreSQL string_agg.
func GroupConcat(e any, sep ...any) drops.Expression {
	args := []any{e}
	if len(sep) > 0 {
		args = append(args, sep[0])
	}
	return funcCall("group_concat", args)
}

// Filter wraps an aggregate with a FILTER (WHERE ...) clause. Supported
// by SQLite 3.30+:
//
//	sqlite.Filter(sqlite.Count(UserID), UserStatus.Eq("active"))
func Filter(agg, pred drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(agg)
		b.WriteString(" FILTER (WHERE ")
		b.Append(pred)
		b.WriteByte(')')
	})
}

// distinctAgg renders <name>(DISTINCT <e>).
func distinctAgg(name string, e drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString(name)
		b.WriteString("(DISTINCT ")
		b.Append(e)
		b.WriteByte(')')
	})
}

// Scalar helpers -------------------------------------------------------

// Lower / Upper case-folding helpers.
func Lower(e drops.Expression) drops.Expression { return funcCall("lower", []any{e}) }
func Upper(e drops.Expression) drops.Expression { return funcCall("upper", []any{e}) }

// Coalesce renders coalesce(<args...>).
func Coalesce(args ...any) drops.Expression { return funcCall("coalesce", args) }

// IfNull renders ifnull(<a>, <b>) — SQLite's two-argument coalesce.
func IfNull(a, b any) drops.Expression { return funcCall("ifnull", []any{a, b}) }

// NullIf renders nullif(<a>, <b>).
func NullIf(a, b any) drops.Expression { return funcCall("nullif", []any{a, b}) }

// As renames an arbitrary expression: "<expr> AS <alias>".
func As(e drops.Expression, alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(e)
		b.WriteString(" AS ")
		b.WriteIdent(alias)
	})
}
