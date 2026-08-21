package pg_test

import (
	"context"
	"errors"
	"fmt"
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

// exemptWidenedConstructors is the same promise for the constructors
// only the WIDENED census can see: those returning something rendered
// into a statement without being an expression. It is separate from
// exemptConstructors because each census checks its own map in both
// directions, and an entry that excuses a constructor the other census
// cannot see would fail there as stale.
var exemptWidenedConstructors = map[string]string{
	// The same rule the three view constructors above are exempt under,
	// for the other schema object that stores an expression. An index's
	// key expressions and its partial-index predicate are rendered by
	// CREATE INDEX, in a migration, with no ctx and no tenant — and an
	// index built for the tenant who happened to run the migration
	// would index one tenant's rows for everybody. See
	// exemptRelationEntryPoints["Index.Where"], which is the same
	// object refusing the same thing from the other census.
	"NewIndex": "DDL: an index is a declaration rendered by the migrator, so a resolved key or predicate would pin one tenant",
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
		if covered[name] || exemptConstructors[name] != "" || exemptWidenedConstructors[name] != "" {
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
	return censusOperandConstructorsIn(loadPgSyntax(t))
}

// censusOperandConstructorsIn is the census body, taking the parsed
// package so it can be pointed at one built to slip past it — the shape
// every check in this family has since round 8.
func censusOperandConstructorsIn(p *pgSyntax) []string {
	var out []string
	for _, fn := range p.funcs {
		if !fn.Name.IsExported() {
			continue
		}
		if returnsExpression(fn.Type.Results) && p.takesOperand(fn.Type.Params) {
			out = append(out, fn.Name.Name)
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

func (p *pgSyntax) takesOperand(fl *ast.FieldList) bool {
	if fl == nil {
		return false
	}
	for _, f := range fl.List {
		if p.isOperandType(f.Type) {
			return true
		}
	}
	return false
}

// isOperandType reports whether a parameter can carry a statement: an
// `any` (or a slice/variadic of one) and a drops.Expression can, a
// string or a *Table cannot.
//
// The question is what the declaration ADMITS, which is why it goes
// through [pgSyntax.interfaceOf] rather than matching `any` and the
// empty interface literally. Those two were the whole answer once, and
// one line defeated it:
//
//	type operand interface{ drops.Expression }
//
// A parameter of that type takes a statement exactly as an `any` does
// — it is an *ast.Ident whose name is not "any", so the census walked
// past every helper that declared one.
func (p *pgSyntax) isOperandType(e ast.Expr) bool {
	if isExpressionType(elementType(e)) {
		return true
	}
	if it := p.interfaceOf(e); it != nil {
		return p.interfaceAdmits(it, p.rendering)
	}
	return false
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

// ----------------------------------------------------------------------
// The third invariant, enforced against the package source
//
//	No expression list the renderer reads may be invisible to the
//	resolver.
//
// Round 4 said an operand a caller can hand a *SelectBuilder to must
// not be an opaque closure. Round 5 enforced it with the census above:
// every exported package-level constructor returning a drops.Expression
// that takes an operand is exercised with a scoped subquery. Two leaks
// obeyed that rule and leaked anyway, and the census structurally could
// not see either one — it enumerates constructors, and neither leak was
// one.
//
// The first sat on a builder FIELD. SelectBuilder.distinctOn was
// written by writeCore and absent from the list resolveCtx walks, so
// DISTINCT ON (a scoped subquery) rendered the inner statement through
// the renderer alone — no ctx, its DefaultFilters written, its
// ContextFilters not, and no refusal on a ctx with no tenant at all.
//
// The second sat on a filter's RETURN value: the predicate a
// ContextFilter or a DefaultFilter answers with was handed to the
// builder and rendered without ever being walked, so a Guard whose
// predicate named another scoped table was itself the unscoped read.
//
// What the two have in common is not a constructor and not a closure.
// It is a list the renderer reads that the resolver does not. The three
// tests below say that mechanically, so the next one fails on the day
// it is written:
//
//   - TestNoRenderedExpressionListIsInvisibleToTheResolver reads the
//     four statement builders' struct fields and requires every
//     expression-bearing field the render closure touches to be read by
//     the resolve closure;
//   - TestEveryBuilderOperandMethodIsCensused requires every exported
//     builder METHOD that takes an operand — which is how a caller puts
//     an expression into one of those fields — to be exercised with a
//     scoped subquery;
//   - TestEveryWidenedOperandConstructorIsCensused re-runs the round-5
//     function census under the wider rules the narrow one missed:
//     multi-result signatures, *SelectBuilder-typed and ColumnValue
//     parameters, and results that are rendered without being
//     drops.Expression themselves.
//
// Both exemption lists are checked in both directions, so a stale entry
// fails as loudly as a missing one — the pattern round 5 established.

// pgSyntax is the package's own source, indexed for the checks below:
// its named types, which of them can hold a caller's expression, and
// the methods declared on each. It is a second parse rather than a
// refactor of the two above: those carry round 5's assertions, and this
// work removes no test line that already passes.
type pgSyntax struct {
	fset    *token.FileSet
	structs map[string]*ast.StructType
	ifaces  map[string]*ast.InterfaceType
	aliases map[string]ast.Expr                 // named types that are not structs or interfaces
	methods map[string]map[string]*ast.FuncDecl // receiver type name -> method name -> decl
	funcs   []*ast.FuncDecl                     // package-level functions
	bearing map[string]bool                     // types that can hold a caller's expression

	// statementBuilding is the set of package types that implement one
	// of resolveExpr's arms and render — a statement builder, derived
	// from the resolver's own decision instead of from the name
	// *SelectBuilder. See statementBuildingTypes.
	statementBuilding map[string]bool

	// rendering is the set of package types that render a caller's
	// expression, and it is the set an interface's method demand is
	// answered against. bearing is the wrong set for that question:
	// bearing follows field types to any depth, so a *Column is bearing
	// because it holds a *Table which holds DefaultFilters — and a
	// column reference is not a statement, which is the distinction
	// typesRenderingCallerExpressions exists to draw.
	rendering map[string]bool
}

// loadPgSyntax parses the non-test sources of package pg and computes
// which of its named types can hold an expression a caller supplied.
//
// "Can hold an expression" is computed rather than listed, because a
// list is the thing that went stale after each of the previous rounds.
// A struct bears expressions when one of its fields is a
// drops.Expression (at any slice, array, pointer or map depth) or is
// itself a bearing type; a named slice or map type bears them when its
// element does; and an interface bears them when some bearing struct in
// the package implements it — which is how ColumnValue qualifies, since
// exprBinding holds a drops.Expression and satisfies it. The fixpoint
// is run to convergence so a type three hops from an expression still
// counts.
func loadPgSyntax(t *testing.T) *pgSyntax {
	t.Helper()
	return loadPgSyntaxWith(t)
}

// loadPgSyntaxWith is loadPgSyntax over the package source PLUS the
// synthetic sources in extra, each the text of one additional file of
// package pg.
//
// It exists so that a check can be pointed at a package that contains
// the shape it is supposed to refuse. A check nobody has ever seen fail
// is a check nobody has tested, and every bypass closed in this round
// was found by someone writing the bypass rather than by reading the
// checker: see pg/scopecheckself_test.go, which rebuilds each one and
// requires the check to name it.
func loadPgSyntaxWith(t *testing.T, extra ...string) *pgSyntax {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	for i, src := range extra {
		f, err := parser.ParseFile(fset, fmt.Sprintf("synthetic%d.go", i), src, 0)
		if err != nil {
			t.Fatalf("parse synthetic source %d: %v", i, err)
		}
		files = append(files, f)
	}
	p := &pgSyntax{
		fset:    fset,
		structs: map[string]*ast.StructType{},
		ifaces:  map[string]*ast.InterfaceType{},
		aliases: map[string]ast.Expr{},
		methods: map[string]map[string]*ast.FuncDecl{},
		bearing: map[string]bool{},
	}
	{
		for _, f := range files {
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok {
					if fn.Recv == nil {
						p.funcs = append(p.funcs, fn)
						continue
					}
					r := typeName(fn.Recv.List[0].Type)
					if p.methods[r] == nil {
						p.methods[r] = map[string]*ast.FuncDecl{}
					}
					p.methods[r][fn.Name.Name] = fn
					continue
				}
				gd, ok := d.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, sp := range gd.Specs {
					ts, ok := sp.(*ast.TypeSpec)
					if !ok {
						continue
					}
					switch u := ts.Type.(type) {
					case *ast.StructType:
						p.structs[ts.Name.Name] = u
					case *ast.InterfaceType:
						p.ifaces[ts.Name.Name] = u
					default:
						p.aliases[ts.Name.Name] = ts.Type
					}
				}
			}
		}
	}
	for {
		changed := false
		for name, st := range p.structs {
			if p.bearing[name] {
				continue
			}
			for _, f := range st.Fields.List {
				if p.bearsExpression(f.Type) {
					p.bearing[name], changed = true, true
					break
				}
			}
		}
		for name, u := range p.aliases {
			if !p.bearing[name] && p.bearsExpression(u) {
				p.bearing[name], changed = true, true
			}
		}
		for name := range p.ifaces {
			if p.bearing[name] {
				continue
			}
			if p.interfaceHolds(name, p.bearing) {
				p.bearing[name], changed = true, true
			}
		}
		if !changed {
			p.rendering = p.typesRenderingCallerExpressions()
			p.statementBuilding = p.statementBuildingTypes()
			return p
		}
	}
}

// statementBuildingTypes returns the package types the resolver can
// enter AND that render: the derived spelling of "*SelectBuilder".
//
// It reads resolveExpr's own type switch, which is the one place that
// decides what a statement is, and asks which types satisfy an arm of
// it. Written as a name, the answer went stale the day the second
// builder was written — which is the whole history of this file. The
// staleness fatals live in resolverArms; this returns an empty set
// rather than reporting, so a synthetic package can be walked with it.
func (p *pgSyntax) statementBuildingTypes() map[string]bool {
	fn := p.allFuncs()["resolveExpr"]
	if fn == nil {
		return map[string]bool{}
	}
	var arms [][]string
	ast.Inspect(fn, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok {
			return true
		}
		for _, e := range cc.List {
			if tn := typeName(e); tn != "" && p.ifaces[tn] != nil {
				arms = append(arms, p.interfaceMethods(tn))
			}
		}
		return true
	})
	out := map[string]bool{}
	for name := range p.structs {
		renders := false
		for _, m := range p.methods[name] {
			if takesBuilder(m.Type) {
				renders = true
				break
			}
		}
		if !renders {
			continue
		}
		for _, arm := range arms {
			if len(arm) > 0 && hasAllMethods(p.methods[name], arm) {
				out[name] = true
				break
			}
		}
	}
	return out
}

// interfaceOf returns the interface a declaration's type IS, whichever
// of the three ways it is spelled: a local interface name, an inline
// anonymous interface, or `any`.
//
// The three are one promise, and every check that matched only the last
// two was walked past by writing the first. Slice, pointer, variadic
// and map wrappers are stripped on the way, because a []operand takes
// operands.
func (p *pgSyntax) interfaceOf(e ast.Expr) *ast.InterfaceType {
	switch v := elementType(e).(type) {
	case *ast.InterfaceType:
		return v
	case *ast.Ident:
		if v.Name == "any" {
			return emptyInterface
		}
		return p.ifaces[v.Name]
	}
	return nil
}

// emptyInterface stands for `any`, which is an identifier rather than
// an *ast.InterfaceType in the syntax tree and the same promise in the
// language.
var emptyInterface = &ast.InterfaceType{Methods: &ast.FieldList{}}

func hasAllMethods(have map[string]*ast.FuncDecl, want []string) bool {
	for _, w := range want {
		if have[w] == nil {
			return false
		}
	}
	return true
}

// interfaceHolds reports whether a field declared with this local
// interface type can hold an expression a caller supplied, given the
// set of package types that already can.
//
// It answers by what the interface DEMANDS, and the three answers are
// three different reasons — which is the point, because the version
// that only knew the third had a hole a caller could walk through:
//
//   - an interface that embeds drops.Expression holds one by
//     definition. This is the bypass that was open. The check asked
//     only "which bearing struct in this package implements it?", and
//     for `type holder interface{ drops.Expression }` the answer was
//     "none of them, it declares no methods of its own" — so a field
//     typed holder was read as holding nothing, and everything the
//     round-6 and round-7 checks say about expression-bearing fields
//     stopped applying to it. An interface is a promise about what a
//     value can do, and this one promises exactly the thing the
//     resolver walks.
//   - an interface that demands nothing at all — `any`, or one that
//     embeds only interfaces from other packages — holds anything,
//     including a statement.
//   - otherwise it holds one when some bearing type in the package
//     satisfies its own methods, which is how ColumnValue qualifies:
//     exprBinding holds a drops.Expression and implements it.
//
// The first two are fail-closed answers: an interface whose method set
// a caller's expression can satisfy is treated as holding one, because
// being wrong in that direction costs a check that asks for scoping
// where none was needed, and being wrong the other way costs a
// statement nobody walked.
func (p *pgSyntax) interfaceHolds(name string, bearing map[string]bool) bool {
	return p.interfaceAdmits(p.ifaces[name], bearing)
}

// interfaceAdmits is interfaceHolds asked of the interface itself
// rather than of its name, which is what lets the same three answers
// serve a field declared with an INLINE anonymous interface.
//
// A name is a spelling. `holder` and `interface{ drops.Expression }`
// are the same promise, and a check that could only answer for the
// first was defeated by deleting one line of the declaration — the same
// shape as the bypass interfaceHolds was written to close, one syntax
// further out.
func (p *pgSyntax) interfaceAdmits(it *ast.InterfaceType, bearing map[string]bool) bool {
	if it == nil {
		return false
	}
	own := p.interfaceMethodsOf(it)
	if len(own) == 0 {
		// Demands nothing at all — `any`, or an interface embedding
		// only interfaces from other packages. It holds anything,
		// including a statement.
		return true
	}
	if !sealedToCallers(own) && p.embedsExpression(it) {
		// Embeds drops.Expression and can be implemented from outside:
		// it promises exactly the thing the resolver walks. This is the
		// bypass that was open — the check asked only "which bearing
		// struct implements it?", and `interface{ drops.Expression }`
		// declares no methods of its own, so the answer was "none".
		return true
	}
	// Otherwise it holds one when some type that already does satisfies
	// its methods, which is how ColumnValue qualifies: exprBinding holds
	// a drops.Expression and implements it.
	//
	// A SEALED interface reaches this answer and no other, and that is
	// the difference between ColumnValue and ColRef. Both demand an
	// unexported method, so no caller can implement either; what a
	// caller CAN do is get one from an exported constructor of this
	// package, and whether that hands them somewhere to put a statement
	// is a question about this package's own implementors rather than
	// about the embedded drops.Expression. exprBinding holds a caller's
	// expression, so ColumnValue admits one. *Column is the only ColRef
	// and renders a qualified name, so ColRef admits none — which it
	// must not, or every helper taking a column reference is read as
	// taking a statement.
	for impl := range bearing {
		if bearing[impl] && p.methods[impl] != nil && hasAllMethods(p.methods[impl], own) {
			return true
		}
	}
	return false
}

// interfaceMethodsOf returns every method name an interface demands of
// its own, following the local interfaces it embeds. An embedded
// interface from another package is skipped: drops.Expression is
// answered by embedsExpression, on different terms.
func (p *pgSyntax) interfaceMethodsOf(it *ast.InterfaceType) []string {
	if it == nil || it.Methods == nil {
		return nil
	}
	var out []string
	for _, m := range it.Methods.List {
		if len(m.Names) == 0 {
			if inner, ok := m.Type.(*ast.InterfaceType); ok {
				out = append(out, p.interfaceMethodsOf(inner)...)
				continue
			}
			if e := typeName(m.Type); e != "" && p.ifaces[e] != nil {
				out = append(out, p.interfaceMethodsOf(p.ifaces[e])...)
			}
			continue
		}
		for _, n := range m.Names {
			out = append(out, n.Name)
		}
	}
	return out
}

// embedsExpression reports whether the interface embeds
// drops.Expression, directly or through a local interface.
func (p *pgSyntax) embedsExpression(it *ast.InterfaceType) bool {
	if it == nil || it.Methods == nil {
		return false
	}
	for _, m := range it.Methods.List {
		if len(m.Names) != 0 {
			continue
		}
		if isExpressionType(m.Type) {
			return true
		}
		if inner, ok := m.Type.(*ast.InterfaceType); ok && p.embedsExpression(inner) {
			return true
		}
		if e := typeName(m.Type); e != "" && p.embedsExpression(p.ifaces[e]) {
			return true
		}
	}
	return false
}

// sealedToCallers reports whether an interface demands a method no
// caller can write: an unexported one. Nothing outside this package can
// implement it, so "a caller could satisfy this with a statement" is
// not one of the reasons it holds one.
func sealedToCallers(methods []string) bool {
	for _, m := range methods {
		if !ast.IsExported(m) {
			return true
		}
	}
	return false
}

// bearsRelation reports whether a value declared with this type
// expression can carry a *Table, given the set of package types that
// already count as carrying one.
//
// It is [pgSyntax.bearsExpression] for relations, and it exists because
// every check that had a relation in it decided by the NAME "Table":
// isOperandType, isWideOperandType, takesRelation, and the holdsTable
// and holdsDB walks the two censuses run. All five were walked past
// with one line —
//
//	type rel interface{ drops.Expression }
//
// — because a field or a parameter declared with a local interface is
// an *ast.Ident naming something that is not called Table, and every
// one of those checks stopped there. A *Table IS a drops.Expression,
// which is exactly how FromExpr took a relation on the expression path
// and nothing scoped it; a local interface that embeds one takes a
// relation on the same terms and under a different spelling.
//
// The three answers are interfaceAdmits's, and they are the right three
// here for the same reasons: an interface that embeds drops.Expression
// admits a *Table by definition, an interface that demands nothing
// admits everything, and otherwise it admits one when some type that
// carries a relation satisfies its methods. Being wrong in the
// admitting direction costs a check that asks for scoping where none
// was needed; being wrong the other way costs a relation nobody
// scoped.
func (p *pgSyntax) bearsRelation(e ast.Expr, holders map[string]bool) bool {
	if isExpressionType(elementType(e)) {
		// A *Table IS a drops.Expression, and this is the answer
		// FromExpr took a relation through while nothing scoped it. It
		// is the fail-closed half, so it belongs to the questions asked
		// of what a caller can HAND IN — a parameter, a field a
		// renderer reads — and not to namesRelation.
		return true
	}
	return p.namesRelation(e, holders)
}

// namesRelation is bearsRelation without the drops.Expression answer:
// what the package DECLARES this to be, rather than what could arrive
// through it.
//
// The two are asymmetric on purpose. A parameter typed drops.Expression
// can be handed a *Table by a caller, so it is a door. A function
// declared to answer with a drops.Expression is answering with whatever
// it built — Subquery, And, Count — and reading every one of those as
// "hands a relation back" would make the census the whole package. What
// makes a returned value a second handle on a relation is that the
// package says so, in the type: a *Table, or a local interface only a
// relation satisfies, which is the one spelling of Table.As's shape
// that the name "Table" could not see.
func (p *pgSyntax) namesRelation(e ast.Expr, holders map[string]bool) bool {
	if name := typeName(e); name != "" && holders[name] {
		return true
	}
	// interfaceOf rather than a lookup in p.ifaces: `any` is an
	// identifier and an inline interface is not an identifier at all,
	// and all three spellings are the same promise.
	if it := p.interfaceOf(e); it != nil {
		return p.interfaceAdmits(it, holders)
	}
	return false
}

// declaresRelation is namesRelation minus the answer "demands nothing".
//
// `any` is a promise about nothing at all, and which way that cuts
// depends on the direction the value travels. As a PARAMETER it is a
// door: a caller can put a *Table through it, which is why
// bearsRelation admits it. As a RETURN type, or as the handle a
// function is HANDED, it says only that the package did not commit to
// anything — and reading it as a relation made (*SelectBuilder).ToSQL,
// which answers (string, []any), a door that hands relations out.
//
// An interface that demands something, or that embeds drops.Expression,
// is a commitment and is read as one.
func (p *pgSyntax) declaresRelation(e ast.Expr, holders map[string]bool) bool {
	if name := typeName(e); name != "" && holders[name] {
		return true
	}
	it := p.interfaceOf(e)
	if it == nil {
		return false
	}
	if len(p.interfaceMethodsOf(it)) == 0 && !p.embedsExpression(it) {
		return false
	}
	return p.interfaceAdmits(it, holders)
}

// relationHolders is the fixpoint the three relation predicates are
// asked against: the package types a *Table can be reached through,
// computed rather than listed.
//
// seed names the type at the root — "Table" for the relation census,
// "DB" for the executor one, since the two walks are the same question
// about two different handles.
//
// The fields are walked with declaresRelation rather than
// bearsRelation: a struct with an `any` field has committed to nothing,
// and reading one as holding a relation makes an event payload and a
// saga's state bag into relation handles. What a field DOES commit to —
// the type itself, or an interface that demands something a relation
// satisfies — is followed to convergence, so a *Table three hops away
// still counts.
func (p *pgSyntax) relationHolders(seed string) map[string]bool {
	holders := map[string]bool{seed: true}
	for {
		changed := false
		for name, st := range p.structs {
			if holders[name] {
				continue
			}
			for _, f := range st.Fields.List {
				if p.declaresRelation(f.Type, holders) {
					holders[name], changed = true, true
					break
				}
			}
		}
		for name, u := range p.aliases {
			if !holders[name] && p.declaresRelation(u, holders) {
				holders[name], changed = true, true
			}
		}
		for name, it := range p.ifaces {
			if !holders[name] && p.interfaceAdmits(it, holders) {
				holders[name], changed = true, true
			}
		}
		if !changed {
			return holders
		}
	}
}

// bearsExpression reports whether a value of type e can carry an
// expression a caller supplied.
func (p *pgSyntax) bearsExpression(e ast.Expr) bool {
	return isExpressionType(elementType(e)) || p.bearing[typeName(e)]
}

// elementType strips the wrappers a field type can put between the
// declaration and the expression inside it.
func elementType(e ast.Expr) ast.Expr {
	switch v := e.(type) {
	case *ast.StarExpr:
		return elementType(v.X)
	case *ast.ArrayType:
		return elementType(v.Elt)
	case *ast.Ellipsis:
		return elementType(v.Elt)
	case *ast.MapType:
		return elementType(v.Value)
	}
	return e
}

// typeName returns the package-local name a type expression refers to,
// after the same stripping, and "" for anything else.
func typeName(e ast.Expr) string {
	switch v := elementType(e).(type) {
	case *ast.IndexExpr:
		return typeName(v.X)
	case *ast.IndexListExpr:
		return typeName(v.X)
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// fieldsRead returns the fields of the receiver type that the closure
// of methods reachable from seeds reads off the receiver itself.
//
// Two things about it are deliberate.
//
// The closure follows calls to other methods on the same receiver, so
// that resolveCtx delegating to resolveJoins counts as resolveCtx
// reading s.joins. Written by hand, the seed list would have gone stale
// the first time a helper was extracted.
//
// It counts only "<receiver>.<field>" — a read off the value being
// resolved — and not a write to the per-execution copy. That is what
// makes the check mean what it says: a resolveCtx that assigns
// cp.distinctOn without ever looking at s.distinctOn has not resolved
// anything, and a check that accepted the assignment would have passed
// on the day the leak was written.
//
// The closure also follows the receiver into PACKAGE-LEVEL functions
// that are handed it, and that is not a refinement. While it followed
// methods only, a renderer that delegated —
//
//	func (s *SelectBuilder) WriteSQL(b *drops.Builder) { writeDistinct(b, s) }
//
// — read no field at all as far as this walk was concerned, so every
// field writeDistinct rendered was invisible to
// TestNoRenderedExpressionListIsInvisibleToTheResolver and to the
// candidate walk in TestEveryStatementBearingExpressionIsReachableByTheResolver.
// Extracting a helper is an ordinary edit, and it must not be able to
// take a field out of the checks' sight; the rebuilt bypass is in
// pg/scopecheckself_test.go.
func (p *pgSyntax) fieldsRead(recv string, seeds ...string) map[string]bool {
	st := p.structs[recv]
	if st == nil {
		return nil
	}
	fields := map[string]bool{}
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			fields[n.Name] = true
		}
	}
	var sites []walkSite
	for _, s := range seeds {
		sites = append(sites, walkSite{fn: p.methods[recv][s]})
	}
	return p.fieldsReadAt(recv, fields, sites)
}

// fieldsReadAt is fieldsRead over walk sites rather than method names,
// so a caller can seed the walk with a package-level function that
// renders the type without being a method of it.
func (p *pgSyntax) fieldsReadAt(recv string, fields map[string]bool, sites []walkSite) map[string]bool {
	read := map[string]bool{}
	queue := append([]walkSite(nil), sites...)
	seen := map[string]bool{}
	for len(queue) > 0 {
		site := queue[0]
		queue = queue[1:]
		fn := site.fn
		if fn == nil {
			continue
		}
		self := site.self
		if self == "" {
			if fn.Recv == nil || len(fn.Recv.List[0].Names) == 0 {
				continue
			}
			self = fn.Recv.List[0].Names[0].Name
		}
		key := funcKey(fn) + "/" + self
		if seen[key] {
			continue
		}
		seen[key] = true
		ast.Inspect(fn, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				base, ok := v.X.(*ast.Ident)
				if !ok || base.Name != self {
					return true
				}
				if fields[v.Sel.Name] {
					read[v.Sel.Name] = true
				}
				if m := p.methods[recv][v.Sel.Name]; m != nil {
					queue = append(queue, walkSite{fn: m})
				}
			case *ast.CallExpr:
				for _, site := range p.helperSites(v, self) {
					queue = append(queue, site)
				}
			}
			return true
		})
	}
	return read
}

// walkSite is one function to walk, and the name the value being walked
// goes by inside it — the receiver's own name in a method, and the
// parameter it arrived as in a package-level helper.
type walkSite struct {
	fn   *ast.FuncDecl
	self string
}

// funcKey names a declaration for the visited set: a package-level
// function by its name, a method by receiver and name.
func funcKey(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return typeName(fn.Recv.List[0].Type) + "." + fn.Name.Name
}

// helperSites returns the package-level functions this call hands the
// value named self to, each paired with the parameter name it arrives
// under. That is what lets the walks above follow a renderer or a
// resolver that delegates to a free function.
func (p *pgSyntax) helperSites(call *ast.CallExpr, self string) []walkSite {
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return nil
	}
	var fn *ast.FuncDecl
	for _, f := range p.funcs {
		if f.Name.Name == id.Name {
			fn = f
			break
		}
	}
	if fn == nil || fn.Type.Params == nil {
		return nil
	}
	var out []walkSite
	for i, a := range call.Args {
		arg, ok := a.(*ast.Ident)
		if !ok || arg.Name != self {
			continue
		}
		if name := paramNameAt(fn.Type.Params, i); name != "" && name != "_" {
			out = append(out, walkSite{fn: fn, self: name})
		}
	}
	return out
}

// paramNameAt returns the name of the i'th parameter of a signature,
// counting grouped declarations (a, b int) as the two they are.
func paramNameAt(fl *ast.FieldList, i int) string {
	pos := 0
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			if pos == i {
				return ""
			}
			pos++
			continue
		}
		for _, n := range f.Names {
			if pos == i {
				return n.Name
			}
			pos++
		}
	}
	return ""
}

