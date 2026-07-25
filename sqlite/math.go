package sqlite

import "github.com/bernardoforcillo/drops"

// SQLite numeric functions and arithmetic operators.
//
// The trigonometric, logarithmic and rounding helpers (ceil, floor,
// sin, ln, …) require SQLite built with the math extension
// (SQLITE_ENABLE_MATH_FUNCTIONS, the default in most modern
// distributions and in the CLI since 3.35). abs, round, random and the
// arithmetic operators are always available.

func Abs(e any) drops.Expression   { return funcCall("abs", []any{e}) }
func Ceil(e any) drops.Expression  { return funcCall("ceil", []any{e}) }
func Floor(e any) drops.Expression { return funcCall("floor", []any{e}) }
func Trunc(e any) drops.Expression { return funcCall("trunc", []any{e}) }

// Round renders round(<e>) or round(<e>, <digits>) when digits is given.
func Round(e any, digits ...int) drops.Expression {
	args := []any{e}
	if len(digits) > 0 {
		args = append(args, digits[0])
	}
	return funcCall("round", args)
}

// Mod renders the SQL "%" modulo operator: (a % b). SQLite has no mod()
// function; the operator is the portable form.
func Mod(a, b any) drops.Expression { return binOp(a, "%", b) }

func Power(a, b any) drops.Expression { return funcCall("pow", []any{a, b}) }
func Sqrt(e any) drops.Expression     { return funcCall("sqrt", []any{e}) }
func Sign(e any) drops.Expression     { return funcCall("sign", []any{e}) }
func Exp(e any) drops.Expression      { return funcCall("exp", []any{e}) }
func Ln(e any) drops.Expression       { return funcCall("ln", []any{e}) }

// Log renders log(<e>) — base-10 logarithm in SQLite — or
// log(<base>, <e>) when a base is supplied.
func Log(e any, base ...any) drops.Expression {
	if len(base) > 0 {
		return funcCall("log", []any{base[0], e})
	}
	return funcCall("log", []any{e})
}

// Greatest / Least map to SQLite's multi-argument max() / min(), which
// act as scalar (not aggregate) functions when given two or more
// arguments.
func Greatest(args ...any) drops.Expression { return funcCall("max", args) }
func Least(args ...any) drops.Expression    { return funcCall("min", args) }

// Random renders random() — a signed 64-bit integer in SQLite.
func Random() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("random()") })
}

// Trigonometric helpers — pass radians (require the math extension).
func Sin(e any) drops.Expression  { return funcCall("sin", []any{e}) }
func Cos(e any) drops.Expression  { return funcCall("cos", []any{e}) }
func Tan(e any) drops.Expression  { return funcCall("tan", []any{e}) }
func Asin(e any) drops.Expression { return funcCall("asin", []any{e}) }
func Acos(e any) drops.Expression { return funcCall("acos", []any{e}) }
func Atan(e any) drops.Expression { return funcCall("atan", []any{e}) }

// Arithmetic operators as parenthesised binary expressions.
func Plus(left, right any) drops.Expression  { return binOp(left, "+", right) }
func Minus(left, right any) drops.Expression { return binOp(left, "-", right) }
func Mul(left, right any) drops.Expression   { return binOp(left, "*", right) }
func Div(left, right any) drops.Expression   { return binOp(left, "/", right) }
