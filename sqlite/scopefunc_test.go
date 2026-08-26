package sqlite_test

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
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
//	SELECT coalesce((SELECT "posts"."id" FROM "posts"), ?) FROM "plain"
//	SELECT count(*) FILTER (WHERE EXISTS (SELECT … FROM "posts")) FROM "plain"
//	SELECT row_number() OVER (PARTITION BY (SELECT … FROM "posts")) FROM "plain"
//	SELECT CAST((SELECT … FROM "posts") AS INTEGER) FROM "plain"
//	SELECT CASE WHEN EXISTS (SELECT … FROM "posts") THEN ? END FROM "plain"
//
// sqlite.Coalesce(sqlite.Subquery(sel), 0) in a projection is an
// ordinary scalar-subquery idiom, not an exotic shape.
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

func fnCtx() context.Context { return sqlite.WithTenant(context.Background(), fnTenant) }

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
// exported function in package sqlite that returns a drops.Expression
// and takes an operand a caller can hand a statement to.
//
// It is a table of CONSTRUCTORS rather than of rendered statements
// because the invariant is about the constructor: whatever it builds
// must keep the operand reachable. What each one renders is pinned
// separately, in TestExpressionsRenderUnchanged.
func operandConstructors(col drops.Expression) []operandCase {
	return []operandCase{
		// op.go — comparisons, connectives, membership, null tests.
		{"Eq", func(s drops.Expression) drops.Expression { return sqlite.Eq(col, s) }},
		{"Ne", func(s drops.Expression) drops.Expression { return sqlite.Ne(col, s) }},
		{"Gt", func(s drops.Expression) drops.Expression { return sqlite.Gt(col, s) }},
		{"Gte", func(s drops.Expression) drops.Expression { return sqlite.Gte(col, s) }},
		{"Lt", func(s drops.Expression) drops.Expression { return sqlite.Lt(col, s) }},
		{"Lte", func(s drops.Expression) drops.Expression { return sqlite.Lte(col, s) }},
		{"IsDistinctFrom", func(s drops.Expression) drops.Expression { return sqlite.IsDistinctFrom(col, s) }},
		{"IsNotDistinctFrom", func(s drops.Expression) drops.Expression {
			return sqlite.IsNotDistinctFrom(col, s)
		}},
		{"Like", func(s drops.Expression) drops.Expression { return sqlite.Like(col, s) }},
		{"NotLike", func(s drops.Expression) drops.Expression { return sqlite.NotLike(col, s) }},
		{"LikeEscape", func(s drops.Expression) drops.Expression { return sqlite.LikeEscape(col, s, "!") }},
		{"Glob", func(s drops.Expression) drops.Expression { return sqlite.Glob(col, s) }},
		{"Regexp", func(s drops.Expression) drops.Expression { return sqlite.Regexp(col, s) }},
		{"And", func(s drops.Expression) drops.Expression {
			return sqlite.And(sqlite.Exists(s), drops.Raw("1"))
		}},
		{"Or", func(s drops.Expression) drops.Expression {
			return sqlite.Or(sqlite.Exists(s), drops.Raw("0"))
		}},
		{"Not", func(s drops.Expression) drops.Expression { return sqlite.Not(sqlite.Exists(s)) }},
		{"In", func(s drops.Expression) drops.Expression { return sqlite.In(col, s) }},
		{"NotIn", func(s drops.Expression) drops.Expression { return sqlite.NotIn(col, s) }},
		{"IsNull", func(s drops.Expression) drops.Expression { return sqlite.IsNull(sqlite.Subquery(s)) }},
		{"IsNotNull", func(s drops.Expression) drops.Expression {
			return sqlite.IsNotNull(sqlite.Subquery(s))
		}},
		{"Between", func(s drops.Expression) drops.Expression {
			return sqlite.Between(col, sqlite.Subquery(s), 10)
		}},
		{"NotBetween", func(s drops.Expression) drops.Expression {
			return sqlite.NotBetween(col, sqlite.Subquery(s), 10)
		}},

		// subquery.go — the helpers that name a subquery outright.
		{"Exists", sqlite.Exists},
		{"NotExists", sqlite.NotExists},
		{"Subquery", sqlite.Subquery},
		{"InSub", func(s drops.Expression) drops.Expression { return sqlite.InSub(col, s) }},
		{"NotInSub", func(s drops.Expression) drops.Expression { return sqlite.NotInSub(col, s) }},

		// funcs.go
		{"Func", func(s drops.Expression) drops.Expression { return sqlite.Func("f", sqlite.Subquery(s)) }},
		{"Count", func(s drops.Expression) drops.Expression { return sqlite.Count(sqlite.Subquery(s)) }},
		{"CountDistinct", func(s drops.Expression) drops.Expression {
			return sqlite.CountDistinct(sqlite.Subquery(s))
		}},
		{"Sum", func(s drops.Expression) drops.Expression { return sqlite.Sum(sqlite.Subquery(s)) }},
		{"Avg", func(s drops.Expression) drops.Expression { return sqlite.Avg(sqlite.Subquery(s)) }},
		{"Min", func(s drops.Expression) drops.Expression { return sqlite.Min(sqlite.Subquery(s)) }},
		{"Max", func(s drops.Expression) drops.Expression { return sqlite.Max(sqlite.Subquery(s)) }},
		{"SumDistinct", func(s drops.Expression) drops.Expression {
			return sqlite.SumDistinct(sqlite.Subquery(s))
		}},
		{"AvgDistinct", func(s drops.Expression) drops.Expression {
			return sqlite.AvgDistinct(sqlite.Subquery(s))
		}},
		{"Total", func(s drops.Expression) drops.Expression { return sqlite.Total(sqlite.Subquery(s)) }},
		{"GroupConcat", func(s drops.Expression) drops.Expression {
			return sqlite.GroupConcat(sqlite.Subquery(s))
		}},
		{"Filter", func(s drops.Expression) drops.Expression {
			return sqlite.Filter(sqlite.CountAll(), sqlite.Exists(s))
		}},
		{"Lower", func(s drops.Expression) drops.Expression { return sqlite.Lower(sqlite.Subquery(s)) }},
		{"Upper", func(s drops.Expression) drops.Expression { return sqlite.Upper(sqlite.Subquery(s)) }},
		{"Coalesce", func(s drops.Expression) drops.Expression {
			return sqlite.Coalesce(sqlite.Subquery(s), 0)
		}},
		{"IfNull", func(s drops.Expression) drops.Expression {
			return sqlite.IfNull(sqlite.Subquery(s), 0)
		}},
		{"NullIf", func(s drops.Expression) drops.Expression {
			return sqlite.NullIf(sqlite.Subquery(s), 0)
		}},
		{"As", func(s drops.Expression) drops.Expression { return sqlite.As(sqlite.Subquery(s), "x") }},

		// cast.go
		{"CastAs", func(s drops.Expression) drops.Expression {
			return sqlite.CastAs(sqlite.Subquery(s), "INTEGER")
		}},
		{"Cast", func(s drops.Expression) drops.Expression {
			return sqlite.Cast(sqlite.Subquery(s), "INTEGER")
		}},
		{"Case when", func(s drops.Expression) drops.Expression {
			return sqlite.Case().When(sqlite.Exists(s), 1).End()
		}},
		{"CaseOn value", func(s drops.Expression) drops.Expression {
			return sqlite.CaseOn(sqlite.Subquery(s)).When(1, 2).End()
		}},

		// strings.go
		{"ConcatOp", func(s drops.Expression) drops.Expression { return sqlite.ConcatOp(col, s) }},
		{"Concat", func(s drops.Expression) drops.Expression { return sqlite.Concat(col, sqlite.Subquery(s)) }},
		{"ConcatWS", func(s drops.Expression) drops.Expression {
			return sqlite.ConcatWS("-", sqlite.Subquery(s))
		}},
		{"Length", func(s drops.Expression) drops.Expression { return sqlite.Length(sqlite.Subquery(s)) }},
		{"OctetLength", func(s drops.Expression) drops.Expression {
			return sqlite.OctetLength(sqlite.Subquery(s))
		}},
		{"Substr", func(s drops.Expression) drops.Expression { return sqlite.Substr(sqlite.Subquery(s), 1) }},
		{"Trim", func(s drops.Expression) drops.Expression { return sqlite.Trim(sqlite.Subquery(s)) }},
		{"LTrim", func(s drops.Expression) drops.Expression { return sqlite.LTrim(sqlite.Subquery(s)) }},
		{"RTrim", func(s drops.Expression) drops.Expression { return sqlite.RTrim(sqlite.Subquery(s)) }},
		{"Replace", func(s drops.Expression) drops.Expression {
			return sqlite.Replace(sqlite.Subquery(s), "a", "b")
		}},
		{"Instr", func(s drops.Expression) drops.Expression { return sqlite.Instr(sqlite.Subquery(s), "a") }},
		{"Hex", func(s drops.Expression) drops.Expression { return sqlite.Hex(sqlite.Subquery(s)) }},
		{"Unhex", func(s drops.Expression) drops.Expression { return sqlite.Unhex(sqlite.Subquery(s)) }},
		{"Quote", func(s drops.Expression) drops.Expression { return sqlite.Quote(sqlite.Subquery(s)) }},
		{"Chr", func(s drops.Expression) drops.Expression { return sqlite.Chr(sqlite.Subquery(s)) }},
		{"Unicode", func(s drops.Expression) drops.Expression { return sqlite.Unicode(sqlite.Subquery(s)) }},
		{"Format", func(s drops.Expression) drops.Expression {
			return sqlite.Format("%s", sqlite.Subquery(s))
		}},
		{"Printf", func(s drops.Expression) drops.Expression {
			return sqlite.Printf("%s", sqlite.Subquery(s))
		}},

		// math.go
		{"Abs", func(s drops.Expression) drops.Expression { return sqlite.Abs(sqlite.Subquery(s)) }},
		{"Ceil", func(s drops.Expression) drops.Expression { return sqlite.Ceil(sqlite.Subquery(s)) }},
		{"Floor", func(s drops.Expression) drops.Expression { return sqlite.Floor(sqlite.Subquery(s)) }},
		{"Trunc", func(s drops.Expression) drops.Expression { return sqlite.Trunc(sqlite.Subquery(s)) }},
		{"Round", func(s drops.Expression) drops.Expression { return sqlite.Round(sqlite.Subquery(s)) }},
		{"Mod", func(s drops.Expression) drops.Expression { return sqlite.Mod(col, s) }},
		{"Power", func(s drops.Expression) drops.Expression {
			return sqlite.Power(sqlite.Subquery(s), 2)
		}},
		{"Sqrt", func(s drops.Expression) drops.Expression { return sqlite.Sqrt(sqlite.Subquery(s)) }},
		{"Sign", func(s drops.Expression) drops.Expression { return sqlite.Sign(sqlite.Subquery(s)) }},
		{"Exp", func(s drops.Expression) drops.Expression { return sqlite.Exp(sqlite.Subquery(s)) }},
		{"Ln", func(s drops.Expression) drops.Expression { return sqlite.Ln(sqlite.Subquery(s)) }},
		{"Log", func(s drops.Expression) drops.Expression { return sqlite.Log(sqlite.Subquery(s)) }},
		{"Greatest", func(s drops.Expression) drops.Expression {
			return sqlite.Greatest(sqlite.Subquery(s), 1)
		}},
		{"Least", func(s drops.Expression) drops.Expression { return sqlite.Least(sqlite.Subquery(s), 1) }},
		{"Sin", func(s drops.Expression) drops.Expression { return sqlite.Sin(sqlite.Subquery(s)) }},
		{"Cos", func(s drops.Expression) drops.Expression { return sqlite.Cos(sqlite.Subquery(s)) }},
		{"Tan", func(s drops.Expression) drops.Expression { return sqlite.Tan(sqlite.Subquery(s)) }},
		{"Asin", func(s drops.Expression) drops.Expression { return sqlite.Asin(sqlite.Subquery(s)) }},
		{"Acos", func(s drops.Expression) drops.Expression { return sqlite.Acos(sqlite.Subquery(s)) }},
		{"Atan", func(s drops.Expression) drops.Expression { return sqlite.Atan(sqlite.Subquery(s)) }},
		{"Plus", func(s drops.Expression) drops.Expression { return sqlite.Plus(col, s) }},
		{"Minus", func(s drops.Expression) drops.Expression { return sqlite.Minus(col, s) }},
		{"Mul", func(s drops.Expression) drops.Expression { return sqlite.Mul(col, s) }},
		{"Div", func(s drops.Expression) drops.Expression { return sqlite.Div(col, s) }},

		// json.go
		{"JSONExtract", func(s drops.Expression) drops.Expression {
			return sqlite.JSONExtract(sqlite.Subquery(s), "$.a")
		}},
		{"JSONGet", func(s drops.Expression) drops.Expression { return sqlite.JSONGet(col, s) }},
		{"JSONGetText", func(s drops.Expression) drops.Expression { return sqlite.JSONGetText(col, s) }},
		{"JSONArrayLength", func(s drops.Expression) drops.Expression {
			return sqlite.JSONArrayLength(sqlite.Subquery(s))
		}},
		{"JSONType", func(s drops.Expression) drops.Expression {
			return sqlite.JSONType(sqlite.Subquery(s))
		}},
		{"JSONValid", func(s drops.Expression) drops.Expression {
			return sqlite.JSONValid(sqlite.Subquery(s))
		}},
		{"JSONQuote", func(s drops.Expression) drops.Expression {
			return sqlite.JSONQuote(sqlite.Subquery(s))
		}},
		{"JSONObject", func(s drops.Expression) drops.Expression {
			return sqlite.JSONObject("a", sqlite.Subquery(s))
		}},
		{"JSONArray", func(s drops.Expression) drops.Expression {
			return sqlite.JSONArray(sqlite.Subquery(s))
		}},
		{"JSONSet", func(s drops.Expression) drops.Expression {
			return sqlite.JSONSet(col, "$.a", sqlite.Subquery(s))
		}},
		{"JSONInsert", func(s drops.Expression) drops.Expression {
			return sqlite.JSONInsert(col, "$.a", sqlite.Subquery(s))
		}},
		{"JSONReplace", func(s drops.Expression) drops.Expression {
			return sqlite.JSONReplace(col, "$.a", sqlite.Subquery(s))
		}},
		{"JSONRemove", func(s drops.Expression) drops.Expression {
			return sqlite.JSONRemove(col, sqlite.Subquery(s))
		}},
		{"JSONPatch", func(s drops.Expression) drops.Expression {
			return sqlite.JSONPatch(col, sqlite.Subquery(s))
		}},
		{"JSONGroupArray", func(s drops.Expression) drops.Expression {
			return sqlite.JSONGroupArray(sqlite.Subquery(s))
		}},
		{"JSONGroupObject", func(s drops.Expression) drops.Expression {
			return sqlite.JSONGroupObject("a", sqlite.Subquery(s))
		}},

		// datetime.go
		{"DateOf", func(s drops.Expression) drops.Expression { return sqlite.DateOf(sqlite.Subquery(s)) }},
		{"TimeOf", func(s drops.Expression) drops.Expression { return sqlite.TimeOf(sqlite.Subquery(s)) }},
		{"DateTime", func(s drops.Expression) drops.Expression {
			return sqlite.DateTime(sqlite.Subquery(s))
		}},
		{"JulianDay", func(s drops.Expression) drops.Expression {
			return sqlite.JulianDay(sqlite.Subquery(s))
		}},
		{"UnixEpoch", func(s drops.Expression) drops.Expression {
			return sqlite.UnixEpoch(sqlite.Subquery(s))
		}},
		{"StrfTime", func(s drops.Expression) drops.Expression {
			return sqlite.StrfTime("%Y", sqlite.Subquery(s))
		}},

		// window.go
		{"Over partition key", func(s drops.Expression) drops.Expression {
			return sqlite.Over(sqlite.RowNumber(), sqlite.WindowSpec().PartitionBy(sqlite.Subquery(s)))
		}},
		{"Over order key", func(s drops.Expression) drops.Expression {
			return sqlite.Over(sqlite.RowNumber(), sqlite.WindowSpec().OrderBy(sqlite.Subquery(s)))
		}},
		{"Over function", func(s drops.Expression) drops.Expression {
			return sqlite.Over(sqlite.Count(sqlite.Subquery(s)), sqlite.WindowSpec())
		}},
		{"Ntile", func(s drops.Expression) drops.Expression { return sqlite.Ntile(sqlite.Subquery(s)) }},
		{"Lag", func(s drops.Expression) drops.Expression { return sqlite.Lag(sqlite.Subquery(s)) }},
		{"Lead", func(s drops.Expression) drops.Expression { return sqlite.Lead(sqlite.Subquery(s)) }},
		{"FirstValue", func(s drops.Expression) drops.Expression {
			return sqlite.FirstValue(sqlite.Subquery(s))
		}},
		{"LastValue", func(s drops.Expression) drops.Expression {
			return sqlite.LastValue(sqlite.Subquery(s))
		}},
		{"NthValue", func(s drops.Expression) drops.Expression {
			return sqlite.NthValue(sqlite.Subquery(s), 1)
		}},
	}
}