// fieldsResolved returns the fields of the receiver type that the
// closure of methods reachable from seeds actually hands to the ctx —
// as an argument of a call that also receives the ctx, or as the
// receiver of one.
//
// It exists because reading a field is not resolving it. The round-6
// check asked only whether the resolve closure mentioned the field, and
// a mention is cheap: a field the resolver merely LOOKS at — a length
// test, a validation branch, a nil check — passed the check while its
// contents went to the renderer unwalked. That is the same fail-open
// one layer up, and this is what closes it: the field has to reach a
// call that has the ctx in its hands, because a call with no ctx cannot
// resolve anything against one.
//
// "Reaches" is a taint walk rather than a syntactic match, because the
// resolvers do not hand fields to calls directly. SelectBuilder.resolveCtx
// builds a slice of {src, dst} structs from seven fields and passes
// l.src; resolveJoins ranges s.joins and calls j.table.resolveContextFilters(ctx);
// InsertBuilder.resolveCtx takes cols and rows through two locals and a
// helper before stampTenantColumn(ctx, ...) sees them. So a local
// carries the taint of everything assigned into it, a range variable
// the taint of what is ranged over, and the result of a call to another
// method on the same receiver the taint of every field that method
// reads.
//
// That last rule is deliberately generous: it credits a field with
// flowing out of a helper that merely read it. The alternative — no
// cross-method taint — fails honest resolvers, and this check is here
// to catch a field nobody walks at all, not to trace which byte came
// out of which helper.
func (p *pgSyntax) fieldsResolved(recv string, seeds ...string) map[string]bool {
	st := p.structs[recv]
	if st == nil {
		return nil
	}
	fields := map[string]bool{}
	for _, f := range st.Fields.List {
		for _, n := range f.Names {
			fields[n.Name] = true
		}
	}

	resolved, seen := map[string]bool{}, map[string]bool{}
	var queue []walkSite
	for _, s := range seeds {
		queue = append(queue, walkSite{fn: p.methods[recv][s]})
	}
	for len(queue) > 0 {
		site := queue[0]
		queue = queue[1:]
		fn := site.fn
		if fn == nil || fn.Type.Params == nil {
			continue
		}
		self := site.self
		if self == "" {
			if fn.Recv == nil || len(fn.Recv.List[0].Names) == 0 {
				continue
			}
			self = fn.Recv.List[0].Names[0].Name
		}
		if seen[funcKey(fn)+"/"+self] {
			continue
		}
		seen[funcKey(fn)+"/"+self] = true
		for _, called := range p.methodsCalledOn(fn, self, recv) {
			queue = append(queue, walkSite{fn: p.methods[recv][called]})
		}
		// The receiver handed whole to a package-level helper is walked
		// there, under the name it arrives as: a resolver that delegates
		// to a free function resolves exactly as much as one that
		// delegates to a method of its own.
		ast.Inspect(fn, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				queue = append(queue, p.helperSites(call, self)...)
			}
			return true
		})

		ctxName := ""
		for _, param := range fn.Type.Params.List {
			if isContextType(param.Type) && len(param.Names) == 1 {
				ctxName = param.Names[0].Name
			}
		}
		if ctxName == "" {
			continue
		}

		// taint maps a local name to the receiver fields whose contents
		// can have reached it. Three passes, because a loop body can
		// read a local the loop itself assigns and ast.Inspect walks the
		// source once, in order.
		taint := map[string]map[string]bool{}
		for pass := 0; pass < 3; pass++ {
			ast.Inspect(fn, func(n ast.Node) bool {
				switch v := n.(type) {
				case *ast.AssignStmt:
					var from map[string]bool
					for _, r := range v.Rhs {
						from = unionFields(from, p.taintOf(r, self, recv, fields, taint))
					}
					for _, l := range v.Lhs {
						if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
							taint[id.Name] = unionFields(taint[id.Name], from)
						}
					}
				case *ast.RangeStmt:
					from := p.taintOf(v.X, self, recv, fields, taint)
					for _, l := range []ast.Expr{v.Key, v.Value} {
						if id, ok := l.(*ast.Ident); ok && id.Name != "_" {
							taint[id.Name] = unionFields(taint[id.Name], from)
						}
					}
				case *ast.CallExpr:
					if !mentionsIdent(v, ctxName) {
						return true
					}
					var reached map[string]bool
					if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
						reached = unionFields(reached, p.taintOf(sel.X, self, recv, fields, taint))
					}
					for _, a := range v.Args {
						if id, ok := a.(*ast.Ident); ok && id.Name == ctxName {
							continue
						}
						reached = unionFields(reached, p.taintOf(a, self, recv, fields, taint))
					}
					for f := range reached {
						resolved[f] = true
					}
				}
				return true
			})
		}
	}
	return resolved
}

