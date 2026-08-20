package pg_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// The rest of the expression package, held rather than closed over.
//
// An operand a caller can hand a *SelectBuilder to must not be an
// opaque closure. Round 4 made that true of op.go and array.go and of
// nothing else, so every function helper in the package still swallowed
// its arguments in a drops.ExprFunc: the resolver walk terminated at
// the closure and the statement inside rendered through WriteSQL, which
// has no ctx, so it wrote its DefaultFilters and none of its
// ContextFilters. These rendered, and sent, on a ctx with no tenant at
// all:
//
//	SELECT coalesce((SELECT "posts"."id" FROM "posts"), $1) FROM "plain"
//	SELECT count(*) FILTER (WHERE EXISTS (SELECT ... FROM "posts")) FROM "plain"
//	SELECT row_number() OVER (PARTITION BY (SELECT ... FROM "posts")) FROM "plain"
//	SELECT ((SELECT ... FROM "posts"))::bigint FROM "plain"
//	SELECT CASE WHEN EXISTS (SELECT ... FROM "posts") THEN $1 END FROM "plain"
//
// pg.Coalesce(pg.Subquery(sel), 0) in a projection is an ordinary
// scalar-subquery idiom, not an exotic shape, and the fail-open was in
// the one feature sold as fail-closed.
//
// The tests here are organised by the invariant rather than by the list
// of helpers that leaked, because the list is what went stale after
// each of the four previous rounds:
//
//   - every exported operand-taking constructor is exercised with a
//     scoped subquery, and the table is checked against the package's
//     own source so a helper added later cannot quietly miss it;
//   - the same table is run again on a bare ctx, where each must refuse
//     rather than render;
//   - a corpus of the same helpers with no statement inside pins the
//     rendered SQL and args byte for byte, because a scoping fix that
//     rewrote every query log would not be worth having.

// fnTenant is the tenant these tests put on ctx. It differs from
// reachTenant so a stray helper cannot pass by coincidence.
const fnTenant = int64(23)

func fnCtx() context.Context { return pg.WithTenant(context.Background(), fnTenant) }

// placeholderRe normalises $1, $2, ... so one expected inner statement
// can be matched wherever in the outer statement a helper renders it.
var placeholderRe = regexp.MustCompile(`\$\d+`)

