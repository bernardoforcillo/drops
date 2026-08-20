package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
	"github.com/bernardoforcillo/drops/vector"
)

func openPG(t *testing.T) *pg.DB {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvPostgres)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return pg.New(stdlib.New(sqlDB))
}

func execPG(t *testing.T, db *pg.DB, e drops.Expression) {
	t.Helper()
	if _, err := db.ExecExpr(context.Background(), e); err != nil {
		text, args := drops.String(e)
		t.Fatalf("PostgreSQL rejected the statement: %v\n%s\nargs: %v", err, text, args)
	}
}

// dropPG removes a table at test end so a rerun starts clean.
func dropPG(t *testing.T, db *pg.DB, tbl *pg.Table) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.ExecExpr(context.Background(), pg.DropTableIfExists(tbl))
	})
}

// Every column type the dialect declares, in one table the server has
// to accept.
func TestPGEveryColumnTypeIsAccepted(t *testing.T) {
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "kitchen"))
	dropPG(t, db, tbl)

	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("a_text"))
	pg.Add(tbl, pg.Varchar("a_varchar", 255))
	pg.Add(tbl, pg.Char("a_char", 8))
	pg.Add(tbl, pg.SmallInt("a_smallint"))
	pg.Add(tbl, pg.Integer("a_integer"))
	pg.Add(tbl, pg.BigInt("a_bigint"))
	pg.Add(tbl, pg.SmallSerial("a_smallserial"))
	pg.Add(tbl, pg.Real("a_real"))
	pg.Add(tbl, pg.DoublePrecision("a_double"))
	pg.Add(tbl, pg.Numeric("a_numeric", 10, 2))
	pg.Add(tbl, pg.Boolean("a_bool"))
	pg.Add(tbl, pg.Date("a_date"))
	pg.Add(tbl, pg.Time("a_time"))
	pg.Add(tbl, pg.Timestamp("a_ts", false))
	pg.Add(tbl, pg.Timestamp("a_tstz", true))
	pg.Add(tbl, pg.Interval("an_interval"))
	pg.Add(tbl, pg.UUID("a_uuid"))
	pg.Add(tbl, pg.JSON("a_json"))
	pg.Add(tbl, pg.JSONB("a_jsonb"))
	pg.Add(tbl, pg.Bytea("a_bytea"))

	execPG(t, db, pg.CreateTable(tbl))
}

// The composite key whose DDL was invalid until the integration suite
// existed. This is the test that would have caught it.
func TestPGCompositePrimaryKeyIsAccepted(t *testing.T) {
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "memberships"))
	dropPG(t, db, tbl)

	orgID := pg.Add(tbl, pg.BigInt("orgId").PrimaryKey())
	userID := pg.Add(tbl, pg.BigInt("userId").PrimaryKey())
	role := pg.Add(tbl, pg.Text("role").NotNull())
	execPG(t, db, pg.CreateTable(tbl))

	type membership struct {
		OrgID  int64 `drop:"orgId"`
		UserID int64 `drop:"userId"`
		Role   string
	}
	ent := pg.NewEntity[membership](tbl)
	ctx := context.Background()

	m := membership{OrgID: 1, UserID: 2, Role: "admin"}
	if err := ent.Create(db, ctx, &m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := ent.Get(db, ctx, int64(1), int64(2))
	if err != nil {
		t.Fatalf("Get by composite key: %v", err)
	}
	if got.Role != "admin" {
		t.Errorf("got %+v", got)
	}

	got.Role = "member"
	if err := ent.Update(db, ctx, &got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	after, err := ent.Get(db, ctx, int64(1), int64(2))
	if err != nil {
		t.Fatal(err)
	}
	if after.Role != "member" {
		t.Errorf("Update did not persist: %+v", after)
	}

	if _, err := ent.Delete(db, ctx, int64(1), int64(2)); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, _, _ = orgID, userID, role
}

// Indexes, including the functional and partial forms, which is where
// the qualified-name bug lived.
func TestPGIndexesAreAccepted(t *testing.T) {
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "docs"))
	dropPG(t, db, tbl)

	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	title := pg.Add(tbl, pg.Text("title").NotNull())
	lang := pg.Add(tbl, pg.Text("lang"))
	execPG(t, db, pg.CreateTable(tbl))

	execPG(t, db, pg.CreateIndex(pg.NewIndex(integration.UniqueName(t, "i1"), tbl, title)))
	execPG(t, db, pg.CreateIndex(pg.NewIndex(integration.UniqueName(t, "i2"), tbl, title, lang).Unique()))
	execPG(t, db, pg.CreateIndex(pg.NewIndex(integration.UniqueName(t, "i3"), tbl, lang).Where(lang.IsNotNull())))
}

// The PostGIS helpers emitted every placeholder twice until recently.
// That was fixed by reading the code; this is the test that can
// actually confirm it, and it skips cleanly where PostGIS is absent.
func TestPGGeoHelpersAreAccepted(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS postgis"); err != nil {
		t.Skipf("PostGIS unavailable: %v", err)
	}

	tbl := pg.NewTable(integration.UniqueName(t, "drivers"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pos := pg.Add(tbl, pg.Custom[pg.Point]("position", "geography(Point,4326)"))
	execPG(t, db, pg.CreateTable(tbl))

	here := pg.Point{Lat: 41.9, Lon: 12.5}
	if _, err := db.Insert(tbl).Row(pos.Val(here)).Exec(ctx); err != nil {
		t.Fatalf("insert a point: %v", err)
	}

	// Each helper must survive being the SECOND expression in the
	// statement — the position where hand-written "$1" bound the wrong
	// parameter.
	box := pg.Box{SW: pg.Point{Lat: 41.8, Lon: 12.4}, NE: pg.Point{Lat: 42.0, Lon: 12.6}}
	for name, pred := range map[string]drops.Expression{
		"Within":       pg.And(id.Gt(0), pg.Within(pos, box)),
		"WithinRadius": pg.And(id.Gt(0), pg.WithinRadius(pos, here, 5000)),
	} {
		var n int64
		sel := db.Select(pg.CountAll()).From(tbl).Where(pred)
		if err := sel.One(ctx, &n); err != nil {
			text, args := drops.String(sel)
			t.Errorf("%s rejected: %v\n%s\nargs: %v", name, err, text, args)
		}
	}
	for name, order := range map[string]drops.Expression{
		"DistanceFrom": pg.DistanceFrom(pos, here),
		"NearestFrom":  pg.NearestFrom(pos, here),
	} {
		var out []struct{ ID int64 }
		sel := db.Select(id).From(tbl).Where(id.Gt(0)).OrderBy(order)
		if err := sel.All(ctx, &out); err != nil {
			text, args := drops.String(sel)
			t.Errorf("%s rejected: %v\n%s\nargs: %v", name, err, text, args)
		}
	}
}

// pgvector, through the portable vector.Store interface.
func TestPGVectorStoreAgainstTheEngine(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	if _, err := db.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		t.Skipf("pgvector unavailable: %v", err)
	}

	tbl := pg.NewTable(integration.UniqueName(t, "embeddings"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	emb := pg.Add(tbl, pg.Vector("embedding", 3))
	lang := pg.Add(tbl, pg.Text("lang"))
	meta := pg.Add(tbl, pg.JSONB("meta"))
	execPG(t, db, pg.CreateTable(tbl))

	rows := []struct {
		vec  []float32
		lang string
	}{
		{[]float32{1, 0, 0}, "it"},
		{[]float32{0.9, 0.1, 0}, "it"},
		{[]float32{0, 0, 1}, "en"},
	}
	for _, r := range rows {
		_, err := db.Insert(tbl).
			Row(emb.Expr(drops.Raw("'"+pg.FormatVector(r.vec)+"'::vector")), lang.Val(r.lang),
				meta.Val([]byte(`{"k":1}`))).
			Exec(ctx)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	store := pg.NewVectorStore(db, tbl, id, emb,
		pg.WithField("lang", lang),
		pg.WithPayloadColumn(meta))

	res, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).
		TopK(2).
		Metric(vector.Cosine).
		Where(vector.Eq("lang", "it")).
		WithPayload().
		Build())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("got %d hits, want the two Italian rows", len(res.Hits))
	}
	if res.Hits[0].Distance > res.Hits[1].Distance {
		t.Errorf("hits are not ordered nearest first: %v", res.Hits)
	}
	if res.Hits[0].Payload["k"] != float64(1) {
		t.Errorf("payload did not come back: %v", res.Hits[0].Payload)
	}

	// Every metric must at least render into SQL the server accepts.
	// L1 rides on pgvector's <+> operator, which arrived in 0.7.0; on
	// an older extension its absence is the server's news, not a
	// defect in the rendering.
	for _, m := range []vector.Metric{vector.Cosine, vector.L2, vector.InnerProduct, vector.L1} {
		_, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).Metric(m).TopK(1).Build())
		if err == nil {
			continue
		}
		if strings.Contains(err.Error(), "42883") {
			t.Logf("metric %s needs an operator this pgvector does not have: %v", m, err)
			continue
		}
		t.Errorf("metric %s rejected: %v", m, err)
	}

	// And the keyset cursor must walk the set without repeating.
	seen := map[any]int{}
	for cursor, page := "", 0; ; page++ {
		r, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).TopK(1).After(cursor).Build())
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, h := range r.Hits {
			seen[h.ID]++
		}
		if !r.HasMore || page > 5 {
			break
		}
		cursor = r.NextCursor
	}
	if len(seen) != 3 {
		t.Errorf("paged over %d distinct rows, want 3", len(seen))
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("row %v returned %d times", k, n)
		}
	}
}

