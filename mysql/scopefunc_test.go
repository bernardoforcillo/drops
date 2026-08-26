package mysql_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// The expression package, held rather than closed over.
//
// An operand a caller can hand a statement to must not be an opaque
// closure. Every constructor in this package used to swallow its
// arguments in a drops.ExprFunc, so the resolver walk terminated at the
// closure and the statement inside rendered through WriteSQL, which has
// no ctx: it wrote its DefaultFilters, none of its ContextFilters, and
// refused nothing on a ctx with no tenant at all. These render, and
// send, today only because the operands are held:
//
//	SELECT coalesce((SELECT `posts`.`id` FROM `posts`), ?) FROM `plain`
//	SELECT count(CASE WHEN EXISTS (SELECT … FROM `posts`) THEN 1 END) …
//	SELECT row_number() OVER (PARTITION BY (SELECT … FROM `posts`)) …
//	SELECT CAST((SELECT … FROM `posts`) AS SIGNED) FROM `plain`
//	SELECT * FROM `plain` WHERE (`plain`.`id` IN ((SELECT … FROM `posts`)))
//
// mysql.Coalesce(mysql.Subquery(sel), 0) in a projection is an ordinary
// scalar-subquery idiom, not an exotic shape.
//
// The tests are organised by the invariant rather than by the list of
// helpers that leaked, because a list is what goes stale:
//
//   - every exported operand-taking constructor is exercised with a
//     scoped subquery, and the table is checked against the package's
//     own source so a helper added later cannot quietly miss it;
//   - the same table is run again on a bare ctx, where each must refuse
//     rather than render;
//   - a corpus of the same helpers with NO statement inside pins the
//     rendered SQL and args byte for byte, because a scoping fix that
//     rewrote every query log would not be worth having.

// fnTenant differs from scopeTenant so a stray helper cannot pass by
// coincidence.
const fnTenant = int64(23)

func fnCtx() context.Context { return mysql.WithTenant(context.Background(), fnTenant) }

// operandCase is one exported constructor, built with a scoped subquery
// in an operand position a caller can reach.
//
// name is the constructor's Go name, optionally followed by a space and
// a note naming which operand position the case covers; the census
// below reads the first word.
type operandCase struct {
	name  string
	build func(sub drops.Expression) drops.Expression
}