func normalisePlaceholders(sql string) string {
	return placeholderRe.ReplaceAllString(sql, "$$?")
}

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
// exported function in package pg that returns a drops.Expression and
// takes an operand a caller can hand a statement to.
//
// It is a table of constructors rather than of rendered statements
// because the invariant is about the constructor: whatever it builds
// must keep the operand reachable. What each one renders is pinned
// separately, in TestFunctionExpressionsRenderUnchanged.
func operandConstructors(col drops.Expression) []operandCase {
	return []operandCase{
		// op.go — comparisons, connectives, membership, null tests.
		{"Eq", func(s drops.Expression) drops.Expression { return pg.Eq(col, s) }},
		{"Ne", func(s drops.Expression) drops.Expression { return pg.Ne(col, s) }},
		{"Gt", func(s drops.Expression) drops.Expression { return pg.Gt(col, s) }},
		{"Gte", func(s drops.Expression) drops.Expression { return pg.Gte(col, s) }},
		{"Lt", func(s drops.Expression) drops.Expression { return pg.Lt(col, s) }},
		{"Lte", func(s drops.Expression) drops.Expression { return pg.Lte(col, s) }},
		{"Like", func(s drops.Expression) drops.Expression { return pg.Like(col, s) }},
		{"ILike", func(s drops.Expression) drops.Expression { return pg.ILike(col, s) }},
		{"And", func(s drops.Expression) drops.Expression { return pg.And(s, drops.Raw(`TRUE`)) }},
		{"Or", func(s drops.Expression) drops.Expression { return pg.Or(s, drops.Raw(`FALSE`)) }},
		{"Not", func(s drops.Expression) drops.Expression { return pg.Not(s) }},
		{"In", func(s drops.Expression) drops.Expression { return pg.In(col, s) }},
		{"NotIn", func(s drops.Expression) drops.Expression { return pg.NotIn(col, s) }},
		{"IsNull", func(s drops.Expression) drops.Expression { return pg.IsNull(s) }},
		{"IsNotNull", func(s drops.Expression) drops.Expression { return pg.IsNotNull(s) }},
		{"Between", func(s drops.Expression) drops.Expression { return pg.Between(col, s, 9) }},

		// subquery.go — the constructors that name a subquery.
		{"Exists", func(s drops.Expression) drops.Expression { return pg.Exists(s) }},
		{"NotExists", func(s drops.Expression) drops.Expression { return pg.NotExists(s) }},
		{"Subquery", func(s drops.Expression) drops.Expression { return pg.Subquery(s) }},
		{"AnySub", func(s drops.Expression) drops.Expression { return pg.AnySub(col, s) }},
		{"AllSub", func(s drops.Expression) drops.Expression { return pg.AllSub(col, s) }},

		// array.go.
		{"ArrayContains", func(s drops.Expression) drops.Expression { return pg.ArrayContains(col, s) }},
		{"ArrayContainedIn", func(s drops.Expression) drops.Expression { return pg.ArrayContainedIn(col, s) }},
		{"ArrayOverlaps", func(s drops.Expression) drops.Expression { return pg.ArrayOverlaps(col, s) }},
		{"ArrayConcat", func(s drops.Expression) drops.Expression { return pg.ArrayConcat(col, s) }},
		{"Any", func(s drops.Expression) drops.Expression { return pg.Any(col, s) }},
		{"All", func(s drops.Expression) drops.Expression { return pg.All(col, s) }},
		{"ArrayAgg", func(s drops.Expression) drops.Expression { return pg.ArrayAgg(s) }},
		{"ArrayLength", func(s drops.Expression) drops.Expression { return pg.ArrayLength(s, 1) }},
		{"ArrayUpper", func(s drops.Expression) drops.Expression { return pg.ArrayUpper(s, 1) }},
		{"ArrayLower", func(s drops.Expression) drops.Expression { return pg.ArrayLower(s, 1) }},
		{"ArrayAppend", func(s drops.Expression) drops.Expression { return pg.ArrayAppend(s, 1) }},
		{"ArrayPrepend", func(s drops.Expression) drops.Expression { return pg.ArrayPrepend(1, s) }},
		{"ArrayRemove", func(s drops.Expression) drops.Expression { return pg.ArrayRemove(s, 1) }},
		{"ArrayReplace", func(s drops.Expression) drops.Expression { return pg.ArrayReplace(s, 1, 2) }},
		{"ArrayPosition", func(s drops.Expression) drops.Expression { return pg.ArrayPosition(s, 1) }},
		{"ArrayPositions", func(s drops.Expression) drops.Expression { return pg.ArrayPositions(s, 1) }},
		{"ArrayToString", func(s drops.Expression) drops.Expression { return pg.ArrayToString(s, ",") }},
		{"StringToArray", func(s drops.Expression) drops.Expression { return pg.StringToArray(s, ",") }},
		{"Cardinality", func(s drops.Expression) drops.Expression { return pg.Cardinality(s) }},
		{"Unnest", func(s drops.Expression) drops.Expression { return pg.Unnest(s) }},
		{"ArrayLit", func(s drops.Expression) drops.Expression { return pg.ArrayLit(s, 1) }},

		// pgvector.go — distance operators, built from the same node.
		{"L2Distance", func(s drops.Expression) drops.Expression { return pg.L2Distance(col, s) }},
		{"InnerProduct", func(s drops.Expression) drops.Expression { return pg.InnerProduct(col, s) }},
		{"CosineDistance", func(s drops.Expression) drops.Expression { return pg.CosineDistance(col, s) }},
		{"L1Distance", func(s drops.Expression) drops.Expression { return pg.L1Distance(col, s) }},
		{"HammingDistance", func(s drops.Expression) drops.Expression { return pg.HammingDistance(col, s) }},
		{"JaccardDistance", func(s drops.Expression) drops.Expression { return pg.JaccardDistance(col, s) }},

		// funcs.go — aggregates and the generic call.
		{"Count", func(s drops.Expression) drops.Expression { return pg.Count(s) }},
		{"CountDistinct", func(s drops.Expression) drops.Expression { return pg.CountDistinct(s) }},
		{"SumDistinct", func(s drops.Expression) drops.Expression { return pg.SumDistinct(s) }},
		{"AvgDistinct", func(s drops.Expression) drops.Expression { return pg.AvgDistinct(s) }},
		{"Filter aggregate", func(s drops.Expression) drops.Expression { return pg.Filter(s, drops.Raw(`TRUE`)) }},
		{"Filter predicate", func(s drops.Expression) drops.Expression { return pg.Filter(pg.CountAll(), pg.Exists(s)) }},
		{"StringAgg", func(s drops.Expression) drops.Expression { return pg.StringAgg(s, ",") }},
		{"BoolAnd", func(s drops.Expression) drops.Expression { return pg.BoolAnd(s) }},
		{"BoolOr", func(s drops.Expression) drops.Expression { return pg.BoolOr(s) }},
		{"Every", func(s drops.Expression) drops.Expression { return pg.Every(s) }},
		{"Sum", func(s drops.Expression) drops.Expression { return pg.Sum(s) }},
		{"Avg", func(s drops.Expression) drops.Expression { return pg.Avg(s) }},
		{"Min", func(s drops.Expression) drops.Expression { return pg.Min(s) }},
		{"Max", func(s drops.Expression) drops.Expression { return pg.Max(s) }},
		{"Lower", func(s drops.Expression) drops.Expression { return pg.Lower(s) }},
		{"Upper", func(s drops.Expression) drops.Expression { return pg.Upper(s) }},
		{"Coalesce", func(s drops.Expression) drops.Expression { return pg.Coalesce(s, 0) }},
		{"Func", func(s drops.Expression) drops.Expression { return pg.Func("f", s) }},
		{"As", func(s drops.Expression) drops.Expression { return pg.As(s, "x") }},

		// strings.go.
		{"Concat", func(s drops.Expression) drops.Expression { return pg.Concat(s, "a") }},
		{"ConcatWS", func(s drops.Expression) drops.Expression { return pg.ConcatWS("-", s) }},
		{"ConcatOp", func(s drops.Expression) drops.Expression { return pg.ConcatOp(col, s) }},
		{"Length", func(s drops.Expression) drops.Expression { return pg.Length(s) }},
		{"CharLength", func(s drops.Expression) drops.Expression { return pg.CharLength(s) }},
		{"Substring", func(s drops.Expression) drops.Expression { return pg.Substring(s, 1, 2) }},
		{"Substring without FOR", func(s drops.Expression) drops.Expression { return pg.Substring(s, 1, nil) }},
		{"Trim", func(s drops.Expression) drops.Expression { return pg.Trim(s) }},
		{"LTrim", func(s drops.Expression) drops.Expression { return pg.LTrim(s) }},
		{"RTrim", func(s drops.Expression) drops.Expression { return pg.RTrim(s) }},
		{"Replace", func(s drops.Expression) drops.Expression { return pg.Replace(s, "a", "b") }},
		{"RegexpReplace", func(s drops.Expression) drops.Expression { return pg.RegexpReplace(s, "a", "b") }},
		{"RegexpReplace with flags", func(s drops.Expression) drops.Expression {
			return pg.RegexpReplace(s, "a", "b", "g")
		}},
		{"RegexpMatch", func(s drops.Expression) drops.Expression { return pg.RegexpMatch(s, "a") }},
		{"Position", func(s drops.Expression) drops.Expression { return pg.Position(s, col) }},
		{"StrPos", func(s drops.Expression) drops.Expression { return pg.StrPos(s, "a") }},
		{"Initcap", func(s drops.Expression) drops.Expression { return pg.Initcap(s) }},
		{"Format", func(s drops.Expression) drops.Expression { return pg.Format(s) }},
		{"ToChar", func(s drops.Expression) drops.Expression { return pg.ToChar(s, "YYYY") }},
		{"Md5", func(s drops.Expression) drops.Expression { return pg.Md5(s) }},
		{"Encode", func(s drops.Expression) drops.Expression { return pg.Encode(s, "hex") }},
		{"Decode", func(s drops.Expression) drops.Expression { return pg.Decode(s, "hex") }},

		// math.go.
		{"Abs", func(s drops.Expression) drops.Expression { return pg.Abs(s) }},
		{"Ceil", func(s drops.Expression) drops.Expression { return pg.Ceil(s) }},
		{"Floor", func(s drops.Expression) drops.Expression { return pg.Floor(s) }},
		{"Round", func(s drops.Expression) drops.Expression { return pg.Round(s) }},
		{"Round with digits", func(s drops.Expression) drops.Expression { return pg.Round(s, 2) }},
		{"Mod", func(s drops.Expression) drops.Expression { return pg.Mod(s, 2) }},
		{"Power", func(s drops.Expression) drops.Expression { return pg.Power(s, 2) }},
		{"Sqrt", func(s drops.Expression) drops.Expression { return pg.Sqrt(s) }},
		{"Sign", func(s drops.Expression) drops.Expression { return pg.Sign(s) }},
		{"Exp", func(s drops.Expression) drops.Expression { return pg.Exp(s) }},
		{"Ln", func(s drops.Expression) drops.Expression { return pg.Ln(s) }},
		{"Log", func(s drops.Expression) drops.Expression { return pg.Log(s) }},
		{"Greatest", func(s drops.Expression) drops.Expression { return pg.Greatest(s, 1) }},
		{"Least", func(s drops.Expression) drops.Expression { return pg.Least(s, 1) }},
		{"Sin", func(s drops.Expression) drops.Expression { return pg.Sin(s) }},
		{"Cos", func(s drops.Expression) drops.Expression { return pg.Cos(s) }},
		{"Tan", func(s drops.Expression) drops.Expression { return pg.Tan(s) }},
		{"Asin", func(s drops.Expression) drops.Expression { return pg.Asin(s) }},
		{"Acos", func(s drops.Expression) drops.Expression { return pg.Acos(s) }},
		{"Atan", func(s drops.Expression) drops.Expression { return pg.Atan(s) }},
		{"Plus", func(s drops.Expression) drops.Expression { return pg.Plus(col, s) }},
		{"Minus", func(s drops.Expression) drops.Expression { return pg.Minus(col, s) }},
		{"Mul", func(s drops.Expression) drops.Expression { return pg.Mul(col, s) }},
		{"Div", func(s drops.Expression) drops.Expression { return pg.Div(col, s) }},

		// json.go.
		{"JSONGet", func(s drops.Expression) drops.Expression { return pg.JSONGet(s, "k") }},
		{"JSONGetText", func(s drops.Expression) drops.Expression { return pg.JSONGetText(s, "k") }},
		{"JSONGetPath", func(s drops.Expression) drops.Expression { return pg.JSONGetPath(s, []string{"k"}) }},
		{"JSONGetPathText", func(s drops.Expression) drops.Expression {
			return pg.JSONGetPathText(s, []string{"k"})
		}},
		{"JSONBContains", func(s drops.Expression) drops.Expression { return pg.JSONBContains(s, "{}") }},
		{"JSONBContainedIn", func(s drops.Expression) drops.Expression { return pg.JSONBContainedIn(s, "{}") }},
		{"JSONBHasKey", func(s drops.Expression) drops.Expression { return pg.JSONBHasKey(s, "k") }},
		{"JSONBHasAnyKey", func(s drops.Expression) drops.Expression {
			return pg.JSONBHasAnyKey(s, []string{"k"})
		}},
		{"JSONBHasAllKeys", func(s drops.Expression) drops.Expression {
			return pg.JSONBHasAllKeys(s, []string{"k"})
		}},
		{"JSONBConcat", func(s drops.Expression) drops.Expression { return pg.JSONBConcat(s, "{}") }},
		{"JSONBDelete", func(s drops.Expression) drops.Expression { return pg.JSONBDelete(s, "k") }},
		{"ToJSON", func(s drops.Expression) drops.Expression { return pg.ToJSON(s) }},
		{"ToJSONB", func(s drops.Expression) drops.Expression { return pg.ToJSONB(s) }},
		{"JSONArrayLength", func(s drops.Expression) drops.Expression { return pg.JSONArrayLength(s) }},
		{"JSONBArrayLength", func(s drops.Expression) drops.Expression { return pg.JSONBArrayLength(s) }},
		{"JSONTypeof", func(s drops.Expression) drops.Expression { return pg.JSONTypeof(s) }},
		{"JSONBTypeof", func(s drops.Expression) drops.Expression { return pg.JSONBTypeof(s) }},
		{"JSONBuildObject", func(s drops.Expression) drops.Expression { return pg.JSONBuildObject("a", s) }},
		{"JSONBuildArray", func(s drops.Expression) drops.Expression { return pg.JSONBuildArray(s) }},
		{"JSONBBuildObject", func(s drops.Expression) drops.Expression { return pg.JSONBBuildObject("a", s) }},
		{"JSONBBuildArray", func(s drops.Expression) drops.Expression { return pg.JSONBBuildArray(s) }},
		{"JSONBSet", func(s drops.Expression) drops.Expression { return pg.JSONBSet(s, "{a}", 1) }},
		{"JSONBInsert", func(s drops.Expression) drops.Expression { return pg.JSONBInsert(s, "{a}", 1) }},
		{"JSONBStripNulls", func(s drops.Expression) drops.Expression { return pg.JSONBStripNulls(s) }},
		{"JSONBPretty", func(s drops.Expression) drops.Expression { return pg.JSONBPretty(s) }},
		{"JSONAgg", func(s drops.Expression) drops.Expression { return pg.JSONAgg(s) }},
		{"JSONBAgg", func(s drops.Expression) drops.Expression { return pg.JSONBAgg(s) }},
		{"JSONObjectAgg", func(s drops.Expression) drops.Expression { return pg.JSONObjectAgg(s, col) }},
		{"JSONBObjectAgg", func(s drops.Expression) drops.Expression { return pg.JSONBObjectAgg(s, col) }},

		// datetime.go.
		{"DateTrunc", func(s drops.Expression) drops.Expression { return pg.DateTrunc("day", s) }},
		{"Extract", func(s drops.Expression) drops.Expression { return pg.Extract("year", s) }},
		{"DatePart", func(s drops.Expression) drops.Expression { return pg.DatePart("day", s) }},
		{"Age", func(s drops.Expression) drops.Expression { return pg.Age(s) }},
		{"MakeDate", func(s drops.Expression) drops.Expression { return pg.MakeDate(s, 1, 1) }},
		{"MakeTime", func(s drops.Expression) drops.Expression { return pg.MakeTime(s, 1, 1) }},
		{"MakeTimestamp", func(s drops.Expression) drops.Expression { return pg.MakeTimestamp(s, 1, 1, 1, 1, 1) }},
		{"MakeTimestampTZ", func(s drops.Expression) drops.Expression {
			return pg.MakeTimestampTZ(s, 1, 1, 1, 1, 1)
		}},
		{"ToDate", func(s drops.Expression) drops.Expression { return pg.ToDate(s, "YYYY") }},
		{"ToTimestamp", func(s drops.Expression) drops.Expression { return pg.ToTimestamp(s, "YYYY") }},
		{"ToNumber", func(s drops.Expression) drops.Expression { return pg.ToNumber(s, "999") }},
		{"AtTimeZone", func(s drops.Expression) drops.Expression { return pg.AtTimeZone(s, "UTC") }},

		// window.go — the function, and both halves of the window spec.
		{"Over function", func(s drops.Expression) drops.Expression { return pg.Over(pg.Count(s), pg.WindowSpec()) }},
		{"Over PARTITION BY", func(s drops.Expression) drops.Expression {
			return pg.Over(pg.RowNumber(), pg.WindowSpec().PartitionBy(s))
		}},
		{"Over ORDER BY", func(s drops.Expression) drops.Expression {
			return pg.Over(pg.RowNumber(), pg.WindowSpec().OrderBy(s))
		}},
		{"Ntile", func(s drops.Expression) drops.Expression { return pg.Ntile(s) }},
		{"Lag", func(s drops.Expression) drops.Expression { return pg.Lag(s) }},
		{"Lead", func(s drops.Expression) drops.Expression { return pg.Lead(s) }},
		{"FirstValue", func(s drops.Expression) drops.Expression { return pg.FirstValue(s) }},
		{"LastValue", func(s drops.Expression) drops.Expression { return pg.LastValue(s) }},
		{"NthValue", func(s drops.Expression) drops.Expression { return pg.NthValue(s, 1) }},

		// cast.go — the two cast spellings and every CASE position.
		{"Cast", func(s drops.Expression) drops.Expression { return pg.Cast(s, "bigint") }},
		{"CastAs", func(s drops.Expression) drops.Expression { return pg.CastAs(s, "bigint") }},
		{"Case WHEN", func(s drops.Expression) drops.Expression { return pg.Case().When(pg.Exists(s), 1).End() }},
		{"Case THEN", func(s drops.Expression) drops.Expression {
			return pg.Case().When(drops.Raw(`TRUE`), s).End()
		}},
		{"Case ELSE", func(s drops.Expression) drops.Expression {
			return pg.Case().When(drops.Raw(`TRUE`), 1).Else(s).End()
		}},
		{"CaseOn value", func(s drops.Expression) drops.Expression { return pg.CaseOn(s).When(1, 2).End() }},

		// jsonpath.go — the containment helper's value operand.
		{"JSONContains", func(s drops.Expression) drops.Expression { return pg.JSONContains(fnCol, s) }},

		// ddl.go — setval reads a value like any other function.
		{"SetVal", func(s drops.Expression) drops.Expression { return pg.SetVal("seq", s) }},
	}
}

