package pg

import (
	"reflect"
	"time"

	"github.com/bernardoforcillo/drops"
)

// PostgreSQL string functions. Each helper produces a SQL fragment;
// arguments may be drops.Expression (column/expr) or Go values (bound
// as parameters).

// Concat renders concat(args...).
func Concat(args ...any) drops.Expression { return variadicAnyCall("concat", args) }

// ConcatWS renders concat_ws(sep, args...).
func ConcatWS(sep any, args ...any) drops.Expression {
	all := append([]any{sep}, args...)
	return variadicAnyCall("concat_ws", all)
}

// ConcatOp renders the SQL || concatenation operator: (a || b).
func ConcatOp(left, right any) drops.Expression { return binOp(left, "||", right) }

// Length renders length(<e>).
func Length(e any) drops.Expression { return funcExpr("length", []any{e}) }

// CharLength renders char_length(<e>).
func CharLength(e any) drops.Expression { return funcExpr("char_length", []any{e}) }

// Substring renders substring(<e> FROM <from> FOR <count>). count may be
// nil to omit the FOR clause.
//
// The three operands are held rather than closed over, so any of them
// may be a statement and is scoped as one. The optional FOR is decided
// here, when the expression is built, rather than while rendering —
// see opBuilder.
func Substring(e, from, count any) drops.Expression {
	var o opBuilder
	o.text("substring(")
	o.value(e)
	o.text(" FROM ")
	o.value(from)
	if count != nil {
		o.text(" FOR ")
		o.value(count)
	}
	o.text(")")
	return o.done()
}

// Trim renders trim(<e>).
func Trim(e any) drops.Expression { return funcExpr("trim", []any{e}) }

// LTrim renders ltrim(<e>).
func LTrim(e any) drops.Expression { return funcExpr("ltrim", []any{e}) }

// RTrim renders rtrim(<e>).
func RTrim(e any) drops.Expression { return funcExpr("rtrim", []any{e}) }

// Replace renders replace(<e>, <from>, <to>).
func Replace(e, from, to any) drops.Expression { return funcExpr("replace", []any{e, from, to}) }

// RegexpReplace renders regexp_replace(<e>, <pattern>, <replacement>, [<flags>]).
func RegexpReplace(e, pattern, replacement any, flags ...string) drops.Expression {
	var o opBuilder
	o.text("regexp_replace(")
	o.value(e)
	o.text(", ")
	o.value(pattern)
	o.text(", ")
	o.value(replacement)
	if len(flags) > 0 && flags[0] != "" {
		o.text(", ")
		// A flag string is always bound, never rendered: it is a value
		// the caller typed, not an operand they can put a statement in.
		o.operand(drops.Param{Value: flags[0]})
	}
	o.text(")")
	return o.done()
}

// RegexpMatch renders regexp_match(<e>, <pattern>).
func RegexpMatch(e, pattern any) drops.Expression {
	return funcExpr("regexp_match", []any{e, pattern})
}

// Position renders position(<substring> IN <string>).
func Position(substring, str any) drops.Expression {
	return &opExpr{
		parts:    []string{"position(", " IN ", ")"},
		operands: []drops.Expression{operandExpr(substring), operandExpr(str)},
	}
}

// StrPos renders strpos(<string>, <substring>).
func StrPos(str, substring any) drops.Expression {
	return funcExpr("strpos", []any{str, substring})
}

// Initcap renders initcap(<e>).
func Initcap(e any) drops.Expression { return funcExpr("initcap", []any{e}) }

// Format renders format(<fmt>, args...).
func Format(format any, args ...any) drops.Expression {
	all := append([]any{format}, args...)
	return funcExpr("format", all)
}

// ToChar renders to_char(<value>, <pattern>).
func ToChar(value, pattern any) drops.Expression {
	return funcExpr("to_char", []any{value, pattern})
}

// Md5 renders md5(<e>).
func Md5(e any) drops.Expression { return funcExpr("md5", []any{e}) }

// Encode renders encode(<bytea>, <format>) — format is 'base64', 'hex', or 'escape'.
func Encode(bytea, format any) drops.Expression {
	return funcExpr("encode", []any{bytea, format})
}

// Decode renders decode(<text>, <format>).
func Decode(text, format any) drops.Expression {
	return funcExpr("decode", []any{text, format})
}

// variadicAnyCall renders a call to a VARIADIC "any" function, naming
// the type of every bound argument.
//
// concat and concat_ws declare their arguments as VARIADIC "any", and
// PostgreSQL will not resolve a bare placeholder against that: the
// statement fails to parse with "could not determine data type of
// parameter" (SQLSTATE 42P18), before any argument is sent. Spelling
// the type on the placeholder is what makes the call resolvable, and
// the Go value is the only place that type can come from.
//
// The cast belongs to the argument, so it is decided per argument at
// construction and written into the text that follows it — an
// Expression argument gets none, since it carries its own type. That is
// what lets the arguments be held rather than closed over: a
// json_build_object over a scalar subquery is walked like any other
// operand, instead of rendering the statement inside without its
// context filters.
func variadicAnyCall(name string, args []any) drops.Expression {
	var o opBuilder
	o.text(name + "(")
	for i, a := range args {
		if i > 0 {
			o.text(", ")
		}
		if e, ok := a.(drops.Expression); ok {
			o.operand(e)
			continue
		}
		o.operand(drops.Param{Value: a})
		o.text("::" + pgTypeOf(a))
	}
	o.text(")")
	return o.done()
}

// pgTypeOf names the PostgreSQL type a bound Go value should be cast
// to. Anything unrecognised is called text, which is what concat would
// have coerced it to anyway.
func pgTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "text"
	case time.Time:
		return "timestamptz"
	case []byte:
		return "bytea"
	}
	switch reflect.ValueOf(v).Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "bigint"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "numeric"
	case reflect.Float32, reflect.Float64:
		return "double precision"
	}
	return "text"
}
