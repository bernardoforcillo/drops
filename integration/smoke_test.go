package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"
	_ "modernc.org/sqlite"
)

// The premise of this package, at its smallest: a statement drops
// generated, parsed and executed by a real engine.
//
// Everything else here is this test with more surface area.
func TestSQLiteRoundTripThroughARealEngine(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	db := sqlite.New(stdlib.New(sqlDB))
	ctx := context.Background()

	tbl := sqlite.NewTable("notes")
	// BigInt, not Integer: SQLite rowids are 64-bit, and sqlite.Integer
	// is typed *Col[int32].
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	title := sqlite.Add(tbl, sqlite.Text("title").NotNull())
	views := sqlite.Add(tbl, sqlite.Integer("views").NotNull().Default("0"))

	// DDL: the server is the one deciding whether this parses.
	if _, err := db.ExecExpr(ctx, sqlite.CreateTable(tbl)); err != nil {
		sqlText, _ := drops.StringWithDialect(sqlite.Dialect, sqlite.CreateTable(tbl))
		t.Fatalf("CREATE TABLE rejected by SQLite: %v\n%s", err, sqlText)
	}

	// An index, which is where one of the three known bugs lived: a
	// qualified column name inside the column list is a syntax error.
	idx := sqlite.CreateIndex("notes_title_idx", tbl, title)
	if _, err := db.ExecExpr(ctx, idx); err != nil {
		sqlText, _ := drops.StringWithDialect(sqlite.Dialect, idx)
		t.Fatalf("CREATE INDEX rejected by SQLite: %v\n%s", err, sqlText)
	}

	if _, err := db.Insert(tbl).Values(title.Val("first"), views.Val(3)).Exec(ctx); err != nil {
		t.Fatalf("INSERT: %v", err)
	}
	if _, err := db.Insert(tbl).Values(title.Val("second"), views.Val(9)).Exec(ctx); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	type note struct {
		ID    int64
		Title string
		Views int32
	}
	var got []note
	err = db.Select(id, title, views).
		From(tbl).
		Where(views.Gte(5)).
		OrderBy(id.Desc()).
		All(ctx, &got)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if len(got) != 1 || got[0].Title != "second" || got[0].Views != 9 {
		t.Fatalf("got %+v, want the one row with views >= 5", got)
	}
	if got[0].ID == 0 {
		t.Error("AUTOINCREMENT key came back as zero")
	}

	// UPDATE and DELETE, so every statement kind has been parsed.
	if _, err := db.Update(tbl).Set(views.Val(10)).Where(id.Eq(got[0].ID)).Exec(ctx); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if _, err := db.Delete(tbl).Where(views.Lt(5)).Exec(ctx); err != nil {
		t.Fatalf("DELETE: %v", err)
	}

	var remaining []note
	if err := db.Select(id, title, views).From(tbl).All(ctx, &remaining); err != nil {
		t.Fatalf("SELECT after mutations: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Views != 10 {
		t.Fatalf("after update+delete got %+v", remaining)
	}
}

// A rendering that a unit test would happily accept and a server will
// not: this asserts the failure mode is loud, so a future regression
// surfaces as a test failure with the offending SQL attached rather
// than as a silent wrong answer.
func TestInvalidSQLIsRejectedLoudly(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	db := sqlite.New(stdlib.New(sqlDB))
	_, err = db.Exec(context.Background(), `CREATE TABLE "t" ("t"."id" INTEGER)`)
	if err == nil {
		t.Fatal("SQLite accepted a qualified column name in CREATE TABLE; the premise of this suite is wrong")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "syntax") {
		t.Logf("rejected, though not as a syntax error: %v", err)
	}
}