// primaryKeyColumnsPG reads a table's PRIMARY KEY out of the
// catalogue, in key order. An empty result means the table has no
// primary key at all.
func primaryKeyColumnsPG(t *testing.T, db *pg.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT a.attname
		FROM pg_constraint c
		JOIN pg_class rel ON rel.oid = c.conrelid
		JOIN unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = rel.oid AND a.attnum = k.attnum
		WHERE c.contype = 'p' AND rel.relname = $1
		ORDER BY k.ord`, table)
	if err != nil {
		t.Fatalf("read primary key: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// A composite key declared on the table rather than on each column.
//
// drops accepts both spellings; before they were reconciled this one
// reached neither the DDL — the CREATE TABLE came out with no PRIMARY
// KEY clause at all — nor NewEntity, which panicked saying the table
// had no primary key.
func TestPGCompositePrimaryKeyDeclaredOnTheTable(t *testing.T) {
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "members"))
	dropPG(t, db, tbl)

	orgID := pg.Add(tbl, pg.BigInt("orgId").NotNull())
	userID := pg.Add(tbl, pg.BigInt("userId").NotNull())
	pg.Add(tbl, pg.Text("role").NotNull())
	tbl.PrimaryKey(orgID, userID)
	execPG(t, db, pg.CreateTable(tbl))

	got := primaryKeyColumnsPG(t, db, tbl.Name())
	want := []string{"orgId", "userId"}
	if len(got) != len(want) {
		t.Fatalf("primary key columns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("primary key columns = %v, want %v (order matters)", got, want)
		}
	}

	type member struct {
		OrgID  int64 `drop:"orgId"`
		UserID int64 `drop:"userId"`
		Role   string
	}
	ent := pg.NewEntity[member](tbl)
	if len(ent.PKs()) != 2 {
		t.Fatalf("Entity PKs = %d, want the two key columns", len(ent.PKs()))
	}
	ctx := context.Background()
	m := member{OrgID: 1, UserID: 2, Role: "admin"}
	if err := ent.Create(db, ctx, &m); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := ent.Get(db, ctx, int64(1), int64(2)); err != nil {
		t.Fatalf("Get by composite key: %v", err)
	}
	// The key is real: a second row with the same pair must be refused.
	dup := member{OrgID: 1, UserID: 2, Role: "member"}
	if err := ent.Create(db, ctx, &dup); err == nil {
		t.Error("the duplicate key was accepted; the PRIMARY KEY is not enforced")
	}
}

// The same key declared the other way must reach the catalogue too.
func TestPGCompositePrimaryKeyDeclaredOnTheColumns(t *testing.T) {
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "members"))
	dropPG(t, db, tbl)

	pg.Add(tbl, pg.BigInt("orgId").PrimaryKey())
	pg.Add(tbl, pg.BigInt("userId").PrimaryKey())
	execPG(t, db, pg.CreateTable(tbl))

	got := primaryKeyColumnsPG(t, db, tbl.Name())
	if len(got) != 2 || got[0] != "orgId" || got[1] != "userId" {
		t.Errorf("primary key columns = %v, want [orgId userId]", got)
	}
}

// COMMENT is a utility statement: PostgreSQL's grammar has no
// placeholder slot in it, so binding the text as a parameter made the
// statement unparseable.
func TestPGCommentsAreAccepted(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "commented"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	note := pg.Add(tbl, pg.Text("note"))
	execPG(t, db, pg.CreateTable(tbl))

	// The awkward text belongs in the test: a comment carrying a quote
	// must come back intact, not end the literal early.
	const tableComment = "everything we know, it's all here"
	execPG(t, db, pg.CommentOnTable(tbl, tableComment))
	execPG(t, db, pg.CommentOnColumn(note, "a free-form note"))

	var got string
	rows, err := db.Query(ctx,
		`SELECT obj_description(c.oid, 'pg_class') FROM pg_class c WHERE c.relname = $1`,
		tbl.Name())
	if err != nil {
		t.Fatalf("read comment: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != tableComment {
		t.Errorf("table comment = %q, want %q", got, tableComment)
	}
}

// An operator class belongs to the column it was set on. Stamping it
// on every column of a multi-column index is not a cosmetic slip:
// PostgreSQL rejects a class whose input type does not match.
func TestPGIndexOpClassAppliesToOneColumn(t *testing.T) {
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "opclass"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id"))
	name := pg.Add(tbl, pg.Text("name"))
	execPG(t, db, pg.CreateTable(tbl))

	// text_pattern_ops does not accept bigint, so the statement only
	// parses if the class stayed on the column it was declared for.
	idx := pg.NewIndex(integration.UniqueName(t, "opix"), tbl, id, name).
		OpClass(pg.VectorOpClass("text_pattern_ops"))
	execPG(t, db, pg.CreateIndex(idx))
}

// concat and its VARIADIC "any" siblings cannot resolve an untyped
// placeholder: the statement fails to parse with SQLSTATE 42P18.
func TestPGVariadicAnyFunctionsBindTypedParameters(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "concat"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	execPG(t, db, pg.CreateTable(tbl))
	if _, err := db.Insert(tbl).Row(name.Val("ada")).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for label, e := range map[string]drops.Expression{
		"Concat":           pg.Concat(name, " <", "boxed", ">"),
		"ConcatNonString":  pg.Concat(name, 1, 2.5, true),
		"ConcatWS":         pg.ConcatWS("-", name, "x"),
		"JSONBBuildObject": pg.JSONBBuildObject("name", name, "n", 1),
		"JSONBBuildArray":  pg.JSONBBuildArray(name, 1),
	} {
		var out string
		sel := db.Select(pg.Cast(e, "text")).From(tbl).Where(id.Gt(0))
		if err := sel.One(ctx, &out); err != nil {
			text, args := drops.String(sel)
			t.Errorf("%s rejected: %v\n%s\nargs: %v", label, err, text, args)
		}
	}
}

// A sequence name inside nextval's literal is parsed as an identifier,
// so a camelCase one has to be quoted there or the lookup is
// case-folded and fails.
func TestPGSequenceHelpersFindACamelCaseSequence(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	seq := integration.UniqueName(t, "orderSeq")
	execPG(t, db, pg.CreateSequenceIfNotExists(seq))
	t.Cleanup(func() { _, _ = db.ExecExpr(ctx, pg.DropSequenceIfExists(seq)) })

	tbl := pg.NewTable(integration.UniqueName(t, "seqUse"))
	dropPG(t, db, tbl)
	n := pg.Add(tbl, pg.BigInt("n"))
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).Row(n.Expr(pg.NextVal(seq))).Exec(ctx); err != nil {
		t.Fatalf("NextVal: %v", err)
	}
	var got int64
	if err := db.Select(pg.CurrVal(seq)).From(tbl).One(ctx, &got); err != nil {
		t.Fatalf("CurrVal: %v", err)
	}
	if err := db.Select(pg.SetVal(seq, 100)).From(tbl).One(ctx, &got); err != nil {
		t.Fatalf("SetVal: %v", err)
	}
	if got != 100 {
		t.Errorf("SetVal returned %d, want 100", got)
	}
}

// CREATE TYPE quotes the enum name, so the column definition has to
// quote it too — an unquoted camelCase type is case-folded and
// reported missing.
func TestPGEnumColumnTypeIsAccepted(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	status := pg.NewEnum(integration.UniqueName(t, "orderStatus"), "active", "pending")
	execPG(t, db, pg.CreateEnum(status))
	t.Cleanup(func() { _, _ = db.ExecExpr(ctx, pg.DropEnumIfExists(status.Name())) })

	tbl := pg.NewTable(integration.UniqueName(t, "orders"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	st := pg.Add(tbl, status.Col("status").NotNull())
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).Row(st.Val("active")).Exec(ctx); err != nil {
		t.Fatalf("insert an enum value: %v", err)
	}
}

// CreateTable has to carry every constraint that lives inside the
// parentheses; CreateTableWithIndexes adds the statements that do not.
func TestPGCreateTableCarriesTableLevelConstraints(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	users := pg.NewTable(integration.UniqueName(t, "cUsers"))
	dropPG(t, db, users)
	uid := pg.Add(users, pg.BigInt("id").PrimaryKey())
	utenant := pg.Add(users, pg.BigInt("tenantId").NotNull())
	uname := pg.Add(users, pg.Text("name").NotNull())
	uage := pg.Add(users, pg.Integer("age").NotNull())
	users.AddUnique("cUsersTenantName", utenant, uname)
	users.AddCheck("cUsersAgeSane", `"age" >= 0`)
	users.AddUnique("cUsersTenantId", utenant, uid)
	users.AddIndex(pg.NewIndex(integration.UniqueName(t, "cUsersNameIdx"), users, uname))

	orders := pg.NewTable(integration.UniqueName(t, "cOrders"))
	dropPG(t, db, orders)
	pg.Add(orders, pg.BigSerial("id").PrimaryKey())
	otenant := pg.Add(orders, pg.BigInt("tenantId").NotNull())
	ouser := pg.Add(orders, pg.BigInt("userId").NotNull())
	orders.ForeignKeyN([]pg.ColRef{otenant, ouser}, users, []pg.ColRef{utenant, uid})

	for _, e := range pg.CreateTableWithIndexes(users) {
		execPG(t, db, e)
	}
	for _, e := range pg.CreateTableWithIndexes(orders) {
		execPG(t, db, e)
	}

	if _, err := db.Insert(users).
		Row(uid.Val(1), utenant.Val(10), uname.Val("ada"), uage.Val(30)).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Insert(users).
		Row(uid.Val(2), utenant.Val(10), uname.Val("ada"), uage.Val(30)).
		Exec(ctx); err == nil {
		t.Error("the composite UNIQUE constraint is not enforced")
	}
	if _, err := db.Insert(users).
		Row(uid.Val(3), utenant.Val(10), uname.Val("grace"), uage.Val(-1)).
		Exec(ctx); err == nil {
		t.Error("the CHECK constraint is not enforced")
	}
	if _, err := db.Insert(orders).Row(otenant.Val(99), ouser.Val(1)).Exec(ctx); err == nil {
		t.Error("the multi-column FOREIGN KEY is not enforced")
	}
	if _, err := db.Insert(orders).Row(otenant.Val(10), ouser.Val(1)).Exec(ctx); err != nil {
		t.Errorf("a valid multi-column FK row was refused: %v", err)
	}
}

// Push has to be safe to re-run: everything the schema layer can
// declare must be read back by Introspect, or the second push tries to
// create what is already there.
func TestPGPushIsIdempotent(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	users := pg.NewSchemaTable(schemaName, "users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	uname := pg.Add(users, pg.Text("name").NotNull())
	uemail := pg.Add(users, pg.Text("email").Unique())
	uage := pg.Add(users, pg.Integer("age"))
	users.AddUnique("usersNameEmail", uname, uemail)
	users.AddCheck("usersAgeSane", `"age" >= 0`)
	users.AddIndex(pg.NewIndex("usersNameIdx", users, uname))
	users.AddIndex(pg.NewIndex("usersAgeIdx", users, uage).Concurrently())

	posts := pg.NewSchemaTable(schemaName, "posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	author := pg.Add(posts, pg.BigInt("authorId"))
	posts.ForeignKey(author.Column, uid.Column)

	sch := pg.NewSchema(users, posts)
	opts := pg.PushOptions{Schema: schemaName}

	first, err := pg.Push(ctx, db, sch, opts)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if !first.Applied {
		t.Fatal("the first push applied nothing")
	}

	second, err := pg.Push(ctx, db, sch, opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("the second push is not a no-op:\n%s", strings.Join(second.Statements, "\n"))
	}
}

// pushScratchPG opens a connection pinned to one physical connection —
// so the SET search_path below governs every later statement — and
// gives it an empty schema of its own to push into.
func pushScratchPG(t *testing.T) (*pg.DB, string) {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvPostgres)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	db := pg.New(stdlib.New(sqlDB))
	ctx := context.Background()

	schemaName := "push_" + integration.UniqueName(t, "s")
	if len(schemaName) > 60 {
		schemaName = schemaName[:60]
	}
	for _, s := range []string{
		`DROP SCHEMA IF EXISTS "` + schemaName + `" CASCADE`,
		`CREATE SCHEMA "` + schemaName + `"`,
		`SET search_path TO "` + schemaName + `"`,
	} {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DROP SCHEMA IF EXISTS "`+schemaName+`" CASCADE`)
	})
	return db, schemaName
}

