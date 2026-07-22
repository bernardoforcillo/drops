package sqlite

import "github.com/bernardoforcillo/drops"

// SQLite JSON1 functions and operators. SQLite stores JSON in TEXT (see
// the JSON/JSONB column constructors in types.go); these helpers query
// and transform it.
//
// The -> and ->> operators require SQLite 3.38+ (2022); the json_*
// functions are available whenever the JSON1 extension is compiled in
// (the default since 3.38, and common well before).

// JSONExtract renders json_extract(<e>, <path>) — path is a JSON path
// string such as "$.name" or "$.items[0]".
func JSONExtract(e, path any) drops.Expression { return funcCall("json_extract", []any{e, path}) }

// JSONGet renders <e> -> <path> (returns a JSON representation).
func JSONGet(e, path any) drops.Expression { return binOp(e, "->", path) }

// JSONGetText renders <e> ->> <path> (returns a SQL text/numeric value).
func JSONGetText(e, path any) drops.Expression { return binOp(e, "->>", path) }

// JSONArrayLength renders json_array_length(<e>) or, with a path,
// json_array_length(<e>, <path>).
func JSONArrayLength(e any, path ...any) drops.Expression {
	args := []any{e}
	if len(path) > 0 {
		args = append(args, path[0])
	}
	return funcCall("json_array_length", args)
}

// JSONType renders json_type(<e>) or json_type(<e>, <path>).
func JSONType(e any, path ...any) drops.Expression {
	args := []any{e}
	if len(path) > 0 {
		args = append(args, path[0])
	}
	return funcCall("json_type", args)
}

// JSONValid renders json_valid(<e>) — 1 when e is well-formed JSON.
func JSONValid(e any) drops.Expression { return funcCall("json_valid", []any{e}) }

// JSONQuote renders json_quote(<e>) — the JSON representation of a value.
func JSONQuote(e any) drops.Expression { return funcCall("json_quote", []any{e}) }

// JSONObject renders json_object(k1, v1, k2, v2, ...).
func JSONObject(args ...any) drops.Expression { return funcCall("json_object", args) }

// JSONArray renders json_array(args...).
func JSONArray(args ...any) drops.Expression { return funcCall("json_array", args) }

// JSONSet / JSONInsert / JSONReplace render the corresponding mutating
// functions: json_set(<e>, path, value, ...) and friends. Arguments
// after e are path/value pairs.
func JSONSet(e any, args ...any) drops.Expression {
	return funcCall("json_set", append([]any{e}, args...))
}
func JSONInsert(e any, args ...any) drops.Expression {
	return funcCall("json_insert", append([]any{e}, args...))
}
func JSONReplace(e any, args ...any) drops.Expression {
	return funcCall("json_replace", append([]any{e}, args...))
}

// JSONRemove renders json_remove(<e>, paths...).
func JSONRemove(e any, paths ...any) drops.Expression {
	return funcCall("json_remove", append([]any{e}, paths...))
}

// JSONPatch renders json_patch(<target>, <patch>) — RFC 7396 merge.
func JSONPatch(target, patch any) drops.Expression {
	return funcCall("json_patch", []any{target, patch})
}

// JSONGroupArray / JSONGroupObject are the JSON aggregate functions.
func JSONGroupArray(e any) drops.Expression     { return funcCall("json_group_array", []any{e}) }
func JSONGroupObject(k, v any) drops.Expression { return funcCall("json_group_object", []any{k, v}) }
