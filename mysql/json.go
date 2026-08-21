package mysql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/bernardoforcillo/drops"
)

// MySQL JSON helpers, ported from drops/pg's json.go — which is where
// the two dialects are least alike, so this file is as much a map of
// what is *not* here as of what is.
//
// # One type, one path language, no operators
//
// PostgreSQL has two types (json and jsonb) and a dozen infix
// operators over them. MySQL has one JSON type and a function set. The
// helpers below therefore all render calls, and the pg names that
// describe an operator rather than an operation are gone:
//
//   - JSONBContains / JSONBContainedIn (@>, <@) become [JSONContains]
//     and [JSONContainedIn], which render JSON_CONTAINS with the
//     arguments in the order the operator had.
//   - JSONBHasKey and friends (?, ?|, ?&) become [JSONHasKey],
//     [JSONHasAnyKey] and [JSONHasAllKeys] over JSON_CONTAINS_PATH.
//     The operator itself could not survive the port for a second
//     reason: "?" is the parameter placeholder.
//   - JSONBConcat (||) is [JSONMergePatch] / [JSONMergePreserve].
//     MySQL splits pg's single || into two merge rules, and neither is
//     exactly it: JSON_MERGE_PATCH overwrites a duplicate key (which
//     is what || does) but also deletes a key whose new value is null,
//     which || does not.
//   - JSONBDelete (-) is [JSONRemove].
//
// One function goes the other way. JSON_QUERY is MariaDB's and not
// MySQL's — see [JSONQuery] — so it is the single helper in this file
// that does not work on both servers, and its doc says so rather than
// the file leaving it out, because MariaDB users have a use for it.
//
// MySQL's own -> and ->> operators are deliberately not exposed:
// MariaDB does not implement them at all — 10.11 answers a syntax
// error even against a JSON column — so [JSONGet] and [JSONGetText]
// render JSON_EXTRACT and JSON_UNQUOTE(JSON_EXTRACT(…)), which are the
// forms both servers take and exactly what the operators are shorthand
// for.
//
// # Left out, and why
//
//   - pg's ToJSON / ToJSONB. MySQL spells the cast CAST(x AS JSON);
//     MariaDB has no JSON type to cast to (its JSON is an alias for
//     LONGTEXT with a CHECK) and rejects the syntax outright. There is
//     no spelling that means the same thing on both, so drops does not
//     offer one. [JSONExtract](doc, "$") parses a JSON string on
//     either server if that is what you were after.
//   - pg's JSONBStripNulls. MySQL has no JSON_STRIP_NULLS and no
//     recursive rewrite that would stand in for it.
//   - pg's JSONBSet with createMissing=false. MySQL splits that switch
//     into two functions, so it is [JSONSet] and [JSONReplace] here.
//   - pg's JSONBInsert insertAfter flag. MySQL's JSON_INSERT decides
//     from the path, not from a flag.
//
// # Values that differ, not just spellings
//
// [JSONType] returns MySQL's vocabulary, which is upper case and finer
// grained than PostgreSQL's: INTEGER and DOUBLE where json_typeof says
// "number", OBJECT / ARRAY / STRING / BOOLEAN / NULL where it says the
// same words in lower case. Code that switches on the result has to be
// rewritten, not just re-pointed.

// JSONPath builds a MySQL JSON path expression from a sequence of
// keys: a string is an object member, an int an array index. A leading
// "$" is added.
//
//	JSONPath("b", "c")  // $."b"."c"
//	JSONPath("d", 0)    // $."d"[0]
//
// Member names are always quoted. MySQL only requires it for names
// that are not plain identifiers, but quoting unconditionally is what
// makes a key called "first name" — or "a.b" — address the key the
// caller meant rather than a path they did not write.
func JSONPath(keys ...any) string {
	var sb strings.Builder
	sb.WriteByte('$')
	for _, k := range keys {
		switch v := k.(type) {
		case string:
			sb.WriteString(`."`)
			sb.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v))
			sb.WriteByte('"')
		case int:
			sb.WriteByte('[')
			sb.WriteString(strconv.Itoa(v))
			sb.WriteByte(']')
		case int64:
			sb.WriteByte('[')
			sb.WriteString(strconv.FormatInt(v, 10))
			sb.WriteByte(']')
		default:
			panic(fmt.Sprintf("drops/mysql: JSON path element %v (%T) is neither a member name nor an array index", k, k))
		}
	}
	return sb.String()
}

