package integration_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"
	_ "modernc.org/sqlite"
)

func openSQLite(t *testing.T) *sqlite.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlite.New(stdlib.New(sqlDB))
}

// exec runs a statement and, on rejection, prints the SQL that was
// rejected. A failure here is worth nothing without the rendering.
func exec(t *testing.T, db *sqlite.DB, e drops.Expression) {
	t.Helper()
	if _, err := db.ExecExpr(context.Background(), e); err != nil {
		text, args := drops.StringWithDialect(sqlite.Dialect, e)
		t.Fatalf("SQLite rejected the statement: %v\n%s\nargs: %v", err, text, args)
	}
}

// Every column type the dialect can declare, in one table the server
// has to accept. A type constructor that renders something SQLite does
// not understand fails here and nowhere else in the repository.
func TestEveryColumnTypeIsAcceptedByTheEngine(t *testing.T) {
	db := openSQLite(t)
	tbl := sqlite.NewTable("kitchen_sink")

	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(tbl, sqlite.Text("a_text"))
	sqlite.Add(tbl, sqlite.Varchar("a_varchar", 255))
	sqlite.Add(tbl, sqlite.Char("a_char", 8))
	sqlite.Add(tbl, sqlite.SmallInt("a_smallint"))
	sqlite.Add(tbl, sqlite.Integer("a_integer"))
	sqlite.Add(tbl, sqlite.Real("a_real"))
	sqlite.Add(tbl, sqlite.DoublePrecision("a_double"))
	sqlite.Add(tbl, sqlite.Numeric("a_numeric", 10, 2))
	sqlite.Add(tbl, sqlite.Boolean("a_bool"))
	sqlite.Add(tbl, sqlite.Date("a_date"))
	sqlite.Add(tbl, sqlite.Time("a_time"))
	sqlite.Add(tbl, sqlite.Timestamp("a_ts", false))
	sqlite.Add(tbl, sqlite.UUID("a_uuid"))
	sqlite.Add(tbl, sqlite.JSON("a_json"))
	sqlite.Add(tbl, sqlite.JSONB("a_jsonb"))
	sqlite.Add(tbl, sqlite.Blob("a_blob"))
	sqlite.Add(tbl, sqlite.Bytea("a_bytea"))
	sqlite.Add(tbl, sqlite.Custom[string]("a_custom", "TEXT"))

	exec(t, db, sqlite.CreateTable(tbl))
}