// taintOf returns the receiver fields whose contents can have reached
// the value e denotes.
func (p *pgSyntax) taintOf(e ast.Expr, self, recv string, fields map[string]bool, taint map[string]map[string]bool) map[string]bool {
	var out map[string]bool
	ast.Inspect(e, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if base, ok := v.X.(*ast.Ident); ok && base.Name == self && fields[v.Sel.Name] {
				out = unionFields(out, map[string]bool{v.Sel.Name: true})
			}
		case *ast.Ident:
			out = unionFields(out, taint[v.Name])
		case *ast.CallExpr:
			sel, ok := v.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			base, ok := sel.X.(*ast.Ident)
			if !ok || base.Name != self || p.methods[recv][sel.Sel.Name] == nil {
				return true
			}
			out = unionFields(out, p.fieldsRead(recv, sel.Sel.Name))
		}
		return true
	})
	return out
}

// methodsCalledOn lists the methods of recv that fn calls on its own
// receiver, so the walk follows a resolver that delegates.
func (p *pgSyntax) methodsCalledOn(fn *ast.FuncDecl, self, recv string) []string {
	var out []string
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := sel.X.(*ast.Ident)
		if !ok || base.Name != self || p.methods[recv][sel.Sel.Name] == nil {
			return true
		}
		out = append(out, sel.Sel.Name)
		return true
	})
	return out
}

