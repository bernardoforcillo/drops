package clickhouse_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// Schema fixtures used throughout the test file.
var (
	events    = clickhouse.NewTable("events")
	eventID   = clickhouse.Add(events, clickhouse.UUID("id"))
	eventTS   = clickhouse.Add(events, clickhouse.DateTime("ts", "UTC"))
	eventUser = clickhouse.Add(events, clickhouse.UInt64("userId"))
	eventKind = clickhouse.Add(events, clickhouse.String("kind").LowCardinality())
	eventTags = clickhouse.Add(events, clickhouse.Custom[string]("tags", "Array(String)"))
	eventDur  = clickhouse.Add(events, clickhouse.Float64("durationMs"))
)

// eventTags is part of the representative fixture schema but not
// referenced by every test; keep it declared without tripping unused.
var _ = eventTags

func init() {
	events.
		Engine(clickhouse.MergeTree()).
		OrderBy(eventTS, eventUser).
		PartitionBy(clickhouse.ToYYYYMM(eventTS)).
		Setting("index_granularity", "8192")
}

// --- Helpers ---------------------------------------------------------

func checkExpr(t *testing.T, e drops.Expression, wantSQL string, wantArgs ...any) {
	t.Helper()
	gotSQL, gotArgs := clickhouse.ToSQL(e)
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

// --- Placeholder dialect --------------------------------------------

func TestUsesQuestionMarkPlaceholders(t *testing.T) {
	db := clickhouse.New(nil)
	q := db.Select(eventID).
		From(events).
		Where(eventUser.Eq(42), eventKind.Eq("click")).
		OrderBy(eventTS.Desc()).
		Limit(100)
	got, _ := q.ToSQL()
	if strings.Contains(got, "$") {
		t.Errorf("expected `?` placeholders, found `$N` in: %s", got)
	}
	if !strings.Contains(got, "= ?") {
		t.Errorf("expected `= ?`, got: %s", got)
	}
}

// --- DDL: CREATE TABLE -----------------------------------------------

func TestCreateTableMergeTree(t *testing.T) {
	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(events))
	for _, want := range []string{
		`CREATE TABLE "events"`,
		`"id" UUID`,
		`"ts" DateTime('UTC')`,
		`"userId" UInt64`,
		`"kind" LowCardinality(String)`,
		`"tags" Array(String)`,
		`"durationMs" Float64`,
		`ENGINE = MergeTree()`,
		`ORDER BY ("ts", "userId")`,
		`PARTITION BY (toYYYYMM("ts"))`,
		`SETTINGS index_granularity = 8192`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing fragment %q in:\n%s", want, got)
		}
	}
}

// ClickHouse's column-declaration parser reads the optional clauses in
// one fixed pass — DEFAULT, then COMMENT, then CODEC, then TTL — so a
// comment emitted after either of the last two ends the declaration
// early and the whole CREATE TABLE fails to parse.
func TestCreateTableColumnClauseOrder(t *testing.T) {
	tbl := clickhouse.NewTable("annotated")
	ts := clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.Float64("amount").
		Comment("money").
		Codec("ZSTD(3)").
		TTL("ts + INTERVAL 30 DAY"))
	clickhouse.Add(tbl, clickhouse.UInt64("n").Default("7").Comment("c").Codec("LZ4"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ts)

	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(tbl))
	for _, want := range []string{
		`"amount" Float64 COMMENT 'money' CODEC(ZSTD(3)) TTL ts + INTERVAL 30 DAY`,
		`"n" UInt64 DEFAULT 7 COMMENT 'c' CODEC(LZ4)`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing fragment %q in:\n%s", want, got)
		}
	}
}

func TestCreateTableErrReturnsErrWhenEngineMissing(t *testing.T) {
	bare := clickhouse.NewTable("no_engine")
	clickhouse.Add(bare, clickhouse.UInt32("x"))
	_, err := clickhouse.CreateTableErr(bare)
	if !errors.Is(err, clickhouse.ErrEngineRequired) {
		t.Errorf("expected ErrEngineRequired, got %v", err)
	}
}

