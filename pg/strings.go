package pg

import "github.com/bernardoforcillo/drops"

// PostgreSQL string functions. Each helper produces a SQL fragment;
// arguments may be drops.Expression (column/expr) or Go values (bound
// as parameters).

// Concat renders concat(args...).
func Concat(args ...any) drops.Expression { return funcCall("concat", args) }

// ConcatWS renders concat_ws(sep, args...).
func ConcatWS(sep any, args ...any) drops.Expression {
	all := append([]any{sep}, args...)
	return funcCall("concat_ws", all)
}

// ConcatOp renders the SQL || concatenation operator: (a || b).
func ConcatOp(left, right any) drops.Expression { return binOp(left, "||", right) }

// Length renders length(<e>).
func Length(e any) drops.Expression { return funcCall("length", []any{e}) }

// CharLength renders char_length(<e>).
func CharLength(e any) drops.Expression { return funcCall("char_length", []any{e}) }

// Substring renders substring(<e> FROM <from> FOR <count>). count may be
// nil to omit the FOR clause.
func Substring(e any, from any, count any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("substring(")
		writeOperand(b, e)
		b.WriteString(" FROM ")
		writeOperand(b, from)
		if count != nil {
			b.WriteString(" FOR ")
			writeOperand(b, count)
		}
		b.WriteByte(')')
	})
}

// Trim renders trim(<e>).
func Trim(e any) drops.Expression { return funcCall("trim", []any{e}) }

// LTrim renders ltrim(<e>).
func LTrim(e any) drops.Expression { return funcCall("ltrim", []any{e}) }

// RTrim renders rtrim(<e>).
func RTrim(e any) drops.Expression { return funcCall("rtrim", []any{e}) }

// Replace renders replace(<e>, <from>, <to>).
func Replace(e, from, to any) drops.Expression { return funcCall("replace", []any{e, from, to}) }

// RegexpReplace renders regexp_replace(<e>, <pattern>, <replacement>, [<flags>]).
func RegexpReplace(e, pattern, replacement any, flags ...string) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("regexp_replace(")
		writeOperand(b, e)
		b.WriteString(", ")
		writeOperand(b, pattern)
		b.WriteString(", ")
		writeOperand(b, replacement)
		if len(flags) > 0 && flags[0] != "" {
			b.WriteString(", ")
			b.AddArg(flags[0])
		}
		b.WriteByte(')')
	})
}

// RegexpMatch renders regexp_match(<e>, <pattern>).
func RegexpMatch(e, pattern any) drops.Expression {
	return funcCall("regexp_match", []any{e, pattern})
}

// Position renders position(<substring> IN <string>).
func Position(substring, str any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("position(")
		writeOperand(b, substring)
		b.WriteString(" IN ")
		writeOperand(b, str)
		b.WriteByte(')')
	})
}

// StrPos renders strpos(<string>, <substring>).
func StrPos(str, substring any) drops.Expression {
	return funcCall("strpos", []any{str, substring})
}

// Initcap renders initcap(<e>).
func Initcap(e any) drops.Expression { return funcCall("initcap", []any{e}) }

// Format renders format(<fmt>, args...).
func Format(format any, args ...any) drops.Expression {
	all := append([]any{format}, args...)
	return funcCall("format", all)
}

// ToChar renders to_char(<value>, <pattern>).
func ToChar(value, pattern any) drops.Expression {
	return funcCall("to_char", []any{value, pattern})
}

// Md5 renders md5(<e>).
func Md5(e any) drops.Expression { return funcCall("md5", []any{e}) }

// Encode renders encode(<bytea>, <format>) — format is 'base64', 'hex', or 'escape'.
func Encode(bytea, format any) drops.Expression {
	return funcCall("encode", []any{bytea, format})
}

// Decode renders decode(<text>, <format>).
func Decode(text, format any) drops.Expression {
	return funcCall("decode", []any{text, format})
}

// funcCall is the shared renderer for "<name>(<args>)" expressions.
// args is a slice so the caller passes variadic-shaped data already
// flattened — avoids the recursive variadic-of-variadic awkwardness.
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