func unionFields(a, b map[string]bool) map[string]bool {
	if len(b) == 0 {
		return a
	}
	if a == nil {
		a = map[string]bool{}
	}
	for k := range b {
		a[k] = true
	}
	return a
}

// mentionsIdent reports whether n contains a bare reference to name —
// which for the ctx parameter is the question "does this call have the
// ctx in its hands?".
func mentionsIdent(n ast.Node, name string) bool {
	found := false
	ast.Inspect(n, func(x ast.Node) bool {
		if id, ok := x.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}

// statementBuilder is one type this invariant is about: the methods it
// renders through, and the methods the resolver enters it by.
type statementBuilder struct {
	name    string
	render  []string
	resolve []string
}

// statementBuilders derives the types the invariant covers, and the
// derivation is the point rather than a tidy-up.
//
// It used to be four names in a slice literal — inside the check whose
// entire purpose is ending lists of names. A fifth builder would have
// been checked on the day someone remembered to add a line, which is
// the same promise resolveExpr's type switch made before round 7 broke
// it, and the same promise resolveCTEs made before round 6 broke it.
//
// So the list is computed from the one place that already decides what
// a statement is: resolveExpr's own type switch. Every arm of it that
// hands back a resolved expression is an interface; a package type
// implementing one of those arms is a type the resolver can enter, and
// the arm's expression-answering methods are HOW it enters — which
// makes them the resolve seeds. The render seeds are every method that
// takes a *drops.Builder, because that is what rendering is.
//
// Nothing here names SelectBuilder, WriteSQL or resolveCtx. Add a fifth
// builder and it is covered before it has a test of its own; that
// claim is not left to trust, and pg/scopecheckself_test.go adds one to
// a synthetic package and requires this check to catch its unwalked
// field.
func statementBuilders(t *testing.T, p *pgSyntax) []statementBuilder {
	t.Helper()
	arms := resolverArms(t, p)
	var out []statementBuilder
	for _, name := range sortedKeys(p.structs) {
		var resolve []string
		for _, arm := range sortedKeys(arms) {
			if hasAllMethods(p.methods[name], arms[arm]) {
				resolve = append(resolve, p.expressionAnsweringMethods(arm)...)
			}
		}
		if len(resolve) == 0 {
			continue
		}
		var render []string
		for m, fn := range p.methods[name] {
			if takesBuilder(fn.Type) {
				render = append(render, m)
			}
		}
		if len(render) == 0 {
			continue
		}
		sort.Strings(render)
		sort.Strings(resolve)
		out = append(out, statementBuilder{name: name, render: render, resolve: resolve})
	}
	return out
}

// expressionAnsweringMethods returns the methods of an interface — its
// own and those of the local interfaces it embeds — that hand back a
// drops.Expression. Those are the methods through which the resolver
// gets a resolved value back, as opposed to the ones through which it
// merely asks a question.
func (p *pgSyntax) expressionAnsweringMethods(name string) []string {
	it := p.ifaces[name]
	if it == nil || it.Methods == nil {
		return nil
	}
	var out []string
	for _, m := range it.Methods.List {
		if len(m.Names) == 0 {
			if e := typeName(m.Type); e != "" && p.ifaces[e] != nil {
				out = append(out, p.expressionAnsweringMethods(e)...)
			}
			continue
		}
		ft, ok := m.Type.(*ast.FuncType)
		if !ok || ft.Results == nil {
			continue
		}
		for _, r := range ft.Results.List {
			if isExpressionType(elementType(r.Type)) {
				for _, n := range m.Names {
					out = append(out, n.Name)
				}
				break
			}
		}
	}
	return out
}

// resolverOwnedFields names the expression-bearing builder fields the
// renderer reads and the resolver deliberately does not read back, with
// the reason. Every entry is a field the resolver WRITES: its contents
// are the resolver's own output, already walked at the point they were
// produced, and reading them again would resolve a resolved statement
// twice — which binds the tenant twice, as SelectBuilder.resolved
// documents.
//
// This is the list a leak would hide in, so an entry has to name a
// field whose contents the resolver produced, not merely a field
// somebody would rather not walk.
var resolverOwnedFields = map[string]string{
	"SelectBuilder.ctxFrom":  "written by resolveCtx from Table.resolveContextFilters, which walks the predicates it returns",
	"SelectBuilder.ctxJoins": "written by resolveJoins from Table.resolveContextFilters, which walks the predicates it returns",
	"SelectBuilder.defaults": "written by resolveCtx from Table.resolveDefaultFilterExprs, which walks the filters it returns",
	"UpdateBuilder.defaults": "written by resolveCtx from Table.resolveDefaultFilterExprs, which walks the filters it returns",
	"DeleteBuilder.defaults": "written by resolveCtx from Table.resolveDefaultFilterExprs, which walks the filters it returns",
}

// TestNoRenderedExpressionListIsInvisibleToTheResolver is the check
// that would have caught DISTINCT ON on the day it was written.
//
// For each statement builder it takes every struct field that can hold
// a caller's expression, asks whether the render closure touches it,
// and requires the resolve closure to read it off the builder. A field
// that is rendered and not read is a statement the executor will send
// having never looked inside it — which is precisely how a scoped
// subquery in a DISTINCT ON list went out with another tenant's rows
// in it.
//
// Reading it is necessary and it is not sufficient, which is the second
// assertion in the loop below and the round-7 addition. The check as it
// first shipped asked only whether the resolve closure MENTIONED the
// field, and a mention is cheap: a field the resolver merely looks at —
// a length test, a validation branch, a nil check — passed while its
// contents went to the renderer unwalked. So the field must also flow
// into a call that holds the ctx (fieldsResolved), because a call
// without a ctx cannot resolve anything against one.
func TestNoRenderedExpressionListIsInvisibleToTheResolver(t *testing.T) {
	for _, problem := range invisibleExpressionListProblems(t, loadPgSyntax(t)) {
		t.Error(problem)
	}
}

// invisibleExpressionListProblems is the check body, returning what it
// found rather than reporting it, so the same code can be pointed at a
// synthetic package that contains the defect — see
// pg/scopecheckself_test.go. A check that has never been seen to fail
// is a check nobody has tested.
func invisibleExpressionListProblems(t *testing.T, p *pgSyntax) []string {
	t.Helper()
	var problems []string
	rendered := map[string]bool{}

	builders := statementBuilders(t, p)
	if len(builders) == 0 {
		t.Fatalf("no type in this package implements an arm of resolveExpr and renders — the derivation has gone stale")
	}
	// The walks have to be doing something. Asked of each builder these
	// would fail for an honest type that holds no expressions at all,
	// so they are asked of the derived set as a whole: if no builder
	// anywhere reads a field, or none hands one to the ctx, the closure
	// walk or the taint walk has gone stale and every verdict below is
	// vacuous.
	anyRead, anyResolved := false, false
	for _, b := range builders {
		st := p.structs[b.name]
		renders := p.fieldsRead(b.name, b.render...)
		resolves := p.fieldsRead(b.name, b.resolve...)
		resolved := p.fieldsResolved(b.name, b.resolve...)
		if len(renders) == 0 {
			problems = append(problems, fmt.Sprintf("%s: %v reads no field at all — a renderer that reads nothing renders nothing", b.name, b.render))
			continue
		}
		anyRead = anyRead || len(resolves) > 0
		anyResolved = anyResolved || len(resolved) > 0
		for _, f := range st.Fields.List {
			if !p.bearsExpression(f.Type) {
				continue
			}
			for _, n := range f.Names {
				key := b.name + "." + n.Name
				if !renders[n.Name] {
					continue
				}
				rendered[key] = true
				if resolves[n.Name] && !resolved[n.Name] && resolverOwnedFields[key] == "" {
					problems = append(problems, fmt.Sprintf("%s: %s: %v reads it but never hands it to the ctx — a field the resolver only LOOKS at is a field the renderer alone walks",
						p.fset.Position(n.Pos()), key, b.resolve))
				}
				if resolves[n.Name] || resolverOwnedFields[key] != "" {
					continue
				}
				problems = append(problems, fmt.Sprintf("%s: %s: %v renders it and %v never reads it — a statement written there is invisible to the resolver",
					p.fset.Position(n.Pos()), key, b.render, b.resolve))
			}
		}
	}

	if !anyRead {
		t.Fatalf("no builder's resolve closure reads a field at all — the closure walk has gone stale")
	}
	if !anyResolved {
		t.Fatalf("no builder's resolve closure hands a field to the ctx at all — the taint walk has gone stale")
	}

	// The other direction: an exemption naming a field that is no
	// longer rendered, no longer holds expressions, or no longer
	// exists, is a reason that has stopped being true.
	for key := range resolverOwnedFields {
		if !rendered[key] {
			problems = append(problems, fmt.Sprintf("exempt field %q is no longer a rendered expression-bearing field — drop the exemption", key))
		}
	}
	sort.Strings(problems)
	return problems
}

// ----------------------------------------------------------------------
// The METHOD census: how a caller reaches those fields
// ----------------------------------------------------------------------

// builderMethodCase is one exported builder method, called with a
// scoped statement in the operand position a caller can reach.
//
// name is the method's Go name, optionally followed by a space and a
// note naming which operand position the case covers; the census below
// reads the first word, exactly as operandConstructors does.
type builderMethodCase struct {
	name  string
	build func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer
}

// builderFixture is the unscoped scenery the method cases build on. The
// only scoped thing anywhere in them is the statement handed to the
// method, so a tenant predicate in the rendered SQL can only have come
// through the operand under test.
type builderFixture struct {
	db     *pg.DB
	plain  *pg.Table
	other  *pg.Table
	id     *pg.Col[int64]
	n      *pg.Col[int64]
	otherI *pg.Col[int64]
}

func newBuilderFixture() *builderFixture {
	f := &builderFixture{db: pg.New(nil), plain: pg.NewTable("bm_plain"), other: pg.NewTable("bm_other")}
	f.id = pg.Add(f.plain, pg.BigSerial("id").PrimaryKey())
	f.n = pg.Add(f.plain, pg.BigInt("n"))
	f.otherI = pg.Add(f.other, pg.BigSerial("id").PrimaryKey())
	return f
}

// from is the unscoped SELECT the SELECT cases hang their operand off.
func (f *builderFixture) from() *pg.SelectBuilder {
	return f.db.Select(f.id).From(f.plain).Unscoped()
}

// builderOperandMethods is the table the method census enforces: one
// entry per exported method of a statement builder that takes an
// operand a caller can hand a statement to.
//
// It is the missing half of round 5. That census enumerated
// package-level constructors, so it could see pg.Coalesce and could not
// see (*SelectBuilder).DistinctOn — and DistinctOn is where the leak
// was. A method that stores a caller's expression into a builder field
// is the same kind of entry point as a constructor that stores it in a
// node, and it reaches the same renderer.
func builderOperandMethods() []builderMethodCase {
	return []builderMethodCase{
		// select.go — the clause list methods.
		{"DistinctOn", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().DistinctOn(pg.Subquery(sub()))
		}},
		{"FromExpr", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().FromExpr(pg.Subquery(sub()))
		}},
		{"Where", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().Where(pg.Exists(sub()))
		}},
		{"GroupBy", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().GroupBy(pg.Subquery(sub()))
		}},
		{"Having", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().Having(pg.Exists(sub()))
		}},
		{"OrderBy", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().OrderBy(pg.Subquery(sub()))
		}},

		// select.go — the join ON clauses, one per kind, because the
		// placement rules differ per kind and the ON clause is walked
		// by a resolution of its own.
		{"Join", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().Join(f.other, pg.Exists(sub()))
		}},
		{"LeftJoin", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().LeftJoin(f.other, pg.Exists(sub()))
		}},
		{"RightJoin", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().RightJoin(f.other, pg.Exists(sub()))
		}},
		{"FullJoin", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().FullJoin(f.other, pg.Exists(sub()))
		}},

		// select.go — the set operations, whose operand is a whole
		// statement rather than an expression. The round-5 census could
		// not see these either: it looks for an `any` or a
		// drops.Expression, and a *SelectBuilder is neither.
		{"Union", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().Union(sub())
		}},
		{"UnionAll", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().UnionAll(sub())
		}},
		{"Intersect", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().Intersect(sub())
		}},
		{"IntersectAll", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().IntersectAll(sub())
		}},
		{"Except", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().Except(sub())
		}},
		{"ExceptAll", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.from().ExceptAll(sub())
		}},

		// update.go.
		{"Set update", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Update(f.plain).Set(f.n.Expr(pg.Subquery(sub())))
		}},
		{"Where update", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Update(f.plain).Set(f.n.Val(1)).Where(pg.Exists(sub()))
		}},
		{"Returning update", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Update(f.plain).Set(f.n.Val(1)).Returning(pg.Subquery(sub()))
		}},

		// delete.go.
		{"Where delete", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Delete(f.plain).Where(pg.Exists(sub()))
		}},
		{"Returning delete", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Delete(f.plain).Where(pg.Eq(f.id, 1)).Returning(pg.Subquery(sub()))
		}},

		// insert.go — the row values, the batch, the RETURNING list and
		// both halves of the ON CONFLICT update.
		{"Row", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Insert(f.plain).Row(f.n.Expr(pg.Subquery(sub())))
		}},
		{"Rows", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Insert(f.plain).Rows([]pg.ColumnValue{f.n.Expr(pg.Subquery(sub()))})
		}},
		{"Returning insert", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Insert(f.plain).Row(f.n.Val(1)).Returning(pg.Subquery(sub()))
		}},
		{"Set conflict", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Insert(f.plain).Row(f.n.Val(1)).
				OnConflictUpdate(f.id).Set(f.n.Expr(pg.Subquery(sub()))).Done()
		}},
		{"Where conflict", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Insert(f.plain).Row(f.n.Val(1)).
				OnConflictUpdate(f.id).Set(f.n.Val(2)).Where(pg.Exists(sub())).Done()
		}},
	}
}