// JSONGet renders JSON_EXTRACT(<e>, '<path>') for one key — a string
// member name or an int array index, as in PostgreSQL's ->.
//
// The result is a JSON value, so a string member comes back quoted:
// "x", not x. [JSONGetText] is the unquoting form.
func JSONGet(e, key any) drops.Expression { return JSONExtract(e, JSONPath(key)) }

// JSONGetText renders JSON_UNQUOTE(JSON_EXTRACT(<e>, '<path>')) — the
// ->> of PostgreSQL, and of MySQL for that matter.
//
// A path that matches nothing yields SQL NULL rather than the string
// "null", which is the same answer ->> gives.
func JSONGetText(e, key any) drops.Expression {
	return JSONUnquote(JSONExtract(e, JSONPath(key)))
}

// JSONGetPath walks a key sequence: pg's #> operator, whose right-hand
// side is a text[] there and a path expression here.
func JSONGetPath(e any, keys ...any) drops.Expression {
	return JSONExtract(e, JSONPath(keys...))
}

// JSONGetPathText is [JSONGetPath] unquoted — pg's #>>.
func JSONGetPathText(e any, keys ...any) drops.Expression {
	return JSONUnquote(JSONExtract(e, JSONPath(keys...)))
}

// JSONExtract renders JSON_EXTRACT(<e>, <paths...>) with the paths
// written as given. Passing more than one path collects the matches
// into a JSON array, which is MySQL's own extension and has no
// PostgreSQL counterpart.
//
// Paths bind as parameters. That is safe against injection but opaque
// to the optimiser: a functional index declared over
// JSON_EXTRACT(doc, '$.id') is only used when the path is a literal,
// so an indexed lookup wants drops.Raw for the path.
func JSONExtract(e any, paths ...any) drops.Expression {
	return funcCall("json_extract", append([]any{e}, paths...))
}

// JSONValue renders JSON_VALUE(<e>, <path>) — extract and unquote in
// one call, and unlike JSON_UNQUOTE(JSON_EXTRACT(…)) it can carry a
// RETURNING type. MySQL 8.0.21+ and MariaDB 10.2.3+; [JSONGetText] is
// the form that works further back.
func JSONValue(e, path any) drops.Expression { return funcCall("json_value", []any{e, path}) }

// JSONQuery renders JSON_QUERY(<e>, <path>) — like JSON_VALUE but for
// an object or array result rather than a scalar.
//
// MariaDB only. MySQL never implemented JSON_QUERY: it took JSON_VALUE
// from the same standard clause and stopped there, so 8.4 answers
// error 1305, "FUNCTION json_query does not exist". [JSONExtract] is
// the spelling that returns the same object or array on both, which is
// why it — and not this — is what [JSONGet] and [JSONGetPath] render.
func JSONQuery(e, path any) drops.Expression { return funcCall("json_query", []any{e, path}) }

// JSONUnquote renders JSON_UNQUOTE(<e>).
func JSONUnquote(e any) drops.Expression { return funcCall("json_unquote", []any{e}) }

// JSONQuote renders JSON_QUOTE(<e>) — the inverse: a string becomes a
// JSON string literal, escapes and all.
func JSONQuote(e any) drops.Expression { return funcCall("json_quote", []any{e}) }

// JSONContains renders JSON_CONTAINS(<target>, <candidate> [, <path>])
// — "does target contain candidate", the same direction as pg's @>.
//
// candidate must itself be valid JSON text, so a scalar goes in
// quoted: JSONContains(Doc, `"active"`) for a string, `1` for a
// number.
//
// The two servers part company on a candidate that is not JSON at all.
// MySQL raises error 3141, "Invalid JSON text in argument 2", and the
// statement fails. MariaDB returns NULL — no error, no warning — so
// the predicate is neither true nor false and a WHERE clause built on
// it quietly matches nothing. Do not rely on the argument being
// checked for you; quote the scalar.
func JSONContains(target, candidate any, path ...any) drops.Expression {
	args := []any{target, candidate}
	if len(path) > 0 {
		args = append(args, path[0])
	}
	return funcCall("json_contains", args)
}

// JSONContainedIn is [JSONContains] with the arguments swapped — pg's
// <@.
func JSONContainedIn(candidate, target any) drops.Expression {
	return funcCall("json_contains", []any{target, candidate})
}

// JSONHasKey reports whether one key is present — pg's ? operator,
// rendered JSON_CONTAINS_PATH(<e>, 'one', '<path>').
func JSONHasKey(e, key any) drops.Expression {
	return funcCall("json_contains_path", []any{e, "one", JSONPath(key)})
}

