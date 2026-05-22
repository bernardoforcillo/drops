package pg

import (
	"fmt"

	"github.com/bernardoforcillo/drops"
)

// PostgreSQL date/time helpers.

// CurrentDate renders current_date.
func CurrentDate() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("current_date") })
}

// CurrentTime renders current_time.
func CurrentTime() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("current_time") })
}

// CurrentTimestamp renders current_timestamp.
func CurrentTimestamp() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("current_timestamp") })
}

// LocalTime / LocalTimestamp (without time zone).
func LocalTime() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("localtime") })
}
func LocalTimestamp() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("localtimestamp") })
}

// DateTrunc renders date_trunc('field', <ts>) — e.g. DateTrunc("day", col).
func DateTrunc(field string, ts any) drops.Expression {
	return funcCall("date_trunc", []any{field, ts})
}

// Extract renders extract(<field> FROM <ts>).
func Extract(field string, ts any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("extract(")
		b.WriteString(field)
		b.WriteString(" FROM ")
		writeOperand(b, ts)
		b.WriteByte(')')
	})
}

// DatePart renders date_part('field', <ts>) — the function form of EXTRACT.
func DatePart(field string, ts any) drops.Expression {
	return funcCall("date_part", []any{field, ts})
}

// Age renders age(<a>, <b>) — the interval between two timestamps. Pass
// only one timestamp to compute age(now(), ts).
func Age(args ...any) drops.Expression { return funcCall("age", args) }

// IntervalLit renders an INTERVAL literal — e.g. IntervalLit("1 day"),
// IntervalLit("2 hours 30 minutes"). The value is wrapped in
// INTERVAL '...' with single quotes doubled.
//
// Named with "Lit" to avoid a collision with the Interval(name) column
// type constructor in types.go.
func IntervalLit(literal string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("INTERVAL ")
		b.WriteString(quoteLiteral(literal))
	})
}

// Day / Hour / Minute / Second build INTERVAL literals from numeric n.
func Day(n int) drops.Expression    { return IntervalLit(fmt.Sprintf("%d day", n)) }
func Hour(n int) drops.Expression   { return IntervalLit(fmt.Sprintf("%d hour", n)) }
func Minute(n int) drops.Expression { return IntervalLit(fmt.Sprintf("%d minute", n)) }
func Second(n int) drops.Expression { return IntervalLit(fmt.Sprintf("%d second", n)) }
func Week(n int) drops.Expression   { return IntervalLit(fmt.Sprintf("%d week", n)) }
func Month(n int) drops.Expression  { return IntervalLit(fmt.Sprintf("%d month", n)) }
func Year(n int) drops.Expression   { return IntervalLit(fmt.Sprintf("%d year", n)) }

// MakeDate renders make_date(<y>, <m>, <d>).
func MakeDate(y, m, d any) drops.Expression { return funcCall("make_date", []any{y, m, d}) }

// MakeTime renders make_time(<h>, <m>, <s>).
func MakeTime(h, m, s any) drops.Expression { return funcCall("make_time", []any{h, m, s}) }

// MakeTimestamp renders make_timestamp(y, mo, d, h, mi, s).
func MakeTimestamp(args ...any) drops.Expression { return funcCall("make_timestamp", args) }

// MakeTimestampTZ is the TZ-aware variant.
func MakeTimestampTZ(args ...any) drops.Expression { return funcCall("make_timestamptz", args) }

// ToDate / ToTimestamp / ToNumber — text conversion helpers.
func ToDate(text, pattern any) drops.Expression { return funcCall("to_date", []any{text, pattern}) }
func ToTimestamp(text, pattern any) drops.Expression {
	return funcCall("to_timestamp", []any{text, pattern})
}
func ToNumber(text, pattern any) drops.Expression {
	return funcCall("to_number", []any{text, pattern})
}

// AtTimeZone renders <ts> AT TIME ZONE <zone>.
func AtTimeZone(ts, zone any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, ts)
		b.WriteString(" AT TIME ZONE ")
		writeOperand(b, zone)
		b.WriteByte(')')
	})
}
