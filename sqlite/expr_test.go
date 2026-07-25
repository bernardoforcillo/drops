package sqlite_test

import (
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// wantSQL renders e with the SQLite dialect and checks the text and the
// bound-argument count.
func wantSQL(t *testing.T, e drops.Expression, sql string, nargs int) {
	t.Helper()
	got, args := sqlite.ToSQL(e)
	if got != sql {
		t.Errorf("SQL mismatch:\n  got:  %s\n  want: %s", got, sql)
	}
	if len(args) != nargs {
		t.Errorf("arg count: got %d want %d (%v)", len(args), nargs, args)
	}
}

func col(name string) *sqlite.Column {
	tbl := sqlite.NewTable("t")
	return sqlite.Add(tbl, sqlite.Integer(name)).Column
}

// --- operators & predicates -------------------------------------------

func TestExprOperators(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.Eq(c, 1), `("t"."a" = ?)`, 1)
	wantSQL(t, sqlite.Ne(c, 1), `("t"."a" <> ?)`, 1)
	wantSQL(t, sqlite.Gt(c, 1), `("t"."a" > ?)`, 1)
	wantSQL(t, sqlite.Gte(c, 1), `("t"."a" >= ?)`, 1)
	wantSQL(t, sqlite.Lt(c, 1), `("t"."a" < ?)`, 1)
	wantSQL(t, sqlite.Lte(c, 1), `("t"."a" <= ?)`, 1)
	wantSQL(t, sqlite.IsNotDistinctFrom(c, 1), `("t"."a" IS ?)`, 1)
	wantSQL(t, sqlite.IsDistinctFrom(c, 1), `("t"."a" IS NOT ?)`, 1)
}

func TestExprPatternMatch(t *testing.T) {
	c := col("name")
	wantSQL(t, sqlite.Like(c, "a%"), `("t"."name" LIKE ?)`, 1)
	wantSQL(t, sqlite.NotLike(c, "a%"), `("t"."name" NOT LIKE ?)`, 1)
	wantSQL(t, sqlite.LikeEscape(c, "a!%", "!"), `("t"."name" LIKE ? ESCAPE ?)`, 2)
	wantSQL(t, sqlite.Glob(c, "a*"), `("t"."name" GLOB ?)`, 1)
}

func TestExprLogical(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.Not(sqlite.Eq(c, 1)), `(NOT ("t"."a" = ?))`, 1)
	wantSQL(t, sqlite.And(sqlite.Gt(c, 1), sqlite.Lt(c, 9)),
		`(("t"."a" > ?) AND ("t"."a" < ?))`, 2)
	wantSQL(t, sqlite.Or(sqlite.Eq(c, 1), sqlite.Eq(c, 2)),
		`(("t"."a" = ?) OR ("t"."a" = ?))`, 2)
}

func TestExprMembership(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.In(c, 1, 2, 3), `("t"."a" IN (?, ?, ?))`, 3)
	// single slice arg is expanded
	wantSQL(t, sqlite.In(c, []int{1, 2}), `("t"."a" IN (?, ?))`, 2)
	wantSQL(t, sqlite.NotIn(c, 4, 5), `("t"."a" NOT IN (?, ?))`, 2)
	// empty lists collapse to static booleans
	wantSQL(t, sqlite.In(c), `(0)`, 0)
	wantSQL(t, sqlite.NotIn(c), `(1)`, 0)
}

func TestExprNullAndRange(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.IsNull(c), `("t"."a" IS NULL)`, 0)
	wantSQL(t, sqlite.IsNotNull(c), `("t"."a" IS NOT NULL)`, 0)
	wantSQL(t, sqlite.Between(c, 1, 10), `("t"."a" BETWEEN ? AND ?)`, 2)
	wantSQL(t, sqlite.NotBetween(c, 1, 10), `("t"."a" NOT BETWEEN ? AND ?)`, 2)
}

// --- aggregates & scalar functions ------------------------------------

