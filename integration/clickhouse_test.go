package integration_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
)

func openCH(t *testing.T) *clickhouse.DB {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvClickHouse)
	sqlDB, err := sql.Open("clickhouse", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return clickhouse.New(stdlib.New(sqlDB))
}

func execCH(t *testing.T, db *clickhouse.DB, e drops.Expression) {
	t.Helper()
	if _, err := db.ExecExpr(context.Background(), e); err != nil {
		text, args := drops.StringWithDialect(clickhouse.Dialect, e)
		t.Fatalf("ClickHouse rejected the statement: %v\n%s\nargs: %v", err, text, args)
	}
}

func dropCH(t *testing.T, db *clickhouse.DB, tbl *clickhouse.Table) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS `"+tbl.Name()+"`")
	})
}

// The CREATE TABLE whose sorting key was rendered with qualified column
// names until recently. ClickHouse cannot resolve a reference to a
// table that does not exist yet, so this is the test that settles it.
func TestCHCreateTableIsAccepted(t *testing.T) {
	db := openCH(t)
	tbl := clickhouse.NewTable(integration.UniqueName(t, "events"))
	dropCH(t, db, tbl)

	id := clickhouse.Add(tbl, clickhouse.UUID("id"))
	ts := clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.UInt64("userId"))
	clickhouse.Add(tbl, clickhouse.Custom[string]("kind", "LowCardinality(String)"))
	clickhouse.Add(tbl, clickhouse.Custom[[]string]("tags", "Array(String)"))
	clickhouse.Add(tbl, clickhouse.Float64("durationMs"))

	tbl.Engine(clickhouse.MergeTree()).
		OrderBy(ts, id).
		PartitionBy(clickhouse.ToYYYYMM(ts)).
		Setting("index_granularity", "8192")

	execCH(t, db, clickhouse.CreateTable(tbl))
}

// A mirror table derived from a Postgres one, created on a real
// ClickHouse. The type mapping in mirror.MapType is a claim about what
// ClickHouse accepts, and this is where it gets checked.
func TestCHDerivedMirrorTableIsAccepted(t *testing.T) {
	db := openCH(t)

	src := pg.NewTable("docs")
	pg.Add(src, pg.BigSerial("id").PrimaryKey())
	pg.Add(src, pg.Text("title").NotNull())
	pg.Add(src, pg.Text("body"))
	pg.Add(src, pg.Integer("views").NotNull())
	pg.Add(src, pg.Boolean("draft").NotNull())
	pg.Add(src, pg.Timestamp("createdAt", true).NotNull())
	pg.Add(src, pg.Timestamp("naiveAt", false))
	pg.Add(src, pg.Date("publishedOn"))
	pg.Add(src, pg.UUID("authorId"))
	pg.Add(src, pg.Numeric("price", 10, 2))
	pg.Add(src, pg.Numeric("unscaled", 0, 0))
	pg.Add(src, pg.JSONB("meta"))
	pg.Add(src, pg.Bytea("thumb"))
	pg.Add(src, pg.Real("score"))
	pg.Add(src, pg.SmallInt("rank"))

	chTbl, err := mirror.DeriveClickHouse(src, mirror.WithName(integration.UniqueName(t, "docs")))
	if err != nil {
		t.Fatalf("DeriveClickHouse: %v", err)
	}
	dropCH(t, db, chTbl)
	execCH(t, db, clickhouse.CreateTable(chTbl))

	// And the sink's INSERT has to be one ClickHouse accepts, with the
	// bookkeeping columns bound as the engine expects.
	sink, err := mirror.NewClickHouseSink(db, chTbl)
	if err != nil {
		t.Fatalf("NewClickHouseSink: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	err = sink.Apply(context.Background(), []mirror.Change{
		{
			Op: mirror.OpInsert, Key: "1", Version: 1, At: now,
			Row: map[string]any{
				"id": int64(1), "title": "hello", "views": int32(0),
				"draft": false, "createdAt": now,
			},
		},
		{Op: mirror.OpDelete, Key: "2", Version: 2, At: now},
	})
	if err != nil {
		t.Fatalf("sink.Apply: %v", err)
	}

	// Read it back the way the docs say to: FINAL plus the tombstone
	// filter.
	var n uint64
	sel := db.Select(clickhouse.CountAll()).From(chTbl).Final().Where(mirror.NotDeleted(chTbl))
	if err := sel.One(context.Background(), &n); err != nil {
		text, args := drops.StringWithDialect(clickhouse.Dialect, sel)
		t.Fatalf("read back: %v\n%s\nargs: %v", err, text, args)
	}
	if n != 1 {
		t.Errorf("live rows = %d, want 1 (the tombstoned row must not count)", n)
	}
}