func TestDropAndTruncateAndOptimize(t *testing.T) {
	checkExpr(t, clickhouse.DropTable(events), `DROP TABLE "events"`)
	checkExpr(t, clickhouse.DropTableIfExists(events), `DROP TABLE IF EXISTS "events"`)
	checkExpr(t, clickhouse.TruncateTable(events), `TRUNCATE TABLE "events"`)
	checkExpr(t, clickhouse.OptimizeTable(events, true), `OPTIMIZE TABLE "events" FINAL`)
	checkExpr(t, clickhouse.CreateDatabaseIfNotExists("analytics"),
		`CREATE DATABASE IF NOT EXISTS "analytics"`)
}

// --- Engine helpers --------------------------------------------------

func TestEngineConstructorsRender(t *testing.T) {
	cases := []struct {
		name string
		eng  clickhouse.Engine
		want string
	}{
		{"MergeTree", clickhouse.MergeTree(), "MergeTree()"},
		{"ReplacingMergeTree(version)", clickhouse.ReplacingMergeTree("version"), `ReplacingMergeTree("version")`},
		{"ReplacingMergeTree empty", clickhouse.ReplacingMergeTree(""), "ReplacingMergeTree()"},
		{"SummingMergeTree(a,b)", clickhouse.SummingMergeTree("a", "b"), `SummingMergeTree(("a", "b"))`},
		{"SummingMergeTree(a)", clickhouse.SummingMergeTree("a"), `SummingMergeTree(("a"))`},
		{"SummingMergeTree empty", clickhouse.SummingMergeTree(), "SummingMergeTree()"},
		{"CollapsingMergeTree", clickhouse.CollapsingMergeTree("sign"), `CollapsingMergeTree("sign")`},
		{"ReplicatedMergeTree", clickhouse.ReplicatedMergeTree("/path/foo", "{replica}"),
			"ReplicatedMergeTree('/path/foo', '{replica}')"},
		{"Memory", clickhouse.Memory(), "Memory()"},
		{"Raw", clickhouse.Raw("Distributed(c, db, t, rand())"), "Distributed(c, db, t, rand())"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := drops.NewBuilder(clickhouse.Placeholder)
			tc.eng.WriteEngine(b)
			got, _ := b.SQL()
			if got != tc.want {
				t.Errorf("engine %s: got %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// --- Operators -------------------------------------------------------

func TestOperators(t *testing.T) {
	cases := []struct {
		name string
		expr drops.Expression
		want string
		args []any
	}{
		{"eq", eventKind.Eq("click"), `("events"."kind" = ?)`, []any{"click"}},
		{"ne", eventKind.Ne("hit"), `("events"."kind" != ?)`, []any{"hit"}},
		{"gt", eventDur.Gt(0.5), `("events"."durationMs" > ?)`, []any{0.5}},
		{"in", eventKind.In("a", "b"), `("events"."kind" IN (?, ?))`, []any{"a", "b"}},
		{"between", eventDur.Between(0, 1), `("events"."durationMs" BETWEEN ? AND ?)`, []any{0.0, 1.0}},
		{"and", clickhouse.And(eventUser.Eq(1), eventKind.Eq("c")),
			`(("events"."userId" = ?) AND ("events"."kind" = ?))`,
			[]any{uint64(1), "c"}},
		{"in empty", clickhouse.In(eventKind), `(false)`, nil},
		{"not in empty", clickhouse.NotIn(eventKind), `(true)`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkExpr(t, tc.expr, tc.want, tc.args...)
		})
	}
}

// --- Aggregates / functions -----------------------------------------

func TestClickHouseSpecificAggregates(t *testing.T) {
	checkExpr(t, clickhouse.Uniq(eventUser), `uniq("events"."userId")`)
	checkExpr(t, clickhouse.UniqExact(eventUser), `uniqExact("events"."userId")`)
	checkExpr(t, clickhouse.Quantile(0.95, eventDur),
		`quantile(?)("events"."durationMs")`, 0.95)
	checkExpr(t, clickhouse.QuantileTiming(0.99, eventDur),
		`quantileTiming(?)("events"."durationMs")`, 0.99)
	checkExpr(t, clickhouse.GroupArray(eventKind), `groupArray("events"."kind")`)
	checkExpr(t, clickhouse.ArgMax(eventUser, eventTS),
		`argMax("events"."userId", "events"."ts")`)
}

// --- SELECT builder --------------------------------------------------

func TestSelectBasic(t *testing.T) {
	db := clickhouse.New(nil)
	q := db.Select(eventKind, clickhouse.As(clickhouse.CountAll(), "n")).
		From(events).
		Where(eventTS.Gte(time.Unix(0, 0))).
		GroupBy(eventKind).
		OrderBy(eventKind.Asc()).
		Limit(10)
	got, args := q.ToSQL()
	want := `SELECT "events"."kind", count() AS "n" FROM "events" WHERE ("events"."ts" >= ?) GROUP BY "events"."kind" ORDER BY "events"."kind" ASC LIMIT ?`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %v", args)
	}
}

func TestSelectFinalSamplePrewhereSettings(t *testing.T) {
	db := clickhouse.New(nil)
	q := db.Select(eventID).
		From(events).Final().
		SampleBy(0.1).
		Prewhere(eventKind.Eq("click")).
		Where(eventDur.Gt(0.5)).
		Setting("max_threads", "4")
	got, _ := q.ToSQL()
	for _, want := range []string{
		`FROM "events" FINAL`,
		` SAMPLE ?`,
		` PREWHERE ("events"."kind" = ?)`,
		` WHERE ("events"."durationMs" > ?)`,
		` SETTINGS max_threads = 4`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
}

func TestSelectJoinKinds(t *testing.T) {
	users := clickhouse.NewTable("users")
	clickhouse.Add(users, clickhouse.UInt64("id"))
	users.Engine(clickhouse.MergeTree()).OrderBy(eventUser)

	db := clickhouse.New(nil)
	q := db.Select().From(events).
		AnyJoin(users, eventUser.EqCol(eventUser)).
		Limit(1)
	got, _ := q.ToSQL()
	if !strings.Contains(got, "ANY INNER JOIN") {
		t.Errorf("expected ANY INNER JOIN in: %s", got)
	}
}

// --- INSERT builder --------------------------------------------------

func TestInsertSingleRow(t *testing.T) {
	db := clickhouse.New(nil)
	q := db.Insert(events).Row(
		eventID.Val("00000000-0000-0000-0000-000000000001"),
		eventUser.Val(42),
		eventKind.Val("click"),
		eventDur.Val(0.25),
	)
	got, args := q.ToSQL()
	want := `INSERT INTO "events" ("id", "userId", "kind", "durationMs") VALUES (?, ?, ?, ?)`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}
	if len(args) != 4 {
		t.Errorf("expected 4 args, got %v", args)
	}
}

// An aliased handle is what a caller holds once it has written a
// self-join, and INSERT has no alias position in ClickHouse's grammar.
func TestInsertIgnoresTableAlias(t *testing.T) {
	db := clickhouse.New(nil)
	got, _ := db.Insert(events.As("e")).Row(eventUser.Val(42)).ToSQL()
	want := `INSERT INTO "events" ("userId") VALUES (?)`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}
}

func TestInsertBatchAlignsAndDefaultsNull(t *testing.T) {
	db := clickhouse.New(nil)
	q := db.Insert(events).
		Row(eventUser.Val(1), eventKind.Val("a")).
		Row(eventUser.Val(2)) // kind missing → NULL
	got, _ := q.ToSQL()
	if !strings.Contains(got, "VALUES (?, ?), (?, NULL)") {
		t.Errorf("expected null fill, got: %s", got)
	}
}

func TestInsertEmptyReturnsSentinel(t *testing.T) {
	_, err := clickhouse.New(nil).Insert(events).Exec(context.Background())
	if !errors.Is(err, clickhouse.ErrNoRowsToInsert) {
		t.Errorf("expected ErrNoRowsToInsert, got %v", err)
	}
}

// --- Identifier validation -------------------------------------------

func TestNewTablePanicsOnInvalidName(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, clickhouse.ErrInvalidIdentifier) {
			t.Errorf("expected ErrInvalidIdentifier, got %v", r)
		}
	}()
	clickhouse.NewTable("")
}

