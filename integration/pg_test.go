package integration_test

import (
	"context"
	"database/sql"
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
	for _, m := range []vector.Metric{vector.Cosine, vector.L2, vector.InnerProduct, vector.L1} {
		if _, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).Metric(m).TopK(1).Build()); err != nil {
			t.Errorf("metric %s rejected: %v", m, err)
		}
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