// operandConstructors is the table the census enforces: one entry per
// exported function in package mysql that returns a drops.Expression
// and takes an operand a caller can hand a statement to.
//
// It is a table of CONSTRUCTORS rather than of rendered statements
// because the invariant is about the constructor: whatever it builds
// must keep the operand reachable. What each one renders is pinned
// separately, in TestExpressionsRenderUnchanged.
func operandConstructors(col drops.Expression) []operandCase {
	return []operandCase{
		{"Abs", func(s drops.Expression) drops.Expression { return mysql.Abs(s) }},
		{"Acos", func(s drops.Expression) drops.Expression { return mysql.Acos(s) }},
		{"AllSub", func(s drops.Expression) drops.Expression { return mysql.AllSub(col, s) }},
		{"And", func(s drops.Expression) drops.Expression { return mysql.And(s, drops.Raw("1")) }},
		{"AnySub", func(s drops.Expression) drops.Expression { return mysql.AnySub(col, s) }},
		{"As", func(s drops.Expression) drops.Expression { return mysql.As(s, "a") }},
		{"Asin", func(s drops.Expression) drops.Expression { return mysql.Asin(s) }},
		{"Atan", func(s drops.Expression) drops.Expression { return mysql.Atan(s) }},
		{"Avg", func(s drops.Expression) drops.Expression { return mysql.Avg(s) }},
		{"AvgDistinct", func(s drops.Expression) drops.Expression { return mysql.AvgDistinct(s) }},
		{"Between", func(s drops.Expression) drops.Expression { return mysql.Between(s, col, col) }},
		{"BoolAnd", func(s drops.Expression) drops.Expression { return mysql.BoolAnd(s) }},
		{"BoolOr", func(s drops.Expression) drops.Expression { return mysql.BoolOr(s) }},
		{"Cast", func(s drops.Expression) drops.Expression { return mysql.Cast(s, "SIGNED") }},
		{"Ceil", func(s drops.Expression) drops.Expression { return mysql.Ceil(s) }},
		{"CharLength", func(s drops.Expression) drops.Expression { return mysql.CharLength(s) }},
		{"CmpAll", func(s drops.Expression) drops.Expression { return mysql.CmpAll(col, ">", s) }},
		{"CmpAny", func(s drops.Expression) drops.Expression { return mysql.CmpAny(col, "<", s) }},
		{"Coalesce", func(s drops.Expression) drops.Expression { return mysql.Coalesce(s, int64(0)) }},
		{"Collate", func(s drops.Expression) drops.Expression { return mysql.Collate(s, "utf8mb4_bin") }},
		{"Concat", func(s drops.Expression) drops.Expression { return mysql.Concat(s) }},
		{"ConcatWS", func(s drops.Expression) drops.Expression { return mysql.ConcatWS(s, col) }},
		{"ConvertTZ", func(s drops.Expression) drops.Expression { return mysql.ConvertTZ(s, col, col) }},
		{"Cos", func(s drops.Expression) drops.Expression { return mysql.Cos(s) }},
		{"Count", func(s drops.Expression) drops.Expression { return mysql.Count(s) }},
		{"CountDistinct", func(s drops.Expression) drops.Expression { return mysql.CountDistinct(s) }},
		{"CountIf", func(s drops.Expression) drops.Expression { return mysql.CountIf(s) }},
		{"DateAdd", func(s drops.Expression) drops.Expression { return mysql.DateAdd(s, col) }},
		{"DateFormat", func(s drops.Expression) drops.Expression { return mysql.DateFormat(s, col) }},
		{"DateSub", func(s drops.Expression) drops.Expression { return mysql.DateSub(s, col) }},
		{"DateTrunc", func(s drops.Expression) drops.Expression { return mysql.DateTrunc("day", s) }},
		{"DateTrunc week", func(s drops.Expression) drops.Expression { return mysql.DateTrunc("week", s) }},
		{"DateTrunc quarter", func(s drops.Expression) drops.Expression { return mysql.DateTrunc("quarter", s) }},
		{"DayOfWeek", func(s drops.Expression) drops.Expression { return mysql.DayOfWeek(s) }},
		{"DayOfYear", func(s drops.Expression) drops.Expression { return mysql.DayOfYear(s) }},
		{"Div", func(s drops.Expression) drops.Expression { return mysql.Div(s, col) }},
		{"Eq", func(s drops.Expression) drops.Expression { return mysql.Eq(s, col) }},
		{"Exists", func(s drops.Expression) drops.Expression { return mysql.Exists(s) }},
		{"Exp", func(s drops.Expression) drops.Expression { return mysql.Exp(s) }},
		{"Extract", func(s drops.Expression) drops.Expression { return mysql.Extract("YEAR", s) }},
		{"FirstValue", func(s drops.Expression) drops.Expression { return mysql.FirstValue(s) }},
		{"Floor", func(s drops.Expression) drops.Expression { return mysql.Floor(s) }},
		{"FromBase64", func(s drops.Expression) drops.Expression { return mysql.FromBase64(s) }},
		{"FromUnixTime", func(s drops.Expression) drops.Expression { return mysql.FromUnixTime(s) }},
		{"Func", func(s drops.Expression) drops.Expression { return mysql.Func("f", s) }},
		{"Greatest", func(s drops.Expression) drops.Expression { return mysql.Greatest(s) }},
		{"GroupConcat", func(s drops.Expression) drops.Expression { return mysql.GroupConcat(s, ",") }},
		{"GroupConcatDistinct", func(s drops.Expression) drops.Expression {
			return mysql.GroupConcatDistinct(col, []drops.Expression{s}, ",")
		}},
		{"Gt", func(s drops.Expression) drops.Expression { return mysql.Gt(s, col) }},
		{"Gte", func(s drops.Expression) drops.Expression { return mysql.Gte(s, col) }},
		{"Hex", func(s drops.Expression) drops.Expression { return mysql.Hex(s) }},
		{"IfNull", func(s drops.Expression) drops.Expression { return mysql.IfNull(s, col) }},
		{"In value", func(s drops.Expression) drops.Expression { return mysql.In(col, s) }},
		{"InSub", func(s drops.Expression) drops.Expression { return mysql.InSub(col, s) }},
		{"IntDiv", func(s drops.Expression) drops.Expression { return mysql.IntDiv(s, col) }},
		{"Interval", func(s drops.Expression) drops.Expression { return mysql.Interval(s, "DAY") }},
		{"IsNotNull", func(s drops.Expression) drops.Expression { return mysql.IsNotNull(s) }},
		{"IsNull", func(s drops.Expression) drops.Expression { return mysql.IsNull(s) }},
		{"JSONAgg", func(s drops.Expression) drops.Expression { return mysql.JSONAgg(s) }},
		{"JSONArrayLength", func(s drops.Expression) drops.Expression { return mysql.JSONArrayLength(s) }},
		{"JSONBuildArray", func(s drops.Expression) drops.Expression { return mysql.JSONBuildArray(s) }},
		{"JSONBuildObject", func(s drops.Expression) drops.Expression { return mysql.JSONBuildObject(s) }},
		{"JSONContainedIn", func(s drops.Expression) drops.Expression { return mysql.JSONContainedIn(s, col) }},
		{"JSONContains", func(s drops.Expression) drops.Expression { return mysql.JSONContains(s, col, col) }},
		{"JSONDepth", func(s drops.Expression) drops.Expression { return mysql.JSONDepth(s) }},
		{"JSONExtract", func(s drops.Expression) drops.Expression { return mysql.JSONExtract(s, col) }},
		{"JSONGet", func(s drops.Expression) drops.Expression { return mysql.JSONGet(s, "a") }},
		{"JSONGetPath", func(s drops.Expression) drops.Expression { return mysql.JSONGetPath(s, "a") }},
		{"JSONGetPathText", func(s drops.Expression) drops.Expression { return mysql.JSONGetPathText(s, "a") }},
		{"JSONGetText", func(s drops.Expression) drops.Expression { return mysql.JSONGetText(s, "a") }},
		{"JSONHasAllKeys", func(s drops.Expression) drops.Expression { return mysql.JSONHasAllKeys(s, "a") }},
		{"JSONHasAnyKey", func(s drops.Expression) drops.Expression { return mysql.JSONHasAnyKey(s, "a") }},
		{"JSONHasKey", func(s drops.Expression) drops.Expression { return mysql.JSONHasKey(s, "a") }},
		{"JSONInsert", func(s drops.Expression) drops.Expression { return mysql.JSONInsert(s, col) }},
		{"JSONKeys", func(s drops.Expression) drops.Expression { return mysql.JSONKeys(s, col) }},
		{"JSONLength", func(s drops.Expression) drops.Expression { return mysql.JSONLength(s, col) }},
		{"JSONMergePatch", func(s drops.Expression) drops.Expression { return mysql.JSONMergePatch(s) }},
		{"JSONMergePreserve", func(s drops.Expression) drops.Expression { return mysql.JSONMergePreserve(s) }},
		{"JSONObjectAgg", func(s drops.Expression) drops.Expression { return mysql.JSONObjectAgg(s, col) }},
		{"JSONOverlaps", func(s drops.Expression) drops.Expression { return mysql.JSONOverlaps(s, col) }},
		{"JSONPretty", func(s drops.Expression) drops.Expression { return mysql.JSONPretty(s) }},
		{"JSONQuery", func(s drops.Expression) drops.Expression { return mysql.JSONQuery(s, col) }},
		{"JSONQuote", func(s drops.Expression) drops.Expression { return mysql.JSONQuote(s) }},
		{"JSONRemove", func(s drops.Expression) drops.Expression { return mysql.JSONRemove(s, col) }},
		{"JSONReplace", func(s drops.Expression) drops.Expression { return mysql.JSONReplace(s, col) }},
		{"JSONSearch", func(s drops.Expression) drops.Expression { return mysql.JSONSearch(s, col, col) }},
		{"JSONSet", func(s drops.Expression) drops.Expression { return mysql.JSONSet(s, col) }},
		{"JSONTable", func(s drops.Expression) drops.Expression {
			return mysql.JSONTable(s, "$[*]", "jt", mysql.JSONCol("a", "INT", "$.a"))
		}},
		{"JSONType", func(s drops.Expression) drops.Expression { return mysql.JSONType(s) }},
		{"JSONUnquote", func(s drops.Expression) drops.Expression { return mysql.JSONUnquote(s) }},
		{"JSONValid", func(s drops.Expression) drops.Expression { return mysql.JSONValid(s) }},
		{"JSONValue", func(s drops.Expression) drops.Expression { return mysql.JSONValue(s, col) }},
		{"LPad", func(s drops.Expression) drops.Expression { return mysql.LPad(s, col, col) }},
		{"LTrim", func(s drops.Expression) drops.Expression { return mysql.LTrim(s) }},
		{"Lag", func(s drops.Expression) drops.Expression { return mysql.Lag(s, col) }},
		{"LastDay", func(s drops.Expression) drops.Expression { return mysql.LastDay(s) }},
		{"LastValue", func(s drops.Expression) drops.Expression { return mysql.LastValue(s) }},
		{"Lead", func(s drops.Expression) drops.Expression { return mysql.Lead(s, col) }},
		{"Least", func(s drops.Expression) drops.Expression { return mysql.Least(s) }},
		{"Left", func(s drops.Expression) drops.Expression { return mysql.Left(s, col) }},
		{"Length", func(s drops.Expression) drops.Expression { return mysql.Length(s) }},
		{"Like", func(s drops.Expression) drops.Expression { return mysql.Like(s, col) }},
		{"Ln", func(s drops.Expression) drops.Expression { return mysql.Ln(s) }},
		{"Locate", func(s drops.Expression) drops.Expression { return mysql.Locate(s, col, col) }},
		{"Log", func(s drops.Expression) drops.Expression { return mysql.Log(s) }},
		{"Log10", func(s drops.Expression) drops.Expression { return mysql.Log10(s) }},
		{"Log2", func(s drops.Expression) drops.Expression { return mysql.Log2(s) }},
		{"LogBase", func(s drops.Expression) drops.Expression { return mysql.LogBase(s, col) }},
		{"Lower", func(s drops.Expression) drops.Expression { return mysql.Lower(s) }},
		{"Lt", func(s drops.Expression) drops.Expression { return mysql.Lt(s, col) }},
		{"Lte", func(s drops.Expression) drops.Expression { return mysql.Lte(s, col) }},
		{"MakeDate", func(s drops.Expression) drops.Expression { return mysql.MakeDate(s, col, col) }},
		{"MakeTime", func(s drops.Expression) drops.Expression { return mysql.MakeTime(s, col, col) }},
		{"MakeTimestamp", func(s drops.Expression) drops.Expression { return mysql.MakeTimestamp(s, col, col, col, col, col) }},
		{"Max", func(s drops.Expression) drops.Expression { return mysql.Max(s) }},
		{"Md5", func(s drops.Expression) drops.Expression { return mysql.Md5(s) }},
		{"Min", func(s drops.Expression) drops.Expression { return mysql.Min(s) }},
		{"Minus", func(s drops.Expression) drops.Expression { return mysql.Minus(s, col) }},
		{"Mod", func(s drops.Expression) drops.Expression { return mysql.Mod(s, col) }},
		{"Mul", func(s drops.Expression) drops.Expression { return mysql.Mul(s, col) }},
		{"Ne", func(s drops.Expression) drops.Expression { return mysql.Ne(s, col) }},
		{"Not", func(s drops.Expression) drops.Expression { return mysql.Not(s) }},
		{"NotExists", func(s drops.Expression) drops.Expression { return mysql.NotExists(s) }},
		{"NotIn value", func(s drops.Expression) drops.Expression { return mysql.NotIn(col, s) }},
		{"NotInSub", func(s drops.Expression) drops.Expression { return mysql.NotInSub(col, s) }},
		{"NthValue", func(s drops.Expression) drops.Expression { return mysql.NthValue(s, col) }},
		{"Ntile", func(s drops.Expression) drops.Expression { return mysql.Ntile(s) }},
		{"NullIf", func(s drops.Expression) drops.Expression { return mysql.NullIf(s, col) }},
		{"OctetLength", func(s drops.Expression) drops.Expression { return mysql.OctetLength(s) }},
		{"Or", func(s drops.Expression) drops.Expression { return mysql.Or(s, drops.Raw("1")) }},
		{"Over", func(s drops.Expression) drops.Expression { return mysql.Over(s, nil) }},
		{"Over partition", func(s drops.Expression) drops.Expression {
			return mysql.Over(mysql.RowNumber(), mysql.WindowSpec().PartitionBy(s))
		}},
		{"Over order", func(s drops.Expression) drops.Expression {
			return mysql.Over(mysql.RowNumber(), mysql.WindowSpec().OrderBy(s))
		}},
		{"Plus", func(s drops.Expression) drops.Expression { return mysql.Plus(s, col) }},
		{"Position", func(s drops.Expression) drops.Expression { return mysql.Position(s, col) }},
		{"Power", func(s drops.Expression) drops.Expression { return mysql.Power(s, col) }},
		{"Quarter", func(s drops.Expression) drops.Expression { return mysql.Quarter(s) }},
		{"RPad", func(s drops.Expression) drops.Expression { return mysql.RPad(s, col, col) }},
		{"RTrim", func(s drops.Expression) drops.Expression { return mysql.RTrim(s) }},
		{"RegexpInstr", func(s drops.Expression) drops.Expression { return mysql.RegexpInstr(s, col) }},
		{"RegexpLike", func(s drops.Expression) drops.Expression { return mysql.RegexpLike(s, col) }},
		{"RegexpReplace", func(s drops.Expression) drops.Expression { return mysql.RegexpReplace(s, col, col) }},
		{"RegexpSubstr", func(s drops.Expression) drops.Expression { return mysql.RegexpSubstr(s, col) }},
		{"Repeat", func(s drops.Expression) drops.Expression { return mysql.Repeat(s, col) }},
		{"Replace", func(s drops.Expression) drops.Expression { return mysql.Replace(s, col, col) }},
		{"Reverse", func(s drops.Expression) drops.Expression { return mysql.Reverse(s) }},
		{"Right", func(s drops.Expression) drops.Expression { return mysql.Right(s, col) }},
		{"Round", func(s drops.Expression) drops.Expression { return mysql.Round(s, 2) }},
		{"Sha1", func(s drops.Expression) drops.Expression { return mysql.Sha1(s) }},
		{"Sha2", func(s drops.Expression) drops.Expression { return mysql.Sha2(s, col) }},
		{"Sign", func(s drops.Expression) drops.Expression { return mysql.Sign(s) }},
		{"Sin", func(s drops.Expression) drops.Expression { return mysql.Sin(s) }},
		{"Sqrt", func(s drops.Expression) drops.Expression { return mysql.Sqrt(s) }},
		{"StrPos", func(s drops.Expression) drops.Expression { return mysql.StrPos(s, col) }},
		{"StrToDate", func(s drops.Expression) drops.Expression { return mysql.StrToDate(s, col) }},
		{"Subquery", func(s drops.Expression) drops.Expression { return mysql.Subquery(s) }},
		{"Substring", func(s drops.Expression) drops.Expression { return mysql.Substring(s, col, col) }},
		{"SubstringIndex", func(s drops.Expression) drops.Expression { return mysql.SubstringIndex(s, col, col) }},
		{"Sum", func(s drops.Expression) drops.Expression { return mysql.Sum(s) }},
		{"SumDistinct", func(s drops.Expression) drops.Expression { return mysql.SumDistinct(s) }},
		{"SumIf", func(s drops.Expression) drops.Expression { return mysql.SumIf(s, col) }},
		{"Tan", func(s drops.Expression) drops.Expression { return mysql.Tan(s) }},
		{"TimestampDiff", func(s drops.Expression) drops.Expression { return mysql.TimestampDiff("DAY", s, col) }},
		{"ToBase64", func(s drops.Expression) drops.Expression { return mysql.ToBase64(s) }},
		{"Trim", func(s drops.Expression) drops.Expression { return mysql.Trim(s) }},
		{"TrimChars", func(s drops.Expression) drops.Expression { return mysql.TrimChars("BOTH", s, col) }},
		{"Truncate", func(s drops.Expression) drops.Expression { return mysql.Truncate(s, col) }},
		{"Unhex", func(s drops.Expression) drops.Expression { return mysql.Unhex(s) }},
		{"UnixTimestamp", func(s drops.Expression) drops.Expression { return mysql.UnixTimestamp(s) }},
		{"Upper", func(s drops.Expression) drops.Expression { return mysql.Upper(s) }},
		{"Weekday", func(s drops.Expression) drops.Expression { return mysql.Weekday(s) }},
	}
}

