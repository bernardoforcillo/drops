package mysql

import (
	"fmt"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// MySQL date and time helpers, ported from drops/pg's datetime.go.
//
// # The shape of the difference
//
// PostgreSQL has timestamptz, a real interval type, and a formatting
// language (to_char) shared across dates and numbers. MySQL has none
// of the three. What it has instead:
//
//   - DATETIME and TIMESTAMP, neither of which stores a zone. A
//     TIMESTAMP is converted from the session's time_zone on the way in
//     and back on the way out; a DATETIME is stored verbatim. So "the
//     same row read by two sessions" can differ, which is a class of
//     bug PostgreSQL does not have.
//   - INTERVAL as *syntax*, not as a value. "INTERVAL 1 DAY" is only
//     legal as the right operand of + or -, or inside DATE_ADD /
//     DATE_SUB. SELECT INTERVAL 1 DAY is a syntax error on both
//     servers. See [Interval].
//   - DATE_FORMAT patterns (%Y, %m, %i) rather than to_char's
//     (YYYY, MM, MI). The two languages do not overlap, which is why
//     pg's ToChar / ToDate / ToTimestamp / ToNumber have no counterpart
//     here: [DateFormat] and [StrToDate] are the MySQL-native pair.
//
// # Left out
//
//   - pg's DatePart. MySQL has no date_part() function; [Extract] is
//     the only spelling.
//   - pg's Age, which returns an interval. [TimestampDiff] answers the
//     same question in one named unit.
//   - pg's AtTimeZone. MySQL's [ConvertTZ] needs the source zone as
//     well as the target, and — read its doc — silently returns NULL
//     for a named zone on a server whose time-zone tables were never
//     loaded, which is the default for both MySQL and MariaDB.
//   - pg's MakeTimestampTZ. There is no zone-aware timestamp to make.

// CurrentDate renders current_date.
func CurrentDate() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("current_date") })
}

// CurrentTime renders current_time(6). The precision is explicit
// because MySQL defaults these to whole seconds where PostgreSQL keeps
// microseconds, and a truncation that only shows up in the sub-second
// digits is the kind that reaches production.
func CurrentTime() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("current_time(6)") })
}

// CurrentTimestamp renders current_timestamp(6).
func CurrentTimestamp() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("current_timestamp(6)") })
}

// LocalTime and LocalTimestamp render localtime(6) / localtimestamp(6).
//
// In PostgreSQL these are the "without time zone" forms. In MySQL they
// are plain synonyms of NOW() — every one of these functions already
// answers in the session's time zone, and none of them carries one. The
// names are kept so a ported query still compiles; the meaning is
// [Now]'s.
func LocalTime() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("localtime(6)") })
}

func LocalTimestamp() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("localtimestamp(6)") })
}

// UTCTimestamp renders utc_timestamp(6) — the one clock reading in
// MySQL that does not move with the session's time zone. No pg
// counterpart is needed there, where now() carries its zone.
func UTCTimestamp() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("utc_timestamp(6)") })
}

// dateTruncFormat maps a date_trunc field to the DATE_FORMAT pattern
// that flattens everything below it. The fields that cannot be
// expressed as one pattern — week and quarter — are handled in
// DateTrunc itself.
var dateTruncFormat = map[string]string{
	"second": "%Y-%m-%d %H:%i:%s",
	"minute": "%Y-%m-%d %H:%i:00",
	"hour":   "%Y-%m-%d %H:00:00",
	"day":    "%Y-%m-%d 00:00:00",
	"month":  "%Y-%m-01 00:00:00",
	"year":   "%Y-01-01 00:00:00",
}

// DateTrunc flattens a timestamp to the start of the given field:
// "second", "minute", "hour", "day", "week", "month", "quarter" or
// "year". It is PostgreSQL's date_trunc, which neither MySQL nor
// MariaDB has, rebuilt out of DATE_FORMAT and a CAST back to
// DATETIME(6) so the result is a timestamp rather than a string.
//
// "week" truncates to Monday, matching PostgreSQL. Note that MySQL's
// own WEEK() function does not — it defaults to Sunday-based, mode 0 —
// so the two disagree about which week a Sunday belongs to everywhere
// except here.
//
// The ts expression is written twice for "week" and "quarter", which
// have no single-pattern form. That is invisible for a column and
// costs a second bound parameter for a literal; it also means ts must
// not be something with a side effect or a fresh value per call.
//
// Any other field panics, at build time rather than as a server error
// on a string that happened to render.
func DateTrunc(field string, ts any) drops.Expression {
	f := strings.ToLower(strings.TrimSpace(field))
	if pattern, ok := dateTruncFormat[f]; ok {
		return castDatetime(funcCall("date_format", []any{ts, pattern}))
	}
	switch f {
	case "week":
		// Back up to Monday first, then flatten the time. WEEKDAY() is
		// 0 on a Monday, which is the number of days to subtract.
		monday := funcCall("date_sub", []any{ts, drops.ExprFunc(func(b *drops.Builder) {
			b.WriteString("INTERVAL ")
			funcCall("weekday", []any{ts}).WriteSQL(b)
			b.WriteString(" DAY")
		})})
		return castDatetime(funcCall("date_format", []any{monday, "%Y-%m-%d 00:00:00"}))
	case "quarter":
		// January 1st of the year, advanced by whole quarters.
		firstOfYear := funcCall("makedate", []any{funcCall("year", []any{ts}), 1})
		start := drops.ExprFunc(func(b *drops.Builder) {
			b.WriteByte('(')
			firstOfYear.WriteSQL(b)
			b.WriteString(" + INTERVAL ")
			funcCall("quarter", []any{ts}).WriteSQL(b)
			b.WriteString("-1 QUARTER)")
		})
		return castDatetime(funcCall("date_format", []any{start, "%Y-%m-%d 00:00:00"}))
	}
	panic(fmt.Sprintf("drops/mysql: DateTrunc field %q is not one of second, minute, hour, day, week, month, quarter, year", field))
}