// A composite PRIMARY KEY has to reach Push, not just CreateTable.
//
// Both spellings are exercised because they used to travel different
// routes into the snapshot: BuildSnapshot read only the table-level
// declaration, so a key marked on each column reached the DDL and the
// Entity but never Diff — the first push created a table with two
// inline PRIMARY KEY clauses, which PostgreSQL refuses outright.
func TestPGPushSeesACompositePrimaryKey(t *testing.T) {
	for _, tc := range []struct {
		name    string
		declare func(*pg.Table)
	}{
		{"per column", func(tbl *pg.Table) {
			pg.Add(tbl, pg.BigInt("orgId").PrimaryKey())
			pg.Add(tbl, pg.BigInt("userId").PrimaryKey())
			pg.Add(tbl, pg.Text("role").NotNull())
		}},
		{"on the table", func(tbl *pg.Table) {
			org := pg.Add(tbl, pg.BigInt("orgId"))
			user := pg.Add(tbl, pg.BigInt("userId"))
			pg.Add(tbl, pg.Text("role").NotNull())
			tbl.PrimaryKey(org, user)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, schemaName := pushScratchPG(t)
			ctx := context.Background()

			members := pg.NewSchemaTable(schemaName, "members")
			tc.declare(members)
			sch := pg.NewSchema(members)
			opts := pg.PushOptions{Schema: schemaName}

			first, err := pg.Push(ctx, db, sch, opts)
			if err != nil {
				t.Fatalf("first push: %v", err)
			}
			if !first.Applied {
				t.Fatal("the first push applied nothing")
			}
			got := primaryKeyColumnsPG(t, db, "members")
			if len(got) != 2 || got[0] != "orgId" || got[1] != "userId" {
				t.Fatalf("primary key columns = %v, want [orgId userId]", got)
			}

			second, err := pg.Push(ctx, db, sch, opts)
			if err != nil {
				t.Fatalf("second push: %v", err)
			}
			if len(second.Statements) != 0 {
				t.Errorf("the second push is not a no-op:\n%s", strings.Join(second.Statements, "\n"))
			}
		})
	}
}