// Every mapping mirror.MapType can produce, declared in one table.
// A mapping that names a type ClickHouse does not have fails here.
func TestCHEveryMirrorTypeMappingIsAccepted(t *testing.T) {
	db := openCH(t)
	tbl := clickhouse.NewTable(integration.UniqueName(t, "types"))
	dropCH(t, db, tbl)

	pgTypes := []string{
		"smallint", "integer", "bigint", "serial", "bigserial",
		"real", "double precision", "boolean", "uuid", "date",
		"timestamp", "timestamptz", "numeric(12,4)", "numeric",
		"text", "varchar(255)", "char(8)", "jsonb", "json",
		"bytea", "interval", "time",
	}
	clickhouse.Add(tbl, clickhouse.UInt64("id"))
	for i, pgType := range pgTypes {
		chType, err := mirror.MapType(pgType, false)
		if err != nil {
			t.Fatalf("MapType(%q): %v", pgType, err)
		}
		clickhouse.Add(tbl, clickhouse.Custom[string](colName(i), chType))
		nullable, err := mirror.MapType(pgType, true)
		if err != nil {
			t.Fatalf("MapType(%q, nullable): %v", pgType, err)
		}
		clickhouse.Add(tbl, clickhouse.Custom[string](colName(i)+"_n", nullable))
	}
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tbl.Col("id"))
	execCH(t, db, clickhouse.CreateTable(tbl))
}

func colName(i int) string {
	return "c" + string(rune('a'+i%26)) + string(rune('a'+i/26))
}