// Constraints, defaults and foreign keys — the clauses whose ORDER in
// the emitted definition the grammar cares about.
func TestConstraintsAreAcceptedByTheEngine(t *testing.T) {
	db := openSQLite(t)

	parents := sqlite.NewTable("parents")
	parentID := sqlite.Add(parents, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	exec(t, db, sqlite.CreateTable(parents))

	children := sqlite.NewTable("children")
	sqlite.Add(children, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(children, sqlite.Text("name").NotNull().Unique())
	sqlite.Add(children, sqlite.Integer("count").NotNull().Default("0"))
	sqlite.Add(children, sqlite.Text("note").Default("'none'"))
	sqlite.Add(children, sqlite.BigInt("parentId").
		NotNull().
		References(parentID, sqlite.OnDelete("CASCADE"), sqlite.OnUpdate("RESTRICT")))
	exec(t, db, sqlite.CreateTable(children))

	// And the constraints have to actually bite, or they were rendered
	// somewhere the engine ignores.
	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO "children" ("name","count","parentId") VALUES ('a',1,1)`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "children" ("name","count","parentId") VALUES ('a',1,1)`); err == nil {
		t.Error("UNIQUE was rendered but not enforced")
	}
	var note string
	row, err := db.Query(ctx, `SELECT "note" FROM "children" LIMIT 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer row.Close()
	if row.Next() {
		if err := row.Scan(&note); err != nil {
			t.Fatal(err)
		}
	}
	if note != "none" {
		t.Errorf("DEFAULT did not apply: note = %q", note)
	}
}

// A composite primary key, which the entity layer now supports and no
// test had ever asked a server to parse.
func TestCompositePrimaryKeyIsAcceptedByTheEngine(t *testing.T) {
	db := openSQLite(t)
	tbl := sqlite.NewTable("memberships")
	sqlite.Add(tbl, sqlite.BigInt("orgId").PrimaryKey())
	sqlite.Add(tbl, sqlite.BigInt("userId").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("role").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	ctx := context.Background()
	if _, err := db.Exec(ctx, `INSERT INTO "memberships" VALUES (1,2,'admin')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "memberships" VALUES (1,2,'member')`); err == nil {
		t.Error("the composite key was rendered but not enforced")
	}
	if _, err := db.Exec(ctx, `INSERT INTO "memberships" VALUES (1,3,'member')`); err != nil {
		t.Errorf("a distinct composite key was rejected: %v", err)
	}
}

// Indexes: the shape of one of the three known invalid-SQL bugs.
func TestIndexesAreAcceptedByTheEngine(t *testing.T) {
	db := openSQLite(t)
	tbl := sqlite.NewTable("docs")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	title := sqlite.Add(tbl, sqlite.Text("title").NotNull())
	lang := sqlite.Add(tbl, sqlite.Text("lang"))
	exec(t, db, sqlite.CreateTable(tbl))

	exec(t, db, sqlite.CreateIndex("docs_title_idx", tbl, title))
	exec(t, db, sqlite.CreateUniqueIndex("docs_title_lang_idx", tbl, title, lang))
	exec(t, db, sqlite.CreateIndexIfNotExists("docs_lang_idx", tbl, lang))
}

// The entity layer end to end: create, read back, update, delete —
// every statement generated by drops rather than written by hand.
func TestEntityCRUDAgainstTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("users")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	name := sqlite.Add(tbl, sqlite.Text("name").NotNull())
	sqlite.Add(tbl, sqlite.Integer("age").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	type user struct {
		ID   int64
		Name string
		Age  int32
	}
	ent := sqlite.NewEntity[user](tbl)

	u := user{Name: "Ada", Age: 36}
	if err := ent.Create(db, ctx, &u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	all, err := ent.Query(db).Where(name.Eq("Ada")).All(ctx)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(all) != 1 || all[0].Age != 36 {
		t.Fatalf("read back %+v", all)
	}
	key := all[0].ID
	if key == 0 {
		t.Fatal("the generated key did not come back")
	}

	got, err := ent.Get(db, ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Ada" {
		t.Errorf("Get returned %+v", got)
	}

	got.Age = 37
	if err := ent.Update(db, ctx, &got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := ent.Get(db, ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if after.Age != 37 {
		t.Errorf("Update did not persist: %+v", after)
	}

	if _, err := ent.Delete(db, ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ent.Get(db, ctx, key); err == nil {
		t.Error("the row survived Delete")
	}
	_ = id
}

// Keyset pagination, whose whole promise is that it walks every row
// exactly once. A cursor bug is invisible to a rendering test and
// obvious here.
func TestKeysetPaginationWalksEveryRowOnce(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("items")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	label := sqlite.Add(tbl, sqlite.Text("label").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	type item struct {
		ID    int64
		Label string
	}
	ent := sqlite.NewEntity[item](tbl)
	const total = 25
	for i := 0; i < total; i++ {
		r := item{Label: strings.Repeat("x", i%5+1)}
		if err := ent.Create(db, ctx, &r); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	seen := map[int64]int{}
	pages := 0
	for cursor := ""; ; {
		page, err := ent.Page(db).OrderBy(sqlite.Asc(id)).Limit(7).After(cursor).All(ctx)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, it := range page.Items {
			seen[it.ID]++
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("walked %d distinct rows, want %d", len(seen), total)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("row %d returned %d times", k, n)
		}
	}
	_ = label
	_ = time.Now
}

// SQLite's grammar reaches OFFSET only through LIMIT, so a query that
// sets an offset and no limit has to carry one anyway. Rendering shows
// nothing wrong; the engine rejects it outright.
func TestOffsetWithoutLimitIsAcceptedByTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("rows")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(tbl, sqlite.Text("label").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	type row struct {
		ID    int64
		Label string
	}
	ent := sqlite.NewEntity[row](tbl)
	for _, l := range []string{"a", "b", "c"} {
		r := row{Label: l}
		if err := ent.Create(db, ctx, &r); err != nil {
			t.Fatalf("seed %q: %v", l, err)
		}
	}

	var got []row
	sel := db.Select().From(tbl).OrderBy(id.Asc()).Offset(1)
	if err := sel.All(ctx, &got); err != nil {
		text, args := drops.StringWithDialect(sqlite.Dialect, sel)
		t.Fatalf("SQLite rejected an offset without a limit: %v\n%s\nargs: %v", err, text, args)
	}
	if len(got) != 2 || got[0].Label != "b" {
		t.Errorf("offset 1 returned %+v", got)
	}
}

// CreateMany renders one multi-row INSERT, and a row whose key the
// caller left zero binds one fewer column than a row that set it. The
// two shapes cannot share a VALUES list, and the engine is the only
// thing that says so.
func TestCreateManyWithMixedKeysIsAcceptedByTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("accounts")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())
	sqlite.Add(tbl, sqlite.Text("role").NotNull().Default("'member'"))
	exec(t, db, sqlite.CreateTable(tbl))

	type account struct {
		ID   int64
		Name string
		Role string
	}
	ent := sqlite.NewEntity[account](tbl)
	if _, err := ent.CreateMany(db, ctx, []account{
		{Name: "generated"},
		{ID: 42, Name: "explicit"},
		{Name: "promoted", Role: "admin"},
	}); err != nil {
		t.Fatalf("CreateMany with mixed keys: %v", err)
	}

	all, err := ent.Query(db).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("inserted %d rows, want 3", len(all))
	}
	byName := map[string]account{}
	for _, a := range all {
		byName[a.Name] = a
	}
	if byName["generated"].ID == 0 {
		t.Error("the zero key was written through instead of being generated")
	}
	if byName["explicit"].ID != 42 {
		t.Errorf("the explicit key did not survive: %+v", byName["explicit"])
	}
	// A column a row left zero must still land on its declared
	// default, exactly as it would had the row been inserted alone.
	if byName["generated"].Role != "member" {
		t.Errorf("default not applied to the widened row: %+v", byName["generated"])
	}
	if byName["promoted"].Role != "admin" {
		t.Errorf("an explicit value was overwritten by the default: %+v", byName["promoted"])
	}
}

// A cursor is built to travel through a query string, so its bytes are
// the client's. A malformed one renders as a false predicate carrying
// the decode error inside a block comment — and the error quotes the
// cursor's own type tag, so a tag holding "*/" used to close the
// comment and leave the rest of it standing as SQL.
func TestMaliciousCursorCannotEscapeItsComment(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("posts")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("body").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))

	type post struct {
		ID   int64
		Body string
	}
	ent := sqlite.NewEntity[post](tbl)
	secret := post{ID: 1, Body: "secret"}
	if err := ent.Create(db, ctx, &secret); err != nil {
		t.Fatal(err)
	}

	spec := sqlite.NewCursorSpec(sqlite.OrderKey{Col: id})
	// Two paths quote the client's own bytes back into the error: an
	// unknown type tag, and a "t" value that time.Parse rejects.
	for name, payload := range map[string]string{
		"unknown tag":  `{"v":[{"t":"*/ OR 1=1 --","v":null}]}`,
		"bad RFC 3339": `{"v":[{"t":"t","v":"*/ OR 1=1 --"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			hostile := sqlite.Cursor(base64.RawURLEncoding.EncodeToString([]byte(payload)))
			sel := db.Select().From(tbl).OrderByCursor(spec).AfterCursor(spec, hostile)
			text, _ := drops.StringWithDialect(sqlite.Dialect, sel)
			var got []post
			if err := sel.All(ctx, &got); err != nil {
				t.Fatalf("the guarded query did not run at all: %v\n%s", err, text)
			}
			if len(got) != 0 {
				t.Errorf("a hostile cursor matched %d row(s); the guard was escaped:\n%s", len(got), text)
			}
		})
	}
}

// Two rows can bind the same *number* of columns and still bind
// different ones: a row that sets its key but leaves a defaulted column
// zero binds (id, name), while a row that omits its key but sets that
// column binds (name, role). The engine cannot object — the tuples are
// the same width — so the values land under the wrong column names.
func TestCreateManyWithEquallyWideButDifferentRows(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("members")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())
	sqlite.Add(tbl, sqlite.Text("role").NotNull().Default("'member'"))
	exec(t, db, sqlite.CreateTable(tbl))

	type member struct {
		ID   int64
		Name string
		Role string
	}
	ent := sqlite.NewEntity[member](tbl)
	if _, err := ent.CreateMany(db, ctx, []member{
		{Name: "generated", Role: "admin"},
		{ID: 7, Name: "explicit"},
	}); err != nil {
		t.Fatalf("CreateMany: %v", err)
	}

	all, err := ent.Query(db).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]member{}
	for _, m := range all {
		byName[m.Name] = m
	}
	if _, ok := byName["explicit"]; !ok {
		t.Fatalf("the second row was written under the wrong columns: %+v", all)
	}
	if byName["explicit"].ID != 7 {
		t.Errorf("the explicit key did not reach the id column: %+v", byName["explicit"])
	}
	if byName["explicit"].Role != "member" {
		t.Errorf("the unset column did not fall back to its default: %+v", byName["explicit"])
	}
	if byName["generated"].Role != "admin" {
		t.Errorf("an explicit value was lost: %+v", byName["generated"])
	}
}
