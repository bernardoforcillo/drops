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

func init() {
	events.
		Engine(clickhouse.MergeTree()).
		OrderBy(eventTS, eventUser).
		PartitionBy(clickhouse.ToYYYYMM(eventTS)).
		Setting("index_granularity", "8192")
}

// --- Helpers ---------------------------------------------------------

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
		`ORDER BY ("events"."ts", "events"."userId")`,
		`PARTITION BY (toYYYYMM("events"."ts"))`,
		`SETTINGS index_granularity = 8192`,
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
		{"SummingMergeTree(a,b)", clickhouse.SummingMergeTree("a", "b"), `SummingMergeTree("a", "b")`},
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
