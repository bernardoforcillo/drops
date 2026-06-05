package pg_test

import (
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// --- DDL objects -----------------------------------------------------

func TestCreateSchemaAndExtension(t *testing.T) {
	checkExpr(t, pg.CreateSchema("analytics"), `CREATE SCHEMA "analytics"`)
	checkExpr(t, pg.CreateSchemaIfNotExists("analytics"), `CREATE SCHEMA IF NOT EXISTS "analytics"`)
	checkExpr(t, pg.DropSchemaIfExists("analytics", true), `DROP SCHEMA IF EXISTS "analytics" CASCADE`)

	checkExpr(t, pg.CreateExtensionIfNotExists("pgcrypto"), `CREATE EXTENSION IF NOT EXISTS "pgcrypto"`)
	checkExpr(t, pg.DropExtension("pgcrypto"), `DROP EXTENSION "pgcrypto"`)
}

func TestSequenceDDL(t *testing.T) {
	start := int64(100)
	checkExpr(t,
		pg.CreateSequenceIfNotExists("userIdSeq", pg.SequenceOptions{Start: &start}),
		`CREATE SEQUENCE IF NOT EXISTS "userIdSeq" START WITH 100`,
	)
	checkExpr(t, pg.NextVal("userIdSeq"), `nextval('userIdSeq'::regclass)`)
	checkExpr(t, pg.DropSequenceIfExists("userIdSeq"), `DROP SEQUENCE IF EXISTS "userIdSeq"`)
}

func TestViewDDL(t *testing.T) {
	db := pg.New(nil)
	q := db.Select(userID, userName).From(users)
	checkExpr(t,
		pg.CreateOrReplaceView("activeUsers", q),
		`CREATE OR REPLACE VIEW "activeUsers" AS SELECT "users"."id", "users"."name" FROM "users"`,
	)
	checkExpr(t,
		pg.CreateMaterializedView("mvUsers", q, true),
		`CREATE MATERIALIZED VIEW "mvUsers" AS SELECT "users"."id", "users"."name" FROM "users" WITH DATA`,
	)
	checkExpr(t,
		pg.RefreshMaterializedView("mvUsers", true),
		`REFRESH MATERIALIZED VIEW CONCURRENTLY "mvUsers"`,
	)
}

func TestFunctionAndTriggerDDL(t *testing.T) {
	stmt, _ := drops.String(pg.CreateFunction("touchUpdatedAt", pg.FunctionOptions{
		Args:    "",
		Returns: "trigger",
		Body:    "BEGIN NEW.updatedAt = now(); RETURN NEW; END;",
	}))
	want := "CREATE FUNCTION \"touchUpdatedAt\"() RETURNS trigger LANGUAGE plpgsql AS $func$ BEGIN NEW.updatedAt = now(); RETURN NEW; END; $func$"
	if stmt != want {
		t.Errorf("CreateFunction\n  got:  %s\n  want: %s", stmt, want)
	}

	checkExpr(t,
		pg.CreateTrigger("usersTouch", pg.TriggerOptions{
			Timing:  "BEFORE",
			Events:  "UPDATE",
			Table:   users,
			Execute: "touchUpdatedAt()",
		}),
		`CREATE TRIGGER "usersTouch" BEFORE UPDATE ON "users" FOR EACH ROW EXECUTE FUNCTION touchUpdatedAt()`,
	)
	checkExpr(t, pg.DropTriggerIfExists("usersTouch", users),
		`DROP TRIGGER IF EXISTS "usersTouch" ON "users"`)
}

func TestCommentDDL(t *testing.T) {
	gotSQL, args := drops.String(pg.CommentOnTable(users, "all known users"))
	if gotSQL != `COMMENT ON TABLE "users" IS $1` || len(args) != 1 || args[0] != "all known users" {
		t.Errorf("CommentOnTable: %q args=%v", gotSQL, args)
	}
	gotSQL, args = drops.String(pg.CommentOnColumn(userName, "display name"))
	if gotSQL != `COMMENT ON COLUMN "users"."name" IS $1` || args[0] != "display name" {
		t.Errorf("CommentOnColumn: %q args=%v", gotSQL, args)
	}
}

// --- Indexes ---------------------------------------------------------

func TestCreateIndexUniqueWhereInclude(t *testing.T) {
	idx := pg.NewIndex("usersActiveEmailIdx", users, userName).
		Unique().
		Using("btree").
		Include(userID.Column).
		Where(userAge.Gte(18))
	checkExpr(t, pg.CreateIndex(idx),
		`CREATE UNIQUE INDEX "usersActiveEmailIdx" ON "users" USING btree ("users"."name") INCLUDE ("id") WHERE ("users"."age" >= $1)`,
		int32(18),
	)
}

func TestDropIndex(t *testing.T) {
	checkExpr(t, pg.DropIndexIfExists("usersEmailIdx"), `DROP INDEX IF EXISTS "usersEmailIdx"`)
}

// --- Enums -----------------------------------------------------------

func TestEnumDDLAndColumn(t *testing.T) {
	status := pg.NewEnum("userStatus", "active", "pending", "banned")
	checkExpr(t, pg.CreateEnum(status),
		`CREATE TYPE "userStatus" AS ENUM ('active', 'pending', 'banned')`,
	)
	checkExpr(t, pg.AlterEnumAddValue("userStatus", "archived", "", "banned"),
		`ALTER TYPE "userStatus" ADD VALUE 'archived' AFTER 'banned'`,
	)
	col := status.Col("status").NotNull()
	if col.Type().TypeSQL() != "userStatus" {
		t.Errorf("enum col type SQL: %s", col.Type().TypeSQL())
	}
}

// --- Functions: strings / math / date / json / array -----------------

func TestStringFunctions(t *testing.T) {
	checkExpr(t, pg.Concat(userName, " (", userID, ")"),
		`concat("users"."name", $1, "users"."id", $2)`, " (", ")")
	checkExpr(t, pg.ConcatOp(userName, " v2"), `("users"."name" || $1)`, " v2")
	checkExpr(t, pg.Length(userName), `length("users"."name")`)
	checkExpr(t, pg.Substring(userName, 1, 3), `substring("users"."name" FROM $1 FOR $2)`, 1, 3)
	checkExpr(t, pg.Replace(userName, "a", "b"), `replace("users"."name", $1, $2)`, "a", "b")
	checkExpr(t, pg.Initcap(userName), `initcap("users"."name")`)
}

func TestMathFunctions(t *testing.T) {
	checkExpr(t, pg.Abs(userAge), `abs("users"."age")`)
	checkExpr(t, pg.Round(userAge, 2), `round("users"."age", $1)`, 2)
	checkExpr(t, pg.Greatest(userAge, 18), `greatest("users"."age", $1)`, 18)
	checkExpr(t, pg.Plus(userAge, 1), `("users"."age" + $1)`, 1)
}

func TestDateTimeFunctions(t *testing.T) {
	checkExpr(t, pg.CurrentTimestamp(), `current_timestamp`)
	checkExpr(t, pg.DateTrunc("day", userID), `date_trunc($1, "users"."id")`, "day")
	checkExpr(t, pg.Extract("year", userID), `extract(year FROM "users"."id")`)
	checkExpr(t, pg.Day(7), `INTERVAL '7 day'`)
	checkExpr(t, pg.AtTimeZone(userID, "UTC"), `("users"."id" AT TIME ZONE $1)`, "UTC")
}

func TestJSONOperators(t *testing.T) {
	// Use a dedicated table so we don't mutate the shared `users` fixture.
	docsT := pg.NewTable("docs")
	pg.Add(docsT, pg.BigSerial("id").PrimaryKey())
	doc := pg.Add(docsT, pg.JSONB("payload"))

	checkExpr(t, pg.JSONGet(doc, "name"), `("docs"."payload" -> $1)`, "name")
	checkExpr(t, pg.JSONGetText(doc, "name"), `("docs"."payload" ->> $1)`, "name")
	checkExpr(t, pg.JSONBContains(doc, `{"k":1}`), `("docs"."payload" @> $1)`, `{"k":1}`)
	checkExpr(t, pg.JSONBHasKey(doc, "name"), `("docs"."payload" ? $1)`, "name")
	checkExpr(t, pg.JSONBBuildObject("k", 1), `jsonb_build_object($1, $2)`, "k", 1)
	checkExpr(t, pg.JSONBSet(doc, "{path,to}", `"v"`, true),
		`jsonb_set("docs"."payload", $1, $2, $3)`, "{path,to}", `"v"`, true)
}

func TestArrayOperators(t *testing.T) {
	tagsT := pg.NewTable("things")
	pg.Add(tagsT, pg.BigSerial("id").PrimaryKey())
	tags := pg.Add(tagsT, pg.Custom[string]("tags", "text[]"))
	checkExpr(t, pg.ArrayContains(tags, `{"a","b"}`),
		`("things"."tags" @> $1)`, `{"a","b"}`)
	checkExpr(t, pg.ArrayOverlaps(tags, `{"a"}`),
		`("things"."tags" && $1)`, `{"a"}`)
	checkExpr(t, pg.Any("foo", tags), `($1 = ANY("things"."tags"))`, "foo")
	checkExpr(t, pg.ArrayLit("a", "b"), `ARRAY[$1, $2]`, "a", "b")
	checkExpr(t, pg.Cardinality(tags), `cardinality("things"."tags")`)
}

// --- CTE, set ops, distinct on, window, subquery, cast, case ---------

func TestCTE(t *testing.T) {
	db := pg.New(nil)
	cte := pg.CTEDef("adults", db.Select(userID).From(users).Where(userAge.Gte(18)))
	q := db.Select(userID).
		FromExpr(cte.Ref()).
		With(cte)
	check(t, q,
		`WITH "adults" AS (SELECT "users"."id" FROM "users" WHERE ("users"."age" >= $1)) SELECT "users"."id" FROM "adults"`,
		int32(18),
	)
}

func TestUnion(t *testing.T) {
	db := pg.New(nil)
	a := db.Select(userID).From(users).Where(userAge.Gte(18))
	b := db.Select(userID).From(users).Where(userAge.Lt(13))
	q := a.UnionAll(b)
	check(t, q,
		`SELECT "users"."id" FROM "users" WHERE ("users"."age" >= $1) UNION ALL SELECT "users"."id" FROM "users" WHERE ("users"."age" < $2)`,
		int32(18), int32(13),
	)
}

func TestDistinctOn(t *testing.T) {
	db := pg.New(nil)
	q := db.Select(userID, userName).
		From(users).
		DistinctOn(userName).
		OrderBy(userName.Asc(), userID.Desc())
	check(t, q,
		`SELECT DISTINCT ON ("users"."name") "users"."id", "users"."name" FROM "users" ORDER BY "users"."name" ASC, "users"."id" DESC`,
	)
}

func TestWindowFunctions(t *testing.T) {
	expr := pg.Over(pg.RowNumber(),
		pg.WindowSpec().PartitionBy(userName).OrderBy(userID.Desc()))
	checkExpr(t, expr,
		`row_number() OVER (PARTITION BY "users"."name" ORDER BY "users"."id" DESC)`,
	)
	expr = pg.Over(pg.Lag(userAge, 1),
		pg.WindowSpec().OrderBy(userID.Asc()).Frame("ROWS BETWEEN 1 PRECEDING AND CURRENT ROW"))
	checkExpr(t, expr,
		`lag("users"."age", $1) OVER (ORDER BY "users"."id" ASC ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)`,
		1,
	)
}

func TestExistsSubquery(t *testing.T) {
	db := pg.New(nil)
	sub := db.Select(postID).From(posts).Where(postUserID.EqCol(userID))
	q := db.Select(userID).From(users).Where(pg.Exists(sub))
	check(t, q,
		`SELECT "users"."id" FROM "users" WHERE EXISTS (SELECT "posts"."id" FROM "posts" WHERE ("posts"."userId" = "users"."id"))`,
	)
}

func TestCastAndCase(t *testing.T) {
	checkExpr(t, pg.Cast(userAge, "text"), `("users"."age")::text`)
	checkExpr(t, pg.CastAs(userAge, "bigint"), `CAST("users"."age" AS bigint)`)
	expr := pg.Case().
		When(userAge.Lt(18), "minor").
		When(userAge.Lt(65), "adult").
		Else("senior").
		End()
	checkExpr(t, expr,
		`CASE WHEN ("users"."age" < $1) THEN $2 WHEN ("users"."age" < $3) THEN $4 ELSE $5 END`,
		int32(18), "minor", int32(65), "adult", "senior",
	)
}

func TestAggregateModifiers(t *testing.T) {
	checkExpr(t, pg.CountDistinct(userID), `count(DISTINCT "users"."id")`)
	checkExpr(t, pg.Filter(pg.CountAll(), userAge.Gte(18)),
		`count(*) FILTER (WHERE ("users"."age" >= $1))`, int32(18))
	checkExpr(t, pg.StringAgg(userName, ", "),
		`string_agg("users"."name", $1)`, ", ")
}
