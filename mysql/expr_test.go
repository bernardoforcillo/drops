package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

// render is the shorthand every case here uses: the SQL a dialect-aware
// Builder produces, plus the arguments it bound.
func render(e drops.Expression) (string, []any) {
	return drops.StringWithDialect(mysql.Dialect, e)
}

func wantSQL(t *testing.T, e drops.Expression, want string) {
	t.Helper()
	got, _ := render(e)
	if got != want {
		t.Errorf("SQL = %s\nwant  %s", got, want)
	}
}

// These are rendering tests on purpose: what each function *answers* is
// pinned against a live server in integration/mysql_test.go, because a
// string comparison cannot tell LOG from LOG10.

func TestFunctionsRenderMySQLSpellings(t *testing.T) {
	col := mysql.Integer("n")
	cases := []struct {
		name string
		expr drops.Expression
		want string
	}{
		// The three renamings that carry a different meaning from the
		// PostgreSQL function of the same name.
		{"Log is base 10", mysql.Log(col), "log10(`n`)"},
		{"Ln is natural", mysql.Ln(col), "ln(`n`)"},
		{"Length counts characters", mysql.Length(col), "char_length(`n`)"},
		{"OctetLength counts bytes", mysql.OctetLength(col), "length(`n`)"},
		{"StrPos swaps into LOCATE", mysql.StrPos(col, "x"), "locate(?, `n`)"},
		{"Locate keeps MySQL order", mysql.Locate("x", col), "locate(?, `n`)"},
		{"IntDiv is DIV", mysql.IntDiv(col, 2), "(`n` DIV ?)"},
		{"Random is RAND", mysql.Random(), "rand()"},
		{"RegexpLike is the operator", mysql.RegexpLike(col, "^a"), "(`n` REGEXP ?)"},
		{"BoolAnd is MIN", mysql.BoolAnd(col), "min(`n`)"},
		{"BoolOr is MAX", mysql.BoolOr(col), "max(`n`)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wantSQL(t, tc.expr, tc.want) })
	}
}

func TestCountIfHasNoElseBranch(t *testing.T) {
	// An ELSE 0 would make count() count every row, which is the whole
	// bug this shape exists to avoid.
	sql, _ := render(mysql.CountIf(mysql.Gt(mysql.Integer("n"), 1)))
	if strings.Contains(sql, "ELSE") {
		t.Errorf("CountIf must not emit an ELSE branch: %s", sql)
	}
	if sql != "count(CASE WHEN (`n` > ?) THEN 1 END)" {
		t.Errorf("SQL = %s", sql)
	}
}

// The separator has to be a literal: SEPARATOR ? is a syntax error on
// both servers.
func TestGroupConcatSeparatorIsALiteral(t *testing.T) {
	sql, args := render(mysql.GroupConcat(mysql.Text("tag"), ","))
	if sql != "group_concat(`tag` SEPARATOR ',')" {
		t.Errorf("SQL = %s", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	escaped, _ := render(mysql.GroupConcat(mysql.Text("tag"), "'"))
	if escaped != "group_concat(`tag` SEPARATOR '''')" {
		t.Errorf("SQL = %s", escaped)
	}
}

func TestSubstringUsesTheKeywordForm(t *testing.T) {
	wantSQL(t, mysql.Substring(mysql.Text("s"), 2, 3), "substring(`s` FROM ? FOR ?)")
	wantSQL(t, mysql.Substring(mysql.Text("s"), 2, nil), "substring(`s` FROM ?)")
}

func TestIntervalIsNotAStandaloneValue(t *testing.T) {
	// MySQL only accepts INTERVAL as an operand of + / - or inside
	// DATE_ADD, so the helper renders the bare fragment and the
	// arithmetic wrapper is the caller's job.
	wantSQL(t, mysql.Day(7), "INTERVAL ? DAY")
	wantSQL(t, mysql.Plus(mysql.Timestamp("at", false), mysql.Day(7)),
		"(`at` + INTERVAL ? DAY)")
	wantSQL(t, mysql.DateAdd(mysql.Timestamp("at", false), mysql.Day(7)),
		"date_add(`at`, INTERVAL ? DAY)")
}

func TestIntervalRejectsAnInjectedUnit(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic on a unit that is not one word")
		}
	}()
	mysql.Interval(1, "DAY); DROP TABLE users; --")
}

func TestDateTruncCoversEveryField(t *testing.T) {
	ts := mysql.Timestamp("at", false)
	for _, f := range []string{"second", "minute", "hour", "day", "week", "month", "quarter", "year"} {
		sql, _ := render(mysql.DateTrunc(f, ts))
		if !strings.HasPrefix(sql, "CAST(") || !strings.HasSuffix(sql, " AS DATETIME(6))") {
			t.Errorf("DateTrunc(%q) = %s, want a DATETIME cast", f, sql)
		}
	}
}

func TestDateTruncRejectsAnUnknownField(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic on an unsupported field")
		}
	}()
	mysql.DateTrunc("decade", mysql.Timestamp("at", false))
}

func TestMakeDateIsNotMySQLsMakedate(t *testing.T) {
	// MySQL's own MAKEDATE(year, dayofyear) would read the 3 as the
	// third day of January; the port keeps PostgreSQL's y/m/d meaning.
	sql, _ := render(mysql.MakeDate(2024, 3, 15))
	if strings.Contains(sql, "makedate(") {
		t.Errorf("MakeDate must not render MAKEDATE: %s", sql)
	}
	if sql != "str_to_date(concat_ws(?, ?, ?, ?), ?)" {
		t.Errorf("SQL = %s", sql)
	}
}

