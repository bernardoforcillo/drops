package integration_test

import (
	"context"
	"database/sql"
	"errors"
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
	if v, err := store.LatestVersion(ctx, "match", "abc"); err != nil || v != -1 {
		t.Fatalf("LatestVersion on an untouched stream = %d (err %v), want -1", v, err)
	}
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

// EventStore.LatestVersion asks for coalesce(maxOrNull("version"), -1)
// rather than coalesce(max("version"), -1), and this test exists
// because that choice was made from the documentation without a server
// to check it against.
//
// The reasoning: ClickHouse answers an aggregate over an empty set with
// the return type's default rather than NULL. The setting
// aggregate_functions_null_for_empty exists precisely to opt into the
// SQL-standard answer, and it defaults to 0. So coalesce(max(v), -1)
// yields 0 for a stream nobody has written, Append's expectedVersion of
// -1 compares unequal, and the store returns ErrConcurrencyConflict on
// the first event it will ever be asked to write — a store that cannot
// start.
//
// The premise is asserted first and separately from the consequence:
// if a future ClickHouse changes the default, the raw-SQL half fails
// and says so, instead of the store quietly going back to being
// unstartable.
func TestCHEventStoreFreshStreamStartsAtMinusOne(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "ev")
	tbl := clickhouse.NewEventStoreTable(name)
	dropCH(t, db, tbl)
	execCH(t, db, clickhouse.CreateTable(tbl))

	// The premise. Nothing has been inserted, so both aggregates run
	// over an empty set. The second column is the expression
	// LatestVersion actually sends; the third asks the server, rather
	// than the driver's null handling, whether maxOrNull is NULL.
	var (
		plainMax     int64
		guarded      int64
		orNullIsNull uint8
	)
	q := `SELECT max("version"), coalesce(maxOrNull("version"), -1), isNull(maxOrNull("version")) ` +
		`FROM "` + name + `" WHERE "aggregateType" = ?`
	rows, err := db.Query(ctx, q, "match")
	if err != nil {
		t.Fatalf("aggregate probe: %v", err)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatal("an aggregate with no GROUP BY must produce exactly one row")
	}
	if err := rows.Scan(&plainMax, &guarded, &orNullIsNull); err != nil {
		rows.Close()
		t.Fatalf("scan: %v", err)
	}
	rows.Close()
	if plainMax != 0 {
		t.Errorf(`max("version") over an empty set = %d, want 0 — `+
			`the premise for preferring maxOrNull no longer holds`, plainMax)
	}
	if orNullIsNull != 1 {
		t.Errorf(`isNull(maxOrNull("version")) over an empty set = %d, want 1`, orNullIsNull)
	}
	if guarded != -1 {
		t.Errorf(`coalesce(maxOrNull("version"), -1) over an empty set = %d, want -1`, guarded)
	}

	// The consequence.
	s := clickhouse.NewEventStore(db, name)
	if v, err := s.LatestVersion(ctx, "match", "abc"); err != nil || v != -1 {
		t.Fatalf("LatestVersion on an untouched stream = %d (err %v), want -1", v, err)
	}

	// A store that cannot write its first event is the failure mode
	// this guards, so write one.
	if err := s.Append(ctx, "match", "abc", -1,
		clickhouse.EventInput{Type: "matchStarted"}); err != nil {
		t.Fatalf("Append on a fresh stream: %v", err)
	}
	if v, err := s.LatestVersion(ctx, "match", "abc"); err != nil || v != 0 {
		t.Fatalf("LatestVersion after one event = %d (err %v), want 0", v, err)
	}
	// -1 and 0 have to stay distinguishable: the head is now genuinely
	// 0, and a second append at -1 must be refused.
	if err := s.Append(ctx, "match", "abc", -1,
		clickhouse.EventInput{Type: "late"}); !errors.Is(err, clickhouse.ErrConcurrencyConflict) {
		t.Errorf("re-Append at expectedVersion -1 = %v, want ErrConcurrencyConflict", err)
	}
	if err := s.Append(ctx, "match", "abc", 0,
		clickhouse.EventInput{Type: "playerJoined"}); err != nil {
		t.Fatalf("Append at the real head: %v", err)
	}

	// Load is exclusive on fromVersion and versions start at 0, so -1
	// is what reads a stream from the beginning and 0 skips the first
	// event. The doc used to say 0; this is which one is true.
	all, err := s.Load(ctx, "match", "abc", -1)
	if err != nil {
		t.Fatalf("Load(-1): %v", err)
	}
	if len(all) != 2 || all[0].Type != "matchStarted" || all[0].Version != 0 {
		t.Fatalf("Load(-1) = %d events (%+v), want both from version 0", len(all), all)
	}
	tail, err := s.Load(ctx, "match", "abc", 0)
	if err != nil {
		t.Fatalf("Load(0): %v", err)
	}
	if len(tail) != 1 || tail[0].Type != "playerJoined" {
		t.Fatalf("Load(0) = %+v, want only the event after version 0", tail)
	}
}

// (*Table).As used to hand back a shallow copy whose columns still
// pointed at the un-aliased table, so a SELECT through an aliased
// handle named a relation the FROM clause had already renamed. The
// rendered string is not evidence on its own — this is the server
// saying whether it can resolve the identifiers.
func TestCHSelfJoinThroughAliasIsAccepted(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()

	tbl := clickhouse.NewTable(integration.UniqueName(t, "moves"))
	dropCH(t, db, tbl)
	game := clickhouse.Add(tbl, clickhouse.UInt64("gameId"))
	seq := clickhouse.Add(tbl, clickhouse.UInt64("seq"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(game, seq)
	execCH(t, db, clickhouse.CreateTable(tbl))

	ins := db.Insert(tbl)
	for i := uint64(1); i <= 3; i++ {
		ins.Row(game.Val(7), seq.Val(i))
	}
	if _, err := ins.Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Each move paired with every later move in the same game: the
	// query that is unwritable while both handles render as the same
	// qualified name.
	a, b := tbl.As("a"), tbl.As("b")
	q := db.Select(clickhouse.CountAll()).
		From(a).
		Join(b, clickhouse.Eq(a.Col("gameId"), b.Col("gameId"))).
		Where(clickhouse.Lt(a.Col("seq"), b.Col("seq")))

	var n uint64
	if err := q.One(ctx, &n); err != nil {
		text, args := q.ToSQL()
		t.Fatalf("ClickHouse rejected the self-join: %v\n%s\nargs: %v", err, text, args)
	}
	if n != 3 {
		text, _ := q.ToSQL()
		t.Errorf("self-join counted %d pairs, want 3 (1<2, 1<3, 2<3)\n%s", n, text)
	}
}