// scopedSubqueryFixture builds the scoped table, its column and a plain
// outer table the corpus can select from.
func scopedSubqueryFixture(t *testing.T) (*mysql.DB, *mysql.Table, drops.Expression, *mysql.Table) {
	t.Helper()
	posts, postID, _ := scopedTable("posts")
	plain, _ := plainTable("plain")
	return mysql.New(dropstest.New()), posts, postID, plain
}

// Every constructor, handed a scoped SELECT, renders it with the tenant
// predicate — because the operand is held in a field and the resolver
// walks it, not because anybody enumerated the shapes.
func TestEveryOperandTakingConstructorCarriesTheAxis(t *testing.T) {
	db, posts, postID, plain := scopedSubqueryFixture(t)
	for _, c := range operandConstructors(plain.Col("id")) {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sub := db.Select(postID).From(posts)
			sql, args, err := db.Select(mysql.As(c.build(sub), "v")).From(plain).ToSQLCtx(fnCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if !strings.Contains(sql, "`posts`.`tenantId` = ?") {
				t.Fatalf("the embedded statement went out unscoped: %s", sql)
			}
			var found bool
			for _, a := range args {
				if a == fnTenant {
					found = true
				}
			}
			if !found {
				t.Fatalf("the tenant was not bound: %v in %s", args, sql)
			}
		})
	}
}