func TestExprAggregates(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.Count(c), `count("t"."a")`, 0)
	wantSQL(t, sqlite.CountAll(), `count(*)`, 0)
	wantSQL(t, sqlite.CountDistinct(c), `count(DISTINCT "t"."a")`, 0)
	wantSQL(t, sqlite.Sum(c), `sum("t"."a")`, 0)
	wantSQL(t, sqlite.Total(c), `total("t"."a")`, 0)
	wantSQL(t, sqlite.GroupConcat(c, ","), `group_concat("t"."a", ?)`, 1)
	wantSQL(t, sqlite.Filter(sqlite.CountAll(), sqlite.Gt(c, 0)),
		`count(*) FILTER (WHERE ("t"."a" > ?))`, 1)
}

func TestExprScalars(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.Coalesce(c, 0), `coalesce("t"."a", ?)`, 1)
	wantSQL(t, sqlite.IfNull(c, 0), `ifnull("t"."a", ?)`, 1)
	wantSQL(t, sqlite.NullIf(c, 0), `nullif("t"."a", ?)`, 1)
	wantSQL(t, sqlite.As(sqlite.Sum(c), "total"), `sum("t"."a") AS "total"`, 0)
	wantSQL(t, sqlite.Func("some_fn", c, 1), `some_fn("t"."a", ?)`, 1)
}

// --- math -------------------------------------------------------------

func TestExprMath(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.Abs(c), `abs("t"."a")`, 0)
	wantSQL(t, sqlite.Round(c), `round("t"."a")`, 0)
	wantSQL(t, sqlite.Round(c, 2), `round("t"."a", ?)`, 1)
	wantSQL(t, sqlite.Mod(c, 3), `("t"."a" % ?)`, 1)
	wantSQL(t, sqlite.Power(c, 2), `pow("t"."a", ?)`, 1)
	wantSQL(t, sqlite.Greatest(c, 1, 2), `max("t"."a", ?, ?)`, 2)
	wantSQL(t, sqlite.Least(c, 1), `min("t"."a", ?)`, 1)
	wantSQL(t, sqlite.Plus(c, 1), `("t"."a" + ?)`, 1)
	wantSQL(t, sqlite.Div(c, 2), `("t"."a" / ?)`, 1)
	wantSQL(t, sqlite.Log(c), `log("t"."a")`, 0)
	wantSQL(t, sqlite.Log(c, 2), `log(?, "t"."a")`, 1)
}

// --- strings ----------------------------------------------------------

func TestExprStrings(t *testing.T) {
	c := col("name")
	wantSQL(t, sqlite.ConcatOp(c, "!"), `("t"."name" || ?)`, 1)
	wantSQL(t, sqlite.Length(c), `length("t"."name")`, 0)
	wantSQL(t, sqlite.Substr(c, 1, 3), `substr("t"."name", ?, ?)`, 2)
	wantSQL(t, sqlite.Substr(c, 2), `substr("t"."name", ?)`, 1)
	wantSQL(t, sqlite.Trim(c), `trim("t"."name")`, 0)
	wantSQL(t, sqlite.Trim(c, "x"), `trim("t"."name", ?)`, 1)
	wantSQL(t, sqlite.Replace(c, "a", "b"), `replace("t"."name", ?, ?)`, 2)
	wantSQL(t, sqlite.Instr(c, "z"), `instr("t"."name", ?)`, 1)
	wantSQL(t, sqlite.Format("%d", c), `format(?, "t"."name")`, 1)
}

// --- cast & case ------------------------------------------------------

func TestExprCast(t *testing.T) {
	c := col("a")
	wantSQL(t, sqlite.CastAs(c, "TEXT"), `CAST("t"."a" AS TEXT)`, 0)
	wantSQL(t, sqlite.Cast(c, "REAL"), `CAST("t"."a" AS REAL)`, 0)
}

