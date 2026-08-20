package mysql_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

func cteFixture() (*mysql.Table, *mysql.Col[int64], *mysql.Col[string]) {
	t := mysql.NewTable("employees")
	id := mysql.Add(t, mysql.BigSerial("id").PrimaryKey())
	name := mysql.Add(t, mysql.Varchar("name", 255))
	return t, id, name
}

func TestWithRendersBeforeTheSelect(t *testing.T) {
	tbl, id, _ := cteFixture()
	db := mysql.New(&fakeDriver{})
	inner := db.Select(id).From(tbl)
	cte := mysql.CTEDef("recent", inner)

	sql, _ := (db.Select(drops.Raw("*")).With(cte).FromExpr(cte.Ref())).ToSQL()
	want := "WITH `recent` AS (SELECT `employees`.`id` FROM `employees`) SELECT * FROM `recent`"
	if sql != want {
		t.Errorf("SQL = %s\nwant  %s", sql, want)
	}
}

func TestCTEColumnListAndColumnReference(t *testing.T) {
	db := mysql.New(&fakeDriver{})
	cte := mysql.CTEDef("t", drops.Raw("SELECT 1"), "n")
	sql, _ := db.Select(cte.Col("n")).With(cte).FromExpr(cte.Ref()).ToSQL()
	want := "WITH `t` (`n`) AS (SELECT 1) SELECT `t`.`n` FROM `t`"
	if sql != want {
		t.Errorf("SQL = %s\nwant  %s", sql, want)
	}
}

func TestWithRecursiveIsStickyAcrossEveryCTE(t *testing.T) {
	// One statement has one mode: RECURSIVE is written once, in front
	// of the whole list, which is what both servers require.
	db := mysql.New(&fakeDriver{})
	a := mysql.CTEDef("a", drops.Raw("SELECT 1"))
	b := mysql.CTEDef("b", drops.Raw("SELECT 2"))
	sql, _ := db.Select(drops.Raw("*")).With(a).WithRecursive(b).FromExpr(b.Ref()).ToSQL()
	if !strings.HasPrefix(sql, "WITH RECURSIVE `a` AS (SELECT 1), `b` AS (SELECT 2) ") {
		t.Errorf("SQL = %s", sql)
	}
	if strings.Count(sql, "RECURSIVE") != 1 {
		t.Errorf("RECURSIVE must appear once: %s", sql)
	}
}

func TestCTEArgumentsAreBoundBeforeTheOuterOnes(t *testing.T) {
	// MySQL has no numbered placeholders, so a WITH clause that binds
	// arguments has to render — and therefore bind — before the SELECT
	// body that follows it. Getting this backwards swaps the values.
	tbl, id, name := cteFixture()
	db := mysql.New(&fakeDriver{})
	inner := db.Select(id).From(tbl).Where(name.Eq("alice"))
	cte := mysql.CTEDef("c", inner)
	sql, args := db.Select(drops.Raw("*")).With(cte).FromExpr(cte.Ref()).
		Where(mysql.Gt(drops.Raw("`c`.`id`"), 10)).ToSQL()
	if len(args) != 2 || args[0] != "alice" || args[1] != 10 {
		t.Fatalf("args = %v, want [alice 10]; SQL = %s", args, sql)
	}
	if strings.Index(sql, "WITH") != 0 {
		t.Errorf("WITH must lead the statement: %s", sql)
	}
}

func TestCTEDefRejectsABadName(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic")
		}
	}()
	mysql.CTEDef("", drops.Raw("SELECT 1"))
}

func TestSupportsCTEAndWindowFunctionsFollowTheServer(t *testing.T) {
	cases := []struct {
		info mysql.ServerInfo
		want bool
	}{
		{mysql.ServerInfo{Major: 5, Minor: 7}, false},
		{mysql.ServerInfo{Major: 8, Minor: 0}, true},
		{mysql.ServerInfo{Major: 8, Minor: 4}, true},
		{mysql.ServerInfo{Major: 10, Minor: 1, MariaDB: true}, false},
		{mysql.ServerInfo{Major: 10, Minor: 2, MariaDB: true}, true},
		{mysql.ServerInfo{Major: 10, Minor: 11, MariaDB: true}, true},
	}
	for _, tc := range cases {
		if got := tc.info.SupportsCTE(); got != tc.want {
			t.Errorf("%+v SupportsCTE = %v, want %v", tc.info, got, tc.want)
		}
		if got := tc.info.SupportsWindowFunctions(); got != tc.want {
			t.Errorf("%+v SupportsWindowFunctions = %v, want %v", tc.info, got, tc.want)
		}
	}
}

func TestFromExprCrossJoinsExtraSources(t *testing.T) {
	tbl, _, _ := cteFixture()
	db := mysql.New(&fakeDriver{})
	sql, _ := db.Select(drops.Raw("*")).From(tbl).FromExpr(drops.Raw("`other` AS `o`")).ToSQL()
	if sql != "SELECT * FROM `employees`, `other` AS `o`" {
		t.Errorf("SQL = %s", sql)
	}
}
