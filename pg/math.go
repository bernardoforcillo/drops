package pg

import "github.com/bernardoforcillo/drops"

// PostgreSQL numeric functions.

func Abs(e any) drops.Expression   { return funcCall("abs", []any{e}) }
func Ceil(e any) drops.Expression  { return funcCall("ceil", []any{e}) }
func Floor(e any) drops.Expression { return funcCall("floor", []any{e}) }

// Round renders round(<e>) or round(<e>, <digits>) when digits is non-nil.
func Round(e any, digits ...int) drops.Expression {
	args := []any{e}
	if len(digits) > 0 {
		args = append(args, digits[0])
	}
	return funcCall("round", args)
}

func Mod(a, b any) drops.Expression   { return funcCall("mod", []any{a, b}) }
func Power(a, b any) drops.Expression { return funcCall("power", []any{a, b}) }
func Sqrt(e any) drops.Expression     { return funcCall("sqrt", []any{e}) }
func Sign(e any) drops.Expression     { return funcCall("sign", []any{e}) }
func Exp(e any) drops.Expression      { return funcCall("exp", []any{e}) }
func Ln(e any) drops.Expression       { return funcCall("ln", []any{e}) }
func Log(e any) drops.Expression      { return funcCall("log", []any{e}) }

// Greatest renders greatest(args...).
func Greatest(args ...any) drops.Expression { return funcCall("greatest", args) }

// Least renders least(args...).
func Least(args ...any) drops.Expression { return funcCall("least", args) }

// Random renders random().
func Random() drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) { b.WriteString("random()") })
}

// Trigonometric helpers — pass radians.
func Sin(e any) drops.Expression  { return funcCall("sin", []any{e}) }
func Cos(e any) drops.Expression  { return funcCall("cos", []any{e}) }
func Tan(e any) drops.Expression  { return funcCall("tan", []any{e}) }
func Asin(e any) drops.Expression { return funcCall("asin", []any{e}) }
func Acos(e any) drops.Expression { return funcCall("acos", []any{e}) }
func Atan(e any) drops.Expression { return funcCall("atan", []any{e}) }

// Plus, Minus, Mul, Div: arithmetic operators as parenthesised binary
// expressions. Useful for column-arithmetic in SELECT lists and updates.
func Plus(left, right any) drops.Expression  { return binOp(left, "+", right) }
func Minus(left, right any) drops.Expression { return binOp(left, "-", right) }
func Mul(left, right any) drops.Expression   { return binOp(left, "*", right) }
func Div(left, right any) drops.Expression   { return binOp(left, "/", right) }