// JSONHasAnyKey reports whether any of the keys is present — pg's ?|.
func JSONHasAnyKey(e any, keys ...any) drops.Expression {
	return containsPath(e, "one", keys)
}

// JSONHasAllKeys reports whether every key is present — pg's ?&.
func JSONHasAllKeys(e any, keys ...any) drops.Expression {
	return containsPath(e, "all", keys)
}

func containsPath(e any, mode string, keys []any) drops.Expression {
	args := make([]any, 0, len(keys)+2)
	args = append(args, e, mode)
	for _, k := range keys {
		args = append(args, JSONPath(k))
	}
	return funcCall("json_contains_path", args)
}

// JSONMergePatch renders JSON_MERGE_PATCH(<docs...>) — RFC 7396 merge,
// where a duplicate key takes the later document's value. Closest to
// pg's || for jsonb, with the caveat noted at the top of this file: a
// key whose new value is null is *removed*, not set to null.
func JSONMergePatch(docs ...any) drops.Expression { return funcCall("json_merge_patch", docs) }

// JSONMergePreserve renders JSON_MERGE_PRESERVE(<docs...>), which
// turns a duplicate key's two values into an array rather than
// choosing between them.
func JSONMergePreserve(docs ...any) drops.Expression { return funcCall("json_merge_preserve", docs) }

// JSONSet renders JSON_SET(<target>, <path>, <value>, …) — replaces
// existing paths and creates missing ones, which is pg's jsonb_set
// with create_missing left at its default of true.
func JSONSet(target any, pathValues ...any) drops.Expression {
	return funcCall("json_set", append([]any{target}, pathValues...))
}

// JSONReplace renders JSON_REPLACE(…) — replaces only paths that
// already exist. pg spells this jsonb_set(..., create_missing => false).
func JSONReplace(target any, pathValues ...any) drops.Expression {
	return funcCall("json_replace", append([]any{target}, pathValues...))
}

// JSONInsert renders JSON_INSERT(…) — creates only paths that do not
// already exist; pg's jsonb_insert.
func JSONInsert(target any, pathValues ...any) drops.Expression {
	return funcCall("json_insert", append([]any{target}, pathValues...))
}

// JSONRemove renders JSON_REMOVE(<target>, <paths...>) — pg's - operator.
func JSONRemove(target any, paths ...any) drops.Expression {
	return funcCall("json_remove", append([]any{target}, paths...))
}

// JSONArrayLength renders JSON_LENGTH(<e>), which counts an array's
// elements or an object's top-level keys.
//
// pg splits these: json_array_length errors on an object. MySQL
// answers both from one function, and answers 1 for a scalar. The name
// keeps pg's because that is the question callers are asking; the doc
// is here because the error they may be relying on will not arrive.
func JSONArrayLength(e any) drops.Expression { return funcCall("json_length", []any{e}) }

// JSONLength is [JSONArrayLength] under MySQL's own name, optionally
// scoped to a path.
func JSONLength(e any, path ...any) drops.Expression {
	return funcCall("json_length", append([]any{e}, path...))
}

// JSONDepth renders JSON_DEPTH(<e>).
func JSONDepth(e any) drops.Expression { return funcCall("json_depth", []any{e}) }

// JSONKeys renders JSON_KEYS(<e> [, <path>]) — an array of an object's
// member names.
func JSONKeys(e any, path ...any) drops.Expression {
	return funcCall("json_keys", append([]any{e}, path...))
}

// JSONType renders JSON_TYPE(<e>) — pg's json_typeof, with a different
// vocabulary; see the note at the top of this file.
func JSONType(e any) drops.Expression { return funcCall("json_type", []any{e}) }

// JSONValid renders JSON_VALID(<e>). PostgreSQL has no counterpart
// because a json column cannot hold anything invalid; MariaDB's JSON
// being a LONGTEXT, it very much can.
func JSONValid(e any) drops.Expression { return funcCall("json_valid", []any{e}) }

// JSONPretty renders JSON_PRETTY(<e>) — pg's jsonb_pretty.
func JSONPretty(e any) drops.Expression { return funcCall("json_pretty", []any{e}) }

// JSONSearch renders JSON_SEARCH(<e>, <oneOrAll>, <search>) and
// returns the *path* to a matching string, not the value. The search
// string takes LIKE wildcards. No PostgreSQL counterpart.
func JSONSearch(e, oneOrAll, search any) drops.Expression {
	return funcCall("json_search", []any{e, oneOrAll, search})
}