// The same table on a ctx with no tenant. Rendering one of these is the
// fail-open the whole feature exists to prevent, so each must refuse.
func TestEveryOperandTakingConstructorRefusesWithoutATenant(t *testing.T) {
	db, posts, postID, plain := scopedSubqueryFixture(t)
	for _, c := range operandConstructors(plain.Col("id")) {
		c := c
		t.Run(c.name, func(t *testing.T) {
			sub := db.Select(postID).From(posts)
			sql, _, err := db.Select(mysql.As(c.build(sub), "v")).From(plain).
				ToSQLCtx(context.Background())
			if !errors.Is(err, mysql.ErrTenantMissing) {
				t.Fatalf("err = %v, want ErrTenantMissing (rendered %q)", err, sql)
			}
		})
	}
}

// The census: every exported function in the package that returns a
// drops.Expression and takes an operand a caller can hand a statement
// to has to be in the table above.
//
// It reads the package's own source rather than a list somebody
// maintains, because the failure this whole round is about is a shape
// nobody thought to enumerate. A helper added next year is covered on
// the day it is written, or this test fails.
func TestEveryExportedOperandConstructorIsCensused(t *testing.T) {
	covered := map[string]bool{}
	for _, c := range operandConstructors(drops.Raw("x")) {
		covered[strings.Fields(c.name)[0]] = true
	}
	for _, name := range exportedOperandConstructors(t) {
		if !covered[name] {
			t.Errorf("mysql.%s takes an operand a statement can be written into "+
				"and is not in operandConstructors", name)
		}
	}
}

