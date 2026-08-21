package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
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

// Load's fromVersion is exclusive, and the number it takes is the same
// number Append's expectedVersion and LatestVersion speak: the version
// you already hold, -1 when you hold none. Two of the three dialects
// used to document 0 as "from the beginning", and 0 is the first
// event's own version, so every caller who believed it lost event 0 of
// every stream — silently, since what came back was still an ordered
// run of real events, just missing the one that created the aggregate.
//
// Three events on a fresh stream leave no room for that: each starting
// value has exactly one right answer and the test names all of them.
// The predicate is the server's to evaluate, and the contract is the
// same one pg and sqlite assert, so the three tests are deliberately
// the same test.
func TestCHEventStoreLoadIsExclusiveOnFromVersion(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "ev")
	tbl := clickhouse.NewEventStoreTable(name)
	dropCH(t, db, tbl)
	execCH(t, db, clickhouse.CreateTable(tbl))

	store := clickhouse.NewEventStore(db, name)
	if err := store.Append(ctx, "cart", "cart-1", -1,
		clickhouse.EventInput{Type: "a"},
		clickhouse.EventInput{Type: "b"},
		clickhouse.EventInput{Type: "c"},
	); err != nil {
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
	// the next Append wants — which on ClickHouse is load-bearing, since
	// Append compares it itself rather than leaning on a constraint.
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
	if err := store.Append(ctx, "cart", "cart-1", head,
		clickhouse.EventInput{Type: "d"}); err != nil {
		t.Errorf("Append at LatestVersion: %v", err)
	}
	if tail, err := store.Load(ctx, "cart", "cart-1", head); err != nil || len(tail) != 1 || tail[0].Type != "d" {
		t.Errorf("Load(%d) after appending = %+v (err %v), want only d", head, tail, err)
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

// The alias's column handles must still name the table's columns.
//
// (*Table).As copies its columns so each one can render the alias as
// its qualifier, and the INSERT builder matches a row's values to its
// column list by column identity. Once the copies were strangers to
// the columns they were copied from, a caller who fixed the column
// list through the alias — the handle it holds, having just written a
// self-join — matched nothing, and every value fell through to the
// NULL that stands for "no value given". ClickHouse accepts that row;
// it is a well-formed INSERT of the wrong data. Only reading it back
// says so, which is why this test is here and not in the unit suite.
func TestCHInsertThroughAliasColumnsKeepsItsValues(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()

	tbl := clickhouse.NewTable(integration.UniqueName(t, "moves"))
	dropCH(t, db, tbl)
	game := clickhouse.Add(tbl, clickhouse.UInt64("gameId"))
	seq := clickhouse.Add(tbl, clickhouse.UInt64("seq"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(game, seq)
	execCH(t, db, clickhouse.CreateTable(tbl))

	// Columns through the alias, values through the typed handles —
	// the only way to write it, since Val lives on *Col[T] while
	// (*Table).Col answers with the type-erased *Column.
	a := tbl.As("a")
	ins := db.Insert(a).
		Columns(a.Col("gameId"), a.Col("seq")).
		Row(game.Val(7), seq.Val(2))
	if _, err := ins.Exec(ctx); err != nil {
		text, args := ins.ToSQL()
		t.Fatalf("ClickHouse rejected the INSERT: %v\n%s\nargs: %v", err, text, args)
	}

	var gotGame, gotSeq uint64
	rows, err := db.Query(ctx, `SELECT "gameId", "seq" FROM "`+tbl.Name()+`"`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !rows.Next() {
		rows.Close()
		t.Fatal("the INSERT wrote no row at all")
	}
	if err := rows.Scan(&gotGame, &gotSeq); err != nil {
		rows.Close()
		t.Fatalf("scan: %v", err)
	}
	rows.Close()
	if gotGame != 7 || gotSeq != 2 {
		text, args := ins.ToSQL()
		t.Errorf("row read back as (%d, %d), want (7, 2)\n%s\nargs: %v",
			gotGame, gotSeq, text, args)
	}
}

// ----------------------------------------------------------------------
// Introspection, diffing and Push
//
// These are the tests for clickhouse.Introspect / BuildSnapshot / Diff
// / Push, and they are all here rather than in the package's own test
// file for one reason: every claim they make is a claim about what a
// ClickHouse server does. Which columns system.tables and
// system.columns have, how the server re-spells a type or an engine
// argument when it echoes one back, whether MODIFY COLUMN … REMOVE
// CODEC parses at all — none of that can be settled by rendering a
// string. The package's unit tests pin the shape of the SQL; these
// pin that the server accepts it and reports back what drops expects.
// ----------------------------------------------------------------------

// pushSchema is the declaration the Push tests converge on: an engine
// with a parameter, a composite sorting key, a partition expression, a
// default, a codec, a comment and a setting.
func pushSchema(name string) *clickhouse.Table {
	tbl := clickhouse.NewTable(name)
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ts := clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.String("kind").LowCardinality())
	clickhouse.Add(tbl, clickhouse.Decimal("amount", 10, 2).Default("0"))
	clickhouse.Add(tbl, clickhouse.String("note").Comment("free text").Codec("ZSTD(3)"))
	clickhouse.Add(tbl, clickhouse.UInt64("version"))
	tbl.Engine(clickhouse.ReplacingMergeTree("version")).
		OrderBy(ts, id).
		PartitionBy(clickhouse.ToYYYYMM(ts)).
		Setting("index_granularity", "8192")
	return tbl
}

// The test the introspection layer exists to pass. A table drops
// created, read back through system.tables and system.columns, must
// diff to nothing against the declaration that created it.
//
// This is not a tidiness property. The server echoes a declared
// "Decimal(10,2)" as "Decimal(10, 2)", quotes identifiers only when it
// has to, wraps a codec in CODEC(…), reports an engine's arguments
// only inside engine_full, and materialises index_granularity into the
// metadata whether or not anyone asked for it. Every one of those is a
// difference a naive comparison reads as drift, and a push that reads
// drift where there is none re-emits the same statements for ever.
func TestCHIntrospectRoundTripsADeclaration(t *testing.T) {
	db := openCH(t)
	tbl := pushSchema(integration.UniqueName(t, "roundtrip"))
	dropCH(t, db, tbl)
	execCH(t, db, clickhouse.CreateTable(tbl))

	live, err := clickhouse.Introspect(context.Background(), db,
		clickhouse.IntrospectOptions{Tables: []string{tbl.Name()}})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	ts := live.Tables[tbl.Name()]
	if ts == nil {
		t.Fatalf("the table drops just created is not in %v", live.Tables)
	}
	if ts.Engine != "ReplacingMergeTree" {
		t.Errorf("engine = %q", ts.Engine)
	}
	if len(ts.EngineParams) != 1 || ts.EngineParams[0] != "version" {
		t.Errorf("engine params = %q — they live only in engine_full", ts.EngineParams)
	}
	if ts.SortingKey != "ts,id" {
		t.Errorf("sorting key = %q", ts.SortingKey)
	}
	if ts.PartitionKey != "toYYYYMM(ts)" {
		t.Errorf("partition key = %q", ts.PartitionKey)
	}
	if ts.Settings["index_granularity"] != "8192" {
		t.Errorf("settings = %v", ts.Settings)
	}
	if got := ts.Columns["note"].Codec; got != "ZSTD(3)" {
		t.Errorf("codec = %q, want the CODEC() wrapper undone", got)
	}
	if got := ts.Columns["note"].Comment; got != "free text" {
		t.Errorf("comment = %q", got)
	}
	if got := ts.Columns["amount"].Default; got != "0" {
		t.Errorf("default = %q", got)
	}
	if !ts.Columns["ts"].InSortingKey || !ts.Columns["ts"].InPartitionKey {
		t.Errorf("ts is in the sorting key and the partition key: %+v", ts.Columns["ts"])
	}
	if ts.Columns["kind"].InKey() {
		t.Errorf("kind is in no key: %+v", ts.Columns["kind"])
	}

	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(tbl)))
	if !plan.Aligned() || len(plan.Notices) != 0 {
		t.Errorf("a table diffed against the declaration that made it:\nstatements = %q\nrefusals = %v\nnotices = %v",
			plan.Statements, plan.Refusals, plan.Notices)
	}
}

// Push against an empty database creates the table; push again after
// adding a column emits the ALTER and nothing else; push a third time
// emits nothing at all. The third push is the one that matters — a
// push that is not idempotent is a push nobody can run from CI.
func TestCHPushCreatesThenEvolvesThenSettles(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "push")
	dropCH(t, db, clickhouse.NewTable(name))

	res, err := clickhouse.Push(ctx, db, clickhouse.NewSchema(pushSchema(name)))
	if err != nil {
		t.Fatalf("Push: %v (statements %q)", err, res.Statements)
	}
	if len(res.Statements) != 1 || !strings.HasPrefix(res.Statements[0], "CREATE TABLE") {
		t.Fatalf("statements = %q", res.Statements)
	}

	grown := pushSchema(name)
	clickhouse.Add(grown, clickhouse.String("source").Nullable())
	res, err = clickhouse.Push(ctx, db, clickhouse.NewSchema(grown))
	if err != nil {
		t.Fatalf("Push: %v (statements %q)", err, res.Statements)
	}
	if len(res.Statements) != 1 || !strings.Contains(res.Statements[0], "ADD COLUMN") {
		t.Fatalf("statements = %q", res.Statements)
	}

	res, err = clickhouse.Push(ctx, db, clickhouse.NewSchema(pushSchemaWithSource(name)))
	if err != nil {
		t.Fatalf("Push: %v (statements %q)", err, res.Statements)
	}
	if len(res.Statements) != 0 {
		t.Errorf("a settled schema still wants %q", res.Statements)
	}
	if len(res.Notices) != 0 {
		t.Errorf("notices = %v", res.Notices)
	}
}

// pushSchemaWithSource is pushSchema plus the column the evolution
// added, in the position the ALTER put it.
func pushSchemaWithSource(name string) *clickhouse.Table {
	tbl := pushSchema(name)
	clickhouse.Add(tbl, clickhouse.String("source").Nullable())
	return tbl
}

// The statements drops emits for the properties of a column are the
// ones this repository is least sure of, because their grammar is
// written down in the docs and nowhere else: IF EXISTS after MODIFY
// COLUMN and before REMOVE, an empty literal as the way to clear a
// comment, MODIFY TTL and REMOVE TTL on the table. This test exists to
// let the server rule on all of them at once.
func TestCHColumnPropertyStatementsParse(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "props")
	tbl := clickhouse.NewTable(name)
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ts := clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.String("note").
		Default("'unset'").
		Comment("free text").
		Codec("ZSTD(3)"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ts, id)
	dropCH(t, db, tbl)
	execCH(t, db, clickhouse.CreateTable(tbl))

	q := `ALTER TABLE "` + name + `" `
	for _, stmt := range []string{
		q + `MODIFY COLUMN IF EXISTS "note" REMOVE DEFAULT`,
		q + `MODIFY COLUMN IF EXISTS "note" REMOVE CODEC`,
		q + `COMMENT COLUMN IF EXISTS "note" ''`,
		q + `MODIFY COLUMN IF EXISTS "note" String DEFAULT 'set' COMMENT 'back again' CODEC(LZ4)`,
		q + `MODIFY TTL "ts" + INTERVAL 30 DAY`,
		q + `REMOVE TTL`,
		q + `MODIFY SETTING merge_with_ttl_timeout = 3600`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Errorf("ClickHouse rejected the statement: %v\n%s", err, stmt)
		}
	}

	// And the column really did come back to the shape the last
	// restatement asked for, rather than the statement merely parsing.
	live, err := clickhouse.IntrospectTable(ctx, db, "", name)
	if err != nil {
		t.Fatalf("IntrospectTable: %v", err)
	}
	note := live.Columns["note"]
	if note.Default != "'set'" || note.Comment != "back again" || note.Codec != "LZ4" {
		t.Errorf("note = %+v, want the restated default, comment and codec", note)
	}
	if live.TTL != "" {
		t.Errorf("REMOVE TTL left %q behind", live.TTL)
	}
}

