package pg

import "github.com/bernardoforcillo/drops"

// PostgreSQL JSON / JSONB operators and helpers.

// JSONGet renders <e> -> <key> (JSON object/array element, returns json).
// key may be a string (object key) or int (array index).
func JSONGet(e, key any) drops.Expression { return binOp(e, "->", key) }

// JSONGetText renders <e> ->> <key> (as text).
func JSONGetText(e, key any) drops.Expression { return binOp(e, "->>", key) }

// JSONGetPath renders <e> #> <path> (path is text[] — pass a Go []string).
// Renamed from JSONPath to free that name for the typed accessor in
// jsonpath.go.
func JSONGetPath(e, path any) drops.Expression { return binOp(e, "#>", path) }

// JSONGetPathText renders <e> #>> <path>.
func JSONGetPathText(e, path any) drops.Expression { return binOp(e, "#>>", path) }

// JSONBContains renders <a> @> <b> (does a contain b? jsonb only).
func JSONBContains(a, b any) drops.Expression { return binOp(a, "@>", b) }

// JSONBContainedIn renders <a> <@ <b>.
func JSONBContainedIn(a, b any) drops.Expression { return binOp(a, "<@", b) }

// JSONBHasKey renders <e> ? <key>.
func JSONBHasKey(e, key any) drops.Expression { return binOp(e, "?", key) }

// JSONBHasAnyKey renders <e> ?| <keys> (keys is text[]).
func JSONBHasAnyKey(e, keys any) drops.Expression { return binOp(e, "?|", keys) }

// JSONBHasAllKeys renders <e> ?& <keys>.
func JSONBHasAllKeys(e, keys any) drops.Expression { return binOp(e, "?&", keys) }

// JSONBConcat renders <a> || <b>.
func JSONBConcat(a, b any) drops.Expression { return binOp(a, "||", b) }

// JSONBDelete renders <e> - <key>.
func JSONBDelete(e, key any) drops.Expression { return binOp(e, "-", key) }

// Function helpers ----------------------------------------------------

func ToJSON(e any) drops.Expression           { return funcCall("to_json", []any{e}) }
func ToJSONB(e any) drops.Expression          { return funcCall("to_jsonb", []any{e}) }
func JSONArrayLength(e any) drops.Expression  { return funcCall("json_array_length", []any{e}) }
func JSONBArrayLength(e any) drops.Expression { return funcCall("jsonb_array_length", []any{e}) }
func JSONTypeof(e any) drops.Expression       { return funcCall("json_typeof", []any{e}) }
func JSONBTypeof(e any) drops.Expression      { return funcCall("jsonb_typeof", []any{e}) }

// JSONBuildObject renders json_build_object(args...). Pairs are key/value:
// JSONBuildObject("name", UserName, "age", UserAge).
func JSONBuildObject(args ...any) drops.Expression {
	return funcCall("json_build_object", args)
}

// JSONBuildArray renders json_build_array(args...).
func JSONBuildArray(args ...any) drops.Expression {
	return funcCall("json_build_array", args)
}

// JSONBBuildObject / JSONBBuildArray are the jsonb variants.
func JSONBBuildObject(args ...any) drops.Expression {
	return funcCall("jsonb_build_object", args)
}
func JSONBBuildArray(args ...any) drops.Expression {
	return funcCall("jsonb_build_array", args)
}

// JSONBSet renders jsonb_set(<target>, <path>, <value>, [<createMissing>]).
func JSONBSet(target, path, value any, createMissing ...bool) drops.Expression {
	args := []any{target, path, value}
	if len(createMissing) > 0 {
		args = append(args, createMissing[0])
	}
	return funcCall("jsonb_set", args)
}

// JSONBInsert renders jsonb_insert(<target>, <path>, <newVal>, [<insertAfter>]).
func JSONBInsert(target, path, newVal any, insertAfter ...bool) drops.Expression {
	args := []any{target, path, newVal}
	if len(insertAfter) > 0 {
		args = append(args, insertAfter[0])
	}
	return funcCall("jsonb_insert", args)
}

// JSONBStripNulls renders jsonb_strip_nulls(<e>).
func JSONBStripNulls(e any) drops.Expression { return funcCall("jsonb_strip_nulls", []any{e}) }

// JSONBPretty renders jsonb_pretty(<e>).
func JSONBPretty(e any) drops.Expression { return funcCall("jsonb_pretty", []any{e}) }

// JSONAgg / JSONBAgg are aggregates.
func JSONAgg(e any) drops.Expression  { return funcCall("json_agg", []any{e}) }
func JSONBAgg(e any) drops.Expression { return funcCall("jsonb_agg", []any{e}) }

// JSONObjectAgg / JSONBObjectAgg.
func JSONObjectAgg(k, v any) drops.Expression  { return funcCall("json_object_agg", []any{k, v}) }
func JSONBObjectAgg(k, v any) drops.Expression { return funcCall("jsonb_object_agg", []any{k, v}) }