func TestJSONPathQuotesEveryMember(t *testing.T) {
	cases := map[string][]any{
		`$."a"`:          {"a"},
		`$."a"."b"`:      {"a", "b"},
		`$."a"[0]`:       {"a", 0},
		`$."first name"`: {"first name"},
		`$."a.b"`:        {"a.b"},
		`$."say \"hi\""`: {`say "hi"`},
	}
	for want, keys := range cases {
		if got := mysql.JSONPath(keys...); got != want {
			t.Errorf("JSONPath(%v) = %s, want %s", keys, got, want)
		}
	}
}

func TestJSONGetAvoidsTheArrowOperators(t *testing.T) {
	// MariaDB has no -> or ->>, so neither may appear in what drops
	// renders — the calls are the portable spelling.
	doc := mysql.JSON("doc")
	get, _ := render(mysql.JSONGet(doc, "a"))
	if strings.Contains(get, "->") {
		t.Errorf("JSONGet rendered an arrow operator: %s", get)
	}
	if get != "json_extract(`doc`, ?)" {
		t.Errorf("SQL = %s", get)
	}
	text, args := render(mysql.JSONGetText(doc, "a"))
	if text != "json_unquote(json_extract(`doc`, ?))" {
		t.Errorf("SQL = %s", text)
	}
	if len(args) != 1 || args[0] != `$."a"` {
		t.Errorf("args = %v", args)
	}
}

func TestJSONHasAllKeysAsksForAll(t *testing.T) {
	sql, args := render(mysql.JSONHasAllKeys(mysql.JSON("doc"), "a", "b"))
	if sql != "json_contains_path(`doc`, ?, ?, ?)" {
		t.Errorf("SQL = %s", sql)
	}
	if args[0] != "all" || args[1] != `$."a"` || args[2] != `$."b"` {
		t.Errorf("args = %v", args)
	}
}

func TestJSONTableRejectsAPathThatIsNotOne(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic: JSON_TABLE paths are interpolated, not bound")
		}
	}()
	mysql.JSONTable(mysql.JSON("doc"), "'); DROP TABLE users; --", "jt")
}

func TestJSONTableRenders(t *testing.T) {
	e := mysql.JSONTable(mysql.JSON("doc"), "$.tags[*]", "jt",
		mysql.JSONCol("name", "VARCHAR(64)", "$.name"))
	wantSQL(t, e,
		"JSON_TABLE(`doc`, '$.tags[*]' COLUMNS (`name` VARCHAR(64) PATH '$.name')) AS `jt`")
}

func TestWindowSpecRendersInOrder(t *testing.T) {
	w := mysql.WindowSpec().
		PartitionBy(mysql.Integer("uid")).
		OrderBy(mysql.Integer("at").Desc()).
		Frame("ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
	wantSQL(t, mysql.Over(mysql.RowNumber(), w),
		"row_number() OVER (PARTITION BY `uid` ORDER BY `at` DESC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)")
}

func TestOverWithNoWindowIsTheWholeSet(t *testing.T) {
	wantSQL(t, mysql.Over(mysql.Rank(), nil), "rank() OVER ()")
}

func TestSubqueryHelpers(t *testing.T) {
	inner := drops.Raw("SELECT 1")
	wantSQL(t, mysql.Exists(inner), "EXISTS (SELECT 1)")
	wantSQL(t, mysql.NotExists(inner), "NOT EXISTS (SELECT 1)")
	wantSQL(t, mysql.Subquery(inner), "(SELECT 1)")
	wantSQL(t, mysql.AnySub(mysql.Integer("n"), inner), "(`n` = ANY (SELECT 1))")
	wantSQL(t, mysql.CmpAll(mysql.Integer("n"), ">", inner), "(`n` > ALL (SELECT 1))")
	wantSQL(t, mysql.InSub(mysql.Integer("n"), inner), "(`n` IN (SELECT 1))")
	wantSQL(t, mysql.NotInSub(mysql.Integer("n"), inner), "(`n` NOT IN (SELECT 1))")
}

func TestConcatIsNotThePipesOperator(t *testing.T) {
	// || is OR on a stock server, so nothing here may render it.
	sql, _ := render(mysql.Concat(mysql.Text("a"), mysql.Text("b")))
	if strings.Contains(sql, "||") {
		t.Errorf("Concat rendered the pipes operator: %s", sql)
	}
	if sql != "concat(`a`, `b`)" {
		t.Errorf("SQL = %s", sql)
	}
}

// A collation name is not an identifier: it cannot be backticked, so
// it lands in the SQL as written. The identifier check would let
// "utf8mb4_bin) OR (1=1" through, since it only guards what quoting
// cannot save.
func TestCollateRejectsAnythingButACollationName(t *testing.T) {
	wantSQL(t, mysql.Collate(mysql.Text("s"), "utf8mb4_bin"), "(`s` COLLATE utf8mb4_bin)")
	for _, bad := range []string{"", "utf8mb4_bin) OR (1=1", "utf8mb4 bin", "utf8mb4-bin", "`utf8mb4_bin`"} {
		t.Run(bad, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("Collate accepted %q, which is interpolated verbatim", bad)
				}
			}()
			mysql.Collate(mysql.Text("s"), bad)
		})
	}
}
