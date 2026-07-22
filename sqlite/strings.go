package sqlite

import "github.com/bernardoforcillo/drops"

// SQLite string functions. Arguments may be drops.Expression
// (column/expr) or Go values (bound as parameters).

// ConcatOp renders the SQL "||" concatenation operator: (a || b). This
// is the portable form on every SQLite version; the concat() function
// only arrived in 3.44.
func ConcatOp(left, right any) drops.Expression { return binOp(left, "||", right) }

// Concat renders concat(args...) (SQLite 3.44+). Prefer ConcatOp for
// portability across older SQLite builds.
func Concat(args ...any) drops.Expression { return funcCall("concat", args) }

// ConcatWS renders concat_ws(sep, args...) (SQLite 3.44+).
func ConcatWS(sep any, args ...any) drops.Expression {
	return funcCall("concat_ws", append([]any{sep}, args...))
}

// Length renders length(<e>).
func Length(e any) drops.Expression { return funcCall("length", []any{e}) }

// OctetLength renders octet_length(<e>) (SQLite 3.43+).
func OctetLength(e any) drops.Expression { return funcCall("octet_length", []any{e}) }

// Substr renders substr(<e>, <from>) or substr(<e>, <from>, <count>).
// SQLite indexes are 1-based; a negative from counts from the end.
func Substr(e, from any, count ...any) drops.Expression {
	args := []any{e, from}
	if len(count) > 0 {
		args = append(args, count[0])
	}
	return funcCall("substr", args)
}

// Trim / LTrim / RTrim render trim/ltrim/rtrim(<e>) or the two-argument
// form trim(<e>, <chars>) that strips a custom character set.
func Trim(e any, chars ...any) drops.Expression  { return trimFunc("trim", e, chars) }
func LTrim(e any, chars ...any) drops.Expression { return trimFunc("ltrim", e, chars) }
func RTrim(e any, chars ...any) drops.Expression { return trimFunc("rtrim", e, chars) }

func trimFunc(name string, e any, chars []any) drops.Expression {
	args := []any{e}
	if len(chars) > 0 {
		args = append(args, chars[0])
	}
	return funcCall(name, args)
}

// Replace renders replace(<e>, <from>, <to>).
func Replace(e, from, to any) drops.Expression { return funcCall("replace", []any{e, from, to}) }

// Instr renders instr(<e>, <sub>) — the 1-based position of sub in e, or
// 0 when absent. SQLite's counterpart to PostgreSQL strpos.
func Instr(e, sub any) drops.Expression { return funcCall("instr", []any{e, sub}) }

// Hex / Unhex render hex(<e>) / unhex(<e>) (unhex is SQLite 3.41+).
func Hex(e any) drops.Expression   { return funcCall("hex", []any{e}) }
func Unhex(e any) drops.Expression { return funcCall("unhex", []any{e}) }

// Quote renders quote(<e>) — an SQL-literal-safe rendering of the value.
func Quote(e any) drops.Expression { return funcCall("quote", []any{e}) }

// Chr renders char(<codepoints...>) — a string from Unicode code points.
// Named Chr (not Char) to avoid colliding with the Char column
// constructor in types.go.
func Chr(args ...any) drops.Expression { return funcCall("char", args) }

// Unicode renders unicode(<e>) — the code point of the first character.
func Unicode(e any) drops.Expression { return funcCall("unicode", []any{e}) }

// Format renders format(<fmt>, args...) — SQLite's printf-style
// formatter (also spelled printf()).
func Format(format any, args ...any) drops.Expression {
	return funcCall("format", append([]any{format}, args...))
}

// Printf is the SQLite-native spelling of Format.
func Printf(format any, args ...any) drops.Expression {
	return funcCall("printf", append([]any{format}, args...))
}
