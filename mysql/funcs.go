package mysql

import "github.com/bernardoforcillo/drops"

// Aggregate and scalar function helpers, mirroring drops/pg's funcs.go.
// Each returns a drops.Expression usable in a SELECT list, GROUP BY,
// ORDER BY, HAVING or WHERE clause. Arguments may be drops.Expression
// (a column, another call) or a plain Go value, which is bound as a
// parameter.
//
// Count / CountAll / Sum / Avg / Min / Max / Func / Now already live in
// op.go; this file adds what pg has beyond them.
//
// # What pg has and MySQL cannot
//
// pg.Filter(agg, pred) renders the SQL-standard aggregate FILTER
// (WHERE …) clause. Neither MySQL nor MariaDB implements it — MariaDB
// 10.11 answers a syntax error at the '(' — and there is no rewrite
// that works for an arbitrary aggregate, because the predicate has to
// move *inside* the aggregate's argument. [CountIf] and [SumIf] are
// the two cases that do rewrite, spelled as their own functions so the
// shape of what is possible is visible at the call site.
//
// pg.BoolAnd / pg.BoolOr / pg.Every have no MySQL function either.
// MIN() and MAX() over a boolean expression are the exact equivalents
// — MySQL booleans being 1/0 integers — and are spelled [BoolAnd] and
// [BoolOr] here so a ported query keeps reading the same.

// funcCall is the shared renderer for "<name>(<args>)" expressions,
// taking its arguments already flattened into a slice. It is the same
// call [Func] makes; the slice form is what the helpers in this
// package need, since they build their argument lists.
func funcCall(name string, args []any) drops.Expression {
	return funcExpr(name, operandExprs(args))
}

// CountDistinct renders count(DISTINCT <e>).
func CountDistinct(e drops.Expression) drops.Expression { return distinctAgg("count", e) }

// SumDistinct / AvgDistinct apply DISTINCT to the aggregate.
func SumDistinct(e drops.Expression) drops.Expression { return distinctAgg("sum", e) }
func AvgDistinct(e drops.Expression) drops.Expression { return distinctAgg("avg", e) }

// distinctAgg renders <name>(DISTINCT <e>).
func distinctAgg(name string, e drops.Expression) drops.Expression {
	return &opExpr{parts: []string{name + "(DISTINCT ", ")"}, operands: []drops.Expression{e}}
}

// CountIf renders count(CASE WHEN <pred> THEN 1 END) — the conditional
// count that pg writes as count(*) FILTER (WHERE <pred>).
//
// The ELSE branch is deliberately absent: a CASE with no ELSE yields
// NULL, and count() does not count NULLs. Writing "ELSE 0" instead
// would count every row.
func CountIf(pred drops.Expression) drops.Expression {
	return &opExpr{
		parts:    []string{"count(CASE WHEN ", " THEN 1 END)"},
		operands: []drops.Expression{pred},
	}
}

// SumIf renders sum(CASE WHEN <pred> THEN <e> END) — pg's
// sum(<e>) FILTER (WHERE <pred>).
//
// As in [CountIf] there is no ELSE, so rows failing the predicate
// contribute NULL rather than zero. That matters at the edge: a group
// where no row matches sums to NULL, exactly as PostgreSQL's FILTER
// does, rather than to 0.
func SumIf(pred drops.Expression, e any) drops.Expression {
	return &opExpr{
		parts:    []string{"sum(CASE WHEN ", " THEN ", " END)"},
		operands: []drops.Expression{pred, operandExpr(e)},
	}
}

// GroupConcat renders group_concat(<e> SEPARATOR '<sep>') — MySQL's
// answer to PostgreSQL's string_agg.
//
// The separator is a Go string rather than an expression because the
// grammar demands a literal there: SEPARATOR ? is a syntax error on
// both servers, which is the kind of mistake that renders perfectly
// and only fails once a server sees it. It is written into the SQL
// with its quotes escaped.
//
// GroupConcat also carries a limit string_agg does not: the result is
// truncated at group_concat_max_len bytes, and truncation raises a
// warning rather than an error, so the query returns a short answer
// and succeeds. Stock MySQL 8.4 sets that to 1024; MariaDB 10.11 ships
// 1048576, so the same aggregate can be complete on one server and
// quietly cut off on the other. Raise it for the session before
// aggregating anything unbounded:
//
//	db.Exec(ctx, "SET SESSION group_concat_max_len = 1048576")
//
// Named for the function rather than for pg's StringAgg because
// neither the literal separator nor the truncation is a detail a
// reader should have to discover.
func GroupConcat(e any, sep string) drops.Expression {
	var o opBuilder
	o.text("group_concat(")
	o.value(e)
	o.text(separatorClause(sep) + ")")
	return o.done()
}

// GroupConcatDistinct is [GroupConcat] over the distinct values, in
// the order given.
func GroupConcatDistinct(e any, order []drops.Expression, sep string) drops.Expression {
	var o opBuilder
	o.text("group_concat(DISTINCT ")
	o.value(e)
	if len(order) > 0 {
		o.text(" ORDER BY ")
		o.list(order)
	}
	o.text(separatorClause(sep) + ")")
	return o.done()
}

// separatorClause renders " SEPARATOR '<sep>'". The separator is text
// in the statement rather than an operand because the grammar demands a
// literal there — see [GroupConcat].
func separatorClause(sep string) string {
	return " SEPARATOR '" + quoteLiteral(sep) + "'"
}

// BoolAnd reports whether every row satisfies e, as min(<e>) — see the
// note at the top of this file.
func BoolAnd(e any) drops.Expression { return funcCall("min", []any{e}) }

// BoolOr reports whether any row satisfies e, as max(<e>).
func BoolOr(e any) drops.Expression { return funcCall("max", []any{e}) }

// Lower / Upper case-folding helpers.
//
// Both fold according to the argument's collation, not to Unicode's
// default case mapping: under a binary collation they do nothing at
// all. That is a schema property, so a query that must fold
// predictably should say COLLATE explicitly.
func Lower(e any) drops.Expression { return funcCall("lower", []any{e}) }
func Upper(e any) drops.Expression { return funcCall("upper", []any{e}) }

// Coalesce renders coalesce(<args...>).
func Coalesce(args ...any) drops.Expression { return funcCall("coalesce", args) }

// NullIf renders nullif(<a>, <b>).
func NullIf(a, b any) drops.Expression { return funcCall("nullif", []any{a, b}) }

// IfNull renders ifnull(<a>, <b>) — the two-argument coalesce MySQL
// spells its own way. It has no PostgreSQL counterpart.
func IfNull(a, b any) drops.Expression { return funcCall("ifnull", []any{a, b}) }

// As renames an arbitrary expression: "<expr> AS <alias>". The method
// form on a column is (*Column).As.
func As(e drops.Expression, alias string) drops.Expression { return aliasExpr(e, alias) }

// Cast renders CAST(<e> AS <typ>) with typ written verbatim, so it
// must be one of the types MySQL's CAST accepts — SIGNED, UNSIGNED,
// CHAR, DECIMAL(p,s), DATE, DATETIME, TIME, BINARY.
//
// Note the list does not include the column types: MySQL's CAST
// vocabulary is its own, and CAST(x AS INT) is a syntax error where
// CAST(x AS SIGNED) is what was meant. JSON belongs to that list on
// MySQL 8 and not on MariaDB — see json.go.
func Cast(e any, typ string) drops.Expression {
	return &opExpr{
		parts:    []string{"CAST(", " AS " + typ + ")"},
		operands: []drops.Expression{operandExpr(e)},
	}
}
