package pg_test

import (
	"reflect"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Schema fixtures ------------------------------------------------------

var (
	users    = pg.NewTable("users")
	userID   = pg.Add(users, pg.BigSerial("id").PrimaryKey())
	userName = pg.Add(users, pg.Text("name").NotNull())
	userAge  = pg.Add(users, pg.Integer("age"))

	posts      = pg.NewTable("posts")
	postID     = pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	postUserID = pg.Add(posts, pg.BigInt("user_id").NotNull().References(userID, pg.OnDelete("CASCADE")))
	postTitle  = pg.Add(posts, pg.Text("title").NotNull())
)

var _ = postID
var _ = postTitle

// Helpers -------------------------------------------------------------

type sqlable interface {
	ToSQL() (string, []any)
}

func check(t *testing.T, q sqlable, wantSQL string, wantArgs ...any) {
	t.Helper()
	gotSQL, gotArgs := q.ToSQL()
	if gotSQL != wantSQL {
		t.Errorf("sql mismatch\n  got:  %s\n  want: %s", gotSQL, wantSQL)
	}
	if len(wantArgs) == 0 && len(gotArgs) == 0 {
		return
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("args mismatch\n  got:  %v\n  want: %v", gotArgs, wantArgs)
	}
}

func checkExpr(t *testing.T, e drops.Expression, wantSQL string, wantArgs ...any) {
	t.Helper()
	gotSQL, gotArgs := drops.String(e)
	if gotSQL != wantSQL {
		t.Errorf("sql mismatch\n  got:  %s\n  want: %s", gotSQL, wantSQL)
	}
	if len(wantArgs) == 0 && len(gotArgs) == 0 {
		return
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Errorf("args mismatch\n  got:  %v\n  want: %v", gotArgs, wantArgs)
	}
}

// Operator tests ------------------------------------------------------

func TestTypedOperators(t *testing.T) {
	cases := []struct {
		name    string
		expr    drops.Expression
		wantSQL string
		args    []any
	}{
		{"col.Eq", userID.Eq(1), `("users"."id" = $1)`, []any{int64(1)}},
		{"col.EqCol", userID.EqCol(postUserID), `("users"."id" = "posts"."user_id")`, nil},
		{"col.Ne", userAge.Ne(0), `("users"."age" <> $1)`, []any{int32(0)}},
		{"col.Gt", userAge.Gt(18), `("users"."age" > $1)`, []any{int32(18)}},
		{"col.IsNull", userAge.IsNull(), `("users"."age" IS NULL)`, nil},
		{"col.In typed", userName.In("a", "b"), `("users"."name" IN ($1, $2))`, []any{"a", "b"}},
		{"col.Between", userAge.Between(18, 65), `("users"."age" BETWEEN $1 AND $2)`, []any{int32(18), int32(65)}},
		{"col.Like", userName.Like("A%"), `("users"."name" LIKE $1)`, []any{"A%"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkExpr(t, tc.expr, tc.wantSQL, tc.args...)
		})
	}
}

func TestEmptyInIsFalseAndEmptyNotInIsTrue(t *testing.T) {
	// PostgreSQL forbids an empty IN list. drops emits a static boolean
	// that matches the operator's intended semantics:
	//   IN ∅      → nothing matches      → (false)
	//   NOT IN ∅  → everything matches   → (true)
	checkExpr(t, pg.In(userID), `(false)`)
	checkExpr(t, pg.NotIn(userID), `(true)`)
	// Slice form: passing an empty []int should behave the same.
	checkExpr(t, pg.In(userID, []int{}), `(false)`)
	checkExpr(t, pg.NotIn(userID, []int{}), `(true)`)
}

func TestUntypedOperators(t *testing.T) {
	cases := []struct {
		name    string
		expr    drops.Expression
		wantSQL string
		args    []any
	}{
		{"and", pg.And(userID.Eq(1), userAge.Gt(18)),
			`(("users"."id" = $1) AND ("users"."age" > $2))`,
			[]any{int64(1), int32(18)}},
		{"or", pg.Or(userID.Eq(1), userAge.IsNull()),
			`(("users"."id" = $1) OR ("users"."age" IS NULL))`,
			[]any{int64(1)}},
		{"not", pg.Not(userAge.IsNull()), `(NOT ("users"."age" IS NULL))`, nil},
		{"in slice (untyped)", pg.In(userName, []string{"a", "b"}),
			`("users"."name" IN ($1, $2))`, []any{"a", "b"}},
		{"and zero", pg.And(), `TRUE`, nil},
		{"or zero", pg.Or(), `FALSE`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkExpr(t, tc.expr, tc.wantSQL, tc.args...)
		})
	}
}

// SELECT tests --------------------------------------------------------

func TestSelectBasic(t *testing.T) {
	db := pg.New(nil)
	q := db.Select().From(users)
	check(t, q, `SELECT * FROM "users"`)
}

func TestSelectColumns(t *testing.T) {
	db := pg.New(nil)
	q := db.Select(userID, userName).From(users)
	check(t, q, `SELECT "users"."id", "users"."name" FROM "users"`)
}

func TestSelectWhereOrderLimit(t *testing.T) {
	db := pg.New(nil)
	q := db.Select(userID).
		From(users).
		Where(userAge.Gte(18)).
		OrderBy(userName.Asc()).
		Limit(10).
		Offset(20)
	check(t,
		q,
		`SELECT "users"."id" FROM "users" WHERE ("users"."age" >= $1) ORDER BY "users"."name" ASC LIMIT $2 OFFSET $3`,
		int32(18), int64(10), int64(20),
	)
}