// indexDefPG returns pg_get_indexdef for one index in one schema, or
// "" when the index is not there. The reconstructed DDL is the whole
// point: it shows the INCLUDE list and the partial predicate exactly
// as the server holds them, not as drops hoped it wrote them.
func indexDefPG(t *testing.T, db *pg.DB, schema, name string) string {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT pg_get_indexdef(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind = 'i' AND n.nspname = $1 AND c.relname = $2`, schema, name)
	if err != nil {
		t.Fatalf("read index definition: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return ""
	}
	var def string
	if err := rows.Scan(&def); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return def
}

// noticeFor returns the first notice with the given rule, or the zero
// value when the push reported none.
func noticeFor(notices []pg.SchemaNotice, rule string) pg.SchemaNotice {
	for _, n := range notices {
		if n.Rule == rule {
			return n
		}
	}
	return pg.SchemaNotice{}
}

// An index the Go schema never declared is the one object most often
// created outside it — by a DBA under load, by an extension, by
// another migration tool. Since Introspect started reporting
// standalone indexes, Diff has emitted a DROP INDEX for every one of
// them. Push withholds it and says so; the caller can opt back in.
func TestPGPushLeavesAnUndeclaredIndexAlone(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	users := pg.NewSchemaTable(schemaName, "users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("email").NotNull())
	sch := pg.NewSchema(users)
	opts := pg.PushOptions{Schema: schemaName}

	if _, err := pg.Push(ctx, db, sch, opts); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Somebody adds an index by hand, at 3am, for a query that was
	// timing out. It is in no Go file.
	if _, err := db.Exec(ctx, `CREATE INDEX "usersEmailIdx" ON "`+schemaName+`"."users" ("email")`); err != nil {
		t.Fatalf("hand-made index: %v", err)
	}

	second, err := pg.Push(ctx, db, sch, opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	for _, s := range second.Statements {
		if strings.Contains(s, "DROP INDEX") {
			t.Errorf("Push dropped an index it did not declare: %s", s)
		}
	}
	notice := noticeFor(second.Notices, "unmanaged-index")
	if notice.Object != "usersEmailIdx" {
		t.Fatalf("no unmanaged-index notice for the hand-made index, got %v", second.Notices)
	}
	if !strings.Contains(notice.SQL, "DROP INDEX") {
		t.Errorf("the notice must carry the statement it withheld, got %q", notice.SQL)
	}
	if def := indexDefPG(t, db, schemaName, "usersEmailIdx"); def == "" {
		t.Fatal("the hand-made index is gone")
	}

	// The safety analyser has to have a name for what was withheld,
	// or "make it visible" is only a promise.
	warnings := pg.AnalyzeStatements([]string{notice.SQL})
	if len(warnings) == 0 || warnings[0].Rule != "drop-index" {
		t.Errorf("AnalyzeStatements does not classify %q: %v", notice.SQL, warnings)
	}

	// Opting in performs the drop.
	optIn := opts
	optIn.DropUnmanagedIndexes = true
	third, err := pg.Push(ctx, db, sch, optIn)
	if err != nil {
		t.Fatalf("opt-in push: %v", err)
	}
	if !third.Applied {
		t.Fatalf("DropUnmanagedIndexes applied nothing: %v", third.Statements)
	}
	if def := indexDefPG(t, db, schemaName, "usersEmailIdx"); def != "" {
		t.Errorf("DropUnmanagedIndexes left the index in place: %s", def)
	}
}

// A covering index and a partial one have to survive the round trip
// through the snapshot. Both halves used to be dropped on the floor:
// the INCLUDE list was never recorded, so the index was created plain
// and the push looked idempotent because neither side knew the
// difference, and the predicate was never recorded either.
func TestPGPushCarriesIncludeAndPredicate(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	build := func(minAge int32) *pg.Schema {
		users := pg.NewSchemaTable(schemaName, "users")
		pg.Add(users, pg.BigSerial("id").PrimaryKey())
		email := pg.Add(users, pg.Text("email").NotNull())
		name := pg.Add(users, pg.Text("name").NotNull())
		age := pg.Add(users, pg.Integer("age").NotNull())
		users.AddIndex(pg.NewIndex("usersCoverIdx", users, email).Include(name.Column))
		users.AddIndex(pg.NewIndex("usersAdultIdx", users, name).Where(age.Gte(minAge)))
		return pg.NewSchema(users)
	}
	opts := pg.PushOptions{Schema: schemaName}

	if _, err := pg.Push(ctx, db, build(18), opts); err != nil {
		t.Fatalf("first push: %v", err)
	}
	cover := indexDefPG(t, db, schemaName, "usersCoverIdx")
	if !strings.Contains(cover, "INCLUDE (name)") {
		t.Errorf("the covering index lost its INCLUDE list: %s", cover)
	}
	adult := indexDefPG(t, db, schemaName, "usersAdultIdx")
	if !strings.Contains(adult, "WHERE (age >= 18)") {
		t.Errorf("the partial index lost its predicate: %s", adult)
	}

	second, err := pg.Push(ctx, db, build(18), opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("the second push is not a no-op:\n%s", strings.Join(second.Statements, "\n"))
	}

	// Tightening the predicate has to reach the server. The
	// comparison was loosened to ignore Where entirely, which was
	// only safe while the snapshot carried none.
	third, err := pg.Push(ctx, db, build(21), opts)
	if err != nil {
		t.Fatalf("predicate change push: %v", err)
	}
	if !third.Applied {
		t.Fatalf("a changed index predicate produced no migration: %v", third.Notices)
	}
	if adult := indexDefPG(t, db, schemaName, "usersAdultIdx"); !strings.Contains(adult, "WHERE (age >= 21)") {
		t.Errorf("the predicate change did not reach the index: %s", adult)
	}
	fourth, err := pg.Push(ctx, db, build(21), opts)
	if err != nil {
		t.Fatalf("fourth push: %v", err)
	}
	if len(fourth.Statements) != 0 {
		t.Errorf("the push after the predicate change is not a no-op:\n%s", strings.Join(fourth.Statements, "\n"))
	}
}

// A CHECK constraint was compared by name alone, so tightening its
// expression under an unchanged name produced no migration at all.
// Comparing the text needs the two sides spelled alike, which is what
// the server is asked for: pg_get_constraintdef reports `"age" >= 0`
// as `(age >= 0)`, and no amount of tidying in Go reproduces that.
func TestPGPushSeesAChangedCheckExpression(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	build := func(expr string) *pg.Schema {
		users := pg.NewSchemaTable(schemaName, "users")
		pg.Add(users, pg.BigSerial("id").PrimaryKey())
		pg.Add(users, pg.Integer("age").NotNull())
		users.AddCheck("usersAgeSane", expr)
		return pg.NewSchema(users)
	}
	opts := pg.PushOptions{Schema: schemaName}

	if _, err := pg.Push(ctx, db, build(`"age" >= 0`), opts); err != nil {
		t.Fatalf("first push: %v", err)
	}
	second, err := pg.Push(ctx, db, build(`"age" >= 0`), opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("the second push is not a no-op:\n%s", strings.Join(second.Statements, "\n"))
	}

	third, err := pg.Push(ctx, db, build(`"age" >= 18`), opts)
	if err != nil {
		t.Fatalf("tightened push: %v", err)
	}
	if !third.Applied {
		t.Fatalf("a changed CHECK expression produced no migration: %v", third.Notices)
	}
	// The server is the judge of whether the constraint changed.
	if _, err := db.Exec(ctx, `INSERT INTO "`+schemaName+`"."users" ("age") VALUES (5)`); err == nil {
		t.Error("the tightened CHECK is not enforced")
	}
	fourth, err := pg.Push(ctx, db, build(`"age" >= 18`), opts)
	if err != nil {
		t.Fatalf("fourth push: %v", err)
	}
	if len(fourth.Statements) != 0 {
		t.Errorf("the push after the CHECK change is not a no-op:\n%s", strings.Join(fourth.Statements, "\n"))
	}
}

// When the server will not parse a declared expression there is
// nothing to compare against, so Push leaves the constraint as the
// database has it and says which one it could not check — rather than
// dropping and re-adding it on every run because the two spellings
// will never match.
//
// The good constraint alongside it has to keep working: each probe
// runs under its own savepoint, so one failure does not poison the
// transaction the rest of them share.
func TestPGPushReportsAnUnparseableCheck(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	build := func(broken string) *pg.Schema {
		users := pg.NewSchemaTable(schemaName, "users")
		pg.Add(users, pg.BigSerial("id").PrimaryKey())
		pg.Add(users, pg.Integer("age").NotNull())
		pg.Add(users, pg.Integer("score").NotNull())
		users.AddCheck("usersAgeSane", broken)
		users.AddCheck("usersScoreSane", `"score" >= 0`)
		return pg.NewSchema(users)
	}
	opts := pg.PushOptions{Schema: schemaName}

	if _, err := pg.Push(ctx, db, build(`"age" >= 0`), opts); err != nil {
		t.Fatalf("first push: %v", err)
	}

	second, err := pg.Push(ctx, db, build(`"age" >>> 0`), opts)
	if err != nil {
		t.Fatalf("push with an unparseable check: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("Push acted on an expression it could not check:\n%s", strings.Join(second.Statements, "\n"))
	}
	notice := noticeFor(second.Notices, "check-not-normalised")
	if notice.Object != "usersAgeSane" {
		t.Fatalf("no notice for the unparseable check, got %v", second.Notices)
	}
	// The other constraint went through the same transaction and must
	// still compare clean.
	if n := noticeFor(second.Notices, "check-not-normalised"); n.Object == "usersScoreSane" {
		t.Error("a valid constraint was reported unparseable")
	}
}

// A truncated index is worse than no index: it wears the declared
// name, so both sides agree about it for ever.
//
// An index element that is an expression cannot be described by the
// snapshot. When every element was one, Push said so and created
// nothing. When only some were, the snapshot kept the columns it
// could read — and Push created a different index under the declared
// name, silently, idempotently, with no notice.
func TestPGPushDoesNotShipATruncatedIndex(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	build := func() *pg.Schema {
		users := pg.NewSchemaTable(schemaName, "users")
		pg.Add(users, pg.BigSerial("id").PrimaryKey())
		email := pg.Add(users, pg.Text("email").NotNull())
		name := pg.Add(users, pg.Text("name").NotNull())
		users.AddIndex(pg.NewIndex("usersMixedIdx", users, name, pg.Lower(email)))
		return pg.NewSchema(users)
	}
	opts := pg.PushOptions{Schema: schemaName}

	first, err := pg.Push(ctx, db, build(), opts)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	for _, s := range first.Statements {
		if strings.Contains(s, "usersMixedIdx") {
			t.Errorf("Push created an index it cannot describe: %s", s)
		}
	}
	if n := noticeFor(first.Notices, "unrepresentable-index"); n.Object != "usersMixedIdx" {
		t.Fatalf("no unrepresentable-index notice, got %v", first.Notices)
	}
	if def := indexDefPG(t, db, schemaName, "usersMixedIdx"); def != "" {
		t.Fatalf("a truncated index reached the server: %s", def)
	}

	// Declared with pg.CreateIndex, as the notice says to. Push has to
	// leave it alone from then on: the catalogue reports one named
	// column and one expression, and reading only the named one would
	// have the next push drop an index the schema does declare.
	create, _ := drops.StringWithDialect(pg.Dialect, pg.CreateIndex(
		func() *pg.Index {
			users := pg.NewSchemaTable(schemaName, "users")
			email := pg.Add(users, pg.Text("email").NotNull())
			name := pg.Add(users, pg.Text("name").NotNull())
			return pg.NewIndex("usersMixedIdx", users, name, pg.Lower(email))
		}()))
	if _, err := db.Exec(ctx, create); err != nil {
		t.Fatalf("%s: %v", create, err)
	}

	second, err := pg.Push(ctx, db, build(), opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("the push after the index was created by hand is not a no-op:\n%s",
			strings.Join(second.Statements, "\n"))
	}
	if def := indexDefPG(t, db, schemaName, "usersMixedIdx"); !strings.Contains(def, "lower") {
		t.Errorf("the functional index did not survive the push: %q", def)
	}
}

// The probe is DDL against a real table, so it waits for an ACCESS
// EXCLUSIVE lock and fails when it cannot have one. That failure says
// nothing about the expression, and reporting it as "the server would
// not parse it" left the declared expression compared against
// whatever the database already had — so a tightened CHECK did not
// ship, on exactly the busy system where the lock wait happened, and
// the push reported success.
func TestPGPushFailsWhenTheProbeCannotTakeItsLock(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	build := func(expr string) *pg.Schema {
		users := pg.NewSchemaTable(schemaName, "users")
		pg.Add(users, pg.BigSerial("id").PrimaryKey())
		pg.Add(users, pg.Integer("age").NotNull())
		users.AddCheck("usersAgeSane", expr)
		return pg.NewSchema(users)
	}
	opts := pg.PushOptions{Schema: schemaName}
	if _, err := pg.Push(ctx, db, build(`"age" >= 0`), opts); err != nil {
		t.Fatalf("first push: %v", err)
	}

	// An ordinary reader, on its own connection, holding the lock a
	// SELECT holds.
	blocker, err := sql.Open("pgx", integration.DSN(t, integration.EnvPostgres))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = blocker.Close() }()
	tx, err := blocker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`LOCK TABLE "`+schemaName+`"."users" IN ACCESS SHARE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	if _, err := db.Exec(ctx, `SET lock_timeout = '250ms'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	defer func() { _, _ = db.Exec(context.Background(), `SET lock_timeout = 0`) }()

	res, err := pg.Push(ctx, db, build(`"age" >= 18`), opts)
	if err == nil {
		t.Fatalf("a push whose probe timed out reported success: statements=%v notices=%v",
			res.Statements, res.Notices)
	}
	if !strings.Contains(err.Error(), "55P03") {
		t.Errorf("the lock timeout was not reported as one: %v", err)
	}
}

// Load's fromVersion is exclusive, and the number it takes is the same
// number Append's expectedVersion and LatestVersion speak: the version
// you already hold, -1 when you hold none. The doc used to say 0 read
// from the beginning, and 0 is the first event's own version, so every
// caller who believed it lost event 0 of every stream — silently, since
// what came back was still an ordered run of real events, just missing
// the one that created the aggregate.
//
// Three events on a fresh stream leave no room for that: each starting
// value has exactly one right answer and the test names all of them.
// The predicate is the server's to evaluate, not the renderer's, so
// this belongs here.
func TestPGEventStoreLoadIsExclusiveOnFromVersion(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "ev")
	tbl := pg.NewEventStoreTable(name)
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	store := pg.NewEventStore(db, name)
	if err := db.InTx(ctx, func(tx *pg.DB) error {
		return store.Append(tx, ctx, "cart", "cart-1", -1,
			pg.EventInput{Type: "a"},
			pg.EventInput{Type: "b"},
			pg.EventInput{Type: "c"})
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}

	for _, tc := range []struct {
		from int64
		want string
	}{
		{-1, "a,b,c"},
		{0, "b,c"},
		{1, "c"},
		{2, ""},
	} {
		got, err := store.Load(ctx, "cart", "cart-1", tc.from)
		if err != nil {
			t.Fatalf("Load(%d): %v", tc.from, err)
		}
		var types []string
		for _, ev := range got {
			types = append(types, ev.Type)
		}
		if list := strings.Join(types, ","); list != tc.want {
			t.Errorf("Load(%d) = [%s], want [%s]", tc.from, list, tc.want)
		}
		// The first row returned has to be the version after
		// fromVersion, or "exclusive" means something else.
		if len(got) > 0 && got[0].Version != tc.from+1 {
			t.Errorf("Load(%d) starts at version %d, want %d", tc.from, got[0].Version, tc.from+1)
		}
	}

	// The two ends of the convention: LatestVersion is a resume cursor
	// that leaves nothing to apply, and it is also the expected version
	// the next Append wants.
	head, err := store.LatestVersion(ctx, "cart", "cart-1")
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if head != 2 {
		t.Fatalf("LatestVersion after three events = %d, want 2", head)
	}
	if rest, err := store.Load(ctx, "cart", "cart-1", head); err != nil || len(rest) != 0 {
		t.Errorf("Load(LatestVersion) = %+v (err %v), want nothing left to apply", rest, err)
	}
	if err := db.InTx(ctx, func(tx *pg.DB) error {
		return store.Append(tx, ctx, "cart", "cart-1", head, pg.EventInput{Type: "d"})
	}); err != nil {
		t.Errorf("Append at LatestVersion: %v", err)
	}
	if tail, err := store.Load(ctx, "cart", "cart-1", head); err != nil || len(tail) != 1 || tail[0].Type != "d" {
		t.Errorf("Load(%d) after appending = %+v (err %v), want only d", head, tail, err)
	}
}

// ----------------------------------------------------------------------
// Table.As — the alias renames a reference and changes nothing else.
// ----------------------------------------------------------------------

// aliasCol re-wraps a type-erased column as a typed handle. Table.Col
// answers with *pg.Column, and every value binding lives on *pg.Col[T],
// so this is the seam a caller goes through to bind a value to a
// column reached off an alias.
func aliasCol[T any](c *pg.Column) *pg.Col[T] { return &pg.Col[T]{Column: c} }

// Both sides of a self-join have to be addressable at once. If the
// alias hands back the un-aliased column handles, the join condition
// compares the table with itself and PostgreSQL answers with every
// row's own manager — or with 42P01 when the FROM entry is the alias
// alone.
func TestPGSelfJoinResolvesThroughTheAlias(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "staff"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	managerID := pg.Add(tbl, pg.BigInt("managerId"))
	execPG(t, db, pg.CreateTable(tbl))

	for _, r := range []struct {
		id      int64
		name    string
		manager int64
	}{{1, "Ada", 0}, {2, "Grace", 1}, {3, "Alan", 1}} {
		row := []pg.ColumnValue{id.Val(r.id), name.Val(r.name)}
		if r.manager != 0 {
			row = append(row, managerID.Val(r.manager))
		}
		if _, err := db.Insert(tbl).Row(row...).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
	}

	mgr := tbl.As("mgr")
	var rows []struct {
		Staff   string `drop:"staff"`
		Manager string `drop:"manager"`
	}
	err := db.Select(name.As("staff"), mgr.Col("name").As("manager")).
		From(tbl).
		Join(mgr, pg.Eq(managerID, mgr.Col("id"))).
		OrderBy(name.Asc()).
		All(ctx, &rows)
	if err != nil {
		t.Fatalf("self-join: %v", err)
	}
	want := map[string]string{"Alan": "Ada", "Grace": "Ada"}
	if len(rows) != len(want) {
		t.Fatalf("self-join returned %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for _, r := range rows {
		if want[r.Staff] != r.Manager {
			t.Errorf("%s reports to %q, want %q", r.Staff, r.Manager, want[r.Staff])
		}
	}
}

// The regression the ClickHouse dialect shipped: a value bound through
// an aliased handle matched no column in the INSERT's list, so the row
// rendered as all-DEFAULT. PostgreSQL accepts such a statement — on a
// nullable column with no DEFAULT clause the row lands as NULLs and
// nothing anywhere says so. The check has to be the stored data.
func TestPGRowBoundThroughAnAliasStoresItsValues(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_bind"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name"))
	org := pg.Add(tbl, pg.Text("org"))
	execPG(t, db, pg.CreateTable(tbl))

	u := tbl.As("u")
	// The first row fixes the column list from the declared handles;
	// the second binds the same columns through the alias.
	if _, err := db.Insert(tbl).
		Row(name.Val("declared"), org.Val("first")).
		Row(aliasCol[string](u.Col("name")).Val("aliased"),
			aliasCol[string](u.Col("org")).Val("second")).
		Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var got []struct {
		Name string `drop:"name"`
		Org  string `drop:"org"`
	}
	if err := db.Select(name, org).From(tbl).OrderBy(id.Asc()).All(ctx, &got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("stored %d rows, want 2: %+v", len(got), got)
	}
	if got[1].Name != "aliased" || got[1].Org != "second" {
		t.Errorf("the aliased row stored %+v, want {aliased second}", got[1])
	}
}

// A hook closes over the handle it was declared with — every shipped
// mixin closes over the package-level one. When the caller binds the
// same column through an alias the hook has to see it as already
// bound: PostgreSQL rejects a column named twice in an INSERT with
// 42701 and a column assigned twice in an UPDATE with 42601, so this
// one is loud, but it inverts the documented rule that user-supplied
// values win.
func TestPGAliasedBindingDoesNotDuplicateAHookColumn(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_hook"))
	dropPG(t, db, tbl)

	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	stamps := &pg.TimestampsMixin{}
	pg.ApplyMixins(tbl, stamps)
	execPG(t, db, pg.CreateTable(tbl))

	u := tbl.As("u")
	fixed := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	// The mixin's UPDATE hook wants to bump updatedAt; the caller sets
	// it explicitly through the alias.
	if _, err := db.Insert(tbl).Row(name.Val("a")).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Update(tbl).
		Set(name.Val("b")).
		Set(aliasCol[time.Time](u.Col("updatedAt")).Val(fixed)).
		Exec(ctx); err != nil {
		t.Fatalf("update with an aliased updatedAt binding: %v", err)
	}

	var back []struct {
		Name      string    `drop:"name"`
		UpdatedAt time.Time `drop:"updatedAt"`
	}
	if err := db.Select(name, stamps.Cols.UpdatedAt).From(tbl).All(ctx, &back); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("expected one row, got %d", len(back))
	}
	if !back[0].UpdatedAt.Equal(fixed) {
		t.Errorf("hook overrode the caller's value: updatedAt = %v, want %v",
			back[0].UpdatedAt, fixed)
	}
}

// The key columns and the column definitions come from two different
// places on the Table, and an alias is what first makes them two
// different handles. A CREATE TABLE that loses its PRIMARY KEY is
// accepted by the server and only shows up later, as duplicate rows.
func TestPGAliasedTableKeepsItsPrimaryKey(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_ddl"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	tbl.PrimaryKey(id)

	// The DDL is rendered from the alias; the statement itself names
	// the un-aliased table, since PostgreSQL takes no AS clause here.
	execPG(t, db, pg.CreateTable(tbl.As("u")))

	if _, err := db.Insert(tbl).Row(id.Val(1), name.Val("first")).Exec(ctx); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := db.Insert(tbl).Row(id.Val(1), name.Val("again")).Exec(ctx)
	if err == nil {
		t.Fatal("the aliased CREATE TABLE lost its PRIMARY KEY: a duplicate key was accepted")
	}
	if !strings.Contains(err.Error(), "23505") {
		t.Errorf("expected a unique violation (23505), got: %v", err)
	}
}

type aliasEmployee struct {
	TenantID int64  `drop:"tenantId"`
	ID       int64  `drop:"id"`
	Name     string `drop:"name"`
}

// An Entity built on an alias has to keep its key: the key columns are
// omitted from the SET list, and the predicate qualifies with the
// alias — the only relation an "UPDATE t AS u" puts in scope.
func TestPGEntityOnAnAliasRoundTrips(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_entity"))
	dropPG(t, db, tbl)

	tenantID := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	pg.Add(tbl, pg.Text("name").NotNull())
	tbl.PrimaryKey(tenantID, id)
	execPG(t, db, pg.CreateTable(tbl))

	ent := pg.NewEntity[aliasEmployee](tbl.As("u"))
	row := aliasEmployee{TenantID: 7, ID: 1, Name: "Ada"}
	if err := ent.Create(db, ctx, &row); err != nil {
		t.Fatalf("Create through the alias: %v", err)
	}
	row.Name = "Ada L."
	if err := ent.Update(db, ctx, &row); err != nil {
		t.Fatalf("Update through the alias: %v", err)
	}
	got, err := ent.Get(db, ctx, int64(7), int64(1))
	if err != nil {
		t.Fatalf("Get through the alias: %v", err)
	}
	if got.Name != "Ada L." {
		t.Errorf("Get returned %+v, want the updated name", got)
	}
}

// The tenant axis can be named with an alias handle, since
// ScopeByTenant takes a ColRef. It used to panic at declaration time;
// the guard it installs still has to qualify with the relation the
// entity actually queries.
func TestPGScopeByTenantThroughAnAliasHandle(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_tenant"))
	dropPG(t, db, tbl)

	tenantID := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	tbl.PrimaryKey(tenantID, id)
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).
		Row(tenantID.Val(int64(7)), id.Val(int64(1)), name.Val("mine")).
		Row(tenantID.Val(int64(8)), id.Val(int64(1)), name.Val("theirs")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ent := pg.NewEntity[aliasEmployee](tbl).
		ScopeByTenant(tbl.As("u").Col("tenantId"))

	got, err := ent.Get(db, pg.WithTenant(ctx, int64(7)), int64(7), int64(1))
	if err != nil {
		t.Fatalf("Get under tenant 7: %v", err)
	}
	if got.Name != "mine" {
		t.Errorf("Get returned %+v, want the tenant-7 row", got)
	}
	if _, err := ent.Get(db, pg.WithTenant(ctx, int64(9)), int64(7), int64(1)); err == nil {
		t.Error("the tenant guard did not exclude another tenant's row")
	}
}

// The limitation As's doc comment states, pinned against the server
// that enforces it. A default filter is an already-built expression
// closed over the handles it was given, so aliasing the table does not
// move it: SoftDeleteMixin's guard goes on naming the un-aliased
// table, and a SELECT whose only FROM entry is the alias cannot
// resolve it. Rewriting the filter would mean rebuilding arbitrary
// expressions, so the documented route out is Unscoped plus an
// explicit predicate — which has to keep working, or the limitation
// has no exit.
func TestPGDefaultFilterIsNotRewrittenByAnAlias(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_softdel"))
	dropPG(t, db, tbl)

	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	pg.ApplyMixins(tbl, &pg.SoftDeleteMixin{})
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).Row(name.Val("live")).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	u := tbl.As("u")
	var rows []struct {
		Name string `drop:"name"`
	}
	err := db.Select(u.Col("name")).From(u).All(ctx, &rows)
	if err == nil {
		t.Fatal("a scoped table aliased into a bare SELECT should not resolve its guard")
	}
	if !strings.Contains(err.Error(), "42P01") {
		t.Errorf("expected 42P01 from the un-rewritten guard, got: %v", err)
	}

	// The documented way through: opt out of the guard and restate it
	// from the alias's own handles.
	rows = nil
	if err := db.Select(u.Col("name")).
		From(u).
		Unscoped().
		Where(pg.IsNull(u.Col("deletedAt"))).
		All(ctx, &rows); err != nil {
		t.Fatalf("Unscoped with an explicit predicate: %v", err)
	}
	if len(rows) != 1 || rows[0].Name != "live" {
		t.Errorf("got %+v, want the one live row", rows)
	}
}

type aliasBoss struct {
	ID      int64         `drop:"id"`
	Name    string        `drop:"name"`
	Reports []aliasReport `dropRel:"reports"`
}

type aliasReport struct {
	ID     int64  `drop:"id"`
	Name   string `drop:"name"`
	BossID int64  `drop:"bossId"`
}

// A relation's near side — the end that belongs to this table — moves
// to the alias with the columns, so an eager load through the alias
// joins on the instance the query actually names. The far side stays
// put: it belongs to the other table, and on a self-referential
// relation it is the un-aliased half of the edge.
func TestPGEagerLoadThroughAnAlias(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_tree"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	bossID := pg.Add(tbl, pg.BigInt("bossId"))
	execPG(t, db, pg.CreateTable(tbl))
	pg.NewRelations(tbl).HasMany("reports", tbl, id, bossID)

	for _, r := range []struct {
		id   int64
		name string
		boss int64
	}{{1, "Ada", 0}, {2, "Grace", 1}, {3, "Alan", 1}} {
		row := []pg.ColumnValue{id.Val(r.id), name.Val(r.name)}
		if r.boss != 0 {
			row = append(row, bossID.Val(r.boss))
		}
		if _, err := db.Insert(tbl).Row(row...).Exec(ctx); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
	}

	// The alias is taken after the relation is declared — As is a
	// snapshot, so an alias taken beside the table would not carry it.
	u := tbl.As("u")
	var bosses []aliasBoss
	if err := db.Find(u).
		Where(pg.IsNull(u.Col("bossId"))).
		Load(u.Rel("reports")).
		All(ctx, &bosses); err != nil {
		t.Fatalf("eager load through the alias: %v", err)
	}
	if len(bosses) != 1 {
		t.Fatalf("expected one root, got %d: %+v", len(bosses), bosses)
	}
	if len(bosses[0].Reports) != 2 {
		t.Errorf("%q loaded %d reports, want 2", bosses[0].Name, len(bosses[0].Reports))
	}
}

type aliasPaged struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

// An ordering column is rendered as well as matched — into the ORDER
// BY and into the cursor guard's row comparison — so a handle from
// another instance of the table names a relation the SELECT has no
// FROM entry for. Nothing in Go objects; the server answers 42P01.
// Both directions are reachable from ordinary code: an entity on the
// base table paged by an alias handle, and an entity on an alias paged
// by the package-level column that is in scope at the call site.
func TestPGPageOrdersByTheRelationTheQueryNames(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_page"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	execPG(t, db, pg.CreateTable(tbl))

	for i := int64(1); i <= 4; i++ {
		if _, err := db.Insert(tbl).
			Row(id.Val(i), name.Val(fmt.Sprintf("n%d", i))).
			Exec(ctx); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	u := tbl.As("u")

	// Entity on the base table, paged by the alias's handle.
	ent := pg.NewEntity[aliasPaged](tbl)
	first, err := ent.Page(db).OrderBy(pg.Asc(u.Col("id"))).Limit(2).All(ctx)
	if err != nil {
		t.Fatalf("page ordered by an alias handle: %v", err)
	}
	if len(first.Items) != 2 || first.Items[0].ID != 1 {
		t.Fatalf("first page is %+v", first.Items)
	}
	// The cursor guard renders the same handles, so the second page is
	// where a mismatch would surface even if the first slipped through.
	second, err := ent.Page(db).
		OrderBy(pg.Asc(u.Col("id"))).
		Limit(2).
		After(first.NextCursor).
		All(ctx)
	if err != nil {
		t.Fatalf("second page through the cursor guard: %v", err)
	}
	if len(second.Items) != 2 || second.Items[0].ID != 3 {
		t.Errorf("second page is %+v, want ids 3 and 4", second.Items)
	}

	// Entity on the alias, paged by the declared handle.
	entU := pg.NewEntity[aliasPaged](u)
	page, err := entU.Page(db).OrderBy(pg.Desc(id)).Limit(2).All(ctx)
	if err != nil {
		t.Fatalf("aliased entity paged by the declared handle: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != 4 {
		t.Errorf("descending page is %+v, want ids 4 and 3", page.Items)
	}
}

// The other half of the same rule, pinned so a change to it is a
// deliberate one: a Patch operation is built by the caller and only
// re-emitted by drops, so — like a predicate — it is not restated
// against the relation the UPDATE names. Built from the alias's own
// handles it works; built from the package-level column against an
// aliased entity the server rejects it. Loud, unlike the bindings and
// key sets that Column.key collapses, but the asymmetry is worth
// stating.
func TestPGPatchIsNotRewrittenByAnAlias(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "alias_patch"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	hits := pg.Add(tbl, pg.Integer("hits").NotNull().Default("0"))
	execPG(t, db, pg.CreateTable(tbl))

	u := tbl.As("u")
	ent := pg.NewEntity[aliasHit](u)
	row := aliasHit{ID: 1, Name: "a", Hits: 1}
	if err := ent.Create(db, ctx, &row); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The alias's own handle: the SET's right-hand side qualifies with
	// the relation "UPDATE t AS u" puts in scope.
	uHits := aliasCol[int32](u.Col("hits"))
	if _, err := ent.Patch(db, ctx, int64(1), pg.Inc(uHits, 10)); err != nil {
		t.Fatalf("Patch through the alias's handle: %v", err)
	}
	got, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hits != 11 {
		t.Errorf("hits = %d, want 11", got.Hits)
	}

	// The declared handle names a relation the statement does not have.
	if _, err := ent.Patch(db, ctx, int64(1), pg.Inc(hits, 100)); err == nil {
		t.Error("a Patch built from the declared handle resolved against an aliased entity")
	} else if !strings.Contains(err.Error(), "42P01") {
		t.Errorf("expected 42P01, got: %v", err)
	}
	_ = id
}

type aliasHit struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
	Hits int32  `drop:"hits"`
}

// --- Patch operations -------------------------------------------------

type pgPatchRow struct {
	ID    int64  `drop:"id"`
	Likes int64  `drop:"likes"`
	Peak  int64  `drop:"peak"`
	Name  string `drop:"name"`
}

// patchFixture is one row of counters with a typed handle per column,
// so each operation can be applied and the stored value read back.
func pgPatchFixture(t *testing.T) (*pg.DB, *pg.Entity[pgPatchRow], *pg.Col[int64], *pg.Col[int64], *pg.Col[string]) {
	t.Helper()
	db := openPG(t)
	tbl := pg.NewTable(integration.UniqueName(t, "patch"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	likes := pg.Add(tbl, pg.BigInt("likes").NotNull().Default("0"))
	peak := pg.Add(tbl, pg.BigInt("peak").NotNull().Default("0"))
	name := pg.Add(tbl, pg.Text("name").NotNull())
	execPG(t, db, pg.CreateTable(tbl))
	ent := pg.NewEntity[pgPatchRow](tbl)
	row := pgPatchRow{ID: 1, Name: "start"}
	if err := ent.Create(db, context.Background(), &row); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return db, ent, likes, peak, name
}

// Every named operation, judged by the value the server ends up
// holding rather than by the SQL that asked for it.
func TestPGPatchOperationsAgainstTheServer(t *testing.T) {
	db, ent, likes, peak, name := pgPatchFixture(t)
	ctx := context.Background()

	if _, err := ent.Patch(db, ctx, int64(1),
		pg.Inc(likes, int64(5)),
		pg.SetIfGreater(peak, int64(10)),
		pg.Set(name, "first"),
	); err != nil {
		t.Fatal(err)
	}
	row, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if row.Likes != 5 || row.Peak != 10 || row.Name != "first" {
		t.Fatalf("after the first patch: %+v", row)
	}

	// A high-watermark only rises.
	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfGreater(peak, int64(3))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Peak != 10 {
		t.Errorf("SetIfGreater lowered the value to %d", row.Peak)
	}
	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfGreater(peak, int64(42))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Peak != 42 {
		t.Errorf("SetIfGreater did not raise the value: %d", row.Peak)
	}

	// A low-watermark only falls.
	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfLess(peak, int64(100))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Peak != 42 {
		t.Errorf("SetIfLess raised the value to %d", row.Peak)
	}
	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfLess(peak, int64(7))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Peak != 7 {
		t.Errorf("SetIfLess did not lower the value: %d", row.Peak)
	}

	// Dec subtracts, which is the whole of what its name promises.
	if _, err := ent.Patch(db, ctx, int64(1), pg.Dec(likes, int64(2))); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Likes != 3 {
		t.Errorf("Dec left likes at %d, want 3", row.Likes)
	}

	// SetIfChanged writes a value that differs and leaves an equal one
	// alone; either way the stored value is the new one.
	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfChanged(name, "second")); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Name != "second" {
		t.Errorf("SetIfChanged = %q, want second", row.Name)
	}
	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfChanged(name, "second")); err != nil {
		t.Fatal(err)
	}
	if row, _ = ent.Get(db, ctx, int64(1)); row.Name != "second" {
		t.Errorf("SetIfChanged on an equal value = %q", row.Name)
	}

	// A key that matches nothing reports zero rows rather than an
	// error — the reason Patch hands back the Result.
	res, err := ent.Patch(db, ctx, int64(999), pg.Inc(likes, int64(1)))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 0 {
		t.Errorf("rows affected for a missing row = %d, %v; want 0", n, err)
	}
}

type pgPatchSeats struct {
	ID    int64  `drop:"id"`
	Seats uint32 `drop:"seats"`
}

// PostgreSQL has no unsigned column type, but the delta's type is the
// Go handle's, and Custom hands out a Col[uint32] over a bigint column.
// Negating the delta to reuse the addition wraps there: the statement
// renders perfectly, the server accepts it without complaint, and the
// counter climbs by four billion instead of falling by five. Only the
// stored value shows it.
func TestPGDecOnAnUnsignedHandleLowersTheCounter(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "unsigned"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	seats := pg.Add(tbl, pg.Custom[uint32]("seats", "bigint").NotNull().Default("0"))
	execPG(t, db, pg.CreateTable(tbl))
	ent := pg.NewEntity[pgPatchSeats](tbl)
	row := pgPatchSeats{ID: 1, Seats: 100}
	if err := ent.Create(db, ctx, &row); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := ent.Patch(db, ctx, int64(1), pg.Dec(seats, uint32(5))); err != nil {
		t.Fatalf("Dec on an unsigned handle: %v", err)
	}
	// Read the column as text, so a wrapped delta shows as the number
	// it is rather than as a scan failure into uint32.
	var stored string
	rows, err := db.Query(ctx, `SELECT seats::text FROM `+drops.StdQuoteIdent(tbl.Name())+` WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("no row")
	}
	if err := rows.Scan(&stored); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if stored != "95" {
		t.Errorf("seats = %s after Dec(5) from 100, want 95", stored)
	}
}

// GREATEST and LEAST ignore NULL arguments in PostgreSQL, so a
// watermark op against a NULL column stores the new value. MySQL's
// return NULL and erase the column instead — the divergence a schema
// served by both dialects trips over, pinned on the side that is
// forgiving.
func TestPGWatermarkOpsIgnoreANullColumn(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "nullpeak"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	high := pg.Add(tbl, pg.BigInt("high"))
	low := pg.Add(tbl, pg.BigInt("low"))
	execPG(t, db, pg.CreateTable(tbl))
	name := drops.StdQuoteIdent(tbl.Name())
	if _, err := db.Exec(ctx, `INSERT INTO `+name+` (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	ent := pg.NewEntity[pgPatchNullable](tbl)
	if _, err := ent.Patch(db, ctx, int64(1),
		pg.SetIfGreater(high, int64(10)),
		pg.SetIfLess(low, int64(10)),
	); err != nil {
		t.Fatal(err)
	}
	gotHigh, gotLow := pgReadNullableInts(t, db, tbl)
	if !gotHigh.Valid || gotHigh.Int64 != 10 {
		t.Errorf("SetIfGreater on a NULL column stored %v, want 10", gotHigh)
	}
	if !gotLow.Valid || gotLow.Int64 != 10 {
		t.Errorf("SetIfLess on a NULL column stored %v, want 10", gotLow)
	}
}

type pgPatchNullable struct {
	ID   int64 `drop:"id"`
	High int64 `drop:"high"`
	Low  int64 `drop:"low"`
}

func pgReadNullableInts(t *testing.T, db *pg.DB, tbl *pg.Table) (sql.NullInt64, sql.NullInt64) {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT high, low FROM `+drops.StdQuoteIdent(tbl.Name())+` WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var high, low sql.NullInt64
	if err := rows.Scan(&high, &low); err != nil {
		t.Fatal(err)
	}
	return high, low
}

// IS DISTINCT FROM is null-safe, which is the operator's first job
// here: a plain <> against a NULL column evaluates to NULL, the CASE
// falls to its ELSE, and the assignment silently never happens.
func TestPGSetIfChangedReachesANullColumn(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "ifchanged_null"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	note := pg.Add(tbl, pg.Text("note"))
	execPG(t, db, pg.CreateTable(tbl))
	name := drops.StdQuoteIdent(tbl.Name())
	if _, err := db.Exec(ctx, `INSERT INTO `+name+` (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
	ent := pg.NewEntity[pgPatchNote](tbl)
	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfChanged(note, "written")); err != nil {
		t.Fatal(err)
	}
	got, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if got.Note != "written" {
		t.Errorf("note = %q, want written — the NULL column was never assigned", got.Note)
	}
}

type pgPatchNote struct {
	ID   int64  `drop:"id"`
	Note string `drop:"note"`
}

// What SetIfChanged does not do is decide whether the row is written.
// PostgreSQL writes a new row version for every UPDATE whose WHERE
// matches, so both the CASE and a plain assignment of the identical
// value touch the row and fire AFTER UPDATE triggers. The doc used to
// claim the CASE was what made that happen.
func TestPGSetIfChangedTouchesTheRowNoMoreThanSetDoes(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "ifchanged_trig"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	note := pg.Add(tbl, pg.Text("note").NotNull())
	execPG(t, db, pg.CreateTable(tbl))
	name := drops.StdQuoteIdent(tbl.Name())
	logName := drops.StdQuoteIdent(tbl.Name() + "_log")
	fn := drops.StdQuoteIdent(tbl.Name() + "_fn")
	for _, stmt := range []string{
		`CREATE TABLE ` + logName + ` (n bigserial primary key)`,
		`CREATE FUNCTION ` + fn + `() RETURNS trigger AS $$ BEGIN INSERT INTO ` + logName + ` DEFAULT VALUES; RETURN NEW; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER t AFTER UPDATE ON ` + name + ` FOR EACH ROW EXECUTE FUNCTION ` + fn + `()`,
		`INSERT INTO ` + name + ` VALUES (1, 'a')`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DROP TABLE IF EXISTS `+logName)
		_, _ = db.Exec(context.Background(), `DROP FUNCTION IF EXISTS `+fn+`() CASCADE`)
	})

	ent := pg.NewEntity[pgPatchNote](tbl)
	countFires := func() int64 {
		rows, err := db.Query(ctx, `SELECT count(*) FROM `+logName)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatal("no count")
		}
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if _, err := ent.Patch(db, ctx, int64(1), pg.SetIfChanged(note, "a")); err != nil {
		t.Fatal(err)
	}
	afterCase := countFires()
	if _, err := ent.Patch(db, ctx, int64(1), pg.Set(note, "a")); err != nil {
		t.Fatal(err)
	}
	afterSet := countFires()
	if afterCase != 1 {
		t.Errorf("SetIfChanged of an equal value fired the trigger %d times, want 1", afterCase)
	}
	if afterSet != 2 {
		t.Errorf("Set of an equal value fired the trigger %d times in total, want 2 — a plain assignment touches the row too", afterSet)
	}
}

// The half of IS DISTINCT FROM that is not null-safety: the comparison
// runs under the column's collation. The default one is deterministic,
// so 'ada' and 'ADA' differ and the change is written — which is why
// drops/mysql's <=> version, comparing under a case-insensitive
// collation, is not the same operation. Declare the column with a
// nondeterministic collation and PostgreSQL declines the write too.
func TestPGSetIfChangedFollowsTheColumnCollation(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	coll := drops.StdQuoteIdent(integration.UniqueName(t, "ci"))
	if _, err := db.Exec(ctx, `CREATE COLLATION `+coll+
		` (provider = icu, locale = 'und-u-ks-level2', deterministic = false)`); err != nil {
		t.Skipf("no nondeterministic ICU collation on this server: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DROP COLLATION IF EXISTS `+coll) })

	tbl := pg.NewTable(integration.UniqueName(t, "coll"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	plain := pg.Add(tbl, pg.Text("plain").NotNull())
	ci := pg.Add(tbl, pg.Custom[string]("ci", `text COLLATE `+coll).NotNull())
	execPG(t, db, pg.CreateTable(tbl))
	name := drops.StdQuoteIdent(tbl.Name())
	if _, err := db.Exec(ctx, `INSERT INTO `+name+` VALUES (1, 'ada', 'ada')`); err != nil {
		t.Fatal(err)
	}
	ent := pg.NewEntity[pgPatchCollated](tbl)

	if _, err := ent.Patch(db, ctx, int64(1),
		pg.SetIfChanged(plain, "ADA"),
		pg.SetIfChanged(ci, "ADA"),
	); err != nil {
		t.Fatal(err)
	}
	got, err := ent.Get(db, ctx, int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if got.Plain != "ADA" {
		t.Errorf("plain = %q; under a deterministic collation a change of case is a change", got.Plain)
	}
	if got.CI != "ada" {
		t.Errorf("ci = %q; under a nondeterministic collation IS DISTINCT FROM is false and the write is declined", got.CI)
	}
	// Set has no such opinion: it writes what it is given.
	if _, err := ent.Patch(db, ctx, int64(1), pg.Set(ci, "ADA")); err != nil {
		t.Fatal(err)
	}
	if got, _ = ent.Get(db, ctx, int64(1)); got.CI != "ADA" {
		t.Errorf("Set on the collated column = %q, want ADA", got.CI)
	}
}

type pgPatchCollated struct {
	ID    int64  `drop:"id"`
	Plain string `drop:"plain"`
	CI    string `drop:"ci"`
}

// ----------------------------------------------------------------------
// Schema-level objects: enums, sequences, views, RLS and policies
// ----------------------------------------------------------------------

// int64p is a pointer to a sequence attribute, which SequenceOptions
// takes by pointer so "unset" is distinguishable from zero.
func int64p(v int64) *int64 { return &v }

// objectSchemaPG declares one table carrying every schema-level object
// drops can express, in the scratch schema the test pushes into.
func objectSchemaPG(schemaName string) *pg.Schema {
	status := pg.NewEnum("doc_status", "draft", "review", "published")
	// A camelCase type name has to survive quoting on the way into the
	// column definition: unquoted, PostgreSQL folds it and reports the
	// type missing.
	kind := pg.NewEnum("docKind", "note", "memo")

	docs := pg.NewSchemaTable(schemaName, "docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	pg.Add(docs, pg.BigInt("tenantId").NotNull())
	pg.Add(docs, pg.Text("owner").NotNull())
	pg.Add(docs, status.Col("status").NotNull())
	pg.Add(docs, kind.Col("kind").NotNull())
	docs.EnableRLS().ForceRLS()
	docs.AddPolicy(pg.NewPolicy("docsTenant").
		Using(`"tenantId" = current_setting('app.tenant')::bigint`).
		WithCheck(`"tenantId" = current_setting('app.tenant')::bigint`))
	docs.AddPolicy(pg.NewPolicy("docsOwnerReads").
		Restrictive().For("SELECT").To("drops").
		Using(`"owner" = current_user`))

	return pg.NewSchema(docs).
		AddEnum(status).
		AddEnum(kind).
		AddSequence(pg.NewSequence("docOrder", pg.SequenceOptions{
			Start: int64p(10), Increment: int64p(5),
		})).
		AddSequence(pg.NewSequence("docPlain")).
		AddView(pg.NewView("activeDocs",
			`SELECT "id", "owner" FROM "docs" WHERE "status" = 'published'`)).
		AddView(pg.NewMaterializedView("docOwners",
			`SELECT "owner", count(*) AS n FROM "docs" GROUP BY "owner"`))
}

// Push has to be re-runnable for a schema that declares more than
// tables. Every one of these objects used to be created on every push:
// Introspect read none of them, so Diff saw each one as new work for
// ever, and a schema with a single enum could never reach a steady
// state.
func TestPGPushIsIdempotentWithSchemaObjects(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()
	sch := objectSchemaPG(schemaName)
	opts := pg.PushOptions{Schema: schemaName}

	first, err := pg.Push(ctx, db, sch, opts)
	if err != nil {
		t.Fatalf("first push: %v", err)
	}
	if !first.Applied {
		t.Fatal("the first push applied nothing")
	}

	second, err := pg.Push(ctx, db, sch, opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("the second push is not a no-op:\n%s", strings.Join(second.Statements, "\n"))
	}
	for _, n := range second.Notices {
		t.Errorf("the second push reported a difference it would not act on: %s", n)
	}
}

// The bar is not the rendered SQL, it is what the server holds: create
// each object from the Go declaration, read it back, and check the two
// describe the same thing.
func TestPGIntrospectReadsBackTheDeclaredObjects(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()
	sch := objectSchemaPG(schemaName)

	if _, err := pg.Push(ctx, db, sch, pg.PushOptions{Schema: schemaName}); err != nil {
		t.Fatalf("push: %v", err)
	}
	live, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	declared := pg.BuildSnapshot(sch)

	// Enums, sequences: the whole entry has to match, Schema aside.
	// NewEnum and NewSequence take no schema, so BuildSnapshot writes
	// the default while Introspect writes the namespace it read; Diff
	// keys both by name and never looks at the field.
	for name, want := range declared.Enums {
		got := live.Enums[name]
		if got == nil {
			t.Errorf("enum %q was not read back", name)
			continue
		}
		got.Schema = want.Schema
		if !reflect.DeepEqual(got, want) {
			t.Errorf("enum %q: read back %+v, declared %+v", name, got, want)
		}
	}
	for name, want := range declared.Sequences {
		got := live.Sequences[name]
		if got == nil {
			t.Errorf("sequence %q was not read back", name)
			continue
		}
		got.Schema = want.Schema
		if !reflect.DeepEqual(got, want) {
			t.Errorf("sequence %q: read back %+v, declared %+v", name, got, want)
		}
	}
	// A view's body and a policy's expressions come back in
	// PostgreSQL's spelling, not the Go schema's — Push respells the
	// declared side before comparing them and TestPGPushIsIdempotent-
	// WithSchemaObjects is what proves that lines up. Everything else
	// about them has to match here.
	for name, want := range declared.Views {
		got := live.Views[name]
		if got == nil {
			t.Errorf("view %q was not read back", name)
			continue
		}
		if got.Materialized != want.Materialized {
			t.Errorf("view %q: materialized=%v, declared %v", name, got.Materialized, want.Materialized)
		}
		if got.Definition == "" {
			t.Errorf("view %q was read back with an empty body", name)
		}
	}
	docs := live.Tables[schemaName+".docs"]
	if docs == nil {
		t.Fatalf("table docs was not read back; got %v", sortedTableKeysPG(live))
	}
	if !docs.IsRLSEnabled || !docs.IsRLSForced {
		t.Errorf("row-level security read back as enabled=%v forced=%v, want both true",
			docs.IsRLSEnabled, docs.IsRLSForced)
	}
	wantDocs := declared.Tables[schemaName+".docs"]
	for name, want := range wantDocs.Policies {
		got := docs.Policies[name]
		if got == nil {
			t.Errorf("policy %q was not read back", name)
			continue
		}
		if got.As != want.As || got.For != want.For {
			t.Errorf("policy %q: as=%q for=%q, declared as=%q for=%q", name, got.As, got.For, want.As, want.For)
		}
		if !reflect.DeepEqual(got.To, want.To) && !(len(got.To) == 0 && len(want.To) == 0) {
			t.Errorf("policy %q: roles %v, declared %v", name, got.To, want.To)
		}
		if got.Using == "" {
			t.Errorf("policy %q was read back with no USING expression", name)
		}
	}
	if wc := docs.Policies["docsOwnerReads"]; wc != nil && wc.WithCheck != "" {
		t.Errorf("a policy declared without WITH CHECK read back with one: %q", wc.WithCheck)
	}
}

// sortedTableKeysPG names the tables a snapshot holds, for a failure
// message that says what was found instead.
func sortedTableKeysPG(snap *pg.Snapshot) []string {
	out := make([]string, 0, len(snap.Tables))
	for k := range snap.Tables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// An enum, a sequence, a view, a policy and a row-level security
// somebody switched on by hand are all things Diff reads as removals
// the moment Introspect can see them. Push withholds each drop and
// names it. The RLS case is the one that matters most: a silent
// DISABLE ROW LEVEL SECURITY hands every row to every caller.
func TestPGPushLeavesUndeclaredObjectsAlone(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	docs := pg.NewSchemaTable(schemaName, "docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	pg.Add(docs, pg.Text("owner").NotNull())
	sch := pg.NewSchema(docs)
	opts := pg.PushOptions{Schema: schemaName}

	if _, err := pg.Push(ctx, db, sch, opts); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Somebody else's objects arrive: another tool's enum, a sequence
	// a job counter uses, a DBA's view, and row-level security with a
	// policy behind it. None of them are in any Go file.
	for _, s := range []string{
		`CREATE TYPE "otherStatus" AS ENUM ('a', 'b')`,
		`CREATE SEQUENCE "otherCounter"`,
		`CREATE VIEW "otherDocs" AS SELECT "id" FROM "docs"`,
		`ALTER TABLE "docs" ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE "docs" FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY "otherOwner" ON "docs" USING ("owner" = current_user)`,
	} {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	second, err := pg.Push(ctx, db, sch, opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	for _, s := range second.Statements {
		t.Errorf("Push acted on an object it does not manage: %s", s)
	}
	for _, want := range []struct{ rule, object string }{
		{"unmanaged-enum", "otherStatus"},
		{"unmanaged-sequence", "otherCounter"},
		{"unmanaged-view", "otherDocs"},
		{"unmanaged-policy", "otherOwner"},
		{"unmanaged-rls", "docs"},
	} {
		n := noticeFor(second.Notices, want.rule)
		if n.Object != want.object {
			t.Errorf("no %s notice for %q; notices were %v", want.rule, want.object, second.Notices)
			continue
		}
		if n.SQL == "" {
			t.Errorf("the %s notice carries no statement to run by hand: %+v", want.rule, n)
		}
	}
	// Both halves of the RLS state are reported, not just the first.
	var rls int
	for _, n := range second.Notices {
		if n.Rule == "unmanaged-rls" {
			rls++
		}
	}
	if rls != 2 {
		t.Errorf("got %d unmanaged-rls notices, want one for ENABLE and one for FORCE", rls)
	}
	if !rlsEnabledPG(t, db, schemaName, "docs") {
		t.Fatal("Push switched row-level security off")
	}

	// Opting in performs every one of them.
	optIn := opts
	optIn.DropUnmanagedObjects = true
	third, err := pg.Push(ctx, db, sch, optIn)
	if err != nil {
		t.Fatalf("opt-in push: %v", err)
	}
	if !third.Applied {
		t.Fatalf("DropUnmanagedObjects applied nothing: %v", third.Statements)
	}
	if rlsEnabledPG(t, db, schemaName, "docs") {
		t.Error("DropUnmanagedObjects left row-level security on")
	}
	after, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if len(after.Enums) != 0 || len(after.Sequences) != 0 || len(after.Views) != 0 {
		t.Errorf("DropUnmanagedObjects left objects behind: enums=%v sequences=%v views=%v",
			after.Enums, after.Sequences, after.Views)
	}
}

// rlsEnabledPG reports pg_class.relrowsecurity for one table, read
// straight from the catalogue rather than through Introspect — the
// thing under test cannot also be the witness.
func rlsEnabledPG(t *testing.T, db *pg.DB, schema, table string) bool {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT c.relrowsecurity FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, table)
	if err != nil {
		t.Fatalf("read relrowsecurity: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("table %q is not in %q", table, schema)
	}
	var on bool
	if err := rows.Scan(&on); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return on
}

// What an extension installed is not drops's to drop. PostGIS puts a
// table and two views in public and a serial column owns a sequence;
// all of them would otherwise be reported as objects the Go schema no
// longer declares, and Diff emits a DROP for each of those.
func TestPGIntrospectSkipsWhatAnExtensionOwns(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	if !extensionInstalledPG(t, db, "postgis") {
		t.Skip("postgis is not installed in this database")
	}
	snap, err := pg.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if _, ok := snap.Tables["public.spatial_ref_sys"]; ok {
		t.Error("PostGIS's spatial_ref_sys is in the snapshot; a push would DROP TABLE it")
	}
	for _, name := range []string{"geometry_columns", "geography_columns"} {
		if _, ok := snap.Views[name]; ok {
			t.Errorf("PostGIS's %q view is in the snapshot; a push would DROP VIEW it", name)
		}
	}

	// A serial column's sequence is owned by the column: it is in no
	// Go declaration and cannot be dropped on its own anyway.
	tbl := pg.NewTable(integration.UniqueName(t, "owned"))
	dropPG(t, db, tbl)
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	execPG(t, db, pg.CreateTable(tbl))

	snap, err = pg.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	for name := range snap.Sequences {
		if strings.HasPrefix(name, tbl.Name()) {
			t.Errorf("the sequence %q behind a serial column is in the snapshot", name)
		}
	}
}

// extensionInstalledPG reports whether an extension is present.
func extensionInstalledPG(t *testing.T, db *pg.DB, name string) bool {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT 1 FROM pg_extension WHERE extname = $1`, name)
	if err != nil {
		t.Fatalf("read pg_extension: %v", err)
	}
	defer rows.Close()
	return rows.Next()
}

// Label order is part of an enum type: it is the order `<` and ORDER BY
// follow, and PostgreSQL cannot change it in place. Push cannot act on
// a reorder, so it has to say so — a push that reported success here
// would be claiming a database sorts the way the Go schema says when it
// does not.
func TestPGPushReportsAnEnumReorder(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	build := func(labels ...string) *pg.Schema {
		docs := pg.NewSchemaTable(schemaName, "docs")
		pg.Add(docs, pg.BigSerial("id").PrimaryKey())
		return pg.NewSchema(docs).AddEnum(pg.NewEnum("doc_status", labels...))
	}
	opts := pg.PushOptions{Schema: schemaName}

	if _, err := pg.Push(ctx, db, build("draft", "published"), opts); err != nil {
		t.Fatalf("first push: %v", err)
	}
	// Appending is something PostgreSQL can do, so it is done and not
	// merely reported.
	third, err := pg.Push(ctx, db, build("draft", "published", "archived"), opts)
	if err != nil {
		t.Fatalf("append push: %v", err)
	}
	if len(third.Statements) == 0 {
		t.Error("a new enum label produced no statement")
	}
	if n := noticeFor(third.Notices, "enum-labels-reordered"); n.Rule != "" {
		t.Errorf("appending a label was reported as a reorder: %s", n)
	}

	// Reordering is not.
	fourth, err := pg.Push(ctx, db, build("published", "draft", "archived"), opts)
	if err != nil {
		t.Fatalf("reorder push: %v", err)
	}
	if len(fourth.Statements) != 0 {
		t.Errorf("Push tried to reorder an enum: %v", fourth.Statements)
	}
	n := noticeFor(fourth.Notices, "enum-labels-reordered")
	if n.Object != "doc_status" {
		t.Fatalf("no enum-labels-reordered notice; got %v", fourth.Notices)
	}
	if !strings.Contains(n.Message, "draft") {
		t.Errorf("the notice does not say what the two orders are: %q", n.Message)
	}
}

// A view is dropped by the CASCADE that drops the table under it, so a
// DROP VIEW emitted afterwards is a statement PostgreSQL refuses
// (SQLSTATE 42P01) and a migration that stops halfway. The whole point
// of reading views back is that Push and GenerateMigration can now
// reach this, so the drop has to come first.
func TestPGDiffDropsAViewBeforeTheTableUnderIt(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	docs := pg.NewSchemaTable(schemaName, "docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	sch := pg.NewSchema(docs).
		AddView(pg.NewView("activeDocs", `SELECT "id" FROM "docs"`)).
		AddView(pg.NewMaterializedView("docCount", `SELECT count(*) AS n FROM "docs"`))

	for _, s := range pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(sch)) {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("building the fixture with %q: %v", s, err)
		}
	}
	live, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	// Withdrawing the table and both views at once is what a migration
	// generated from an emptied schema holds.
	for _, s := range pg.Diff(live, pg.EmptySnapshot()) {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Errorf("the diff shipped a statement the server refused: %q: %v", s, err)
		}
	}
}

// A view that became materialised is a different kind of object, and
// PostgreSQL has no ALTER that converts one into the other. Comparing
// only the body meant Push reported success against a database still
// holding a plain view — and once the body is respelled by the probe
// the two bodies agree, so nothing at all was emitted.
func TestPGDiffTurnsAViewIntoAMaterialisedOne(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	for _, s := range []string{
		`CREATE TABLE "docs" ("id" bigserial PRIMARY KEY NOT NULL)`,
		`CREATE VIEW "summary" AS SELECT "id" FROM "docs"`,
	} {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	live, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	// Declared with the server's own spelling of the body, which is
	// what renormaliseExpressions leaves behind on a real push: the
	// kind is then the only difference left.
	body := live.Views["summary"].Definition
	docs := pg.NewSchemaTable(schemaName, "docs")
	pg.Add(docs, pg.BigSerial("id").PrimaryKey())
	sch := pg.NewSchema(docs).AddView(pg.NewMaterializedView("summary", body))

	stmts := pg.Diff(live, pg.BuildSnapshot(sch))
	if len(stmts) == 0 {
		t.Fatal("turning a view into a materialised view produced no statement")
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("applying %q: %v", s, err)
		}
	}
	if kind := relkindPG(t, db, schemaName, "summary"); kind != "m" {
		t.Errorf("relkind is %q after the push, want \"m\" — the database still holds a plain view", kind)
	}

	// And back again, so the comparison is not one-directional.
	live, err = pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	plainDocs := pg.NewSchemaTable(schemaName, "docs")
	pg.Add(plainDocs, pg.BigSerial("id").PrimaryKey())
	back := pg.NewSchema(plainDocs).AddView(pg.NewView("summary", live.Views["summary"].Definition))
	stmts = pg.Diff(live, pg.BuildSnapshot(back))
	if len(stmts) == 0 {
		t.Fatal("turning a materialised view back into a plain one produced no statement")
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("applying %q: %v", s, err)
		}
	}
	if kind := relkindPG(t, db, schemaName, "summary"); kind != "v" {
		t.Errorf("relkind is %q, want \"v\"", kind)
	}
}

// relkindPG reads pg_class.relkind for one relation, straight from the
// catalogue rather than through Introspect.
func relkindPG(t *testing.T, db *pg.DB, schema, name string) string {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT c.relkind::text FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2`, schema, name)
	if err != nil {
		t.Fatalf("read relkind: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("relation %q is not in %q", name, schema)
	}
	var kind string
	if err := rows.Scan(&kind); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return kind
}

// Label order is read from enumsortorder, not from oid. ALTER TYPE ...
// ADD VALUE BEFORE gives the new label the highest oid in the set and
// a position that is not last, so the two orders disagree — and the
// position is the part that is the type. Reading oid order back would
// have Push report a reorder against a database that matches the Go
// schema exactly.
func TestPGIntrospectReadsEnumLabelsInSortOrder(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	for _, s := range []string{
		`CREATE TYPE "docStatus" AS ENUM ('draft', 'published')`,
		`ALTER TYPE "docStatus" ADD VALUE 'review' BEFORE 'published'`,
	} {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}

	live, err := pg.Introspect(ctx, db, pg.IntrospectOptions{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	got := live.Enums["docStatus"]
	if got == nil {
		t.Fatal("the enum was not read back")
	}
	want := []string{"draft", "review", "published"}
	if !reflect.DeepEqual(got.Values, want) {
		t.Errorf("labels read back as %v, want %v — that is oid order, not the type's order", got.Values, want)
	}

	// And a Go schema declaring them in that order has nothing to do
	// and nothing to say about it.
	sch := pg.NewSchema().AddEnum(pg.NewEnum("docStatus", want...))
	res, err := pg.Push(ctx, db, sch, pg.PushOptions{Schema: schemaName})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Errorf("push wanted to change a matching enum: %v", res.Statements)
	}
	for _, n := range res.Notices {
		t.Errorf("push reported a difference against a matching enum: %s", n)
	}
}

// A policy's expression is handed to the server as a policy, not as a
// CHECK constraint. A CHECK may not hold a subquery (SQLSTATE 0A000)
// and a policy very often does, so probing one as the other reports
// the whole class as unparseable: the comparison silently falls back
// to whatever the database holds, and a changed policy stops shipping.
func TestPGPushNormalisesAPolicyHoldingASubquery(t *testing.T) {
	db, schemaName := pushScratchPG(t)
	ctx := context.Background()

	build := func(role string) *pg.Schema {
		tenants := pg.NewSchemaTable(schemaName, "tenants")
		pg.Add(tenants, pg.BigInt("id").PrimaryKey())
		pg.Add(tenants, pg.Text("owner").NotNull())

		docs := pg.NewSchemaTable(schemaName, "docs")
		pg.Add(docs, pg.BigSerial("id").PrimaryKey())
		pg.Add(docs, pg.BigInt("tenantId").NotNull())
		docs.EnableRLS()
		docs.AddPolicy(pg.NewPolicy("docsVisible").Using(
			`"tenantId" IN (SELECT "id" FROM "tenants" WHERE "owner" = ` + role + `)`))
		return pg.NewSchema(tenants, docs)
	}
	opts := pg.PushOptions{Schema: schemaName}

	if _, err := pg.Push(ctx, db, build("current_user"), opts); err != nil {
		t.Fatalf("first push: %v", err)
	}
	second, err := pg.Push(ctx, db, build("current_user"), opts)
	if err != nil {
		t.Fatalf("second push: %v", err)
	}
	if len(second.Statements) != 0 {
		t.Errorf("the second push is not a no-op:\n%s", strings.Join(second.Statements, "\n"))
	}
	for _, n := range second.Notices {
		t.Errorf("the policy expression went unchecked: %s", n)
	}

	// The point of respelling it is that a real change still ships.
	third, err := pg.Push(ctx, db, build("session_user"), opts)
	if err != nil {
		t.Fatalf("third push: %v", err)
	}
	if len(third.Statements) == 0 {
		t.Fatal("a changed policy expression produced no statement")
	}
	if using := policyUsingPG(t, db, schemaName, "docs", "docsVisible"); !strings.Contains(using, "SESSION_USER") {
		t.Errorf("the database still holds %q; the change did not ship", using)
	}
}

// policyUsingPG reads a policy's USING expression from the catalogue.
func policyUsingPG(t *testing.T, db *pg.DB, schema, table, policy string) string {
	t.Helper()
	rows, err := db.Query(context.Background(), `
		SELECT coalesce(pg_get_expr(pol.polqual, pol.polrelid), '')
		FROM pg_policy pol
		JOIN pg_class c ON c.oid = pol.polrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND pol.polname = $3`, schema, table, policy)
	if err != nil {
		t.Fatalf("read polqual: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("policy %q on %q is not in the catalogue", policy, table)
	}
	var out string
	if err := rows.Scan(&out); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}