func TestColumnPanicsOnNUL(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, clickhouse.ErrInvalidIdentifier) {
			t.Errorf("expected ErrInvalidIdentifier, got %v", r)
		}
	}()
	clickhouse.String("bad\x00name")
}

// --- Type-system wrappers --------------------------------------------

func TestTypeWrappers(t *testing.T) {
	cases := []struct {
		name string
		c    interface{ Type() clickhouse.ColumnType }
		want string
	}{
		{"Array(String)", clickhouse.Custom[string]("tags", "").Column, "Array(String)"},
		{"Nullable wraps", clickhouse.String("opt").Nullable(), "Nullable(String)"},
		{"LowCardinality wraps", clickhouse.String("kind").LowCardinality(), "LowCardinality(String)"},
		{"Decimal(10,2)", clickhouse.Decimal("amount", 10, 2), "Decimal(10, 2)"},
		{"DateTime tz", clickhouse.DateTime("ts", "UTC"), "DateTime('UTC')"},
		{"DateTime64", clickhouse.DateTime64("ts", 3, "UTC"), "DateTime64(3, 'UTC')"},
		{"Enum8", &dummyType{t: clickhouse.TypeEnum8(map[string]int8{"a": 1, "b": 2})},
			"Enum8('a' = 1, 'b' = 2)"},
	}
	// Replace the first case's expectation: we passed an empty typeSQL
	// to Custom, but we can't easily recover Array() without help.
	// Use a dedicated check.
	customCol := clickhouse.Custom[string]("tags", "Array(String)")
	if got := customCol.Type().TypeSQL(); got != "Array(String)" {
		t.Errorf("Custom Array: got %q", got)
	}
	for _, tc := range cases[1:] {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.Type().TypeSQL(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// dummyType wraps a raw ColumnType so it satisfies the test helper
// interface ({ Type() ColumnType }).
type dummyType struct{ t clickhouse.ColumnType }

func (d *dummyType) Type() clickhouse.ColumnType { return d.t }

// --- Hook + Ping + Close --------------------------------------------

type fakeDriver struct {
	queries []string
	closed  bool
}

type fakeResult struct{}

func (fakeResult) RowsAffected() (int64, error) { return 0, nil }

type fakeRows struct{}

func (*fakeRows) Next() bool                 { return false }
func (*fakeRows) Scan(...any) error          { return nil }
func (*fakeRows) Columns() ([]string, error) { return nil, nil }
func (*fakeRows) Close() error               { return nil }
func (*fakeRows) Err() error                 { return nil }
func (f *fakeDriver) Close() error           { f.closed = true; return nil }
func (f *fakeDriver) Exec(_ context.Context, sql string, _ ...any) (drops.Result, error) {
	f.queries = append(f.queries, sql)
	return fakeResult{}, nil
}
func (f *fakeDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	f.queries = append(f.queries, sql)
	return &fakeRows{}, nil
}
func (f *fakeDriver) Begin(_ context.Context) (drops.Tx, error) {
	return &fakeTx{fakeDriver: f}, nil
}

type fakeTx struct{ *fakeDriver }

func (*fakeTx) Commit(_ context.Context) error   { return nil }
func (*fakeTx) Rollback(_ context.Context) error { return nil }

func TestHookFiresOnExecAndPing(t *testing.T) {
	var seen []string
	hook := func(_ context.Context, e drops.QueryEvent) { seen = append(seen, e.Kind) }
	fd := &fakeDriver{}
	db := clickhouse.New(fd).WithHook(hook)
	ctx := context.Background()
	if _, err := db.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.InTx(ctx, func(*clickhouse.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "ping", "begin", "commit"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("hook events: got %v, want %v", seen, want)
	}
}

func TestCloseDelegates(t *testing.T) {
	fd := &fakeDriver{}
	db := clickhouse.New(fd)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if !fd.closed {
		t.Error("driver Close was not called")
	}
}

// The event store writes through one quoting helper and reads through
// another until the two agreed; a name carrying a quote is where they
// part company.
func TestEventStoreQuotesTableNameOnEveryPath(t *testing.T) {
	fd := &fakeDriver{}
	db := clickhouse.New(fd)
	store := clickhouse.NewEventStore(db, `ev"log`)
	ctx := context.Background()

	if err := store.Append(ctx, "match", "abc", -1,
		clickhouse.EventInput{Type: "started", Payload: map[string]int{"a": 1}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := store.Load(ctx, "match", "abc", -1); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := store.Stream(ctx, 0, 10); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := store.SaveSnapshot(ctx, `ev"log`, clickhouse.AggregateSnapshot{
		AggregateType: "match", AggregateID: "abc",
	}); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if _, _, err := store.LoadSnapshot(ctx, `ev"log`, "match", "abc"); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	if len(fd.queries) != 6 {
		t.Fatalf("expected six statements, got %d: %v", len(fd.queries), fd.queries)
	}
	for _, q := range fd.queries {
		if !strings.Contains(q, `ev""log`) {
			t.Errorf("table name not escaped in:\n%s", q)
		}
	}
}

// LatestVersion has to report -1 for a stream nobody has written, and
// Append's expectedVersion of -1 is only reachable when it does.
func TestEventStoreEmptyStreamVersionIsMinusOne(t *testing.T) {
	fd := &fakeDriver{}
	store := clickhouse.NewEventStore(clickhouse.New(fd), "events")
	v, err := store.LatestVersion(context.Background(), "match", "abc")
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if v != -1 {
		t.Errorf("version = %d, want -1", v)
	}
	if !strings.Contains(fd.queries[0], "maxOrNull") {
		t.Errorf("an empty ClickHouse aggregate answers with the type default, not NULL:\n%s", fd.queries[0])
	}
}

// --- Table aliases ---------------------------------------------------
//
// (*Table).As used to hand back a shallow copy, so the aliased handle's
// columns still pointed at the un-aliased table and every reference
// through it rendered "events"."id" under a FROM that had renamed the
// relation to "e". ClickHouse resolves identifiers against what the
// FROM clause put in scope, so those references name a relation the
// query no longer has. The tests below walk each clause that can carry
// a column reference, because an alias that works in FROM and breaks in
// JOIN is the same defect wearing a different hat.

func TestAliasQualifiesEveryClauseWithTheAlias(t *testing.T) {
	db := clickhouse.New(nil)
	e := events.As("e")

	got, _ := db.Select(e.Col("id")).
		From(e).
		Prewhere(clickhouse.Eq(e.Col("kind"), "click")).
		Where(clickhouse.Gt(e.Col("durationMs"), 1.0)).
		GroupBy(e.Col("userId")).
		Having(clickhouse.Gt(clickhouse.CountAll(), 1)).
		OrderBy(e.Col("ts").Desc()).
		ToSQL()

	want := `SELECT "e"."id" FROM "events" AS "e"` +
		` PREWHERE ("e"."kind" = ?) WHERE ("e"."durationMs" > ?)` +
		` GROUP BY "e"."userId" HAVING (count() > ?)` +
		` ORDER BY "e"."ts" DESC`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}
}

// The headline case: two handles on one table, both addressable at
// once. Before the fix both sides rendered "events"."userId" and the ON
// clause was the tautology x = x.
func TestSelfJoinAddressesBothSides(t *testing.T) {
	db := clickhouse.New(nil)
	a, b := events.As("a"), events.As("b")

	got, _ := db.Select(a.Col("id"), b.Col("id")).
		From(a).
		Join(b, clickhouse.Eq(a.Col("userId"), b.Col("userId"))).
		Where(clickhouse.Lt(a.Col("ts"), b.Col("ts"))).
		ToSQL()

	want := `SELECT "a"."id", "b"."id" FROM "events" AS "a"` +
		` INNER JOIN "events" AS "b" ON ("a"."userId" = "b"."userId")` +
		` WHERE ("a"."ts" < "b"."ts")`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}
}

// A typed handle on one side and an aliased handle on the other is the
// ordinary shape of a self-join predicate, so the *Col comparison
// methods take a ColRef.
func TestColComparisonAcceptsAnAliasedHandle(t *testing.T) {
	e := events.As("e")
	checkExpr(t, eventUser.EqCol(e.Col("userId")),
		`("events"."userId" = "e"."userId")`)
	checkExpr(t, eventTS.LtCol(e.Col("ts")),
		`("events"."ts" < "e"."ts")`)
}

func TestAliasSurvivesSubqueryAndCTE(t *testing.T) {
	db := clickhouse.New(nil)
	e := events.As("e")

	sub := db.Select(e.Col("userId")).From(e).Where(clickhouse.Eq(e.Col("kind"), "signup"))
	got, _ := db.Select(eventID).From(events).
		Where(clickhouse.InSub(eventUser, sub)).
		ToSQL()
	want := `SELECT "events"."id" FROM "events"` +
		` WHERE ("events"."userId" IN (SELECT "e"."userId" FROM "events" AS "e" WHERE ("e"."kind" = ?)))`
	if got != want {
		t.Errorf("subquery sql\n  got:  %s\n  want: %s", got, want)
	}

	cte := clickhouse.CTEDef("recent", db.Select(e.Col("id")).From(e))
	got, _ = db.Select().From(events).With(cte).ToSQL()
	want = `WITH "recent" AS (SELECT "e"."id" FROM "events" AS "e") SELECT * FROM "events"`
	if got != want {
		t.Errorf("cte sql\n  got:  %s\n  want: %s", got, want)
	}
}

// The alias copy owns its columns, so nothing it does can reach the
// package-level handles — the point of the copy is that both are live
// at the same time.
func TestAliasLeavesTheOriginalHandlesAlone(t *testing.T) {
	db := clickhouse.New(nil)
	e := events.As("e")

	if e.Col("id") == eventID.Column {
		t.Error("aliased Col returned the original column pointer")
	}
	if e.Col("id").Table() != e {
		t.Error("aliased column is not bound to the aliased table")
	}
	if eventID.Column.Table() != events {
		t.Error("As rebound the original column")
	}
	got, _ := db.Select(eventID).From(events).ToSQL()
	if want := `SELECT "events"."id" FROM "events"`; got != want {
		t.Errorf("original handle\n  got:  %s\n  want: %s", got, want)
	}
	_ = e
}

// CREATE TABLE names the relation, never the alias — the alias exists
// only in a query's scope.
func TestAliasDoesNotReachDDL(t *testing.T) {
	tbl := clickhouse.NewTable("t")
	ts := clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.UInt64("n"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ts)

	got, _ := clickhouse.ToSQL(clickhouse.CreateTable(tbl.As("x")))
	if strings.Contains(got, `"x"`) {
		t.Errorf("alias leaked into DDL: %s", got)
	}
	if !strings.Contains(got, `CREATE TABLE "t"`) || !strings.Contains(got, "ORDER BY (\"ts\")") {
		t.Errorf("unexpected DDL: %s", got)
	}
}

// A default filter is built against the un-aliased columns and is not
// rewritten by As — see the method's doc. Dropping it would silence a
// tenant or soft-delete guard, so it is left in place and the server
// rejects the query; an aliased SELECT wants Unscoped and an explicit
// predicate. Pin the decision so a future change to it is deliberate.
func TestAliasDoesNotRewriteDefaultFilters(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("scoped")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	tenant := clickhouse.Add(tbl, clickhouse.UInt64("tenantId"))
	tbl.DefaultFilter(tenant.Eq(7))

	s := tbl.As("s")
	got, _ := db.Select(s.Col("id")).From(s).ToSQL()
	want := `SELECT "s"."id" FROM "scoped" AS "s" WHERE ("scoped"."tenantId" = ?)`
	if got != want {
		t.Errorf("scoped\n  got:  %s\n  want: %s", got, want)
	}

	got, _ = db.Select(s.Col("id")).From(s).Unscoped().
		Where(clickhouse.Eq(s.Col("tenantId"), 7)).ToSQL()
	want = `SELECT "s"."id" FROM "scoped" AS "s" WHERE ("s"."tenantId" = ?)`
	if got != want {
		t.Errorf("unscoped\n  got:  %s\n  want: %s", got, want)
	}
	_ = id
}

func TestAliasPanicsOnInvalidIdentifier(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic")
		}
		err, ok := r.(error)
		if !ok || !errors.Is(err, clickhouse.ErrInvalidIdentifier) {
			t.Errorf("expected ErrInvalidIdentifier, got %v", r)
		}
	}()
	events.As("")
}

// The alias owns a copy of the column list, so it is a snapshot of the
// table as it stood — which is the price of the columns being separate
// objects at all.
func TestAliasSnapshotsTheColumnList(t *testing.T) {
	tbl := clickhouse.NewTable("late")
	clickhouse.Add(tbl, clickhouse.UInt64("id"))
	e := tbl.As("e")
	clickhouse.Add(tbl, clickhouse.String("added"))

	if got := e.Col("added"); got != nil {
		t.Errorf("alias saw a column added after As: %v", got.Name())
	}
	if len(e.Columns()) != 1 || len(tbl.Columns()) != 2 {
		t.Errorf("alias has %d columns, table has %d; want 1 and 2",
			len(e.Columns()), len(tbl.Columns()))
	}
}

// An alias is a query-scope rename, so the handle it hands out has to
// keep meaning the same column everywhere a column is identified
// rather than rendered. Once As started copying columns, the copies
// were strangers to the originals by pointer, and the two places that
// match columns by pointer both failed quietly: alignRow dropped the
// row's values onto its NULL fill, and an insert hook re-bound a
// column that was already bound. Neither raises anything — the first
// writes a row of NULLs, the second writes the column name twice.
func TestAliasedHandleStillNamesTheSameColumnInInsert(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("moves")
	game := clickhouse.Add(tbl, clickhouse.UInt64("gameId"))
	seq := clickhouse.Add(tbl, clickhouse.UInt64("seq"))

	// Column list fixed through the alias, values through the typed
	// handles — the only way to write it, since Val lives on *Col[T]
	// and (*Table).Col answers with the type-erased *Column.
	a := tbl.As("a")
	got, args := db.Insert(a).
		Columns(a.Col("gameId"), a.Col("seq")).
		Row(game.Val(7), seq.Val(2)).
		ToSQL()
	want := `INSERT INTO "moves" ("gameId", "seq") VALUES (?, ?)`
	if got != want {
		t.Errorf("sql\n  got:  %s\n  want: %s", got, want)
	}
	if len(args) != 2 || args[0] != uint64(7) || args[1] != uint64(2) {
		t.Errorf("args = %v, want [7 2] — the row lost its values to the NULL fill", args)
	}

	// Re-aliasing has to collapse onto the declared column, not onto
	// the intermediate alias, or the second hop is a stranger again.
	c := tbl.As("a").As("b")
	_, args = db.Insert(c).Columns(c.Col("gameId")).Row(game.Val(7)).ToSQL()
	if len(args) != 1 || args[0] != uint64(7) {
		t.Errorf("re-aliased args = %v, want [7]", args)
	}
}

// The same identity, seen by an insert hook. The hook holds the
// declared handle; the caller bound the aliased one. Has must say the
// column is taken, or the hook appends a second binding for it.
func TestInsertHookSeesAnAliasedBindingAsBound(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("audited")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	at := clickhouse.Add(tbl, clickhouse.DateTime("insertedAt", "UTC"))
	tbl.OnInsert(clickhouse.InsertHookFunc(func(ctx *clickhouse.InsertHookCtx) {
		ctx.SetExpr(at.Column, drops.Raw("now()"))
	}))

	a := tbl.As("a")
	got, _ := db.Insert(a).
		Columns(a.Col("id"), a.Col("insertedAt")).
		Row(id.Val(1), at.Val(time.Unix(0, 0).UTC())).
		ToSQL()
	if strings.Count(got, `"insertedAt"`) != 1 {
		t.Errorf("hook re-bound a column the caller had already bound:\n%s", got)
	}
	if strings.Contains(got, "now()") {
		t.Errorf("hook overwrote an explicit binding:\n%s", got)
	}
}
