package clickhouse

import "github.com/bernardoforcillo/drops"

// funcCall renders <name>(<args...>).
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

// Standard aggregates ---------------------------------------------

func Count(e drops.Expression) drops.Expression { return funcCall("count", []any{e}) }
func CountAll() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("count()") })
}
func Sum(e drops.Expression) drops.Expression { return funcCall("sum", []any{e}) }
func Avg(e drops.Expression) drops.Expression { return funcCall("avg", []any{e}) }
func Min(e drops.Expression) drops.Expression { return funcCall("min", []any{e}) }
func Max(e drops.Expression) drops.Expression { return funcCall("max", []any{e}) }

// ClickHouse-specific aggregates ----------------------------------

// Uniq is an approximate-distinct count (HyperLogLog).
func Uniq(e drops.Expression) drops.Expression { return funcCall("uniq", []any{e}) }

// UniqExact is the exact distinct count.
func UniqExact(e drops.Expression) drops.Expression { return funcCall("uniqExact", []any{e}) }

// UniqHLL12 is the configurable HLL approximation.
func UniqHLL12(e drops.Expression) drops.Expression { return funcCall("uniqHLL12", []any{e}) }

// AnyAgg returns an arbitrary value from the group (CH's `any`).
// Named AnyAgg to avoid colliding with Go's any keyword visually.
func AnyAgg(e drops.Expression) drops.Expression { return funcCall("any", []any{e}) }

// AnyLast / AnyHeavy variants.
func AnyLast(e drops.Expression) drops.Expression  { return funcCall("anyLast", []any{e}) }
func AnyHeavy(e drops.Expression) drops.Expression { return funcCall("anyHeavy", []any{e}) }

// Quantile is the approximate quantile aggregate; level is in [0, 1].
//
//	clickhouse.Quantile(0.95, Latency)  // 95th percentile
func Quantile(level float64, e drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("quantile(")
		writeOperand(b, level)
		b.WriteString(")(")
		b.Append(e)
		b.WriteByte(')')
	})
}

// QuantileExact uses the exact algorithm; QuantileTiming is optimised
// for non-negative integers (latency in ms).
func QuantileExact(level float64, e drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("quantileExact(")
		writeOperand(b, level)
		b.WriteString(")(")
		b.Append(e)
		b.WriteByte(')')
	})
}

func QuantileTiming(level float64, e drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("quantileTiming(")
		writeOperand(b, level)
		b.WriteString(")(")
		b.Append(e)
		b.WriteByte(')')
	})
}

// GroupArray returns an array of all values in the group.
func GroupArray(e drops.Expression) drops.Expression { return funcCall("groupArray", []any{e}) }

// GroupUniqArray is the deduplicated variant.
func GroupUniqArray(e drops.Expression) drops.Expression {
	return funcCall("groupUniqArray", []any{e})
}

// Argument-aware aggregates: argMax / argMin.
func ArgMax(value, by drops.Expression) drops.Expression {
	return funcCall("argMax", []any{value, by})
}
func ArgMin(value, by drops.Expression) drops.Expression {
	return funcCall("argMin", []any{value, by})
}

// Scalar / math helpers (mirrors what most apps need) ------------

func Lower(e drops.Expression) drops.Expression { return funcCall("lower", []any{e}) }
func Upper(e drops.Expression) drops.Expression { return funcCall("upper", []any{e}) }
func Length(e any) drops.Expression             { return funcCall("length", []any{e}) }
func Abs(e any) drops.Expression                { return funcCall("abs", []any{e}) }
func Round(e any) drops.Expression              { return funcCall("round", []any{e}) }
func Coalesce(args ...any) drops.Expression     { return funcCall("coalesce", args) }
func IfNull(e, fallback any) drops.Expression   { return funcCall("ifNull", []any{e, fallback}) }
func Now() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("now()") })
}

// Date helpers commonly used in analytics.

func ToDate(e any) drops.Expression                   { return funcCall("toDate", []any{e}) }
func ToDateTime(e any) drops.Expression               { return funcCall("toDateTime", []any{e}) }
func ToStartOfDay(e any) drops.Expression             { return funcCall("toStartOfDay", []any{e}) }
func ToStartOfHour(e any) drops.Expression            { return funcCall("toStartOfHour", []any{e}) }
func ToStartOfMinute(e any) drops.Expression          { return funcCall("toStartOfMinute", []any{e}) }
func ToStartOfMonth(e any) drops.Expression           { return funcCall("toStartOfMonth", []any{e}) }
func ToYYYYMM(e any) drops.Expression                 { return funcCall("toYYYYMM", []any{e}) }
func ToYYYYMMDD(e any) drops.Expression               { return funcCall("toYYYYMMDD", []any{e}) }
func DateDiff(unit string, a, b any) drops.Expression { return funcCall("dateDiff", []any{unit, a, b}) }

// As renames an expression (for SELECT projections).
func As(e drops.Expression, alias string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.Append(e)
		b.WriteString(" AS ")
		b.WriteIdent(alias)
	})
}

// Func is the escape hatch for any function not covered by helpers.
func Func(name string, args ...any) drops.Expression { return funcCall(name, args) }