func castDatetime(e drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("CAST(")
		e.WriteSQL(b)
		b.WriteString(" AS DATETIME(6))")
	})
}

// Extract renders extract(<field> FROM <ts>).
//
// MySQL's unit vocabulary is not PostgreSQL's. YEAR, QUARTER, MONTH,
// WEEK, DAY, HOUR, MINUTE, SECOND and MICROSECOND work on both; pg's
// DOW, DOY, EPOCH, ISOYEAR and CENTURY are not MySQL units at all and
// give error 1064. [DayOfWeek], [DayOfYear] and [UnixTimestamp] are
// the three that have a MySQL answer, and the first two number their
// days differently from pg — see their docs.
//
// EXTRACT(WEEK FROM …) is also not pg's ISO week: MySQL defaults to
// mode 0, Sunday-based, with a week 0 at the start of the year.
func Extract(field string, ts any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("extract(")
		b.WriteString(field)
		b.WriteString(" FROM ")
		writeOperand(b, ts)
		b.WriteByte(')')
	})
}

// DayOfWeek renders dayofweek(<ts>), which numbers Sunday 1 through
// Saturday 7. PostgreSQL's extract(dow) numbers Sunday 0 through
// Saturday 6, so a ported comparison is off by one. [Weekday] is the
// other MySQL numbering, Monday 0 through Sunday 6.
func DayOfWeek(ts any) drops.Expression { return funcCall("dayofweek", []any{ts}) }

// Weekday renders weekday(<ts>): Monday 0 through Sunday 6.
func Weekday(ts any) drops.Expression { return funcCall("weekday", []any{ts}) }

// DayOfYear renders dayofyear(<ts>), 1-based as pg's extract(doy) is.
func DayOfYear(ts any) drops.Expression { return funcCall("dayofyear", []any{ts}) }

// UnixTimestamp renders unix_timestamp(<ts>) — pg's extract(epoch).
// With no argument it is the current time.
//
// It interprets a DATETIME argument in the session's time zone, so the
// same stored value gives different answers to sessions with different
// time_zone settings.
func UnixTimestamp(ts ...any) drops.Expression { return funcCall("unix_timestamp", ts) }

// FromUnixTime renders from_unixtime(<seconds>) — the inverse.
func FromUnixTime(seconds any) drops.Expression { return funcCall("from_unixtime", []any{seconds}) }

// Interval renders "INTERVAL <n> <UNIT>".
//
// It is not an expression on its own: MySQL's grammar only accepts an
// interval as the right operand of + or -, or as the second argument
// of DATE_ADD / DATE_SUB. SELECT INTERVAL 1 DAY is a syntax error on
// both servers, which is why pg's IntervalLit — whose value is a real
// interval — has no equivalent name here.
//
//	mysql.Plus(OrderCreatedAt, mysql.Day(7))   // created_at + INTERVAL 7 DAY
//	mysql.DateAdd(OrderCreatedAt, mysql.Day(7))
//
// unit is written verbatim and must be a MySQL interval unit:
// MICROSECOND, SECOND, MINUTE, HOUR, DAY, WEEK, MONTH, QUARTER, YEAR,
// or one of the compound forms. n binds as a parameter.
func Interval(n any, unit string) drops.Expression {
	u := strings.ToUpper(strings.TrimSpace(unit))
	if u == "" || strings.ContainsAny(u, " \t\n\r'`\"();") {
		panic(fmt.Sprintf("drops/mysql: %q is not an interval unit", unit))
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("INTERVAL ")
		writeOperand(b, n)
		b.WriteByte(' ')
		b.WriteString(u)
	})
}