// fnCol is the jsonb-ish column JSONContains needs a ColRef for. It is
// package-level so operandConstructors can close over it without taking
// a second parameter.
var fnCol = func() pg.ColRef {
	t := pg.NewTable("fn_meta")
	return pg.Add(t, pg.JSONB("meta"))
}()

// exemptConstructors names the exported operand-taking constructors
// that deliberately do NOT resolve their operand, with the reason. It
// is the list the next leak would hide in, so each entry has to say why
// resolving would be wrong rather than merely inconvenient.
var exemptConstructors = map[string]string{
	// A view definition outlives the request that created it. Resolving
	// the body would bake one tenant's predicate into an object every
	// other tenant then reads through — and would refuse outright in
	// the place views are actually created, a migration, where there is
	// no tenant on ctx at all. The body is held in a field rather than
	// closed over (see ddlBody) so it is inspectable, and the doc
	// comment says the body stays the caller's to scope.
	"CreateView":             "DDL: a view outlives the request, so a resolved body would pin one tenant",
	"CreateOrReplaceView":    "DDL: a view outlives the request, so a resolved body would pin one tenant",
	"CreateMaterializedView": "DDL: a matview outlives the request, so a resolved body would pin one tenant",
}

// TestEveryOperandConstructorIsCensused is the enforcement the previous
// four rounds lacked: it reads the package's own source and requires
// every exported function returning a drops.Expression that takes an
// operand — an `any`, a drops.Expression, or a variadic of either — to
// appear in operandConstructors or in exemptConstructors.
//
// The list of leaky helpers went stale after every round because it was
// written by hand from what somebody had looked at. This one cannot:
// a helper added tomorrow fails here until it is exercised with a
// scoped subquery below, which is what a closure cannot survive.
func TestEveryOperandConstructorIsCensused(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range operandConstructors(drops.Raw(`"t"."c"`)) {
		covered[strings.Fields(tc.name)[0]] = true
	}

	var missing []string
	for _, name := range censusOperandConstructors(t) {
		if covered[name] || exemptConstructors[name] != "" {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("constructors take an operand but are not exercised with a scoped subquery: %v", missing)
	}

	// The exemption list is checked in the other direction too: an
	// entry naming a constructor that no longer exists is a comment
	// that has stopped being true.
	present := map[string]bool{}
	for _, name := range censusOperandConstructors(t) {
		present[name] = true
	}
	for name := range exemptConstructors {
		if !present[name] {
			t.Errorf("exempt constructor %q no longer takes an operand — drop the exemption", name)
		}
	}
}

// censusOperandConstructors parses the package sources and returns the
// exported functions returning a drops.Expression that take at least
// one operand a caller could hand a statement to.
func censusOperandConstructors(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || !fn.Name.IsExported() {
					continue
				}
				if returnsExpression(fn.Type.Results) && takesOperand(fn.Type.Params) {
					out = append(out, fn.Name.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func returnsExpression(fl *ast.FieldList) bool {
	return fl != nil && len(fl.List) == 1 && isExpressionType(fl.List[0].Type)
}

func isExpressionType(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "drops" && sel.Sel.Name == "Expression"
}

func takesOperand(fl *ast.FieldList) bool {
	if fl == nil {
		return false
	}
	for _, f := range fl.List {
		if isOperandType(f.Type) {
			return true
		}
	}
	return false
}

// isOperandType reports whether a parameter can carry a statement: an
// `any` (or a slice/variadic of one) and a drops.Expression can, a
// string or a *Table cannot.
func isOperandType(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ellipsis:
		return isOperandType(v.Elt)
	case *ast.ArrayType:
		return isOperandType(v.Elt)
	case *ast.Ident:
		return v.Name == "any"
	case *ast.InterfaceType:
		return v.Methods == nil || len(v.Methods.List) == 0
	}
	return isExpressionType(e)
}

// TestOperandConstructorsCarryTheTenantAxis runs the whole table with a
// scoped subquery in an operand position and asserts the inner
// statement renders its own context filters.
//
// The outer table is deliberately unscoped, so the tenant predicate in
// the rendered SQL and the tenant in the args can only have come from
// the statement inside the helper.
func TestOperandConstructorsCarryTheTenantAxis(t *testing.T) {
	plain := pg.NewTable("fn_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	posts := reachTable("fn_posts")
	db := pg.New(nil)

	wantInner := `SELECT "fn_posts"."id" FROM "fn_posts" ` +
		`WHERE ("fn_posts"."deletedAt" IS NULL) AND ("fn_posts"."tenantId" = $?)`

	for _, tc := range operandConstructors(plain.Col("id")) {
		t.Run(tc.name, func(t *testing.T) {
			sub := pg.Subquery(db.Select(posts.Col("id")).From(posts))
			got, args, err := db.Select(tc.build(sub)).From(plain).Unscoped().ToSQLCtx(fnCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if !strings.Contains(normalisePlaceholders(got), wantInner) {
				t.Errorf("got = %v, want it to contain %v", got, wantInner)
			}
			if !containsArg(args, fnTenant) {
				t.Errorf("args = %v, want them to bind the tenant %v", args, fnTenant)
			}
		})
	}
}

// TestOperandConstructorsFailClosedWithNoTenant is the other half, and
// the half that matters: with no tenant on ctx at all every one of
// these rendered and sent, and came back with another tenant's rows.
func TestOperandConstructorsFailClosedWithNoTenant(t *testing.T) {
	plain := pg.NewTable("fnf_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	posts := reachTable("fnf_posts")
	db := pg.New(nil)

	for _, tc := range operandConstructors(plain.Col("id")) {
		t.Run(tc.name, func(t *testing.T) {
			sub := pg.Subquery(db.Select(posts.Col("id")).From(posts))
			sql, _, err := db.Select(tc.build(sub)).From(plain).Unscoped().ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want no statement at all", sql)
			}
		})
	}
}

// containsArg reports whether the bound argument list holds v.
func containsArg(args []any, v any) bool {
	for _, a := range args {
		if a == v {
			return true
		}
	}
	return false
}

// TestFunctionExpressionProbesRenderExactly pins the statements from
// the report byte for byte, in the clauses they were reported in. The
// table above asserts the tenant predicate is present; these assert
// exactly where it lands and what the args are, because a predicate in
// the wrong clause of the wrong statement is the shape rounds 1 to 3
// kept producing.
func TestFunctionExpressionProbesRenderExactly(t *testing.T) {
	plain := pg.NewTable("fp_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	pg.Add(plain, pg.BigInt("n"))
	posts := reachTable("fp_posts")
	db := pg.New(nil)

	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }
	subSQL := func(n string) string {
		return `SELECT "fp_posts"."id" FROM "fp_posts" ` +
			`WHERE ("fp_posts"."deletedAt" IS NULL) AND ("fp_posts"."tenantId" = ` + n + `)`
	}

	tests := []struct {
		name string
		sel  func() *pg.SelectBuilder
		want string
		args []any
	}{
		{
			name: "coalesce over a scalar subquery in the projection",
			sel: func() *pg.SelectBuilder {
				return db.Select(pg.Coalesce(pg.Subquery(sub()), 0)).From(plain).Unscoped()
			},
			want: `SELECT coalesce((` + subSQL(`$1`) + `), $2) FROM "fp_plain"`,
			args: []any{fnTenant, 0},
		},
		{
			name: "a FILTER clause over EXISTS",
			sel: func() *pg.SelectBuilder {
				return db.Select(pg.Filter(pg.CountAll(), pg.Exists(sub()))).From(plain).Unscoped()
			},
			want: `SELECT count(*) FILTER (WHERE EXISTS (` + subSQL(`$1`) + `)) FROM "fp_plain"`,
			args: []any{fnTenant},
		},
		{
			name: "a window PARTITION BY over a scalar subquery",
			sel: func() *pg.SelectBuilder {
				win := pg.WindowSpec().PartitionBy(pg.Subquery(sub()))
				return db.Select(pg.Over(pg.RowNumber(), win)).From(plain).Unscoped()
			},
			want: `SELECT row_number() OVER (PARTITION BY (` + subSQL(`$1`) + `)) FROM "fp_plain"`,
			args: []any{fnTenant},
		},
		{
			name: "a cast over a scalar subquery",
			sel: func() *pg.SelectBuilder {
				return db.Select(pg.Cast(pg.Subquery(sub()), "bigint")).From(plain).Unscoped()
			},
			want: `SELECT ((` + subSQL(`$1`) + `))::bigint FROM "fp_plain"`,
			args: []any{fnTenant},
		},
		{
			name: "a CASE branch over EXISTS",
			sel: func() *pg.SelectBuilder {
				return db.Select(pg.Case().When(pg.Exists(sub()), 1).End()).From(plain).Unscoped()
			},
			want: `SELECT CASE WHEN EXISTS (` + subSQL(`$1`) + `) THEN $2 END FROM "fp_plain"`,
			args: []any{fnTenant, 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args, err := tc.sel().ToSQLCtx(fnCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}
}

// A function expression is an operand of the two writes as much as of a
// SELECT, and there a cross-tenant read is not a widened result set but
// the input to a write.
func TestFunctionExpressionsCarryTheTenantAxisIntoWrites(t *testing.T) {
	plain := pg.NewTable("fw_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	n := pg.Add(plain, pg.BigInt("n"))
	posts := reachTable("fw_posts")
	db := pg.New(nil)

	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }
	subSQL := func(p string) string {
		return `SELECT "fw_posts"."id" FROM "fw_posts" ` +
			`WHERE ("fw_posts"."deletedAt" IS NULL) AND ("fw_posts"."tenantId" = ` + p + `)`
	}

	t.Run("UPDATE SET", func(t *testing.T) {
		upd := db.Update(plain).Set(n.Expr(pg.Coalesce(pg.Subquery(sub()), 0))).
			Where(pg.Eq(plain.Col("id"), int64(1)))
		want := `UPDATE "fw_plain" SET "n" = coalesce((` + subSQL(`$1`) + `), $2) ` +
			`WHERE ("fw_plain"."id" = $3)`
		got, args, err := upd.ToSQLCtx(fnCtx())
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if got != want {
			t.Errorf("got = %v, want %v", got, want)
		}
		if !sameArgs(args, []any{fnTenant, 0, int64(1)}) {
			t.Errorf("args = %v, want %v", args, []any{fnTenant, 0, int64(1)})
		}
	})

	t.Run("DELETE WHERE", func(t *testing.T) {
		del := db.Delete(plain).Where(pg.Eq(plain.Col("id"), pg.Coalesce(pg.Subquery(sub()), 0)))
		want := `DELETE FROM "fw_plain" WHERE ("fw_plain"."id" = coalesce((` + subSQL(`$1`) + `), $2))`
		got, args, err := del.ToSQLCtx(fnCtx())
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if got != want {
			t.Errorf("got = %v, want %v", got, want)
		}
		if !sameArgs(args, []any{fnTenant, 0}) {
			t.Errorf("args = %v, want %v", args, []any{fnTenant, 0})
		}
	})

	t.Run("UPDATE SET with no tenant refuses", func(t *testing.T) {
		upd := db.Update(plain).Set(n.Expr(pg.Coalesce(pg.Subquery(sub()), 0)))
		sql, _, err := upd.ToSQLCtx(context.Background())
		if !errors.Is(err, pg.ErrTenantMissing) {
			t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
		}
		if sql != "" {
			t.Errorf("got sql = %q, want no statement at all", sql)
		}
	})
}

// A function expression is a value a caller may build once and use in
// two requests. Resolution copies, so the second request resolves
// against its own ctx rather than answering with the first one's
// tenant — the same property scopetree_test.go pins for predicates.
func TestFunctionExpressionIsReusableAcrossRequests(t *testing.T) {
	plain := pg.NewTable("fr_plain")
	pg.Add(plain, pg.BigSerial("id").PrimaryKey())
	posts := reachTable("fr_posts")
	db := pg.New(nil)

	expr := pg.Coalesce(pg.Subquery(db.Select(posts.Col("id")).From(posts)), 0)
	sel := db.Select(expr).From(plain).Unscoped()

	want := `SELECT coalesce((SELECT "fr_posts"."id" FROM "fr_posts" ` +
		`WHERE ("fr_posts"."deletedAt" IS NULL) AND ("fr_posts"."tenantId" = $1)), $2) ` +
		`FROM "fr_plain"`

	for _, tenant := range []int64{fnTenant, treeTenant, fnTenant} {
		got, args, err := sel.ToSQLCtx(pg.WithTenant(context.Background(), tenant))
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if got != want {
			t.Errorf("got = %v, want %v", got, want)
		}
		if !sameArgs(args, []any{tenant, 0}) {
			t.Errorf("args = %v, want %v", args, []any{tenant, 0})
		}
	}
}

// The typed JSON accessors compare against a leaf value rather than an
// expression, so there is no statement for them to hide — but they
// built their predicates out of closures all the same, and a closure is
// the shape this round is removing. These pin what they render.
func TestJSONPathPredicatesRenderUnchanged(t *testing.T) {
	users := pg.NewTable("fj_users")
	meta := pg.Add(users, pg.JSONB("meta"))
	theme := pg.JSONField[string](meta, "settings", "theme")
	bare := pg.JSONField[bool](meta)

	tests := []struct {
		name string
		expr drops.Expression
		want string
		args []any
	}{
		{"Eq", theme.Eq("dark"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text = $1)`, []any{"dark"}},
		{"Ne", theme.Ne("dark"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text <> $1)`, []any{"dark"}},
		{"Gt", theme.Gt("a"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text > $1)`, []any{"a"}},
		{"Gte", theme.Gte("a"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text >= $1)`, []any{"a"}},
		{"Lt", theme.Lt("a"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text < $1)`, []any{"a"}},
		{"Lte", theme.Lte("a"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text <= $1)`, []any{"a"}},
		{"In", theme.In("a", "b"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text IN ($1, $2))`, []any{"a", "b"}},
		{"In of nothing", theme.In(), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text IN ())`, nil},
		{"IsNull", theme.IsNull(), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text IS NULL)`, nil},
		{"IsNotNull", theme.IsNotNull(), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text IS NOT NULL)`, nil},
		{"Like", theme.Like("a%"), `(("fj_users"."meta" -> 'settings' ->> 'theme')::text LIKE $1)`, []any{"a%"}},
		{"a path of no segments", bare.Eq(true), `("fj_users"."meta" = $1)`, []any{true}},
		{"JSONContains", pg.JSONContains(meta, `{"a":1}`), `("fj_users"."meta" @> $1)`, []any{`{"a":1}`}},
		{"JSONContains of nil", pg.JSONContains(meta, nil),
			`/* drops/pg: JSONContains called with nil value */ FALSE`, nil},
		{"JSONHasKey", pg.JSONHasKey(meta, "a"), `("fj_users"."meta" ? $1)`, []any{"a"}},
		{"JSONHasAnyKey", pg.JSONHasAnyKey(meta, []string{"a"}), `("fj_users"."meta" ?| $1)`, nil},
		{"JSONHasAllKeys", pg.JSONHasAllKeys(meta, []string{"a"}), `("fj_users"."meta" ?& $1)`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args := drops.String(tc.expr)
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if tc.args != nil && !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}
}

// TestFunctionExpressionsRenderUnchanged is the byte-identity corpus.
//
// Holding the arguments instead of closing over them must not change
// one byte of any expression that has no statement inside it — which is
// nearly every expression this package has ever rendered. The list
// covers every helper family that moved, including the shapes with an
// optional clause (Substring's FOR, Round's digits, RegexpReplace's
// flags, an empty CASE, an empty window spec) where a rewrite is most
// likely to drop or double a separator.
func TestFunctionExpressionsRenderUnchanged(t *testing.T) {
	col := drops.Raw(`"t"."c"`)

	tests := []struct {
		name string
		expr drops.Expression
		want string
		args []any
	}{
		// funcs.go
		{"Count", pg.Count(col), `count("t"."c")`, nil},
		{"CountDistinct", pg.CountDistinct(col), `count(DISTINCT "t"."c")`, nil},
		{"CountAll", pg.CountAll(), `count(*)`, nil},
		{"SumDistinct", pg.SumDistinct(col), `sum(DISTINCT "t"."c")`, nil},
		{"AvgDistinct", pg.AvgDistinct(col), `avg(DISTINCT "t"."c")`, nil},
		{"Filter", pg.Filter(pg.CountAll(), pg.Eq(col, 1)),
			`count(*) FILTER (WHERE ("t"."c" = $1))`, []any{1}},
		{"StringAgg", pg.StringAgg(col, ","), `string_agg("t"."c", $1)`, []any{","}},
		{"BoolAnd", pg.BoolAnd(col), `bool_and("t"."c")`, nil},
		{"BoolOr", pg.BoolOr(col), `bool_or("t"."c")`, nil},
		{"Every", pg.Every(col), `every("t"."c")`, nil},
		{"Sum", pg.Sum(col), `sum("t"."c")`, nil},
		{"Avg", pg.Avg(col), `avg("t"."c")`, nil},
		{"Min", pg.Min(col), `min("t"."c")`, nil},
		{"Max", pg.Max(col), `max("t"."c")`, nil},
		{"Lower", pg.Lower(col), `lower("t"."c")`, nil},
		{"Upper", pg.Upper(col), `upper("t"."c")`, nil},
		{"Coalesce", pg.Coalesce(col, 0), `coalesce("t"."c", $1)`, []any{0}},
		{"Coalesce of nothing", pg.Coalesce(), `coalesce()`, nil},
		{"Now", pg.Now(), `now()`, nil},
		{"Func", pg.Func("f", col, 1), `f("t"."c", $1)`, []any{1}},
		{"Func of nothing", pg.Func("f"), `f()`, nil},
		{"As", pg.As(col, "x"), `"t"."c" AS "x"`, nil},
		// The one rendering this round changes on purpose: an empty
		// alias used to render `AS ""`, a zero-length delimited
		// identifier PostgreSQL rejects with a syntax error, and now
		// renders no AS clause — which is what AsSubquery has always
		// done with one.
		{"As with an empty alias", pg.As(col, ""), `"t"."c"`, nil},

		// strings.go
		{"Concat", pg.Concat(col, "a", 1), `concat("t"."c", $1::text, $2::bigint)`, []any{"a", 1}},
		{"ConcatWS", pg.ConcatWS("-", col), `concat_ws($1::text, "t"."c")`, []any{"-"}},
		{"ConcatOp", pg.ConcatOp(col, "a"), `("t"."c" || $1)`, []any{"a"}},
		{"Length", pg.Length(col), `length("t"."c")`, nil},
		{"CharLength", pg.CharLength(col), `char_length("t"."c")`, nil},
		{"Substring", pg.Substring(col, 1, 2), `substring("t"."c" FROM $1 FOR $2)`, []any{1, 2}},
		{"Substring without FOR", pg.Substring(col, 1, nil), `substring("t"."c" FROM $1)`, []any{1}},
		{"Trim", pg.Trim(col), `trim("t"."c")`, nil},
		{"LTrim", pg.LTrim(col), `ltrim("t"."c")`, nil},
		{"RTrim", pg.RTrim(col), `rtrim("t"."c")`, nil},
		{"Replace", pg.Replace(col, "a", "b"), `replace("t"."c", $1, $2)`, []any{"a", "b"}},
		{"RegexpReplace", pg.RegexpReplace(col, "a", "b"),
			`regexp_replace("t"."c", $1, $2)`, []any{"a", "b"}},
		{"RegexpReplace with flags", pg.RegexpReplace(col, "a", "b", "g"),
			`regexp_replace("t"."c", $1, $2, $3)`, []any{"a", "b", "g"}},
		{"RegexpReplace with an empty flag", pg.RegexpReplace(col, "a", "b", ""),
			`regexp_replace("t"."c", $1, $2)`, []any{"a", "b"}},
		{"RegexpMatch", pg.RegexpMatch(col, "a"), `regexp_match("t"."c", $1)`, []any{"a"}},
		{"Position", pg.Position("a", col), `position($1 IN "t"."c")`, []any{"a"}},
		{"StrPos", pg.StrPos(col, "a"), `strpos("t"."c", $1)`, []any{"a"}},
		{"Initcap", pg.Initcap(col), `initcap("t"."c")`, nil},
		{"Format", pg.Format("%s", col), `format($1, "t"."c")`, []any{"%s"}},
		{"ToChar", pg.ToChar(col, "YYYY"), `to_char("t"."c", $1)`, []any{"YYYY"}},
		{"Md5", pg.Md5(col), `md5("t"."c")`, nil},
		{"Encode", pg.Encode(col, "hex"), `encode("t"."c", $1)`, []any{"hex"}},
		{"Decode", pg.Decode(col, "hex"), `decode("t"."c", $1)`, []any{"hex"}},

		// math.go
		{"Abs", pg.Abs(col), `abs("t"."c")`, nil},
		{"Ceil", pg.Ceil(col), `ceil("t"."c")`, nil},
		{"Floor", pg.Floor(col), `floor("t"."c")`, nil},
		{"Round", pg.Round(col), `round("t"."c")`, nil},
		{"Round with digits", pg.Round(col, 2), `round("t"."c", $1)`, []any{2}},
		{"Mod", pg.Mod(col, 2), `mod("t"."c", $1)`, []any{2}},
		{"Power", pg.Power(col, 2), `power("t"."c", $1)`, []any{2}},
		{"Sqrt", pg.Sqrt(col), `sqrt("t"."c")`, nil},
		{"Sign", pg.Sign(col), `sign("t"."c")`, nil},
		{"Exp", pg.Exp(col), `exp("t"."c")`, nil},
		{"Ln", pg.Ln(col), `ln("t"."c")`, nil},
		{"Log", pg.Log(col), `log("t"."c")`, nil},
		{"Greatest", pg.Greatest(col, 1), `greatest("t"."c", $1)`, []any{1}},
		{"Least", pg.Least(col, 1), `least("t"."c", $1)`, []any{1}},
		{"Random", pg.Random(), `random()`, nil},
		{"Sin", pg.Sin(col), `sin("t"."c")`, nil},
		{"Cos", pg.Cos(col), `cos("t"."c")`, nil},
		{"Tan", pg.Tan(col), `tan("t"."c")`, nil},
		{"Asin", pg.Asin(col), `asin("t"."c")`, nil},
		{"Acos", pg.Acos(col), `acos("t"."c")`, nil},
		{"Atan", pg.Atan(col), `atan("t"."c")`, nil},
		{"Plus", pg.Plus(col, 1), `("t"."c" + $1)`, []any{1}},
		{"Minus", pg.Minus(col, 1), `("t"."c" - $1)`, []any{1}},
		{"Mul", pg.Mul(col, 1), `("t"."c" * $1)`, []any{1}},
		{"Div", pg.Div(col, 1), `("t"."c" / $1)`, []any{1}},

		// json.go
		{"JSONGet", pg.JSONGet(col, "k"), `("t"."c" -> $1)`, []any{"k"}},
		{"JSONGetText", pg.JSONGetText(col, "k"), `("t"."c" ->> $1)`, []any{"k"}},
		{"ToJSON", pg.ToJSON(col), `to_json("t"."c")`, nil},
		{"ToJSONB", pg.ToJSONB(col), `to_jsonb("t"."c")`, nil},
		{"JSONArrayLength", pg.JSONArrayLength(col), `json_array_length("t"."c")`, nil},
		{"JSONBArrayLength", pg.JSONBArrayLength(col), `jsonb_array_length("t"."c")`, nil},
		{"JSONTypeof", pg.JSONTypeof(col), `json_typeof("t"."c")`, nil},
		{"JSONBTypeof", pg.JSONBTypeof(col), `jsonb_typeof("t"."c")`, nil},
		{"JSONBuildObject", pg.JSONBuildObject("a", col),
			`json_build_object($1::text, "t"."c")`, []any{"a"}},
		{"JSONBuildArray", pg.JSONBuildArray(col, 1.5),
			`json_build_array("t"."c", $1::double precision)`, []any{1.5}},
		{"JSONBBuildObject", pg.JSONBBuildObject("a", col),
			`jsonb_build_object($1::text, "t"."c")`, []any{"a"}},
		{"JSONBBuildArray", pg.JSONBBuildArray(col, true),
			`jsonb_build_array("t"."c", $1::boolean)`, []any{true}},
		{"JSONBuildObject of nothing", pg.JSONBuildObject(), `json_build_object()`, nil},
		{"JSONBSet", pg.JSONBSet(col, "{a}", 1), `jsonb_set("t"."c", $1, $2)`, []any{"{a}", 1}},
		{"JSONBSet with createMissing", pg.JSONBSet(col, "{a}", 1, true),
			`jsonb_set("t"."c", $1, $2, $3)`, []any{"{a}", 1, true}},
		{"JSONBInsert", pg.JSONBInsert(col, "{a}", 1), `jsonb_insert("t"."c", $1, $2)`, []any{"{a}", 1}},
		{"JSONBInsert with insertAfter", pg.JSONBInsert(col, "{a}", 1, true),
			`jsonb_insert("t"."c", $1, $2, $3)`, []any{"{a}", 1, true}},
		{"JSONBStripNulls", pg.JSONBStripNulls(col), `jsonb_strip_nulls("t"."c")`, nil},
		{"JSONBPretty", pg.JSONBPretty(col), `jsonb_pretty("t"."c")`, nil},
		{"JSONAgg", pg.JSONAgg(col), `json_agg("t"."c")`, nil},
		{"JSONBAgg", pg.JSONBAgg(col), `jsonb_agg("t"."c")`, nil},
		{"JSONObjectAgg", pg.JSONObjectAgg(col, col), `json_object_agg("t"."c", "t"."c")`, nil},
		{"JSONBObjectAgg", pg.JSONBObjectAgg(col, col), `jsonb_object_agg("t"."c", "t"."c")`, nil},

		// datetime.go
		{"CurrentDate", pg.CurrentDate(), `current_date`, nil},
		{"CurrentTime", pg.CurrentTime(), `current_time`, nil},
		{"CurrentTimestamp", pg.CurrentTimestamp(), `current_timestamp`, nil},
		{"LocalTime", pg.LocalTime(), `localtime`, nil},
		{"LocalTimestamp", pg.LocalTimestamp(), `localtimestamp`, nil},
		{"DateTrunc", pg.DateTrunc("day", col), `date_trunc($1, "t"."c")`, []any{"day"}},
		{"Extract", pg.Extract("year", col), `extract(year FROM "t"."c")`, nil},
		{"DatePart", pg.DatePart("day", col), `date_part($1, "t"."c")`, []any{"day"}},
		{"Age", pg.Age(col), `age("t"."c")`, nil},
		{"Age of two", pg.Age(col, col), `age("t"."c", "t"."c")`, nil},
		{"IntervalLit", pg.IntervalLit("1 day"), `INTERVAL '1 day'`, nil},
		{"Day", pg.Day(2), `INTERVAL '2 day'`, nil},
		{"MakeDate", pg.MakeDate(2020, 1, 2), `make_date($1, $2, $3)`, []any{2020, 1, 2}},
		{"MakeTime", pg.MakeTime(1, 2, 3), `make_time($1, $2, $3)`, []any{1, 2, 3}},
		{"ToDate", pg.ToDate(col, "YYYY"), `to_date("t"."c", $1)`, []any{"YYYY"}},
		{"ToTimestamp", pg.ToTimestamp(col, "YYYY"), `to_timestamp("t"."c", $1)`, []any{"YYYY"}},
		{"ToNumber", pg.ToNumber(col, "999"), `to_number("t"."c", $1)`, []any{"999"}},
		{"AtTimeZone", pg.AtTimeZone(col, "UTC"), `("t"."c" AT TIME ZONE $1)`, []any{"UTC"}},

		// window.go
		{"RowNumber", pg.RowNumber(), `row_number()`, nil},
		{"Rank", pg.Rank(), `rank()`, nil},
		{"DenseRank", pg.DenseRank(), `dense_rank()`, nil},
		{"PercentRank", pg.PercentRank(), `percent_rank()`, nil},
		{"CumeDist", pg.CumeDist(), `cume_dist()`, nil},
		{"Over an empty spec", pg.Over(pg.RowNumber(), pg.WindowSpec()), `row_number() OVER ()`, nil},
		{"Over PARTITION BY", pg.Over(pg.RowNumber(), pg.WindowSpec().PartitionBy(col, col)),
			`row_number() OVER (PARTITION BY "t"."c", "t"."c")`, nil},
		{"Over ORDER BY", pg.Over(pg.RowNumber(), pg.WindowSpec().OrderBy(col)),
			`row_number() OVER (ORDER BY "t"."c")`, nil},
		{"Over a frame", pg.Over(pg.RowNumber(), pg.WindowSpec().Frame("ROWS UNBOUNDED PRECEDING")),
			`row_number() OVER (ROWS UNBOUNDED PRECEDING)`, nil},
		{"Over the whole spec", pg.Over(pg.RowNumber(),
			pg.WindowSpec().PartitionBy(col).OrderBy(col).Frame("ROWS UNBOUNDED PRECEDING")),
			`row_number() OVER (PARTITION BY "t"."c" ORDER BY "t"."c" ROWS UNBOUNDED PRECEDING)`, nil},
		{"Ntile", pg.Ntile(4), `ntile($1)`, []any{4}},
		{"Lag", pg.Lag(col), `lag("t"."c")`, nil},
		{"Lag with offset and default", pg.Lag(col, 1, 0), `lag("t"."c", $1, $2)`, []any{1, 0}},
		{"Lead", pg.Lead(col), `lead("t"."c")`, nil},
		{"FirstValue", pg.FirstValue(col), `first_value("t"."c")`, nil},
		{"LastValue", pg.LastValue(col), `last_value("t"."c")`, nil},
		{"NthValue", pg.NthValue(col, 2), `nth_value("t"."c", $1)`, []any{2}},

		// cast.go
		{"Cast", pg.Cast(col, "bigint"), `("t"."c")::bigint`, nil},
		{"CastAs", pg.CastAs(col, "bigint"), `CAST("t"."c" AS bigint)`, nil},
		{"Case", pg.Case().When(pg.Eq(col, 1), "a").When(pg.Eq(col, 2), "b").Else("c").End(),
			`CASE WHEN ("t"."c" = $1) THEN $2 WHEN ("t"."c" = $3) THEN $4 ELSE $5 END`,
			[]any{1, "a", 2, "b", "c"}},
		{"Case with no ELSE", pg.Case().When(pg.Eq(col, 1), "a").End(),
			`CASE WHEN ("t"."c" = $1) THEN $2 END`, []any{1, "a"}},
		{"Case with no branches", pg.Case().End(), `CASE END`, nil},
		{"CaseOn", pg.CaseOn(col).When(1, 2).Else(3).End(),
			`CASE "t"."c" WHEN $1 THEN $2 ELSE $3 END`, []any{1, 2, 3}},

		// ddl.go — the one DDL helper that takes a value.
		{"SetVal", pg.SetVal("s", 5), `setval('"s"', $1)`, []any{5}},
		{"NextVal", pg.NextVal("s"), `nextval('"s"'::regclass)`, nil},
		{"CurrVal", pg.CurrVal("s"), `currval('"s"')`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args := drops.String(tc.expr)
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}
}

// Flattening the window specification when Over is called, rather than
// while rendering, snapshots it: a *Window mutated afterwards no longer
// changes what an expression already built renders.
//
// That is the same rule the rest of the package follows — a predicate
// is a value, and what it renders is fixed when it is built — and it is
// what lets the partition and order terms be held as operands instead
// of being reached through a closure the resolver walk cannot enter.
func TestWindowSpecIsSnapshotWhenOverIsCalled(t *testing.T) {
	col := drops.Raw(`"t"."c"`)
	win := pg.WindowSpec().PartitionBy(col)
	expr := pg.Over(pg.RowNumber(), win)

	win.OrderBy(col).Frame("ROWS UNBOUNDED PRECEDING")

	want := `row_number() OVER (PARTITION BY "t"."c")`
	if got, _ := drops.String(expr); got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	// The spec itself keeps accumulating, for the next expression built
	// from it.
	want = `row_number() OVER (PARTITION BY "t"."c" ORDER BY "t"."c" ROWS UNBOUNDED PRECEDING)`
	if got, _ := drops.String(pg.Over(pg.RowNumber(), win)); got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
}

// A DDL view body is the one operand this round deliberately leaves
// unresolved, so what it renders is pinned here rather than left to be
// discovered.
//
// A view outlives the request that created it. Resolving the body would
// bake the creating request's tenant into an object every other tenant
// then reads through, and views are created in migrations, where there
// is no tenant on ctx at all — so resolving would also refuse the
// statement in the one place it is actually issued. The body is held in
// a field rather than swallowed by a closure, and stays the caller's to
// scope, exactly as a CTE body built from raw fragments does.
func TestViewBodiesAreTheCallersToScope(t *testing.T) {
	posts := reachTable("fv_posts")
	db := pg.New(nil)
	body := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }
	// The DefaultFilter renders through WriteSQL; the ContextFilter
	// cannot, having no ctx to resolve against.
	inner := `SELECT "fv_posts"."id" FROM "fv_posts" WHERE ("fv_posts"."deletedAt" IS NULL)`

	tests := []struct {
		name string
		expr drops.Expression
		want string
	}{
		{"CreateView", pg.CreateView("v", body()), `CREATE VIEW "v" AS ` + inner},
		{"CreateOrReplaceView", pg.CreateOrReplaceView("v", body()),
			`CREATE OR REPLACE VIEW "v" AS ` + inner},
		{"CreateMaterializedView", pg.CreateMaterializedView("v", body(), true),
			`CREATE MATERIALIZED VIEW "v" AS ` + inner + ` WITH DATA`},
		{"CreateMaterializedView with no data", pg.CreateMaterializedView("v", body(), false),
			`CREATE MATERIALIZED VIEW "v" AS ` + inner + ` WITH NO DATA`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := drops.String(tc.expr)
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

// The package must contain no drops.ExprFunc that swallows an operand.
//
// This is the invariant stated structurally rather than behaviourally:
// writeOperand — the render-time "is it an Expression or a value?"
// decision the closures were built around — no longer exists, so a new
// helper cannot be written in the old shape by copying its neighbour.
// The check is on the sources because the shape, not the output, is
// what the next round would otherwise have to rediscover.
func TestNoExpressionHelperRendersOperandsAtRenderTime(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "writeOperand" {
					t.Errorf("%s: writeOperand is back — an operand decided at render time is an operand no walk can reach",
						fset.Position(call.Pos()))
				}
				return true
			})
		}
	}
}
