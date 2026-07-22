package sqlite

import "github.com/bernardoforcillo/drops"

// SQLite date/time helpers. SQLite has no dedicated date/time types; the
// five core functions operate on ISO-8601 text, Julian day numbers, or
// Unix timestamps and accept trailing modifier strings such as
// "+1 day", "start of month" or "utc".

// Now renders CURRENT_TIMESTAMP — "YYYY-MM-DD HH:MM:SS" in UTC.
func Now() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("CURRENT_TIMESTAMP") })
}

// CurrentDate / CurrentTime / CurrentTimestamp render the SQL standard
// keywords, which SQLite supports directly.
func CurrentDate() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("CURRENT_DATE") })
}
func CurrentTime() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("CURRENT_TIME") })
}
func CurrentTimestamp() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("CURRENT_TIMESTAMP") })
}

// DateOf renders date(<timeval>, modifiers...) — the date portion.
func DateOf(timeval any, modifiers ...any) drops.Expression {
	return funcCall("date", append([]any{timeval}, modifiers...))
}

// TimeOf renders time(<timeval>, modifiers...) — the time portion.
func TimeOf(timeval any, modifiers ...any) drops.Expression {
	return funcCall("time", append([]any{timeval}, modifiers...))
}

// DateTime renders datetime(<timeval>, modifiers...).
func DateTime(timeval any, modifiers ...any) drops.Expression {
	return funcCall("datetime", append([]any{timeval}, modifiers...))
}

// JulianDay renders julianday(<timeval>, modifiers...).
func JulianDay(timeval any, modifiers ...any) drops.Expression {
	return funcCall("julianday", append([]any{timeval}, modifiers...))
}

// UnixEpoch renders unixepoch(<timeval>, modifiers...) (SQLite 3.38+).
func UnixEpoch(timeval any, modifiers ...any) drops.Expression {
	return funcCall("unixepoch", append([]any{timeval}, modifiers...))
}

// StrfTime renders strftime(<format>, <timeval>, modifiers...) — the
// general-purpose formatter, e.g. StrfTime("%Y-%m", col).
func StrfTime(format, timeval any, modifiers ...any) drops.Expression {
	return funcCall("strftime", append([]any{format, timeval}, modifiers...))
}