// Every operand-taking constructor carries the tenant axis into the
// statement written in its operand.
func TestEveryOperandTakingConstructorCarriesTheAxis(t *testing.T) {
	posts, postID, _ := scopedTable("fn_posts")
	plain, plainID := plainTable("fn_plain")
	db := sqlite.New(nil)

	body := `SELECT "fn_posts"."id" FROM "fn_posts" WHERE ("fn_posts"."tenantId" = ?)`
	for _, tc := range operandConstructors(plainID) {
		t.Run(tc.name, func(t *testing.T) {
			sub := db.Select(postID).From(posts)
			sel := db.Select(tc.build(sub)).From(plain)
			sql, _ := renderCtx(t, sel, fnCtx())
			if !strings.Contains(sql, body) {
				t.Errorf("the inner statement was rendered unscoped:\n%s", sql)
			}
		})
	}
}

// And every one of them REFUSES on a ctx with no tenant, rather than
// rendering the inner statement without the predicate.
func TestEveryOperandTakingConstructorRefusesWithoutATenant(t *testing.T) {
	posts, postID, _ := scopedTable("fr_posts")
	plain, plainID := plainTable("fr_plain")
	db := sqlite.New(nil)

	for _, tc := range operandConstructors(plainID) {
		t.Run(tc.name, func(t *testing.T) {
			sub := db.Select(postID).From(posts)
			sql, _, err := db.Select(tc.build(sub)).From(plain).ToSQLCtx(context.Background())
			if !errors.Is(err, sqlite.ErrTenantMissing) {
				t.Fatalf("err = %v (sql %q), want %v", err, sql, sqlite.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("sql = %q, want no statement at all", sql)
			}
		})
	}
}

// The census: every exported function in package sqlite that returns a
// drops.Expression and takes an operand a caller could hand a statement
// to must appear in operandConstructors.
//
// It reads the package's own source rather than a list somebody
// maintains, because the list is what went stale after every previous
// round of this work in the dialect that pioneered it. A helper added
// tomorrow fails this test on the day it is written.
func TestEveryExportedOperandConstructorIsCensused(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range operandConstructors(drops.Raw("x")) {
		covered[strings.Fields(tc.name)[0]] = true
	}

	var missing []string
	for _, fn := range exportedExpressionConstructors(t) {
		if !covered[fn] {
			missing = append(missing, fn)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("not exercised with a statement in an operand: %s\n"+
			"add each to operandConstructors, or explain in exemptExpressionConstructors why it takes no operand",
			strings.Join(missing, ", "))
	}
}

// exemptExpressionConstructors names the exported constructors that
// return an Expression and cannot hold a statement, with the reason
// each is exempt. Being on this list is a claim, and the claim is
// checked: every entry must exist in the package.
var exemptExpressionConstructors = map[string]string{
	// No operand at all — fixed SQL text.
	"CountAll":         "renders count(*)",
	"Now":              "renders CURRENT_TIMESTAMP",
	"CurrentDate":      "renders CURRENT_DATE",
	"CurrentTime":      "renders CURRENT_TIME",
	"CurrentTimestamp": "renders CURRENT_TIMESTAMP",
	"Random":           "renders random()",
	"RowNumber":        "renders row_number()",
	"Rank":             "renders rank()",
	"DenseRank":        "renders dense_rank()",
	"PercentRank":      "renders percent_rank()",
	"CumeDist":         "renders cume_dist()",

	// The operand is a NAME, not a value: an identifier or a path
	// segment list, which a statement cannot be.
	"JSONHasKey": "takes a column handle and JSON path segments",

	// DDL and schema rendering. Their operands are tables, columns and
	// type names; a statement cannot be written into one, and their
	// output never carries a WHERE clause for a predicate to reach.
	"CreateTable":            "DDL over a table declaration",
	"CreateTableIfNotExists": "DDL over a table declaration",
	"DropTable":              "DDL over a table declaration",
	"DropTableIfExists":      "DDL over a table declaration",
	"CreateIndex":            "DDL over identifiers",
	"CreateIndexIfNotExists": "DDL over identifiers",
	"CreateUniqueIndex":      "DDL over identifiers",
	"DropIndex":              "DDL over identifiers",
	"DropIndexIfExists":      "DDL over identifiers",
}

// exportedExpressionConstructors returns the name of every exported
// function in package sqlite whose result type is drops.Expression,
// minus the exemptions — checking as it goes that every exemption names
// a function that exists, so a rename cannot silently retire one.
func exportedExpressionConstructors(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	pkg, ok := pkgs["sqlite"]
	if !ok {
		t.Fatalf("package sqlite not found in the working directory")
	}

	found := map[string]bool{}
	var out []string
	for _, f := range pkg.Files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if !returnsExpression(fn) {
				continue
			}
			found[fn.Name.Name] = true
			if _, exempt := exemptExpressionConstructors[fn.Name.Name]; exempt {
				continue
			}
			out = append(out, fn.Name.Name)
		}
	}
	for name := range exemptExpressionConstructors {
		if !found[name] {
			t.Errorf("exemptExpressionConstructors names %q, which is not an exported "+
				"Expression constructor in this package any more", name)
		}
	}
	return out
}

