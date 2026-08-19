package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

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