// JSONOverlaps renders JSON_OVERLAPS(<a>, <b>) — true when the two
// documents share at least one element or key/value pair. MySQL
// 8.0.17+ and MariaDB 10.9+; older servers answer error 1305, no such
// function.
func JSONOverlaps(a, b any) drops.Expression { return funcCall("json_overlaps", []any{a, b}) }

// JSONBuildObject renders JSON_OBJECT(<k>, <v>, …) — pg's
// json_build_object. Unlike there, no argument needs a type
// annotation: MySQL resolves a bare placeholder from the value the
// driver binds.
func JSONBuildObject(args ...any) drops.Expression { return funcCall("json_object", args) }

// JSONBuildArray renders JSON_ARRAY(<args...>) — pg's json_build_array.
func JSONBuildArray(args ...any) drops.Expression { return funcCall("json_array", args) }

// JSONAgg renders JSON_ARRAYAGG(<e>) — pg's json_agg. MySQL 8.0.14+
// and MariaDB 10.5+. ([JSONObjectAgg] is the older of the pair on
// MySQL, 5.7.22; the two did not arrive together.)
//
// Neither server accepts an ORDER BY inside it, which PostgreSQL does,
// so the element order is whatever the plan produced. Sort the rows
// into a derived table first when the order is part of the answer.
func JSONAgg(e any) drops.Expression { return funcCall("json_arrayagg", []any{e}) }

// JSONObjectAgg renders JSON_OBJECTAGG(<k>, <v>) — pg's json_object_agg.
func JSONObjectAgg(k, v any) drops.Expression { return funcCall("json_objectagg", []any{k, v}) }

// JSONColumn is one column of a [JSONTable] projection.
type JSONColumn struct {
	name string
	typ  string
	path string
}

// JSONCol declares a JSON_TABLE column: the name it takes in the
// derived table, the SQL type it is read as, and the path it is read
// from. typ is written verbatim into the DDL-shaped column list, so it
// must be a type MySQL's JSON_TABLE accepts — INT, VARCHAR(n),
// DATETIME, JSON.
func JSONCol(name, typ, path string) JSONColumn {
	mustIdent("JSON_TABLE column", name)
	return JSONColumn{name: name, typ: typ, path: path}
}

// JSONTable renders a JSON_TABLE(…) derived table, to be passed to
// (*SelectBuilder).FromExpr — the way to turn a JSON array into rows,
// which is what PostgreSQL's json_array_elements and jsonb_to_recordset
// do and what nothing else in MySQL does.
//
//	sel := db.Select(drops.Raw("`tags`.`name`")).
//	    FromExpr(mysql.JSONTable(doc, "$.tags[*]", "tags",
//	        mysql.JSONCol("name", "VARCHAR(64)", "$.name")))
//
// MySQL 8.0+ and MariaDB 10.6+. The row path and each column path must
// be literals — the server parses them while planning — so they are
// written into the SQL rather than bound, and both are rejected here
// unless they look like a JSON path.
func JSONTable(doc any, rowPath string, alias string, cols ...JSONColumn) drops.Expression {
	mustIdent("JSON_TABLE alias", alias)
	mustJSONPath(rowPath)
	for _, c := range cols {
		mustJSONPath(c.path)
	}
	// Only the document is an operand — a caller can hand that one a
	// scalar subquery, and it is walked like any other operand. The
	// COLUMNS list is text: every part of it is an identifier or a JSON
	// path this function has already validated, and none of it is a
	// place a statement can be written.
	var o opBuilder
	o.text("JSON_TABLE(")
	o.value(doc)
	o.text(", '" + quoteLiteral(rowPath) + "' COLUMNS (")
	for i, c := range cols {
		if i > 0 {
			o.text(", ")
		}
		o.text(quoteIdent(c.name) + " " + c.typ + " PATH '" + quoteLiteral(c.path) + "'")
	}
	o.text("))")
	node := o.done()
	node.alias = alias
	return node
}

// mustJSONPath rejects anything that is not shaped like a JSON path.
// The paths JSON_TABLE takes cannot be bound, so they are interpolated
// — and a value that is interpolated has to be checked at the door.
func mustJSONPath(p string) {
	if p == "" || p[0] != '$' {
		panic(fmt.Sprintf("drops/mysql: %q is not a JSON path; it must start with $", p))
	}
	if strings.ContainsAny(p, "\x00\n\r") {
		panic(fmt.Sprintf("drops/mysql: JSON path %q contains a control character", p))
	}
}