func TestExprCase(t *testing.T) {
	c := col("a")
	searched := sqlite.Case().
		When(sqlite.Lt(c, 18), "minor").
		When(sqlite.Lt(c, 65), "adult").
		Else("senior").
		End()
	wantSQL(t, searched,
		`CASE WHEN ("t"."a" < ?) THEN ? WHEN ("t"."a" < ?) THEN ? ELSE ? END`, 5)

	simple := sqlite.CaseOn(c).When(1, "one").Else("other").End()
	wantSQL(t, simple, `CASE "t"."a" WHEN ? THEN ? ELSE ? END`, 3)
}

// --- subquery & exists ------------------------------------------------

func TestExprSubquery(t *testing.T) {
	users, _ := mkSchema()
	db := sqlite.New(&fakeDriver{})
	q := db.Select(users.Col("id")).From(users)
	wantSQL(t, sqlite.Exists(q),
		`EXISTS (SELECT "users"."id" FROM "users")`, 0)
	wantSQL(t, sqlite.NotExists(q),
		`NOT EXISTS (SELECT "users"."id" FROM "users")`, 0)
	wantSQL(t, sqlite.InSub(users.Col("id"), q),
		`("users"."id" IN (SELECT "users"."id" FROM "users"))`, 0)
}

// --- window functions -------------------------------------------------

func TestExprWindow(t *testing.T) {
	c := col("a")
	w := sqlite.WindowSpec().PartitionBy(c).OrderBy(c).Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
	wantSQL(t, sqlite.Over(sqlite.RowNumber(), w),
		`row_number() OVER (PARTITION BY "t"."a" ORDER BY "t"."a" ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)`, 0)
	wantSQL(t, sqlite.Over(sqlite.Sum(c), sqlite.WindowSpec().PartitionBy(c)),
		`sum("t"."a") OVER (PARTITION BY "t"."a")`, 0)
	wantSQL(t, sqlite.Lag(c, 1, 0), `lag("t"."a", ?, ?)`, 2)
}

// --- json -------------------------------------------------------------

func TestExprJSON(t *testing.T) {
	c := col("doc")
	wantSQL(t, sqlite.JSONExtract(c, "$.name"), `json_extract("t"."doc", ?)`, 1)
	wantSQL(t, sqlite.JSONGet(c, "$.name"), `("t"."doc" -> ?)`, 1)
	wantSQL(t, sqlite.JSONGetText(c, "$.name"), `("t"."doc" ->> ?)`, 1)
	wantSQL(t, sqlite.JSONArrayLength(c), `json_array_length("t"."doc")`, 0)
	wantSQL(t, sqlite.JSONArrayLength(c, "$.items"), `json_array_length("t"."doc", ?)`, 1)
	wantSQL(t, sqlite.JSONSet(c, "$.n", 1), `json_set("t"."doc", ?, ?)`, 2)
	wantSQL(t, sqlite.JSONGroupArray(c), `json_group_array("t"."doc")`, 0)
}

// --- datetime ---------------------------------------------------------

func TestExprDateTime(t *testing.T) {
	c := col("ts")
	wantSQL(t, sqlite.Now(), `CURRENT_TIMESTAMP`, 0)
	wantSQL(t, sqlite.CurrentDate(), `CURRENT_DATE`, 0)
	wantSQL(t, sqlite.DateOf(c, "+1 day"), `date("t"."ts", ?)`, 1)
	wantSQL(t, sqlite.StrfTime("%Y-%m", c), `strftime(?, "t"."ts")`, 1)
	wantSQL(t, sqlite.JulianDay(c), `julianday("t"."ts")`, 0)
}

// --- CTE integration on the SELECT builder ----------------------------

func TestExprCTE(t *testing.T) {
	users, _ := mkSchema()
	db := sqlite.New(&fakeDriver{})
	base := db.Select(users.Col("id")).From(users)
	cte := sqlite.CTEDef("recent", base, "id")
	q := db.Select().From(users).With(cte)
	sql, _ := q.ToSQL()
	want := `WITH "recent" ("id") AS (SELECT "users"."id" FROM "users") SELECT * FROM "users"`
	if sql != want {
		t.Errorf("CTE SQL:\n  got:  %s\n  want: %s", sql, want)
	}
}