// INSERT routed through the FROM/JOIN renderer, which stamps the
// table's alias after the name. ClickHouse's INSERT parser reads a
// table identifier and then expects the column list, VALUES, SELECT or
// FORMAT — there is no alias position, so the statement never parses.
func TestCHInsertIntoAliasedTableIsAccepted(t *testing.T) {
	db := openCH(t)
	tbl := clickhouse.NewTable(integration.UniqueName(t, "events"))
	dropCH(t, db, tbl)

	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	kind := clickhouse.Add(tbl, clickhouse.String("kind"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(id)
	execCH(t, db, clickhouse.CreateTable(tbl))

	// The alias is what a self-join needs and what a caller therefore
	// holds; inserting through that same handle must still be an
	// INSERT ClickHouse accepts.
	ins := db.Insert(tbl.As("e")).Row(id.Val(1), kind.Val("click"))
	if _, err := ins.Exec(context.Background()); err != nil {
		text, args := ins.ToSQL()
		t.Fatalf("ClickHouse rejected the INSERT: %v\n%s\nargs: %v", err, text, args)
	}

	var n uint64
	if err := db.Select(clickhouse.CountAll()).From(tbl).One(context.Background(), &n); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// SummingMergeTree takes its summed columns as one tuple argument.
// Passing them as separate engine arguments leaves the first one over
// as an engine parameter MergeTree's own argument check then rejects.
func TestCHSummingMergeTreeColumnsAreATuple(t *testing.T) {
	db := openCH(t)
	tbl := clickhouse.NewTable(integration.UniqueName(t, "totals"))
	dropCH(t, db, tbl)

	key := clickhouse.Add(tbl, clickhouse.String("key"))
	hits := clickhouse.Add(tbl, clickhouse.UInt64("hits"))
	bytesCol := clickhouse.Add(tbl, clickhouse.UInt64("bytes"))
	tbl.Engine(clickhouse.SummingMergeTree("hits", "bytes")).OrderBy(key)
	execCH(t, db, clickhouse.CreateTable(tbl))

	ctx := context.Background()
	for _, row := range [][2]uint64{{1, 10}, {2, 20}} {
		ins := db.Insert(tbl).Row(key.Val("a"), hits.Val(row[0]), bytesCol.Val(row[1]))
		if _, err := ins.Exec(ctx); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, err := db.Exec(ctx, "OPTIMIZE TABLE `"+tbl.Name()+"` FINAL"); err != nil {
		t.Fatalf("optimize: %v", err)
	}

	var rows, hitsTotal, bytesTotal uint64
	if err := db.Select(clickhouse.CountAll()).From(tbl).One(ctx, &rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows after merge = %d, want 1 — the engine did not collapse the key", rows)
	}
	if err := db.Select(hits).From(tbl).One(ctx, &hitsTotal); err != nil {
		t.Fatalf("read hits: %v", err)
	}
	if err := db.Select(bytesCol).From(tbl).One(ctx, &bytesTotal); err != nil {
		t.Fatalf("read bytes: %v", err)
	}
	if hitsTotal != 3 || bytesTotal != 30 {
		t.Errorf("summed row = (hits %d, bytes %d), want (3, 30) — the tuple did not reach columns_to_sum",
			hitsTotal, bytesTotal)
	}
}

// ClickHouse's column-declaration parser walks the optional clauses in
// a fixed order: DEFAULT, then COMMENT, then CODEC, then TTL. A comment
// emitted after the codec is a syntax error for the whole CREATE TABLE,
// which is why a column carrying both has to be checked on a server.
func TestCHColumnCommentCodecAndTTLOrder(t *testing.T) {
	db := openCH(t)
	tbl := clickhouse.NewTable(integration.UniqueName(t, "annotated"))
	dropCH(t, db, tbl)

	ts := clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.UInt64("id"))
	clickhouse.Add(tbl, clickhouse.Float64("amount").
		Comment("what the customer paid").
		Codec("ZSTD(3)").
		TTL("ts + INTERVAL 30 DAY"))
	clickhouse.Add(tbl, clickhouse.String("note").Comment("free text"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ts)

	execCH(t, db, clickhouse.CreateTable(tbl))
}

// Half of the event store quoted its table name through quoteIdent and
// half interpolated it into a bare "%s", so a name needing an escape
// was written correctly and read back as a syntax error.
func TestCHEventStoreQuotesItsTableName(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "ev") + `"log`
	tbl := clickhouse.NewEventStoreTable(name)
	dropCH(t, db, tbl)
	execCH(t, db, clickhouse.CreateTable(tbl))

	snapName := integration.UniqueName(t, "snap") + `"log`
	snapTbl := clickhouse.NewSnapshotTable(snapName)
	dropCH(t, db, snapTbl)
	execCH(t, db, clickhouse.CreateTable(snapTbl))

	store := clickhouse.NewEventStore(db, name)
	err := store.Append(ctx, "match", "abc", -1,
		clickhouse.EventInput{Type: "matchStarted", Payload: map[string]string{"map": "dust"}})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	events, err := store.Load(ctx, "match", "abc", -1)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(events) != 1 || events[0].Type != "matchStarted" {
		t.Fatalf("Load returned %+v, want one matchStarted event", events)
	}

	err = store.SaveSnapshot(ctx, snapName, clickhouse.AggregateSnapshot{
		AggregateType: "match", AggregateID: "abc", Version: 0,
		State: []byte(`{"round":1}`),
	})
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	snap, ok, err := store.LoadSnapshot(ctx, snapName, "match", "abc")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if !ok || string(snap.State) != `{"round":1}` {
		t.Errorf("LoadSnapshot = %q, ok=%v, want the saved state", snap.State, ok)
	}
}