// returnsExpression reports whether fn's sole result is a
// drops.Expression.
func returnsExpression(fn *ast.FuncDecl) bool {
	res := fn.Type.Results
	if res == nil || len(res.List) != 1 || len(res.List[0].Names) > 0 {
		return false
	}
	sel, ok := res.List[0].Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "drops" && sel.Sel.Name == "Expression"
}

// TestExpressionsRenderUnchanged is the byte-identity corpus.
//
// Holding the arguments instead of closing over them must not change
// one byte of any expression that has no statement inside it — which is
// nearly every expression this package has ever rendered. The list
// covers every helper family that moved, including the shapes with an
// optional clause (Round's digits, Substr's count, Log's base, an empty
// CASE, an empty window spec) where a rewrite is most likely to drop or
// double a separator.
func TestExpressionsRenderUnchanged(t *testing.T) {
	col := drops.Raw(`"t"."c"`)

	tests := []struct {
		name string
		expr drops.Expression
		want string
		args []any
	}{
		// op.go
		{"Eq", sqlite.Eq(col, 1), `("t"."c" = ?)`, []any{1}},
		{"Ne", sqlite.Ne(col, 1), `("t"."c" <> ?)`, []any{1}},
		{"Gt", sqlite.Gt(col, 1), `("t"."c" > ?)`, []any{1}},
		{"Gte", sqlite.Gte(col, 1), `("t"."c" >= ?)`, []any{1}},
		{"Lt", sqlite.Lt(col, 1), `("t"."c" < ?)`, []any{1}},
		{"Lte", sqlite.Lte(col, 1), `("t"."c" <= ?)`, []any{1}},
		{"IsDistinctFrom", sqlite.IsDistinctFrom(col, 1), `("t"."c" IS NOT ?)`, []any{1}},
		{"IsNotDistinctFrom", sqlite.IsNotDistinctFrom(col, 1), `("t"."c" IS ?)`, []any{1}},
		{"Like", sqlite.Like(col, "a%"), `("t"."c" LIKE ?)`, []any{"a%"}},
		{"NotLike", sqlite.NotLike(col, "a%"), `("t"."c" NOT LIKE ?)`, []any{"a%"}},
		{"LikeEscape", sqlite.LikeEscape(col, "a%", "!"),
			`("t"."c" LIKE ? ESCAPE ?)`, []any{"a%", "!"}},
		{"Glob", sqlite.Glob(col, "a*"), `("t"."c" GLOB ?)`, []any{"a*"}},
		{"Regexp", sqlite.Regexp(col, "a"), `("t"."c" REGEXP ?)`, []any{"a"}},
		{"Not", sqlite.Not(drops.Raw("x")), `(NOT x)`, nil},
		{"In", sqlite.In(col, 1, 2), `("t"."c" IN (?, ?))`, []any{1, 2}},
		{"In of a slice", sqlite.In(col, []int{1, 2}), `("t"."c" IN (?, ?))`, []any{1, 2}},
		{"In of nothing", sqlite.In(col), `(0)`, nil},
		{"NotIn", sqlite.NotIn(col, 1), `("t"."c" NOT IN (?))`, []any{1}},
		{"NotIn of nothing", sqlite.NotIn(col), `(1)`, nil},
		{"IsNull", sqlite.IsNull(col), `("t"."c" IS NULL)`, nil},
		{"IsNotNull", sqlite.IsNotNull(col), `("t"."c" IS NOT NULL)`, nil},
		{"Between", sqlite.Between(col, 1, 2), `("t"."c" BETWEEN ? AND ?)`, []any{1, 2}},
		{"NotBetween", sqlite.NotBetween(col, 1, 2), `("t"."c" NOT BETWEEN ? AND ?)`, []any{1, 2}},

		// column.go — the connectives and the typed forms.
		{"And", sqlite.And(drops.Raw("a"), drops.Raw("b")), `(a AND b)`, nil},
		{"And of one", sqlite.And(drops.Raw("a")), `(a)`, nil},
		{"And of nothing", sqlite.And(), `()`, nil},
		{"Or", sqlite.Or(drops.Raw("a"), drops.Raw("b")), `(a OR b)`, nil},
		{"Or of one", sqlite.Or(drops.Raw("a")), `(a)`, nil},
		{"Or of nothing", sqlite.Or(), `()`, nil},

		// subquery.go
		{"Exists", sqlite.Exists(drops.Raw("SELECT 1")), `EXISTS (SELECT 1)`, nil},
		{"NotExists", sqlite.NotExists(drops.Raw("SELECT 1")), `NOT EXISTS (SELECT 1)`, nil},
		{"Subquery", sqlite.Subquery(drops.Raw("SELECT 1")), `(SELECT 1)`, nil},
		{"InSub", sqlite.InSub(col, drops.Raw("SELECT 1")), `("t"."c" IN (SELECT 1))`, nil},
		{"NotInSub", sqlite.NotInSub(col, drops.Raw("SELECT 1")),
			`("t"."c" NOT IN (SELECT 1))`, nil},

		// funcs.go
		{"Func", sqlite.Func("f", col, 1), `f("t"."c", ?)`, []any{1}},
		{"Func of nothing", sqlite.Func("f"), `f()`, nil},
		{"Count", sqlite.Count(col), `count("t"."c")`, nil},
		{"CountAll", sqlite.CountAll(), `count(*)`, nil},
		{"CountDistinct", sqlite.CountDistinct(col), `count(DISTINCT "t"."c")`, nil},
		{"Sum", sqlite.Sum(col), `sum("t"."c")`, nil},
		{"Avg", sqlite.Avg(col), `avg("t"."c")`, nil},
		{"Min", sqlite.Min(col), `min("t"."c")`, nil},
		{"Max", sqlite.Max(col), `max("t"."c")`, nil},
		{"SumDistinct", sqlite.SumDistinct(col), `sum(DISTINCT "t"."c")`, nil},
		{"AvgDistinct", sqlite.AvgDistinct(col), `avg(DISTINCT "t"."c")`, nil},
		{"Total", sqlite.Total(col), `total("t"."c")`, nil},
		{"GroupConcat", sqlite.GroupConcat(col), `group_concat("t"."c")`, nil},
		{"GroupConcat with a separator", sqlite.GroupConcat(col, ","),
			`group_concat("t"."c", ?)`, []any{","}},
		{"Filter", sqlite.Filter(sqlite.CountAll(), sqlite.Eq(col, 1)),
			`count(*) FILTER (WHERE ("t"."c" = ?))`, []any{1}},
		{"Lower", sqlite.Lower(col), `lower("t"."c")`, nil},
		{"Upper", sqlite.Upper(col), `upper("t"."c")`, nil},
		{"Coalesce", sqlite.Coalesce(col, 0), `coalesce("t"."c", ?)`, []any{0}},
		{"Coalesce of nothing", sqlite.Coalesce(), `coalesce()`, nil},
		{"IfNull", sqlite.IfNull(col, 0), `ifnull("t"."c", ?)`, []any{0}},
		{"NullIf", sqlite.NullIf(col, 0), `nullif("t"."c", ?)`, []any{0}},
		{"As", sqlite.As(col, "x"), `"t"."c" AS "x"`, nil},

		// cast.go
		{"CastAs", sqlite.CastAs(col, "INTEGER"), `CAST("t"."c" AS INTEGER)`, nil},
		{"Cast", sqlite.Cast(col, "TEXT"), `CAST("t"."c" AS TEXT)`, nil},
		{"Case", sqlite.Case().When(sqlite.Eq(col, 1), "a").Else("b").End(),
			`CASE WHEN ("t"."c" = ?) THEN ? ELSE ? END`, []any{1, "a", "b"}},
		{"Case with no branches", sqlite.Case().End(), `CASE END`, nil},
		{"CaseOn", sqlite.CaseOn(col).When("a", 1).End(),
			`CASE "t"."c" WHEN ? THEN ? END`, []any{"a", 1}},

		// strings.go
		{"ConcatOp", sqlite.ConcatOp(col, "a"), `("t"."c" || ?)`, []any{"a"}},
		{"Concat", sqlite.Concat(col, "a"), `concat("t"."c", ?)`, []any{"a"}},
		{"ConcatWS", sqlite.ConcatWS("-", col), `concat_ws(?, "t"."c")`, []any{"-"}},
		{"Length", sqlite.Length(col), `length("t"."c")`, nil},
		{"OctetLength", sqlite.OctetLength(col), `octet_length("t"."c")`, nil},
		{"Substr", sqlite.Substr(col, 1), `substr("t"."c", ?)`, []any{1}},
		{"Substr with a count", sqlite.Substr(col, 1, 2), `substr("t"."c", ?, ?)`, []any{1, 2}},
		{"Trim", sqlite.Trim(col), `trim("t"."c")`, nil},
		{"Trim with chars", sqlite.Trim(col, "x"), `trim("t"."c", ?)`, []any{"x"}},
		{"LTrim", sqlite.LTrim(col), `ltrim("t"."c")`, nil},
		{"RTrim", sqlite.RTrim(col), `rtrim("t"."c")`, nil},
		{"Replace", sqlite.Replace(col, "a", "b"), `replace("t"."c", ?, ?)`, []any{"a", "b"}},
		{"Instr", sqlite.Instr(col, "a"), `instr("t"."c", ?)`, []any{"a"}},
		{"Hex", sqlite.Hex(col), `hex("t"."c")`, nil},
		{"Unhex", sqlite.Unhex(col), `unhex("t"."c")`, nil},
		{"Quote", sqlite.Quote(col), `quote("t"."c")`, nil},
		{"Chr", sqlite.Chr(65), `char(?)`, []any{65}},
		{"Unicode", sqlite.Unicode(col), `unicode("t"."c")`, nil},
		{"Format", sqlite.Format("%s", col), `format(?, "t"."c")`, []any{"%s"}},
		{"Printf", sqlite.Printf("%s", col), `printf(?, "t"."c")`, []any{"%s"}},

		// math.go
		{"Abs", sqlite.Abs(col), `abs("t"."c")`, nil},
		{"Ceil", sqlite.Ceil(col), `ceil("t"."c")`, nil},
		{"Floor", sqlite.Floor(col), `floor("t"."c")`, nil},
		{"Trunc", sqlite.Trunc(col), `trunc("t"."c")`, nil},
		{"Round", sqlite.Round(col), `round("t"."c")`, nil},
		{"Round with digits", sqlite.Round(col, 2), `round("t"."c", ?)`, []any{2}},
		{"Mod", sqlite.Mod(col, 2), `("t"."c" % ?)`, []any{2}},
		{"Power", sqlite.Power(col, 2), `pow("t"."c", ?)`, []any{2}},
		{"Sqrt", sqlite.Sqrt(col), `sqrt("t"."c")`, nil},
		{"Sign", sqlite.Sign(col), `sign("t"."c")`, nil},
		{"Exp", sqlite.Exp(col), `exp("t"."c")`, nil},
		{"Ln", sqlite.Ln(col), `ln("t"."c")`, nil},
		{"Log", sqlite.Log(col), `log("t"."c")`, nil},
		{"Log with a base", sqlite.Log(col, 2), `log(?, "t"."c")`, []any{2}},
		{"Greatest", sqlite.Greatest(col, 1), `max("t"."c", ?)`, []any{1}},
		{"Least", sqlite.Least(col, 1), `min("t"."c", ?)`, []any{1}},
		{"Random", sqlite.Random(), `random()`, nil},
		{"Sin", sqlite.Sin(col), `sin("t"."c")`, nil},
		{"Cos", sqlite.Cos(col), `cos("t"."c")`, nil},
		{"Tan", sqlite.Tan(col), `tan("t"."c")`, nil},
		{"Asin", sqlite.Asin(col), `asin("t"."c")`, nil},
		{"Acos", sqlite.Acos(col), `acos("t"."c")`, nil},
		{"Atan", sqlite.Atan(col), `atan("t"."c")`, nil},
		{"Plus", sqlite.Plus(col, 1), `("t"."c" + ?)`, []any{1}},
		{"Minus", sqlite.Minus(col, 1), `("t"."c" - ?)`, []any{1}},
		{"Mul", sqlite.Mul(col, 1), `("t"."c" * ?)`, []any{1}},
		{"Div", sqlite.Div(col, 1), `("t"."c" / ?)`, []any{1}},

		// json.go
		{"JSONExtract", sqlite.JSONExtract(col, "$.a"), `json_extract("t"."c", ?)`, []any{"$.a"}},
		{"JSONGet", sqlite.JSONGet(col, "$.a"), `("t"."c" -> ?)`, []any{"$.a"}},
		{"JSONGetText", sqlite.JSONGetText(col, "$.a"), `("t"."c" ->> ?)`, []any{"$.a"}},
		{"JSONArrayLength", sqlite.JSONArrayLength(col), `json_array_length("t"."c")`, nil},
		{"JSONArrayLength with a path", sqlite.JSONArrayLength(col, "$.a"),
			`json_array_length("t"."c", ?)`, []any{"$.a"}},
		{"JSONType", sqlite.JSONType(col), `json_type("t"."c")`, nil},
		{"JSONType with a path", sqlite.JSONType(col, "$.a"), `json_type("t"."c", ?)`, []any{"$.a"}},
		{"JSONValid", sqlite.JSONValid(col), `json_valid("t"."c")`, nil},
		{"JSONQuote", sqlite.JSONQuote(col), `json_quote("t"."c")`, nil},
		{"JSONObject", sqlite.JSONObject("a", 1), `json_object(?, ?)`, []any{"a", 1}},
		{"JSONArray", sqlite.JSONArray(1, 2), `json_array(?, ?)`, []any{1, 2}},
		{"JSONSet", sqlite.JSONSet(col, "$.a", 1), `json_set("t"."c", ?, ?)`, []any{"$.a", 1}},
		{"JSONInsert", sqlite.JSONInsert(col, "$.a", 1), `json_insert("t"."c", ?, ?)`, []any{"$.a", 1}},
		{"JSONReplace", sqlite.JSONReplace(col, "$.a", 1),
			`json_replace("t"."c", ?, ?)`, []any{"$.a", 1}},
		{"JSONRemove", sqlite.JSONRemove(col, "$.a"), `json_remove("t"."c", ?)`, []any{"$.a"}},
		{"JSONPatch", sqlite.JSONPatch(col, col), `json_patch("t"."c", "t"."c")`, nil},
		{"JSONGroupArray", sqlite.JSONGroupArray(col), `json_group_array("t"."c")`, nil},
		{"JSONGroupObject", sqlite.JSONGroupObject("a", col),
			`json_group_object(?, "t"."c")`, []any{"a"}},

		// datetime.go
		{"Now", sqlite.Now(), `CURRENT_TIMESTAMP`, nil},
		{"CurrentDate", sqlite.CurrentDate(), `CURRENT_DATE`, nil},
		{"CurrentTime", sqlite.CurrentTime(), `CURRENT_TIME`, nil},
		{"CurrentTimestamp", sqlite.CurrentTimestamp(), `CURRENT_TIMESTAMP`, nil},
		{"DateOf", sqlite.DateOf(col), `date("t"."c")`, nil},
		{"DateOf with modifiers", sqlite.DateOf(col, "+1 day"),
			`date("t"."c", ?)`, []any{"+1 day"}},
		{"TimeOf", sqlite.TimeOf(col), `time("t"."c")`, nil},
		{"DateTime", sqlite.DateTime(col), `datetime("t"."c")`, nil},
		{"JulianDay", sqlite.JulianDay(col), `julianday("t"."c")`, nil},
		{"UnixEpoch", sqlite.UnixEpoch(col), `unixepoch("t"."c")`, nil},
		{"StrfTime", sqlite.StrfTime("%Y", col), `strftime(?, "t"."c")`, []any{"%Y"}},

		// window.go
		{"RowNumber", sqlite.RowNumber(), `row_number()`, nil},
		{"Rank", sqlite.Rank(), `rank()`, nil},
		{"DenseRank", sqlite.DenseRank(), `dense_rank()`, nil},
		{"PercentRank", sqlite.PercentRank(), `percent_rank()`, nil},
		{"CumeDist", sqlite.CumeDist(), `cume_dist()`, nil},
		{"Over with an empty window", sqlite.Over(sqlite.RowNumber(), sqlite.WindowSpec()),
			`row_number() OVER ()`, nil},
		{"Over with a partition", sqlite.Over(sqlite.RowNumber(),
			sqlite.WindowSpec().PartitionBy(col)),
			`row_number() OVER (PARTITION BY "t"."c")`, nil},
		{"Over with a partition and an order", sqlite.Over(sqlite.RowNumber(),
			sqlite.WindowSpec().PartitionBy(col).OrderBy(col)),
			`row_number() OVER (PARTITION BY "t"."c" ORDER BY "t"."c")`, nil},
		{"Over with a frame", sqlite.Over(sqlite.RowNumber(),
			sqlite.WindowSpec().OrderBy(col).Frame("ROWS BETWEEN 1 PRECEDING AND CURRENT ROW")),
			`row_number() OVER (ORDER BY "t"."c" ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)`, nil},
		{"Ntile", sqlite.Ntile(4), `ntile(?)`, []any{4}},
		{"Lag", sqlite.Lag(col), `lag("t"."c")`, nil},
		{"Lag with an offset", sqlite.Lag(col, 1), `lag("t"."c", ?)`, []any{1}},
		{"Lead", sqlite.Lead(col), `lead("t"."c")`, nil},
		{"FirstValue", sqlite.FirstValue(col), `first_value("t"."c")`, nil},
		{"LastValue", sqlite.LastValue(col), `last_value("t"."c")`, nil},
		{"NthValue", sqlite.NthValue(col, 2), `nth_value("t"."c", ?)`, []any{2}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args := sqlite.ToSQL(tc.expr)
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}
}

// The typed column methods render unchanged too. Their values are
// always BOUND rather than rendered as expressions — a rule that needs
// pinning because T can be instantiated as an interface, and a *Col[any]
// handed an Expression must still bind it rather than splice it in.
func TestTypedColumnFormsRenderUnchanged(t *testing.T) {
	tbl := sqlite.NewTable("tc_t")
	c := sqlite.Add(tbl, sqlite.BigInt("c"))
	other := sqlite.Add(tbl, sqlite.BigInt("d"))
	anyCol := sqlite.Add(tbl, sqlite.Custom[any]("e", "BLOB"))

	tests := []struct {
		name string
		expr drops.Expression
		want string
		args []any
	}{
		{"Eq", c.Eq(1), `("tc_t"."c" = ?)`, []any{int64(1)}},
		{"Ne", c.Ne(1), `("tc_t"."c" <> ?)`, []any{int64(1)}},
		{"Gt", c.Gt(1), `("tc_t"."c" > ?)`, []any{int64(1)}},
		{"Gte", c.Gte(1), `("tc_t"."c" >= ?)`, []any{int64(1)}},
		{"Lt", c.Lt(1), `("tc_t"."c" < ?)`, []any{int64(1)}},
		{"Lte", c.Lte(1), `("tc_t"."c" <= ?)`, []any{int64(1)}},
		{"EqCol", c.EqCol(other), `("tc_t"."c" = "tc_t"."d")`, nil},
		{"IsNull", c.IsNull(), `("tc_t"."c" IS NULL)`, nil},
		{"IsNotNull", c.IsNotNull(), `("tc_t"."c" IS NOT NULL)`, nil},
		{"In", c.In(1, 2), `("tc_t"."c" IN (?, ?))`, []any{int64(1), int64(2)}},
		{"In of nothing", c.In(), `(0)`, nil},
		{"Asc", c.Asc(), `"tc_t"."c" ASC`, nil},
		{"Desc", c.Desc(), `"tc_t"."c" DESC`, nil},
		{"As", c.As("x"), `"tc_t"."c" AS "x"`, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args := sqlite.ToSQL(tc.expr)
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}

	// The interface-typed handle: an Expression given to a typed
	// comparison is a VALUE, bound like any other, not a fragment
	// spliced into the statement.
	sql, args := sqlite.ToSQL(anyCol.Eq(drops.Raw("1 = 1")))
	if sql != `("tc_t"."e" = ?)` || len(args) != 1 {
		t.Errorf("got = %v %v, want the operand bound rather than rendered", sql, args)
	}
}
