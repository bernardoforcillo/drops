package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

func TestHookSeesEveryStatementKind(t *testing.T) {
	var kinds []string
	db := mysql.New(&fakeDriver{}).WithHook(func(_ context.Context, e drops.QueryEvent) {
		kinds = append(kinds, e.Kind)
	})
	ctx := context.Background()

	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(ctx, func(*mysql.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}

	want := []string{"ping", "exec", "query", "begin", "commit"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
}

func TestInTxRollsBackOnError(t *testing.T) {
	var kinds []string
	db := mysql.New(&fakeDriver{}).WithHook(func(_ context.Context, e drops.QueryEvent) {
		kinds = append(kinds, e.Kind)
	})
	boom := errors.New("boom")
	if err := db.InTx(context.Background(), func(*mysql.DB) error { return boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v want boom", err)
	}
	if !strings.Contains(strings.Join(kinds, ","), "rollback") {
		t.Errorf("expected a rollback, got %v", kinds)
	}
}

// A panic inside the callback must roll back and then keep going up.
func TestInTxRollsBackAndRepanics(t *testing.T) {
	var kinds []string
	db := mysql.New(&fakeDriver{}).WithHook(func(_ context.Context, e drops.QueryEvent) {
		kinds = append(kinds, e.Kind)
	})
	defer func() {
		if r := recover(); r == nil {
			t.Error("panic should propagate")
		}
		if !strings.Contains(strings.Join(kinds, ","), "rollback") {
			t.Errorf("expected a rollback, got %v", kinds)
		}
	}()
	_ = db.InTx(context.Background(), func(*mysql.DB) error { panic("kaboom") })
}

func TestExecExprRunsDDL(t *testing.T) {
	tbl, _, _, _ := usersTable()
	d := &fakeDriver{}
	if _, err := mysql.New(d).ExecExpr(context.Background(), mysql.CreateTable(tbl)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.queries[0], "CREATE TABLE") {
		t.Errorf("got %s", d.queries[0])
	}
}

func TestLastInsertIDReportsAbsence(t *testing.T) {
	if _, ok := mysql.LastInsertID(plainResult{}); ok {
		t.Error("a Result without LastInsertId should report false")
	}
	id, ok := mysql.LastInsertID(lastIDResult{id: 9})
	if !ok || id != 9 {
		t.Errorf("LastInsertID = %d, %v", id, ok)
	}
}

func TestEntityQuery(t *testing.T) {
	ent, tbl := userEntity(t)
	name := tbl.Col("name")
	d := &fakeDriver{}
	if _, err := ent.Query(mysql.New(d)).
		Where(mysql.Eq(name, "ada")).
		OrderBy(name.Asc()).
		Limit(5).
		All(context.Background()); err != nil {
		t.Fatal(err)
	}
	sql := d.queries[0]
	for _, want := range []string{"`users`.`name` = ?", "ORDER BY `users`.`name` ASC", "LIMIT ?"} {
		if !strings.Contains(sql, want) {
			t.Errorf("query missing %s: %s", want, sql)
		}
	}
}

func TestCreateManyAndUpsertMany(t *testing.T) {
	ent, _ := userEntity(t)
	rows := []user{{Name: "a", Email: "a@x"}, {Name: "b", Email: "b@x"}}

	d := &fakeDriver{}
	if _, err := ent.CreateMany(mysql.New(d), context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	if strings.Count(d.queries[0], "(?, ?)") != 2 {
		t.Errorf("expected two value tuples: %s", d.queries[0])
	}

	u := &fakeDriver{}
	if _, err := ent.UpsertMany(mysql.New(u), context.Background(), rows); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u.queries[0], "ON DUPLICATE KEY UPDATE") {
		t.Errorf("upsert clause missing: %s", u.queries[0])
	}

	// An empty batch is a no-op, not an error.
	if res, err := ent.CreateMany(mysql.New(&fakeDriver{}), context.Background(), nil); err != nil || res != nil {
		t.Errorf("empty CreateMany = %v, %v", res, err)
	}
}

func TestDeleteRemovesByKey(t *testing.T) {
	ent, _ := userEntity(t)
	d := &fakeDriver{}
	if _, err := ent.Delete(mysql.New(d), context.Background(), int64(3)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.queries[0], "DELETE FROM `users`") {
		t.Errorf("got %s", d.queries[0])
	}
}

func TestIdentValidation(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"trailing space": "users ",
		"too long":       strings.Repeat("a", 65),
	}
	for label, name := range cases {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: expected a panic for %q", label, name)
				}
			}()
			mysql.NewTable(name)
		}()
	}
	// A name with a backtick is fine — quoting doubles it.
	if mysql.NewTable("we`ird") == nil {
		t.Error("a quotable name should be accepted")
	}
}

