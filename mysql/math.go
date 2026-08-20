package mysql

import "github.com/bernardoforcillo/drops"

// MySQL numeric functions, ported from drops/pg's math.go.
//
// Two of these answer something different from the PostgreSQL function
// of the same name, and both differences are silent:
//
//   - LOG(x) is the *natural* logarithm in MySQL and the base-10 one
//     in PostgreSQL. [Log] therefore renders LOG10, keeping pg's
//     meaning; [Ln] is the natural log under both servers' names.
//   - "/" between two integers yields a decimal in MySQL (5/2 is
//     2.5000) and an integer in PostgreSQL (5/2 is 2). [Div] renders
//     the operator and so inherits MySQL's answer; [IntDiv] renders
//     MySQL's DIV, which truncates the way pg's / does.

func Abs(e any) drops.Expression   { return funcCall("abs", []any{e}) }
func Ceil(e any) drops.Expression  { return funcCall("ceil", []any{e}) }
func Floor(e any) drops.Expression { return funcCall("floor", []any{e}) }

// Round renders round(<e>) or round(<e>, <digits>).
//
// MySQL rounds half away from zero for exact types — ROUND(2.5) is 3,
// ROUND(-2.5) is -3 — as PostgreSQL's numeric does. For an approximate
// (FLOAT/DOUBLE) argument both servers defer to the C library and
// round half to even, so the answer depends on the column's type, not
// on the dialect.
func Round(e any, digits ...int) drops.Expression {
	args := []any{e}
	if len(digits) > 0 {
		args = append(args, digits[0])
	}
	return funcCall("round", args)
}

// Truncate renders truncate(<e>, <digits>) — cut, not round. MySQL
// requires the digits argument that pg's trunc() makes optional.
func Truncate(e, digits any) drops.Expression { return funcCall("truncate", []any{e, digits}) }

func Mod(a, b any) drops.Expression   { return funcCall("mod", []any{a, b}) }
func Power(a, b any) drops.Expression { return funcCall("power", []any{a, b}) }
func Sqrt(e any) drops.Expression     { return funcCall("sqrt", []any{e}) }
func Sign(e any) drops.Expression     { return funcCall("sign", []any{e}) }
func Exp(e any) drops.Expression      { return funcCall("exp", []any{e}) }

// Ln renders ln(<e>) — the natural logarithm, under the name both
// dialects give it.
func Ln(e any) drops.Expression { return funcCall("ln", []any{e}) }

// Log renders log10(<e>).
//
// It is deliberately not MySQL's LOG(): PostgreSQL's log(x) is base
// 10, MySQL's LOG(x) is base e, and a port that kept the spelling
// would silently change every answer by a factor of ln(10). [Ln] is
// the natural log; [LogBase] takes an explicit base.
func Log(e any) drops.Expression { return funcCall("log10", []any{e}) }

// Log10 renders log10(<e>), the same call [Log] makes, under the name
// that says which base it is.
func Log10(e any) drops.Expression { return funcCall("log10", []any{e}) }

// Log2 renders log2(<e>).
func Log2(e any) drops.Expression { return funcCall("log2", []any{e}) }

// LogBase renders log(<base>, <e>) — MySQL's two-argument LOG, whose
// arguments read base first.
func LogBase(base, e any) drops.Expression { return funcCall("log", []any{base, e}) }

// Greatest renders greatest(<args...>); Least renders least(<args...>).
//
// Both return NULL if any argument is NULL, on both servers — so a
// high-watermark update over a nullable column needs a coalesce around
// the column, not around the result.
func Greatest(args ...any) drops.Expression { return funcCall("greatest", args) }
func Least(args ...any) drops.Expression    { return funcCall("least", args) }

// Random renders rand(). Keeps pg's name — MySQL calls it RAND() — and
// answers the same range, a double in [0, 1).
func Random() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("rand()") })
}

// Trigonometric helpers — pass radians.
func Sin(e any) drops.Expression  { return funcCall("sin", []any{e}) }
func Cos(e any) drops.Expression  { return funcCall("cos", []any{e}) }
func Tan(e any) drops.Expression  { return funcCall("tan", []any{e}) }
func Asin(e any) drops.Expression { return funcCall("asin", []any{e}) }
func Acos(e any) drops.Expression { return funcCall("acos", []any{e}) }
func Atan(e any) drops.Expression { return funcCall("atan", []any{e}) }

// Plus, Minus, Mul, Div: arithmetic as parenthesised binary
// expressions, for column arithmetic in SELECT lists and updates.
func Plus(left, right any) drops.Expression  { return binOp(left, "+", right) }
func Minus(left, right any) drops.Expression { return binOp(left, "-", right) }
func Mul(left, right any) drops.Expression   { return binOp(left, "*", right) }

// Div renders (<left> / <right>). MySQL's division never truncates:
// two integers give a DECIMAL. [IntDiv] is the truncating form, and
// the one that matches what pg's / does between integers.
func Div(left, right any) drops.Expression { return binOp(left, "/", right) }

// IntDiv renders (<left> DIV <right>) — integer division, truncating
// toward zero.
func IntDiv(left, right any) drops.Expression { return binOp(left, "DIV", right) }
