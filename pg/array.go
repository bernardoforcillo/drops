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
//
// The array operand is held rather than closed over, so it may be a
// statement — Any(col, Subquery(sel)) — and is scoped as one. That is
// the same treatment AnySub gives its subquery, and the reason the two
// spellings no longer disagree about the tenant axis. See opExpr.
func Any(value, array any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " = ANY(", "))"},
		operands: []drops.Expression{operandExpr(value), operandExpr(array)},
	}
}

// All renders <value> = ALL(<array>). Its operands are held and scoped
// exactly as [Any]'s are.
func All(value, array any) drops.Expression {
	return &opExpr{
		parts:    []string{"(", " = ALL(", "))"},
		operands: []drops.Expression{operandExpr(value), operandExpr(array)},
	}
}

// Aggregate / constructor / inspection functions ----------------------
//
// These render through funcExpr: the arguments are held in a node the
// resolver walk can reach, so array_agg((SELECT ...)) over a scoped
// table carries that table's context filters. Every "<name>(<args>)"
// helper in the package is built from it now — see op.go.

func ArrayAgg(e any) drops.Expression           { return funcExpr("array_agg", []any{e}) }
func ArrayLength(arr, dim any) drops.Expression { return funcExpr("array_length", []any{arr, dim}) }
func ArrayUpper(arr, dim any) drops.Expression  { return funcExpr("array_upper", []any{arr, dim}) }
func ArrayLower(arr, dim any) drops.Expression  { return funcExpr("array_lower", []any{arr, dim}) }
func ArrayAppend(arr, v any) drops.Expression   { return funcExpr("array_append", []any{arr, v}) }
func ArrayPrepend(v, arr any) drops.Expression  { return funcExpr("array_prepend", []any{v, arr}) }
func ArrayRemove(arr, v any) drops.Expression   { return funcExpr("array_remove", []any{arr, v}) }
func ArrayReplace(arr, oldV, newV any) drops.Expression {
	return funcExpr("array_replace", []any{arr, oldV, newV})
}
func ArrayPosition(arr, v any) drops.Expression  { return funcExpr("array_position", []any{arr, v}) }
func ArrayPositions(arr, v any) drops.Expression { return funcExpr("array_positions", []any{arr, v}) }
func ArrayToString(arr, sep any) drops.Expression {
	return funcExpr("array_to_string", []any{arr, sep})
}
func StringToArray(str, sep any) drops.Expression {
	return funcExpr("string_to_array", []any{str, sep})
}
func Cardinality(arr any) drops.Expression { return funcExpr("cardinality", []any{arr}) }
func Unnest(arr any) drops.Expression      { return funcExpr("unnest", []any{arr}) }

// ArrayLit renders an ARRAY[...] literal. Values may be expressions or
// Go values (bound as params), and each is held rather than closed
// over, so a statement among them is scoped as one.
func ArrayLit(values ...any) drops.Expression {
	return listOp("ARRAY[", ", ", "]", operandExprs(values))
}