func TestTableAliasQualifiesColumnsThroughTheAlias(t *testing.T) {
	tbl, id, _, _ := usersTable()
	aliased := tbl.As("u")
	if got, _ := sqlOf(aliased); got != "`users` AS `u`" {
		t.Errorf("alias = %s", got)
	}
	// A column reference through the alias uses it, and the original
	// handle keeps the table name — the two have to disagree, or a
	// self-join has no way to say which side it means.
	if got, _ := sqlOf(aliased.Col("id")); got != "`u`.`id`" {
		t.Errorf("aliased column = %s, want `u`.`id`", got)
	}
	if got, _ := sqlOf(id); got != "`users`.`id`" {
		t.Errorf("original column = %s, want `users`.`id`", got)
	}
	if aliased.Col("id") == id.Column {
		t.Error("the alias handed back the original column, so As only copied the table")
	}
}

func TestDuplicateColumnNamePanics(t *testing.T) {
	tbl, _, _, _ := usersTable()
	defer func() {
		if recover() == nil {
			t.Error("a duplicate column name should panic")
		}
	}()
	mysql.Add(tbl, mysql.Varchar("name", 10))
}

func TestEnumAndCustomTypes(t *testing.T) {
	tbl := mysql.NewTable("t")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Enum("status", "draft", "live"))
	mysql.Add(tbl, mysql.Custom[string]("packed", "BINARY(16)"))
	got, _ := sqlOf(mysql.CreateTable(tbl))
	if !strings.Contains(got, "ENUM('draft', 'live')") {
		t.Errorf("enum not rendered: %s", got)
	}
	if !strings.Contains(got, "`packed` BINARY(16)") {
		t.Errorf("custom type not rendered: %s", got)
	}
}

// The TIMESTAMP/DATETIME distinction is the one MySQL type choice that
// silently changes stored values, so pin it.
func TestTimestampMapsToDatetimeByDefault(t *testing.T) {
	tbl := mysql.NewTable("t")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Timestamp("naive", false))
	mysql.Add(tbl, mysql.Timestamp("zoned", true))
	got, _ := sqlOf(mysql.CreateTable(tbl))
	if !strings.Contains(got, "`naive` DATETIME(6)") {
		t.Errorf("default should be DATETIME: %s", got)
	}
	if !strings.Contains(got, "`zoned` TIMESTAMP(6)") {
		t.Errorf("withTimeZone should be TIMESTAMP: %s", got)
	}
}

func TestOnUpdateExpr(t *testing.T) {
	tbl := mysql.NewTable("t")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(tbl, mysql.Timestamp("updatedAt", false).
		NotNull().Default("CURRENT_TIMESTAMP(6)").OnUpdateExpr("CURRENT_TIMESTAMP(6)"))
	got, _ := sqlOf(mysql.CreateTable(tbl))
	if !strings.Contains(got, "ON UPDATE CURRENT_TIMESTAMP(6)") {
		t.Errorf("ON UPDATE clause missing: %s", got)
	}
}
