package clickhouse_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// Conjunction bracketing: a predicate drops added on the statement's own
// account only guards anything if each conjunct beside it binds to
// itself.
//
// Every case here is a fragment drops did not build — a drops.Raw —
// because those are exactly the ones that need bracketing: every
// predicate this package builds already renders as one bracketed group.
// The control cases at the end are what keeps that true.

// bracketed renders a WHERE clause holding raw beside the tenant guard
// and reports the rendered statement.
func bracketedWhere(t *testing.T, raw string) string {
	t.Helper()
	db := clickhouse.New(nil)
	sql, _, err := db.Select(scHitID).From(scHits).
		Where(drops.Raw(raw)).ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	return sql
}

// The shape the whole thing is about. OR binds looser than AND, so
// "D AND a = 1 OR b = 2 AND T" parses as "(D AND a) OR (b AND T)" and
// returns every row matching the first half, for every tenant — with
// the tenant guard present in the SQL, which is why a review that looks
// for the predicate finds it and passes.
func TestTopLevelOrIsBracketed(t *testing.T) {
	sql := bracketedWhere(t, "a = 1 OR b = 2")
	if !strings.Contains(sql, `WHERE (a = 1 OR b = 2) AND ("hits"."tenantId" = ?)`) {
		t.Errorf("top-level OR was left bare: %s", sql)
	}
}

// The escaping shapes, each of which leaves the guard in the statement
// and binding nothing.
func TestEscapingConjunctsAreBracketed(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"or", "a = 1 OR b = 2"},
		{"or/lowercase", "a = 1 or b = 2"},
		{"line-comment", "a = 1 --"},
		// ClickHouse takes the shell spelling too, so a query file can
		// carry a shebang. No other dialect drops targets does.
		{"hash-comment", "a = 1 #"},
		{"shebang-comment", "a = 1 #!"},
		{"block-comment", "a = 1 /* x"},
		{"terminator", "a = 1;"},
		// The conditional operator binds looser than OR in ClickHouse,
		// so a top-level one takes the whole clause. Telling it from a
		// placeholder is the awkward part — see escapesConjunct.
		{"conditional", "a = 1 ? b : c"},
		{"or-after-literal", "a = 'x' OR b = 2"},
		{"or-after-quoted-ident", `"a" = 1 OR b = 2`},
		{"or-after-backtick-ident", "`a` = 1 OR b = 2"},
		// A backslash escapes the closing quote in ClickHouse — in all
		// three quoting forms, not just the string one — so a scanner
		// that only honoured doubling would think this literal had
		// ended and read the OR inside it as an operator. Bracketing it
		// is harmless; the case that matters is the control below.
		{"unterminated-literal", `a = 'x`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql := bracketedWhere(t, tc.raw)
			if !strings.Contains(sql, "WHERE ("+tc.raw+")") {
				t.Errorf("left bare: %s", sql)
			}
		})
	}
}

// The controls. Bracketing everything unconditionally would be correct
// and would double the parentheses in every statement drops has ever
// generated, so the check has to be precise in both directions.
func TestNonEscapingConjunctsAreLeftAlone(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"plain", "a = 1"},
		{"parenthesised-or", "(a = 1 OR b = 2)"},
		// OR inside a string literal is text, not an operator.
		{"or-in-literal", "a = 'x OR y'"},
		{"or-in-quoted-ident", `"col OR other" = 1`},
		// A backslash escapes the quote, so the literal has not ended
		// and the OR is still inside it.
		{"or-after-escaped-quote", `a = 'x\' OR y'`},
		// "OR" as part of a longer word is not the operator: it is in
		// FORMAT and in ORDERS, and bracketing on either would put
		// parentheses round most of the fragments a caller writes.
		{"or-inside-word", "FORMAT_hint = 1"},
		{"or-starting-word", "a = ORDERS_TOTAL"},
		// A fragment whose parentheses do not balance is reported safe
		// without being read, on purpose: bracketing "a = 1) OR (1 = 1"
		// would CLOSE the injected parenthesis and turn a statement the
		// server refuses into a working, unscoped OR.
		{"unbalanced", "a = 1) OR (1 = 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql := bracketedWhere(t, tc.raw)
			if !strings.Contains(sql, "WHERE "+tc.raw+" AND") {
				t.Errorf("bracketed unnecessarily: %s", sql)
			}
		})
	}
}