// Second / Minute / Hour / Day / Week / Month / Year build interval
// expressions, as their pg counterparts build interval literals.
func Second(n int) drops.Expression { return Interval(n, "SECOND") }
func Minute(n int) drops.Expression { return Interval(n, "MINUTE") }
func Hour(n int) drops.Expression   { return Interval(n, "HOUR") }
func Day(n int) drops.Expression    { return Interval(n, "DAY") }
func Week(n int) drops.Expression   { return Interval(n, "WEEK") }
func Month(n int) drops.Expression  { return Interval(n, "MONTH") }
func Year(n int) drops.Expression   { return Interval(n, "YEAR") }

// DateAdd renders date_add(<ts>, <interval>); DateSub is the inverse.
// Pass the interval from [Interval] or one of its named shorthands.
//
// Month arithmetic clamps rather than overflowing on both servers:
// '2024-01-31' plus one month is '2024-02-29', not March 2nd — the
// same rule PostgreSQL applies.
func DateAdd(ts any, interval drops.Expression) drops.Expression {
	return funcCall("date_add", []any{ts, interval})
}

func DateSub(ts any, interval drops.Expression) drops.Expression {
	return funcCall("date_sub", []any{ts, interval})
}

// TimestampDiff renders timestampdiff(<unit>, <from>, <to>) — how many
// whole units separate the two, counting from the first argument to
// the second. It is what pg's age() is usually reached for.
//
// The truncation is toward zero and by whole units, so two timestamps
// 23 hours apart differ by 0 DAY.
func TimestampDiff(unit string, from, to any) drops.Expression {
	u := strings.ToUpper(strings.TrimSpace(unit))
	if u == "" || strings.ContainsAny(u, " \t\n\r'`\"();") {
		panic(fmt.Sprintf("drops/mysql: %q is not an interval unit", unit))
	}
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("timestampdiff(")
		b.WriteString(u)
		b.WriteString(", ")
		writeOperand(b, from)
		b.WriteString(", ")
		writeOperand(b, to)
		b.WriteByte(')')
	})
}

// DateFormat renders date_format(<ts>, <pattern>) using MySQL's %-code
// pattern language — %Y, %m, %d, %H, %i, %s, %f. Stands in for pg's
// to_char, whose YYYY-MM-DD patterns MySQL does not understand.
func DateFormat(ts, pattern any) drops.Expression {
	return funcCall("date_format", []any{ts, pattern})
}

// StrToDate renders str_to_date(<text>, <pattern>) — the inverse, and
// what pg spells to_date / to_timestamp. A string the pattern does not
// match yields NULL plus a warning, not an error.
func StrToDate(text, pattern any) drops.Expression {
	return funcCall("str_to_date", []any{text, pattern})
}

// MakeDate builds a date from year, month and day.
//
// It is *not* MySQL's MAKEDATE, which takes a year and a day of the
// year — MakeDate(2024, 3, 15) under that function would be the 3rd of
// January. Keeping pg's three-argument meaning under pg's name is the
// safer half of the trade; the rendering goes through STR_TO_DATE.
func MakeDate(y, m, d any) drops.Expression {
	return StrToDate(ConcatWS("-", y, m, d), "%Y-%m-%d")
}

// MakeTime builds a time from hour, minute and second. MySQL's
// MAKETIME means exactly what pg's make_time does.
func MakeTime(h, m, s any) drops.Expression { return funcCall("maketime", []any{h, m, s}) }

// MakeTimestamp builds a DATETIME from year, month, day, hour, minute
// and second — pg's make_timestamp, which MySQL has no single function
// for.
func MakeTimestamp(y, mo, d, h, mi, s any) drops.Expression {
	return StrToDate(
		ConcatWS(" ", ConcatWS("-", y, mo, d), ConcatWS(":", h, mi, s)),
		"%Y-%m-%d %H:%i:%s")
}

// LastDay renders last_day(<ts>) — the last day of that month. pg
// writes this as a date_trunc plus interval arithmetic.
func LastDay(ts any) drops.Expression { return funcCall("last_day", []any{ts}) }

// Quarter renders quarter(<ts>), 1 to 4.
func Quarter(ts any) drops.Expression { return funcCall("quarter", []any{ts}) }

// ConvertTZ renders convert_tz(<ts>, <from>, <to>) — MySQL's answer to
// pg's AT TIME ZONE, needing the source zone as well because a
// DATETIME does not carry one.
//
// Read this before using a named zone: CONVERT_TZ resolves names such
// as 'Europe/Rome' out of the mysql.time_zone_name table, which is
// empty unless an administrator has loaded it (mysql_tzinfo_to_sql).
// On a server where it was not, the call returns NULL — no error, no
// warning — and a NULL timestamp propagates quietly through whatever
// it was feeding. Numeric offsets such as '+00:00' need no tables and
// always work. Neither of the servers in this project's own test
// environment has the tables loaded.
func ConvertTZ(ts, from, to any) drops.Expression {
	return funcCall("convert_tz", []any{ts, from, to})
}
