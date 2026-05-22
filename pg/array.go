package pg

import "github.com/bernardoforcillo/drops"

// PostgreSQL array operators and helpers.

// ArrayContains renders <a> @> <b> (array containment).
func ArrayContains(a, b any) drops.Expression { return binOp(a, "@>", b) }

// ArrayContainedIn renders <a> <@ <b>.
func ArrayContainedIn(a, b any) drops.Expression { return binOp(a, "<@", b) }

// ArrayOverlaps renders <a> && <b>.
func ArrayOverlaps(a, b any) drops.Expression { return binOp(a, "&&", b) }

// ArrayConcat renders <a> || <b>.
func ArrayConcat(a, b any) drops.Expression { return binOp(a, "||", b) }

// Any renders <value> = ANY(<array>).
func Any(value, array any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, value)
		b.WriteString(" = ANY(")
		writeOperand(b, array)
		b.WriteString("))")
	})
}

// All renders <value> = ALL(<array>).
func All(value, array any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		writeOperand(b, value)
		b.WriteString(" = ALL(")
		writeOperand(b, array)
		b.WriteString("))")
	})
}

// Aggregate / constructor / inspection functions ----------------------

func ArrayAgg(e any) drops.Expression           { return funcCall("array_agg", []any{e}) }
func ArrayLength(arr, dim any) drops.Expression { return funcCall("array_length", []any{arr, dim}) }
func ArrayUpper(arr, dim any) drops.Expression  { return funcCall("array_upper", []any{arr, dim}) }
func ArrayLower(arr, dim any) drops.Expression  { return funcCall("array_lower", []any{arr, dim}) }
func ArrayAppend(arr, v any) drops.Expression   { return funcCall("array_append", []any{arr, v}) }
func ArrayPrepend(v, arr any) drops.Expression  { return funcCall("array_prepend", []any{v, arr}) }
func ArrayRemove(arr, v any) drops.Expression   { return funcCall("array_remove", []any{arr, v}) }
func ArrayReplace(arr, oldV, newV any) drops.Expression {
	return funcCall("array_replace", []any{arr, oldV, newV})
}
func ArrayPosition(arr, v any) drops.Expression  { return funcCall("array_position", []any{arr, v}) }
func ArrayPositions(arr, v any) drops.Expression { return funcCall("array_positions", []any{arr, v}) }
func ArrayToString(arr, sep any) drops.Expression {
	return funcCall("array_to_string", []any{arr, sep})
}
func StringToArray(str, sep any) drops.Expression {
	return funcCall("string_to_array", []any{str, sep})
}
func Cardinality(arr any) drops.Expression { return funcCall("cardinality", []any{arr}) }
func Unnest(arr any) drops.Expression      { return funcCall("unnest", []any{arr}) }

// ArrayLit renders an ARRAY[...] literal. Values may be expressions or
// Go values (bound as params).
func ArrayLit(values ...any) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("ARRAY[")
		for i, v := range values {
			if i > 0 {
				b.WriteString(", ")
			}
			writeOperand(b, v)
		}
		b.WriteByte(']')
	})
}