// exportedOperandConstructors parses the package source and returns the
// exported functions that return a drops.Expression and accept at least
// one operand parameter — an `any`, a drops.Expression, or a variadic
// of either. Those are exactly the positions a caller can hand a
// *SelectBuilder to.
func exportedOperandConstructors(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range src.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 ||
				exprTypeName(fn.Type.Results.List[0].Type) != "drops.Expression" {
				continue
			}
			for _, p := range fn.Type.Params.List {
				switch exprTypeName(p.Type) {
				case "any", "...any", "drops.Expression", "...drops.Expression",
					"[]drops.Expression":
					out = append(out, fn.Name.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return dedupe(out)
}

func dedupe(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || in[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// exprTypeName renders the source spelling of a type expression, which
// is all this census needs to recognise an operand position.
func exprTypeName(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprTypeName(v.X)
	case *ast.Ellipsis:
		return "..." + exprTypeName(v.Elt)
	case *ast.ArrayType:
		return "[]" + exprTypeName(v.Elt)
	case *ast.InterfaceType:
		if len(v.Methods.List) == 0 {
			return "any"
		}
	}
	return "?"
}

// The rendering promise: an expression with no statement inside it
// renders exactly the bytes it rendered before the operands came out of
// their closures.
//
// The golden values were captured from the package as it stood BEFORE
// that change, by rendering this same corpus against the previous
// commit. A scoping fix that quietly reformatted every query log would
// not be worth having, and "it looked the same to me" is not a claim
// anybody can check afterwards.
//
// Set DROPS_CORPUS_DUMP=1 to print the corpus as Go source, which is
// how the golden map was produced.
func TestExpressionsRenderUnchanged(t *testing.T) {
	col := drops.Raw("`t`.`n`")
	if os.Getenv("DROPS_CORPUS_DUMP") != "" {
		var names []string
		for _, c := range operandConstructors(col) {
			names = append(names, c.name)
		}
		sort.Strings(names)
		byName := map[string]operandCase{}
		for _, c := range operandConstructors(col) {
			byName[c.name] = c
		}
		for _, n := range names {
			sql, args := drops.StringWithDialect(mysql.Dialect, byName[n].build(drops.Raw("`t`.`m`")))
			fmt.Printf("\t%q: {%q, %d},\n", n, sql, len(args))
		}
		return
	}
	for _, c := range operandConstructors(col) {
		want, ok := renderedCorpus[c.name]
		if !ok {
			t.Errorf("%s: no golden rendering; add one captured from the previous commit", c.name)
			continue
		}
		sql, args := drops.StringWithDialect(mysql.Dialect, c.build(drops.Raw("`t`.`m`")))
		if sql != want.sql || len(args) != want.args {
			t.Errorf("%s rendering changed:\n got  %s  %d args\n want %s  %d args",
				c.name, sql, len(args), want.sql, want.args)
		}
	}
}

type rendering struct {
	sql  string
	args int
}

// renderedCorpus is what each constructor in operandConstructors above
// rendered before its operands came out of their closures, captured
// from the previous commit — the SQL byte for byte and the number of
// arguments bound. Not one of these has a statement inside it, which is
// the point: the scoping work must be invisible to every expression
// that was not hiding one.
var renderedCorpus = map[string]rendering{
	"Abs":                 {"abs(`t`.`m`)", 0},
	"Acos":                {"acos(`t`.`m`)", 0},
	"AllSub":              {"(`t`.`n` = ALL (`t`.`m`))", 0},
	"And":                 {"(`t`.`m` AND 1)", 0},
	"AnySub":              {"(`t`.`n` = ANY (`t`.`m`))", 0},
	"As":                  {"`t`.`m` AS `a`", 0},
	"Asin":                {"asin(`t`.`m`)", 0},
	"Atan":                {"atan(`t`.`m`)", 0},
	"Avg":                 {"avg(`t`.`m`)", 0},
	"AvgDistinct":         {"avg(DISTINCT `t`.`m`)", 0},
	"Between":             {"(`t`.`m` BETWEEN `t`.`n` AND `t`.`n`)", 0},
	"BoolAnd":             {"min(`t`.`m`)", 0},
	"BoolOr":              {"max(`t`.`m`)", 0},
	"Cast":                {"CAST(`t`.`m` AS SIGNED)", 0},
	"Ceil":                {"ceil(`t`.`m`)", 0},
	"CharLength":          {"char_length(`t`.`m`)", 0},
	"CmpAll":              {"(`t`.`n` > ALL (`t`.`m`))", 0},
	"CmpAny":              {"(`t`.`n` < ANY (`t`.`m`))", 0},
	"Coalesce":            {"coalesce(`t`.`m`, ?)", 1},
	"Collate":             {"(`t`.`m` COLLATE utf8mb4_bin)", 0},
	"Concat":              {"concat(`t`.`m`)", 0},
	"ConcatWS":            {"concat_ws(`t`.`m`, `t`.`n`)", 0},
	"ConvertTZ":           {"convert_tz(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"Cos":                 {"cos(`t`.`m`)", 0},
	"Count":               {"count(`t`.`m`)", 0},
	"CountDistinct":       {"count(DISTINCT `t`.`m`)", 0},
	"CountIf":             {"count(CASE WHEN `t`.`m` THEN 1 END)", 0},
	"DateAdd":             {"date_add(`t`.`m`, `t`.`n`)", 0},
	"DateFormat":          {"date_format(`t`.`m`, `t`.`n`)", 0},
	"DateSub":             {"date_sub(`t`.`m`, `t`.`n`)", 0},
	"DateTrunc":           {"CAST(date_format(`t`.`m`, ?) AS DATETIME(6))", 1},
	"DateTrunc quarter":   {"CAST(date_format((makedate(year(`t`.`m`), ?) + INTERVAL quarter(`t`.`m`)-1 QUARTER), ?) AS DATETIME(6))", 2},
	"DateTrunc week":      {"CAST(date_format(date_sub(`t`.`m`, INTERVAL weekday(`t`.`m`) DAY), ?) AS DATETIME(6))", 1},
	"DayOfWeek":           {"dayofweek(`t`.`m`)", 0},
	"DayOfYear":           {"dayofyear(`t`.`m`)", 0},
	"Div":                 {"(`t`.`m` / `t`.`n`)", 0},
	"Eq":                  {"(`t`.`m` = `t`.`n`)", 0},
	"Exists":              {"EXISTS (`t`.`m`)", 0},
	"Exp":                 {"exp(`t`.`m`)", 0},
	"Extract":             {"extract(YEAR FROM `t`.`m`)", 0},
	"FirstValue":          {"first_value(`t`.`m`)", 0},
	"Floor":               {"floor(`t`.`m`)", 0},
	"FromBase64":          {"from_base64(`t`.`m`)", 0},
	"FromUnixTime":        {"from_unixtime(`t`.`m`)", 0},
	"Func":                {"f(`t`.`m`)", 0},
	"Greatest":            {"greatest(`t`.`m`)", 0},
	"GroupConcat":         {"group_concat(`t`.`m` SEPARATOR ',')", 0},
	"GroupConcatDistinct": {"group_concat(DISTINCT `t`.`n` ORDER BY `t`.`m` SEPARATOR ',')", 0},
	"Gt":                  {"(`t`.`m` > `t`.`n`)", 0},
	"Gte":                 {"(`t`.`m` >= `t`.`n`)", 0},
	"Hex":                 {"hex(`t`.`m`)", 0},
	"IfNull":              {"ifnull(`t`.`m`, `t`.`n`)", 0},
	"In value":            {"(`t`.`n` IN (`t`.`m`))", 0},
	"InSub":               {"(`t`.`n` IN (`t`.`m`))", 0},
	"IntDiv":              {"(`t`.`m` DIV `t`.`n`)", 0},
	"Interval":            {"INTERVAL `t`.`m` DAY", 0},
	"IsNotNull":           {"(`t`.`m` IS NOT NULL)", 0},
	"IsNull":              {"(`t`.`m` IS NULL)", 0},
	"JSONAgg":             {"json_arrayagg(`t`.`m`)", 0},
	"JSONArrayLength":     {"json_length(`t`.`m`)", 0},
	"JSONBuildArray":      {"json_array(`t`.`m`)", 0},
	"JSONBuildObject":     {"json_object(`t`.`m`)", 0},
	"JSONContainedIn":     {"json_contains(`t`.`n`, `t`.`m`)", 0},
	"JSONContains":        {"json_contains(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"JSONDepth":           {"json_depth(`t`.`m`)", 0},
	"JSONExtract":         {"json_extract(`t`.`m`, `t`.`n`)", 0},
	"JSONGet":             {"json_extract(`t`.`m`, ?)", 1},
	"JSONGetPath":         {"json_extract(`t`.`m`, ?)", 1},
	"JSONGetPathText":     {"json_unquote(json_extract(`t`.`m`, ?))", 1},
	"JSONGetText":         {"json_unquote(json_extract(`t`.`m`, ?))", 1},
	"JSONHasAllKeys":      {"json_contains_path(`t`.`m`, ?, ?)", 2},
	"JSONHasAnyKey":       {"json_contains_path(`t`.`m`, ?, ?)", 2},
	"JSONHasKey":          {"json_contains_path(`t`.`m`, ?, ?)", 2},
	"JSONInsert":          {"json_insert(`t`.`m`, `t`.`n`)", 0},
	"JSONKeys":            {"json_keys(`t`.`m`, `t`.`n`)", 0},
	"JSONLength":          {"json_length(`t`.`m`, `t`.`n`)", 0},
	"JSONMergePatch":      {"json_merge_patch(`t`.`m`)", 0},
	"JSONMergePreserve":   {"json_merge_preserve(`t`.`m`)", 0},
	"JSONObjectAgg":       {"json_objectagg(`t`.`m`, `t`.`n`)", 0},
	"JSONOverlaps":        {"json_overlaps(`t`.`m`, `t`.`n`)", 0},
	"JSONPretty":          {"json_pretty(`t`.`m`)", 0},
	"JSONQuery":           {"json_query(`t`.`m`, `t`.`n`)", 0},
	"JSONQuote":           {"json_quote(`t`.`m`)", 0},
	"JSONRemove":          {"json_remove(`t`.`m`, `t`.`n`)", 0},
	"JSONReplace":         {"json_replace(`t`.`m`, `t`.`n`)", 0},
	"JSONSearch":          {"json_search(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"JSONSet":             {"json_set(`t`.`m`, `t`.`n`)", 0},
	"JSONTable":           {"JSON_TABLE(`t`.`m`, '$[*]' COLUMNS (`a` INT PATH '$.a')) AS `jt`", 0},
	"JSONType":            {"json_type(`t`.`m`)", 0},
	"JSONUnquote":         {"json_unquote(`t`.`m`)", 0},
	"JSONValid":           {"json_valid(`t`.`m`)", 0},
	"JSONValue":           {"json_value(`t`.`m`, `t`.`n`)", 0},
	"LPad":                {"lpad(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"LTrim":               {"ltrim(`t`.`m`)", 0},
	"Lag":                 {"lag(`t`.`m`, `t`.`n`)", 0},
	"LastDay":             {"last_day(`t`.`m`)", 0},
	"LastValue":           {"last_value(`t`.`m`)", 0},
	"Lead":                {"lead(`t`.`m`, `t`.`n`)", 0},
	"Least":               {"least(`t`.`m`)", 0},
	"Left":                {"left(`t`.`m`, `t`.`n`)", 0},
	"Length":              {"char_length(`t`.`m`)", 0},
	"Like":                {"(`t`.`m` LIKE `t`.`n`)", 0},
	"Ln":                  {"ln(`t`.`m`)", 0},
	"Locate":              {"locate(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"Log":                 {"log10(`t`.`m`)", 0},
	"Log10":               {"log10(`t`.`m`)", 0},
	"Log2":                {"log2(`t`.`m`)", 0},
	"LogBase":             {"log(`t`.`m`, `t`.`n`)", 0},
	"Lower":               {"lower(`t`.`m`)", 0},
	"Lt":                  {"(`t`.`m` < `t`.`n`)", 0},
	"Lte":                 {"(`t`.`m` <= `t`.`n`)", 0},
	"MakeDate":            {"str_to_date(concat_ws(?, `t`.`m`, `t`.`n`, `t`.`n`), ?)", 2},
	"MakeTime":            {"maketime(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"MakeTimestamp":       {"str_to_date(concat_ws(?, concat_ws(?, `t`.`m`, `t`.`n`, `t`.`n`), concat_ws(?, `t`.`n`, `t`.`n`, `t`.`n`)), ?)", 4},
	"Max":                 {"max(`t`.`m`)", 0},
	"Md5":                 {"md5(`t`.`m`)", 0},
	"Min":                 {"min(`t`.`m`)", 0},
	"Minus":               {"(`t`.`m` - `t`.`n`)", 0},
	"Mod":                 {"mod(`t`.`m`, `t`.`n`)", 0},
	"Mul":                 {"(`t`.`m` * `t`.`n`)", 0},
	"Ne":                  {"(`t`.`m` <> `t`.`n`)", 0},
	"Not":                 {"(NOT `t`.`m`)", 0},
	"NotExists":           {"NOT EXISTS (`t`.`m`)", 0},
	"NotIn value":         {"(`t`.`n` NOT IN (`t`.`m`))", 0},
	"NotInSub":            {"(`t`.`n` NOT IN (`t`.`m`))", 0},
	"NthValue":            {"nth_value(`t`.`m`, `t`.`n`)", 0},
	"Ntile":               {"ntile(`t`.`m`)", 0},
	"NullIf":              {"nullif(`t`.`m`, `t`.`n`)", 0},
	"OctetLength":         {"length(`t`.`m`)", 0},
	"Or":                  {"(`t`.`m` OR 1)", 0},
	"Over":                {"`t`.`m` OVER ()", 0},
	"Over order":          {"row_number() OVER (ORDER BY `t`.`m`)", 0},
	"Over partition":      {"row_number() OVER (PARTITION BY `t`.`m`)", 0},
	"Plus":                {"(`t`.`m` + `t`.`n`)", 0},
	"Position":            {"position(`t`.`m` IN `t`.`n`)", 0},
	"Power":               {"power(`t`.`m`, `t`.`n`)", 0},
	"Quarter":             {"quarter(`t`.`m`)", 0},
	"RPad":                {"rpad(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"RTrim":               {"rtrim(`t`.`m`)", 0},
	"RegexpInstr":         {"regexp_instr(`t`.`m`, `t`.`n`)", 0},
	"RegexpLike":          {"(`t`.`m` REGEXP `t`.`n`)", 0},
	"RegexpReplace":       {"regexp_replace(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"RegexpSubstr":        {"regexp_substr(`t`.`m`, `t`.`n`)", 0},
	"Repeat":              {"repeat(`t`.`m`, `t`.`n`)", 0},
	"Replace":             {"replace(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"Reverse":             {"reverse(`t`.`m`)", 0},
	"Right":               {"right(`t`.`m`, `t`.`n`)", 0},
	"Round":               {"round(`t`.`m`, ?)", 1},
	"Sha1":                {"sha1(`t`.`m`)", 0},
	"Sha2":                {"sha2(`t`.`m`, `t`.`n`)", 0},
	"Sign":                {"sign(`t`.`m`)", 0},
	"Sin":                 {"sin(`t`.`m`)", 0},
	"Sqrt":                {"sqrt(`t`.`m`)", 0},
	"StrPos":              {"locate(`t`.`n`, `t`.`m`)", 0},
	"StrToDate":           {"str_to_date(`t`.`m`, `t`.`n`)", 0},
	"Subquery":            {"(`t`.`m`)", 0},
	"Substring":           {"substring(`t`.`m` FROM `t`.`n` FOR `t`.`n`)", 0},
	"SubstringIndex":      {"substring_index(`t`.`m`, `t`.`n`, `t`.`n`)", 0},
	"Sum":                 {"sum(`t`.`m`)", 0},
	"SumDistinct":         {"sum(DISTINCT `t`.`m`)", 0},
	"SumIf":               {"sum(CASE WHEN `t`.`m` THEN `t`.`n` END)", 0},
	"Tan":                 {"tan(`t`.`m`)", 0},
	"TimestampDiff":       {"timestampdiff(DAY, `t`.`m`, `t`.`n`)", 0},
	"ToBase64":            {"to_base64(`t`.`m`)", 0},
	"Trim":                {"trim(`t`.`m`)", 0},
	"TrimChars":           {"trim(BOTH `t`.`m` FROM `t`.`n`)", 0},
	"Truncate":            {"truncate(`t`.`m`, `t`.`n`)", 0},
	"Unhex":               {"unhex(`t`.`m`)", 0},
	"UnixTimestamp":       {"unix_timestamp(`t`.`m`)", 0},
	"Upper":               {"upper(`t`.`m`)", 0},
	"Weekday":             {"weekday(`t`.`m`)", 0},
}