// A "?" that IS a placeholder must not be mistaken for the conditional
// operator, or every predicate drops builds would be bracketed. They
// are told apart by counting: an expression drops built renders exactly
// one "?" per bound argument.
func TestPlaceholdersAreNotMistakenForTheConditional(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := db.Select(scHitID).From(scHits).
		Where(clickhouse.Eq(scHitPath, "/"), clickhouse.Eq(scHitID, 1)).
		ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" WHERE ("hits"."path" = ?)`+
		` AND ("hits"."id" = ?) AND ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "/", 1, "acme")

	// A raw fragment that binds its own argument and carries no
	// conditional is left alone too.
	sql, _, err = db.Select(scHitID).From(scHits).
		Where(drops.Raw("a = 1")).ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if strings.Contains(sql, "WHERE (a = 1)") {
		t.Errorf("bracketed unnecessarily: %s", sql)
	}
}

// A lone conjunct is the whole clause: there is nothing on either side
// of it to reassociate, and this is the shape every single-predicate
// query in the package has. WHERE is safe because the guards are
// PREPENDED — see writeLeadingAnd for the clause where that is not
// enough.
func TestLoneWhereConjunctIsLeftAlone(t *testing.T) {
	db := clickhouse.New(nil)
	sql, _ := db.Select(scPublicID).From(scPublic).Where(drops.Raw("a = 1 OR b = 2")).ToSQL()
	checkSQL(t, sql, `SELECT "public"."id" FROM "public" WHERE a = 1 OR b = 2`)
}

// A connective is a clause in miniature and used to join its operands
// with a bare separator.
func TestConnectivesBracketTheirOperands(t *testing.T) {
	loose := drops.Raw("a OR b")
	got, _ := clickhouse.ToSQL(clickhouse.And(loose, clickhouse.Eq(scHitID, 1)))
	checkSQL(t, got, `((a OR b) AND ("hits"."id" = ?))`)

	// NOT binds tighter than every connective a caller can put inside
	// it, so "(NOT a OR b)" is "((NOT a) OR b)" — true for every row
	// matching b, tenant guard or no tenant guard.
	got, _ = clickhouse.ToSQL(clickhouse.Not(loose))
	checkSQL(t, got, `(NOT (a OR b))`)

	got, _ = clickhouse.ToSQL(clickhouse.Or(loose, clickhouse.Eq(scHitID, 1)))
	checkSQL(t, got, `((a OR b) OR ("hits"."id" = ?))`)
}

// An ON clause is a WHERE clause in a different place: a caller-written
// join condition whose top level is an OR reassociates the guards that
// follow it exactly as it would in a WHERE clause.
func TestJoinOnConditionIsBracketedBeforeTheGuard(t *testing.T) {
	db := clickhouse.New(nil)
	sql, _ := mustSQL(t,
		db.Select(scHitID).From(scHits).
			LeftJoin(scSessions, drops.Raw("a = 1 OR b = 2")),
		tenantCtx("acme"))
	if !strings.Contains(sql, `ON (a = 1 OR b = 2) AND ("sessions"."tenantId" = ?)`) {
		t.Errorf("ON condition was left bare: %s", sql)
	}
}

// ----------------------------------------------------------------------
// The census
// ----------------------------------------------------------------------

// TestNoExpressionConstructorSwallowsItsOperands reads this package's
// own source and fails when a drops.ExprFunc appears in a file whose
// job is to build expressions.
//
// It exists because the defect it guards against is invisible at the
// call site and reappears on the next helper somebody adds. An
// expression built as a closure renders correctly and is unreachable to
// the resolver, so a *SelectBuilder handed to it goes out with none of
// its tables' context filters — with no error, through the exported
// API. There is no test that fails when the NEXT constructor is written
// that way, unless it is this one.
//
// The exemptions are named individually with the reason each is not an
// operand-bearing constructor. Adding to that list is the deliberate
// act the test is asking for.
func TestNoExpressionConstructorSwallowsItsOperands(t *testing.T) {
	exempt := map[string]string{
		// DDL statements. They render a table declaration from a
		// *Table, take no caller expression as an operand, and there is
		// no ctx a CREATE TABLE could be resolved for.
		"ddl.go": "DDL statements hold no caller operand",
		// The alias rename wraps an ALREADY-resolved predicate tree for
		// the length of one render. Its operand was walked before the
		// wrapper was built — see Table.resolveFilterExprs.
		"tablescope.go": "the alias rename wraps an already-resolved tree",
		// andWith assembles an ON clause at render time out of pieces
		// the resolver has already been through.
		"conjunct.go": "andWith composes already-resolved predicates",
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, ok := exempt[name]; ok {
			continue
		}
		// Parsed rather than grepped, so that the doc comments
		// EXPLAINING why a closure is the wrong shape do not themselves
		// fail the census.
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "ExprFunc" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "drops" {
				return true
			}
			t.Errorf("%s:%d builds an expression as a closure; hold the operands in an opExpr "+
				"so the resolver can walk them, or add the file to the exemption list with a reason",
				name, fset.Position(sel.Pos()).Line)
			return true
		})
	}
	if checked < 20 {
		t.Fatalf("the census read only %d files; it is not looking where it thinks it is", checked)
	}
}

// Every operand position of the function helpers, swept in one place.
//
// TestStatementsInsideOperandsAreScoped covers the predicate shapes; this
// covers the function-call shapes, which are the ones a reader is least
// likely to think of as somewhere a statement goes.
func TestFunctionOperandsAreScoped(t *testing.T) {
	db := clickhouse.New(nil)
	sub := func() drops.Expression {
		return clickhouse.Subquery(db.Select(scSessionID).From(scSessions))
	}

	cases := map[string]drops.Expression{
		"Coalesce":       clickhouse.Coalesce(sub(), 0),
		"IfNull":         clickhouse.IfNull(sub(), 0),
		"Length":         clickhouse.Length(sub()),
		"Abs":            clickhouse.Abs(sub()),
		"Round":          clickhouse.Round(sub()),
		"Func":           clickhouse.Func("greatest", sub(), 1),
		"Sum":            clickhouse.Sum(sub()),
		"Count":          clickhouse.Count(sub()),
		"Quantile":       clickhouse.Quantile(0.5, sub()),
		"QuantileExact":  clickhouse.QuantileExact(0.5, sub()),
		"QuantileTiming": clickhouse.QuantileTiming(0.5, sub()),
		"ArgMax":         clickhouse.ArgMax(sub(), scHitID),
		"GroupArray":     clickhouse.GroupArray(sub()),
		"ToDate":         clickhouse.ToDate(sub()),
		"DateDiff":       clickhouse.DateDiff("day", sub(), clickhouse.Now()),
		"CastAs":         clickhouse.CastAs(sub(), "UInt64"),
		"As":             clickhouse.As(sub(), "n"),
		"Lag":            clickhouse.Lag(sub()),
		"Lead":           clickhouse.Lead(sub()),
		"FirstValue":     clickhouse.FirstValue(sub()),
		"NthValue":       clickhouse.NthValue(sub(), 1),
		"Over/order":     clickhouse.Over(clickhouse.CountAll(), clickhouse.WindowSpec().OrderBy(sub())),
		"CaseOn":         clickhouse.CaseOn(sub()).When(1, 2).End(),
		"Between/low":    clickhouse.Between(scPublicID, sub(), 10),
		"Like":           clickhouse.Like(scPublicID, sub()),
		"InSub":          clickhouse.InSub(scPublicID, db.Select(scSessionID).From(scSessions)),
		"Exists":         clickhouse.Exists(db.Select(scSessionID).From(scSessions)),
	}

	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			sql, _ := mustSQL(t, db.Select(e).From(scPublic), tenantCtx("acme"))
			if !strings.Contains(sql, `FROM "sessions" WHERE ("sessions"."tenantId" = ?)`) {
				t.Errorf("statement went out unscoped: %s", sql)
			}
			if _, _, err := db.Select(e).From(scPublic).ToSQLCtx(context.Background()); err == nil {
				t.Errorf("no tenant on ctx and no refusal")
			}
		})
	}
}