// exemptBuilderMethods names the exported builder methods that take an
// operand and deliberately do not resolve it, with the reason. It is
// empty, and that is the point: every operand a caller can put into one
// of these four statements is walked. An entry added here has to say
// why resolving would be wrong rather than merely inconvenient — see
// exemptConstructors for the shape of a reason that qualifies.
var exemptBuilderMethods = map[string]string{}

// TestEveryBuilderOperandMethodIsCensused reads the package source and
// requires every exported method of a statement builder that takes an
// operand to appear in builderOperandMethods or in
// exemptBuilderMethods.
//
// Executors are excluded by taking a context.Context first — the house
// rule that ctx comes first is what makes that mechanical rather than a
// list of names. All(ctx, dest) does take an `any`, but dest is where
// rows are scanned to, not an operand rendered into a statement.
func TestEveryBuilderOperandMethodIsCensused(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range builderOperandMethods() {
		covered[strings.Fields(tc.name)[0]] = true
	}

	census := censusBuilderOperandMethods(t)
	var missing []string
	for _, name := range census {
		if covered[name] || exemptBuilderMethods[name] != "" {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("builder methods take an operand but are not exercised with a scoped subquery: %v", missing)
	}

	present := map[string]bool{}
	for _, name := range census {
		present[name] = true
	}
	for name := range exemptBuilderMethods {
		if !present[name] {
			t.Errorf("exempt builder method %q no longer takes an operand — drop the exemption", name)
		}
	}
}

// operandReceivers derives the types whose exported methods put a
// caller's operand into a statement: the statement builders, and the
// handles they hand out that write into the statement on their behalf —
// ConflictUpdate is one, and it holds the *InsertBuilder it is
// configuring, which is what "on its behalf" looks like from the
// source.
//
// It was five names in a map, which is the smell this round went
// looking for: a list a check knows and could have computed. Nothing
// here names a builder, so a fifth one, and the handle IT hands out,
// are censused on the day they are written.
func operandReceivers(t *testing.T, p *pgSyntax) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, b := range statementBuilders(t, p) {
		out[b.name] = true
	}
	for {
		changed := false
		for recv := range out {
			for _, fn := range p.methods[recv] {
				if fn.Type.Results == nil {
					continue
				}
				for _, r := range fn.Type.Results.List {
					tn := typeName(r.Type)
					if tn == "" || out[tn] || p.structs[tn] == nil || !p.bearing[tn] {
						continue
					}
					// A handle that writes into the statement holds the
					// builder it is writing into. A method that merely
					// answers with a *Table or a *Column hands back
					// scenery, not a way into this statement.
					for _, f := range p.structs[tn].Fields.List {
						if out[typeName(f.Type)] {
							out[tn], changed = true, true
							break
						}
					}
				}
			}
		}
		if !changed {
			return out
		}
	}
}