// The premise behind every RefuseSortingKey: on a ReplacingMergeTree
// the sorting key is not a layout choice, it is the definition of "the
// same row". Two rows that share it become one; the same two rows
// under a wider key stay two. A schema tool that changed the key in
// place would be silently changing how much data the table holds.
func TestCHSortingKeyDecidesWhatOneRowIs(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()

	count := func(tbl *clickhouse.Table) uint64 {
		t.Helper()
		if _, err := db.Exec(ctx, `OPTIMIZE TABLE "`+tbl.Name()+`" FINAL`); err != nil {
			t.Fatalf("optimize: %v", err)
		}
		var n uint64
		if err := db.Select(clickhouse.CountAll()).From(tbl).One(ctx, &n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}

	narrow := clickhouse.NewTable(integration.UniqueName(t, "narrow"))
	nid := clickhouse.Add(narrow, clickhouse.UInt64("id"))
	nslug := clickhouse.Add(narrow, clickhouse.String("slug"))
	nver := clickhouse.Add(narrow, clickhouse.UInt64("version"))
	narrow.Engine(clickhouse.ReplacingMergeTree("version")).OrderBy(nid)
	dropCH(t, db, narrow)
	execCH(t, db, clickhouse.CreateTable(narrow))

	wide := clickhouse.NewTable(integration.UniqueName(t, "wide"))
	wid := clickhouse.Add(wide, clickhouse.UInt64("id"))
	wslug := clickhouse.Add(wide, clickhouse.String("slug"))
	wver := clickhouse.Add(wide, clickhouse.UInt64("version"))
	wide.Engine(clickhouse.ReplacingMergeTree("version")).OrderBy(wid, wslug)
	dropCH(t, db, wide)
	execCH(t, db, clickhouse.CreateTable(wide))

	for _, slug := range []string{"a", "b"} {
		if _, err := db.Insert(narrow).Row(nid.Val(1), nslug.Val(slug), nver.Val(1)).Exec(ctx); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := db.Insert(wide).Row(wid.Val(1), wslug.Val(slug), wver.Val(1)).Exec(ctx); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	if n := count(narrow); n != 1 {
		t.Errorf("ORDER BY (id) left %d rows, want 1 — the engine collapses on the sorting key", n)
	}
	if n := count(wide); n != 2 {
		t.Errorf("ORDER BY (id, slug) left %d rows, want 2 — the same two rows are two rows under this key", n)
	}
}

// Push refuses a sorting key that moved rather than emitting a
// statement for it, and runs nothing at all until it is told to.
func TestCHPushRefusesASortingKeyThatMoved(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "refuse")
	dropCH(t, db, clickhouse.NewTable(name))
	if _, err := clickhouse.Push(ctx, db, clickhouse.NewSchema(pushSchema(name))); err != nil {
		t.Fatalf("Push: %v", err)
	}

	moved := pushSchema(name)
	rekeyed := clickhouse.NewTable(name)
	for _, c := range moved.Columns() {
		clickhouse.Add(rekeyed, clickhouse.Custom[string](c.Name(), c.Type().TypeSQL()))
	}
	rekeyed.Engine(clickhouse.ReplacingMergeTree("version")).
		OrderBy(rekeyed.Col("id")).
		PartitionBy(clickhouse.ToYYYYMM(rekeyed.Col("ts"))).
		Setting("index_granularity", "8192")

	res, err := clickhouse.Push(ctx, db, clickhouse.NewSchema(rekeyed))
	if !errors.Is(err, clickhouse.ErrRefused) {
		t.Fatalf("err = %v, want ErrRefused", err)
	}
	if res.AppliedCount != 0 {
		t.Errorf("a refused push ran %d statements", res.AppliedCount)
	}
	var found bool
	for _, r := range res.Refusals {
		if r.Kind == clickhouse.RefuseSortingKey {
			found = true
			if !strings.Contains(r.Detail, "copy") {
				t.Errorf("the refusal has to name the remedy: %s", r.Detail)
			}
		}
	}
	if !found {
		t.Errorf("refusals = %v", res.Refusals)
	}
}

// The premise behind RefuseColumnType: ClickHouse will not ALTER a
// column that is part of a key, whatever drops or the operator thinks
// about it. If a future version starts allowing it, this test says so
// and the refusal can be relaxed rather than being carried for ever on
// a rule nobody re-checked.
func TestCHRefusesToAlterAKeyColumn(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "keycol")
	tbl := clickhouse.NewTable(name)
	id := clickhouse.Add(tbl, clickhouse.UInt32("id"))
	clickhouse.Add(tbl, clickhouse.String("payload"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(id)
	dropCH(t, db, tbl)
	execCH(t, db, clickhouse.CreateTable(tbl))

	if _, err := db.Exec(ctx, `ALTER TABLE "`+name+`" MODIFY COLUMN "id" String`); err == nil {
		t.Error("ClickHouse accepted an ALTER of a sorting-key column; the RefuseColumnType rule is now wrong")
	}
	if _, err := db.Exec(ctx, `ALTER TABLE "`+name+`" DROP COLUMN "id"`); err == nil {
		t.Error("ClickHouse accepted a DROP of a sorting-key column; the RefuseDropColumn rule is now wrong")
	}
	// And the column outside the key is fine, so the refusal is about
	// the key and not about the table having data.
	if _, err := db.Exec(ctx, `ALTER TABLE "`+name+`" MODIFY COLUMN "payload" Nullable(String)`); err != nil {
		t.Errorf("a non-key column must still be alterable: %v", err)
	}
}

// The premise behind clickhouse.LowCardinalitySafe: the wrapper is
// refused over a fixed-width type unless the server is told to allow
// it, and accepted over String. It decides whether a MODIFY COLUMN can
// carry an operator's encoding onto a new type or has to report the
// loss, so it is worth asking the server rather than the changelog.
func TestCHLowCardinalityOverANumberNeedsTheSetting(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()

	ok := clickhouse.NewTable(integration.UniqueName(t, "lcstr"))
	okID := clickhouse.Add(ok, clickhouse.UInt64("id"))
	clickhouse.Add(ok, clickhouse.Custom[string]("label", "LowCardinality(String)"))
	ok.Engine(clickhouse.MergeTree()).OrderBy(okID)
	dropCH(t, db, ok)
	execCH(t, db, clickhouse.CreateTable(ok))

	bad := clickhouse.NewTable(integration.UniqueName(t, "lcnum"))
	badID := clickhouse.Add(bad, clickhouse.UInt64("id"))
	clickhouse.Add(bad, clickhouse.Custom[string]("n", "LowCardinality(Int32)"))
	bad.Engine(clickhouse.MergeTree()).OrderBy(badID)
	dropCH(t, db, bad)

	sql, _ := drops.StringWithDialect(clickhouse.Dialect, clickhouse.CreateTable(bad))
	if _, err := db.Exec(ctx, sql); err == nil {
		t.Error("ClickHouse accepted LowCardinality(Int32) with its default settings; " +
			"clickhouse.LowCardinalitySafe is now too narrow")
	} else if !strings.Contains(err.Error(), "allow_suspicious_low_cardinality_types") {
		t.Errorf("the server refused for a different reason than drops reports to callers: %v", err)
	}
}

// The one string in a schema statement drops did not write itself. A
// comment travels as a literal in CREATE TABLE, in COMMENT COLUMN and
// inside a MODIFY COLUMN restatement, and ClickHouse's lexer reads
// C-style escapes inside a literal — so a comment ending in a
// backslash escapes its own closing quote and a comment of \' OR 1=1
// -- leaves the literal altogether. Only a server can settle which
// escapes its lexer honours, which is why the round trip is here
// rather than in the package's own tests.
func TestCHACommentCannotEscapeItsLiteral(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()
	const nasty = `C:\ and 'quoted' and \' OR 1=1 --`

	name := integration.UniqueName(t, "comments")
	tbl := clickhouse.NewTable(name)
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	clickhouse.Add(tbl, clickhouse.String("note").Comment(nasty))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(id)
	dropCH(t, db, tbl)
	// A statement the server rejects is the loud failure; a statement
	// it accepts having read something else is the quiet one, so both
	// halves are checked.
	execCH(t, db, clickhouse.CreateTable(tbl))

	live, err := clickhouse.IntrospectTable(ctx, db, "", name)
	if err != nil {
		t.Fatalf("IntrospectTable: %v", err)
	}
	if got := live.Columns["note"].Comment; got != nasty {
		t.Errorf("CREATE TABLE stored the comment as %q, want %q", got, nasty)
	}

	// And again through the statement Diff emits, which is the other
	// writer.
	changed := clickhouse.NewTable(name)
	cid := clickhouse.Add(changed, clickhouse.UInt64("id"))
	clickhouse.Add(changed, clickhouse.String("note").Comment(nasty+` \`))
	changed.Engine(clickhouse.MergeTree()).OrderBy(cid)

	res, err := clickhouse.Push(ctx, db, clickhouse.NewSchema(changed))
	if err != nil {
		t.Fatalf("Push: %v (statements %q)", err, res.Statements)
	}
	if len(res.Statements) != 1 || !strings.Contains(res.Statements[0], "COMMENT COLUMN") {
		t.Fatalf("statements = %q", res.Statements)
	}
	live, err = clickhouse.IntrospectTable(ctx, db, "", name)
	if err != nil {
		t.Fatalf("IntrospectTable: %v", err)
	}
	if got := live.Columns["note"].Comment; got != nasty+` \` {
		t.Errorf("COMMENT COLUMN stored %q, want %q", got, nasty+` \`)
	}
	// The push has to settle, or the comparison is reading the escape
	// back as drift and will re-emit the statement for ever.
	res, err = clickhouse.Push(ctx, db, clickhouse.NewSchema(changed))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Errorf("a settled comment still wants %q", res.Statements)
	}
}

// A view in the same database is not drops's to touch: the Go schema
// cannot declare one, so a diff that treated it as a table nobody
// declared would drop it.
func TestCHPushLeavesAViewAlone(t *testing.T) {
	db := openCH(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "viewbase")
	tbl := pushSchema(name)
	dropCH(t, db, tbl)
	execCH(t, db, clickhouse.CreateTable(tbl))

	view := integration.UniqueName(t, "viewof")
	t.Cleanup(func() { _, _ = db.Exec(ctx, `DROP VIEW IF EXISTS "`+view+`"`) })
	if _, err := db.Exec(ctx, `CREATE VIEW "`+view+`" AS SELECT "id" FROM "`+name+`"`); err != nil {
		t.Fatalf("create view: %v", err)
	}

	res, err := clickhouse.Push(ctx, db, clickhouse.NewSchema(tbl))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, s := range res.Statements {
		if strings.Contains(s, view) {
			t.Errorf("push touched a view it was never told about: %s", s)
		}
	}
	// And the view is still there, which is the half that a statement
	// list cannot prove on its own.
	rows, err := db.Query(ctx,
		`SELECT count() FROM system.tables WHERE database = currentDatabase() AND name = ?`, view)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var n uint64
	if !rows.Next() {
		t.Fatal("count() with no GROUP BY produces exactly one row")
	}
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if n != 1 {
		t.Errorf("the view is gone")
	}
}