func TestSelectMultipleWherePredicatesAreAnded(t *testing.T) {
	db := pg.New(nil)
	q := db.Select().From(users).Where(userAge.Gte(18), userName.Ne(""))
	check(t,
		q,
		`SELECT * FROM "users" WHERE ("users"."age" >= $1) AND ("users"."name" <> $2)`,
		int32(18), "",
	)
}

func TestSelectLeftJoinGroupBy(t *testing.T) {
	db := pg.New(nil)
	q := db.Select(userName, pg.As(pg.CountAll(), "n")).
		From(users).
		LeftJoin(posts, postUserID.EqCol(userID)).
		GroupBy(userID, userName).
		OrderBy(userName.Asc())
	check(t,
		q,
		`SELECT "users"."name", count(*) AS "n" FROM "users" LEFT JOIN "posts" ON ("posts"."user_id" = "users"."id") GROUP BY "users"."id", "users"."name" ORDER BY "users"."name" ASC`,
	)
}

func TestSelectAlias(t *testing.T) {
	db := pg.New(nil)
	u := users.As("u")
	q := db.Select().From(u).Where(userID.Eq(1))
	check(t,
		q,
		`SELECT * FROM "users" AS "u" WHERE ("users"."id" = $1)`,
		int64(1),
	)
}

func TestSelectSubquery(t *testing.T) {
	db := pg.New(nil)
	sub := db.Select(userID).From(users).Where(userAge.Gt(30)).AsSubquery("u")
	q := db.Select().From(posts).Where(pg.In(postUserID, sub))
	check(t,
		q,
		`SELECT * FROM "posts" WHERE ("posts"."user_id" IN ((SELECT "users"."id" FROM "users" WHERE ("users"."age" > $1)) AS "u"))`,
		int32(30),
	)
}

// INSERT tests --------------------------------------------------------

func TestInsertSingleTyped(t *testing.T) {
	db := pg.New(nil)
	q := db.Insert(users).Row(userName.Val("Alice"), userAge.Val(30))
	check(t, q,
		`INSERT INTO "users" ("name", "age") VALUES ($1, $2)`,
		"Alice", int32(30),
	)
}

func TestInsertReturning(t *testing.T) {
	db := pg.New(nil)
	q := db.Insert(users).
		Row(userName.Val("Alice")).
		Returning(userID, userName)
	check(t, q,
		`INSERT INTO "users" ("name") VALUES ($1) RETURNING "users"."id", "users"."name"`,
		"Alice",
	)
}

func TestInsertBatchAlignsAndDefaults(t *testing.T) {
	db := pg.New(nil)
	q := db.Insert(users).
		Row(userName.Val("Alice"), userAge.Val(30)).
		Row(userName.Val("Carol")) // no age → DEFAULT
	check(t, q,
		`INSERT INTO "users" ("name", "age") VALUES ($1, $2), ($3, DEFAULT)`,
		"Alice", int32(30), "Carol",
	)
}

func TestInsertOnConflictDoNothing(t *testing.T) {
	db := pg.New(nil)
	q := db.Insert(users).
		Row(userID.Val(1), userName.Val("Alice")).
		OnConflictDoNothing(userID)
	check(t, q,
		`INSERT INTO "users" ("id", "name") VALUES ($1, $2) ON CONFLICT ("id") DO NOTHING`,
		int64(1), "Alice",
	)
}

func TestInsertUpsert(t *testing.T) {
	db := pg.New(nil)
	q := db.Insert(users).
		Row(userID.Val(1), userName.Val("Alice"), userAge.Val(31)).
		OnConflictUpdate(userID).
		Set(userAge.Expr(userAge.Excluded())).
		Done()
	check(t, q,
		`INSERT INTO "users" ("id", "name", "age") VALUES ($1, $2, $3) ON CONFLICT ("id") DO UPDATE SET "age" = EXCLUDED."age"`,
		int64(1), "Alice", int32(31),
	)
}

// UPDATE tests --------------------------------------------------------

func TestUpdateTyped(t *testing.T) {
	db := pg.New(nil)
	q := db.Update(users).
		Set(userAge.Val(26)).
		Where(userName.Eq("Bob"))
	check(t, q,
		`UPDATE "users" SET "age" = $1 WHERE ("users"."name" = $2)`,
		int32(26), "Bob",
	)
}

func TestUpdateMultipleAssignments(t *testing.T) {
	db := pg.New(nil)
	q := db.Update(users).
		Set(userName.Val("Robert"), userAge.Val(40)).
		Where(userID.Eq(1)).
		Returning(userID, userName)
	check(t, q,
		`UPDATE "users" SET "name" = $1, "age" = $2 WHERE ("users"."id" = $3) RETURNING "users"."id", "users"."name"`,
		"Robert", int32(40), int64(1),
	)
}

// DELETE tests --------------------------------------------------------

func TestDelete(t *testing.T) {
	db := pg.New(nil)
	q := db.Delete(users).Where(userID.Eq(1))
	check(t, q,
		`DELETE FROM "users" WHERE ("users"."id" = $1)`,
		int64(1),
	)
}

// DDL tests -----------------------------------------------------------

func TestCreateTableUsers(t *testing.T) {
	want := "CREATE TABLE \"users\" (\n  \"id\" bigserial PRIMARY KEY,\n  \"name\" text NOT NULL,\n  \"age\" integer\n)"
	got, _ := drops.String(pg.CreateTable(users))
	if got != want {
		t.Errorf("CREATE TABLE mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestCreateTablePostsForeignKey(t *testing.T) {
	want := "CREATE TABLE \"posts\" (\n  \"id\" bigserial PRIMARY KEY,\n  \"user_id\" bigint NOT NULL REFERENCES \"users\" (\"id\") ON DELETE CASCADE,\n  \"title\" text NOT NULL\n)"
	got, _ := drops.String(pg.CreateTable(posts))
	if got != want {
		t.Errorf("CREATE TABLE mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