// censusBuilderOperandMethods returns the exported methods of those
// receivers that take at least one operand a caller could hand a
// statement to.
func censusBuilderOperandMethods(t *testing.T) []string {
	t.Helper()
	p := loadPgSyntax(t)
	receivers := operandReceivers(t, p)
	seen := map[string]bool{}
	var out []string
	for recv, ms := range p.methods {
		if !receivers[recv] {
			continue
		}
		for name, fn := range ms {
			if !ast.IsExported(name) || fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
				continue
			}
			if isContextType(fn.Type.Params.List[0].Type) {
				continue
			}
			for _, param := range fn.Type.Params.List {
				if !p.isWideOperandType(param.Type) {
					continue
				}
				if !seen[name] {
					seen[name] = true
					out = append(out, name)
				}
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// isContextType reports whether e is context.Context — the marker that
// separates an executor from a builder method, since ctx comes first
// everywhere in this package.
func isContextType(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "context" && sel.Sel.Name == "Context"
}

// isWideOperandType is isOperandType widened by the two shapes the
// round-5 census could not see.
//
// A *SelectBuilder parameter is an operand: Union(other *SelectBuilder)
// takes a whole statement, and a statement is the thing that has to be
// resolved. A ColumnValue is an operand too — (*Col[T]).Expr wraps an
// arbitrary expression in one, which is how a subquery reaches an
// INSERT row or an UPDATE SET list, the two places an unresolved read
// decides what gets written rather than what gets returned.
func (p *pgSyntax) isWideOperandType(e ast.Expr) bool {
	if p.isOperandType(e) {
		return true
	}
	// A pointer to a statement builder: Union(other *SelectBuilder)
	// takes a whole statement, and a statement is the thing that has to
	// be resolved. Derived from the arms resolveExpr dispatches on
	// rather than named, so the fifth builder is a wide operand on the
	// day it is written — *SelectBuilder was the name here, and a
	// second builder growing the same method was invisible.
	return p.statementBuilding[typeName(e)]
}

// TestBuilderOperandMethodsCarryTheTenantAxis runs the whole method
// table with a scoped statement in the operand position and asserts the
// inner statement renders its own context filters.
//
// Every table in the fixture is unscoped, so the tenant predicate in
// the SQL and the tenant in the args can only have come from the
// statement handed to the method under test.
func TestBuilderOperandMethodsCarryTheTenantAxis(t *testing.T) {
	posts := reachTable("bm_posts")
	wantInner := `SELECT "bm_posts"."id" FROM "bm_posts" ` +
		`WHERE ("bm_posts"."deletedAt" IS NULL) AND ("bm_posts"."tenantId" = $?)`

	for _, tc := range builderOperandMethods() {
		t.Run(tc.name, func(t *testing.T) {
			f := newBuilderFixture()
			sub := func() *pg.SelectBuilder { return f.db.Select(posts.Col("id")).From(posts) }

			got, args, err := tc.build(f, sub).ToSQLCtx(fnCtx())
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

// TestBuilderOperandMethodsFailClosedWithNoTenant is the other half:
// with no tenant on ctx the statement must be refused rather than sent
// with the guard silently missing from the operand.
func TestBuilderOperandMethodsFailClosedWithNoTenant(t *testing.T) {
	posts := reachTable("bmf_posts")

	for _, tc := range builderOperandMethods() {
		t.Run(tc.name, func(t *testing.T) {
			f := newBuilderFixture()
			sub := func() *pg.SelectBuilder { return f.db.Select(posts.Col("id")).From(posts) }

			sql, _, err := tc.build(f, sub).ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want no statement at all", sql)
			}
		})
	}
}

// ----------------------------------------------------------------------
// The round-5 function census, widened
// ----------------------------------------------------------------------

// statementConstructors covers the exported package-level constructors
// that take an operand and return something the renderer writes into a
// statement without that thing being a drops.Expression itself.
//
// CTEDef is the one today: it returns a *CTE, so the round-5 census —
// which looks for a single drops.Expression result — never saw it,
// even though the body it holds is exactly the kind of statement this
// work is about. The behaviour is pinned in scopereach_test.go; the
// entry here is what keeps the census from having a blind spot the
// shape of a return type.
func statementConstructors() []builderMethodCase {
	return []builderMethodCase{
		{"CTEDef", func(f *builderFixture, sub func() *pg.SelectBuilder) ctxRenderer {
			return f.db.Select(f.id).From(f.plain).Unscoped().
				With(pg.CTEDef("recent", sub()))
		}},
	}
}

// TestEveryWidenedOperandConstructorIsCensused re-runs the round-5
// census under the wider reading of both halves of a signature.
//
// The narrow rule missed three shapes, and a leak only has to fit one
// of them to be invisible: a constructor returning
// (drops.Expression, error) rather than a bare expression; a
// constructor taking a *SelectBuilder or a ColumnValue rather than an
// `any`; and a constructor whose result is rendered into a statement
// without being an expression, which is how CTEDef sat outside the
// census while holding a whole SELECT.
//
// A name is covered when it appears in operandConstructors,
// statementConstructors or builderOperandMethods — the three tables
// that exercise an operand with a scoped subquery — or in
// exemptConstructors with a reason.
func TestEveryWidenedOperandConstructorIsCensused(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range operandConstructors(drops.Raw(`"t"."c"`)) {
		covered[strings.Fields(tc.name)[0]] = true
	}
	for _, tc := range statementConstructors() {
		covered[strings.Fields(tc.name)[0]] = true
	}
	for _, tc := range builderOperandMethods() {
		covered[strings.Fields(tc.name)[0]] = true
	}

	census := censusWidenedOperandConstructors(t)
	var missing []string
	for _, name := range census {
		if covered[name] || exemptConstructors[name] != "" || exemptWidenedConstructors[name] != "" {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("constructors take an operand but are not exercised with a scoped subquery: %v", missing)
	}

	// And the reverse, so an exemption cannot outlive the constructor
	// it excuses. The narrow census already checks this for its own
	// reading; the widened one is a superset, so an entry that survives
	// there survives here too — the check is repeated rather than
	// assumed, because the two readings could drift apart.
	present := map[string]bool{}
	for _, name := range census {
		present[name] = true
	}
	for name := range exemptConstructors {
		if !present[name] {
			t.Errorf("exempt constructor %q no longer takes an operand — drop the exemption", name)
		}
	}
	for name, reason := range exemptWidenedConstructors {
		if reason == "" {
			t.Errorf("exempt constructor %q carries no reason — an exemption without one is a leak with a name", name)
		}
		if !present[name] {
			t.Errorf("exempt constructor %q no longer takes an operand — drop the exemption", name)
		}
	}
}

// censusWidenedOperandConstructors returns the exported package-level
// functions that take an operand and return something rendered into a
// statement.
func censusWidenedOperandConstructors(t *testing.T) []string {
	t.Helper()
	p := loadPgSyntax(t)
	rendered := renderedResultTypes(t, p)
	var out []string
	for _, fn := range p.funcs {
		if !fn.Name.IsExported() || fn.Type.Results == nil {
			continue
		}
		if !p.takesWideOperand(fn.Type.Params) || !returnsRendered(rendered, fn.Type.Results) {
			continue
		}
		out = append(out, fn.Name.Name)
	}
	sort.Strings(out)
	return out
}

func (p *pgSyntax) takesWideOperand(fl *ast.FieldList) bool {
	if fl == nil {
		return false
	}
	for _, f := range fl.List {
		if p.isWideOperandType(f.Type) {
			return true
		}
	}
	return false
}

// renderedResultTypes derives the package's own types that a statement
// renders but that are not drops.Expression: a constructor returning
// one of these is holding a caller's expression on the way into a
// statement, exactly as an expression constructor is.
//
// Seven names in a map is how this started, and each of the three ways
// a type qualifies was one of the names somebody remembered:
//
//   - it renders itself — it has a render site, its own method or a
//     package-level helper handed a *drops.Builder;
//   - a renderer reads it out of a field, which is how CTE and
//     ColumnValue arrive;
//   - it is a handle a builder hands out to write into the statement,
//     which is ConflictUpdate.
//
// Computed, it is a superset of the list it replaces, so the census
// around it got wider rather than narrower.
func renderedResultTypes(t *testing.T, p *pgSyntax) map[string]bool {
	out := operandReceivers(t, p)
	for name, st := range p.structs {
		sites := p.renderSites(name)
		if len(sites) == 0 {
			continue
		}
		out[name] = true
		fields := map[string]bool{}
		fieldType := map[string]string{}
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				fields[n.Name] = true
				fieldType[n.Name] = typeName(f.Type)
			}
		}
		for f := range p.fieldsReadAt(name, fields, sites) {
			tn := fieldType[f]
			if tn == "" {
				continue
			}
			if p.structs[tn] != nil || p.ifaces[tn] != nil || p.aliases[tn] != nil {
				out[tn] = true
			}
		}
	}
	return out
}

// returnsRendered reports whether any result is rendered into a
// statement. Any result rather than the only result: a constructor that
// answers (drops.Expression, error) is as much a constructor as one
// that answers a bare expression, and the round-5 rule — exactly one
// result, and that result an expression — would have let the first one
// past without a word.
func returnsRendered(rendered map[string]bool, fl *ast.FieldList) bool {
	if fl == nil {
		return false
	}
	for _, f := range fl.List {
		if isExpressionType(f.Type) || rendered[typeName(f.Type)] {
			return true
		}
	}
	return false
}

// TestStatementConstructorsCarryTheTenantAxis exercises the widened
// census's own table, on both the resolved path and the refusing one.
func TestStatementConstructorsCarryTheTenantAxis(t *testing.T) {
	posts := reachTable("sc_posts")
	wantInner := `SELECT "sc_posts"."id" FROM "sc_posts" ` +
		`WHERE ("sc_posts"."deletedAt" IS NULL) AND ("sc_posts"."tenantId" = $?)`

	for _, tc := range statementConstructors() {
		t.Run(tc.name, func(t *testing.T) {
			f := newBuilderFixture()
			sub := func() *pg.SelectBuilder { return f.db.Select(posts.Col("id")).From(posts) }

			got, args, err := tc.build(f, sub).ToSQLCtx(fnCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if !strings.Contains(normalisePlaceholders(got), wantInner) {
				t.Errorf("got = %v, want it to contain %v", got, wantInner)
			}
			if !containsArg(args, fnTenant) {
				t.Errorf("args = %v, want them to bind the tenant %v", args, fnTenant)
			}

			f = newBuilderFixture()
			sub = func() *pg.SelectBuilder { return f.db.Select(posts.Col("id")).From(posts) }
			sql, _, err := tc.build(f, sub).ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Fatalf("bare ctx: got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("bare ctx: got sql = %q, want no statement at all", sql)
			}
		})
	}
}
