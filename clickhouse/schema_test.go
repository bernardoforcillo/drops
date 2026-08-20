package clickhouse_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// --- the fixture -----------------------------------------------------

// schemaTable is the declaration the whole file diffs against: an
// engine with a parameter, a composite sorting key, a partition
// expression, a default, a codec, a comment and a setting. Every one
// of those is a field a diff has to read back in the server's own
// spelling or churn on for ever.
func schemaTable() *clickhouse.Table {
	t := clickhouse.NewTable("events")
	id := clickhouse.Add(t, clickhouse.UUID("id"))
	ts := clickhouse.Add(t, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(t, clickhouse.String("kind").LowCardinality())
	clickhouse.Add(t, clickhouse.Decimal("amount", 10, 2).Default("0"))
	clickhouse.Add(t, clickhouse.String("note").Comment("free text").Codec("ZSTD(3)"))
	clickhouse.Add(t, clickhouse.UInt64("version"))
	t.Engine(clickhouse.ReplacingMergeTree("version")).
		OrderBy(ts, id).
		PartitionBy(clickhouse.ToYYYYMM(ts)).
		Setting("index_granularity", "8192")
	return t
}

// schemaLiveRows is what a ClickHouse server reports for the table
// schemaTable declares — in the server's spelling, not drops's:
// identifiers unquoted, "Decimal(10, 2)" with the space ClickHouse
// puts there, the codec wrapped in CODEC(…), and the engine's
// arguments only reachable through engine_full.
func schemaLiveRows() ([][]any, [][]any) {
	tables := [][]any{{
		"events",
		"ReplacingMergeTree",
		"ReplacingMergeTree(version) PARTITION BY toYYYYMM(ts) ORDER BY (ts, id) SETTINGS index_granularity = 8192",
		"toYYYYMM(ts)",
		"ts, id",
		"ts, id",
		"",
	}}
	// table, name, type, position, default_kind, default_expression,
	// comment, compression_codec, is_in_sorting_key,
	// is_in_primary_key, is_in_partition_key, is_in_sampling_key
	columns := [][]any{
		{"events", "id", "UUID", uint64(1), "", "", "", "", uint8(1), uint8(1), uint8(0), uint8(0)},
		{"events", "ts", "DateTime('UTC')", uint64(2), "", "", "", "", uint8(1), uint8(1), uint8(1), uint8(0)},
		{"events", "kind", "LowCardinality(String)", uint64(3), "", "", "", "", uint8(0), uint8(0), uint8(0), uint8(0)},
		{"events", "amount", "Decimal(10, 2)", uint64(4), "DEFAULT", "0", "", "", uint8(0), uint8(0), uint8(0), uint8(0)},
		{"events", "note", "String", uint64(5), "", "", "free text", "CODEC(ZSTD(3))", uint8(0), uint8(0), uint8(0), uint8(0)},
		{"events", "version", "UInt64", uint64(6), "", "", "", "", uint8(0), uint8(0), uint8(0), uint8(0)},
	}
	return tables, columns
}

// --- the fake server -------------------------------------------------

// schemaDriver answers the two system-table reads an introspection
// issues from canned rows, and records every statement a push runs.
// Recording the SQL rather than a parsed form is deliberate: a
// statement that quietly stopped saying IF NOT EXISTS still applies
// cleanly against a fake, and only the text shows it.
type schemaDriver struct {
	tables  [][]any
	columns [][]any

	queries []string
	execs   []string

	queryErr error
	execErr  error
}

func (d *schemaDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	if strings.Contains(sql, "system.tables") {
		return &schemaRows{data: d.tables}, nil
	}
	return &schemaRows{data: d.columns}, nil
}

func (d *schemaDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.execs = append(d.execs, sql)
	if d.execErr != nil {
		return nil, d.execErr
	}
	return schemaResult{}, nil
}

func (d *schemaDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

type schemaResult struct{}

func (schemaResult) RowsAffected() (int64, error) { return 0, nil }

type schemaRows struct {
	data [][]any
	pos  int
}

func (r *schemaRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *schemaRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		reflect.ValueOf(d).Elem().Set(reflect.ValueOf(row[i]))
	}
	return nil
}

func (r *schemaRows) Columns() ([]string, error) { return nil, nil }
func (r *schemaRows) Close() error               { return nil }
func (r *schemaRows) Err() error                 { return nil }

func liveSnapshot(t *testing.T) *clickhouse.Snapshot {
	t.Helper()
	tables, columns := schemaLiveRows()
	db := clickhouse.New(&schemaDriver{tables: tables, columns: columns})
	snap, err := clickhouse.Introspect(context.Background(), db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	return snap
}

// --- snapshots -------------------------------------------------------

// cloneFixture rebuilds the fixture column by column so a test can
// drop or reshape one of them without touching the shared declaration.
// edit returns false to leave the column out; a nil edit keeps them
// all, which makes the clone a copy.
func cloneFixture(edit func(name string, col *clickhouse.Col[string]) bool) *clickhouse.Table {
	out := clickhouse.NewTable("events")
	for _, c := range schemaTable().Columns() {
		col := clickhouse.Custom[string](c.Name(), c.Type().TypeSQL())
		if c.HasDefault() {
			col.Default(c.DefaultSQL())
		}
		if c.Codec() != "" {
			col.Codec(c.Codec())
		}
		if c.Comment() != "" {
			col.Comment(c.Comment())
		}
		if edit != nil && !edit(c.Name(), col) {
			continue
		}
		clickhouse.Add(out, col)
	}
	restoreShape(out)
	return out
}

func TestBuildSnapshotRecordsTheWholeShape(t *testing.T) {
	snap := clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable()))
	ts := snap.Tables["events"]
	if ts == nil {
		t.Fatalf("no table in %+v", snap.Tables)
	}
	if ts.Engine != "ReplacingMergeTree" {
		t.Errorf("engine = %q", ts.Engine)
	}
	if !reflect.DeepEqual(ts.EngineParams, []string{"version"}) {
		t.Errorf("engine params = %q — the version column is what decides which duplicate survives", ts.EngineParams)
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
	if got := ts.Columns["amount"].Type; got != "Decimal(10,2)" {
		t.Errorf("amount type = %q, want the normalised spelling", got)
	}
	if got := ts.Columns["note"].Codec; got != "ZSTD(3)" {
		t.Errorf("note codec = %q", got)
	}
	if !ts.Columns["ts"].InSortingKey || !ts.Columns["ts"].InPartitionKey {
		t.Errorf("ts is in both keys and the snapshot says %+v", ts.Columns["ts"])
	}
	if ts.Columns["kind"].InKey() {
		t.Errorf("kind is in no key: %+v", ts.Columns["kind"])
	}
}

// A declaration with ORDER BY and no PRIMARY KEY has the sorting key
// as its primary key — that is what ClickHouse stores and what
// system.tables reports — so the snapshot has to say the same or every
// push proposes rebuilding the table for a key nobody changed.
func TestPrimaryKeyDefaultsToTheSortingKey(t *testing.T) {
	ts := clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())).Tables["events"]
	if ts.PrimaryKey != ts.SortingKey {
		t.Errorf("primary key = %q, sorting key = %q", ts.PrimaryKey, ts.SortingKey)
	}
}

func TestSnapshotSurvivesJSON(t *testing.T) {
	snap := clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable()))
	body, err := snap.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := clickhouse.UnmarshalSnapshot(body)
	if err != nil {
		t.Fatalf("UnmarshalSnapshot: %v", err)
	}
	if plan := clickhouse.Diff(back, snap); !plan.Aligned() {
		t.Errorf("a snapshot that went through JSON differs from itself: %+v", plan)
	}
}

// --- introspection ---------------------------------------------------

func TestIntrospectReadsBothSystemTables(t *testing.T) {
	tables, columns := schemaLiveRows()
	d := &schemaDriver{tables: tables, columns: columns}
	snap, err := clickhouse.Introspect(context.Background(), clickhouse.New(d), clickhouse.IntrospectOptions{
		Tables: []string{"events"},
	})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if len(d.queries) != 2 {
		t.Fatalf("queries = %v", d.queries)
	}
	for _, want := range []string{"system.tables", "engine_full", "sorting_key", "currentDatabase()", "name IN (?)"} {
		if !strings.Contains(d.queries[0], want) {
			t.Errorf("the system.tables read is missing %q: %s", want, d.queries[0])
		}
	}
	for _, want := range []string{
		"system.columns", "is_in_sorting_key", "is_in_partition_key",
		"compression_codec", "default_expression", "table IN (?)", "ORDER BY table, position",
	} {
		if !strings.Contains(d.queries[1], want) {
			t.Errorf("the system.columns read is missing %q: %s", want, d.queries[1])
		}
	}

	ts := snap.Tables["events"]
	if ts == nil {
		t.Fatalf("no table in %+v", snap.Tables)
	}
	if !reflect.DeepEqual(ts.EngineParams, []string{"version"}) {
		t.Errorf("engine params = %q — they are only in engine_full", ts.EngineParams)
	}
	if ts.Settings["index_granularity"] != "8192" {
		t.Errorf("settings = %v — they are only in engine_full", ts.Settings)
	}
	if got := ts.Columns["note"].Codec; got != "ZSTD(3)" {
		t.Errorf("codec = %q, want the CODEC() wrapper undone so it matches a declaration", got)
	}
	if got := ts.Columns["amount"].Position; got != 4 {
		t.Errorf("position = %d, want 4 — an ADD COLUMN is positioned against it", got)
	}
}

func TestIntrospectScopesToTheNamedDatabase(t *testing.T) {
	tables, columns := schemaLiveRows()
	d := &schemaDriver{tables: tables, columns: columns}
	snap, err := clickhouse.Introspect(context.Background(), clickhouse.New(d),
		clickhouse.IntrospectOptions{Database: "analytics"})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if strings.Contains(d.queries[0], "currentDatabase()") {
		t.Errorf("a named database must be bound, not assumed: %s", d.queries[0])
	}
	if _, ok := snap.Tables["analytics.events"]; !ok {
		t.Errorf("tables = %v, want the qualified key", snap.Tables)
	}
}

func TestIntrospectReadsTheTTLOutOfEngineFull(t *testing.T) {
	tables, columns := schemaLiveRows()
	tables[0][2] = "MergeTree PARTITION BY toYYYYMM(ts) ORDER BY (ts, id) " +
		"TTL ts + toIntervalDay(30) SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1"
	tables[0][1] = "MergeTree"
	snap, err := clickhouse.Introspect(context.Background(),
		clickhouse.New(&schemaDriver{tables: tables, columns: columns}))
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	ts := snap.Tables["events"]
	if ts.TTL != "ts + toIntervalDay(30)" {
		t.Errorf("TTL = %q — it stops at SETTINGS, which is always last", ts.TTL)
	}
	if ts.Settings["ttl_only_drop_parts"] != "1" || ts.Settings["index_granularity"] != "8192" {
		t.Errorf("settings = %v", ts.Settings)
	}
	if len(ts.EngineParams) != 0 {
		t.Errorf("engine params = %v, want none — MergeTree takes no arguments", ts.EngineParams)
	}
}

// --- the round trip --------------------------------------------------

// The test the whole file exists for. A declaration pushed once and
// read back has to produce an empty diff, or every later push re-emits
// the same statements: the server spells identifiers, types, codecs
// and engine arguments its own way, and a comparison that treats the
// server's formatting as drift never settles.
func TestALivelyReadTableMatchesItsDeclaration(t *testing.T) {
	plan := clickhouse.Diff(liveSnapshot(t), clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
	if !plan.Aligned() {
		t.Errorf("statements = %q\nrefusals = %v", plan.Statements, plan.Refusals)
	}
	if len(plan.Notices) != 0 {
		t.Errorf("notices = %v", plan.Notices)
	}
}

// --- diffs that produce statements -----------------------------------

func diffOf(t *testing.T, mutate func(*clickhouse.Table)) clickhouse.Plan {
	t.Helper()
	live := liveSnapshot(t)
	declared := schemaTable()
	mutate(declared)
	return clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(declared)))
}

func onlyStatement(t *testing.T, plan clickhouse.Plan) string {
	t.Helper()
	if len(plan.Statements) != 1 {
		t.Fatalf("statements = %q; refusals = %v", plan.Statements, plan.Refusals)
	}
	return plan.Statements[0]
}

func TestDiffAddsAColumnAfterItsNeighbour(t *testing.T) {
	plan := diffOf(t, func(tbl *clickhouse.Table) {
		clickhouse.Add(tbl, clickhouse.String("source").Nullable())
	})
	got := onlyStatement(t, plan)
	if !strings.Contains(got, `ADD COLUMN "source" Nullable(String)`) {
		t.Errorf("statement = %s", got)
	}
	// The position clause is not housekeeping: without it ClickHouse
	// appends, and a column added to the middle of a declaration lands
	// somewhere nobody chose.
	if !strings.HasSuffix(got, ` AFTER "version"`) {
		t.Errorf("statement does not position the column: %s", got)
	}
}

func TestDiffPutsALeadingColumnFirst(t *testing.T) {
	live := liveSnapshot(t)
	declared := clickhouse.NewTable("events")
	clickhouse.Add(declared, clickhouse.String("tenant"))
	for _, c := range schemaTable().Columns() {
		clickhouse.Add(declared, clickhouse.Custom[string](c.Name(), c.Type().TypeSQL()))
	}
	id, ts := declared.Col("id"), declared.Col("ts")
	declared.Engine(clickhouse.ReplacingMergeTree("version")).
		OrderBy(ts, id).
		PartitionBy(clickhouse.ToYYYYMM(ts)).
		Setting("index_granularity", "8192")

	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(declared)))
	var add string
	for _, s := range plan.Statements {
		if strings.Contains(s, "ADD COLUMN") {
			add = s
		}
	}
	if !strings.HasSuffix(add, " FIRST") {
		t.Errorf("statement = %s, want FIRST — there is no column to go after", add)
	}
}

func TestDiffDropsAColumnOutsideEveryKey(t *testing.T) {
	trimmed := cloneFixture(func(name string, _ *clickhouse.Col[string]) bool { return name != "kind" })
	got := onlyStatement(t, clickhouse.Diff(liveSnapshot(t), clickhouse.BuildSnapshot(clickhouse.NewSchema(trimmed))))
	if got != `ALTER TABLE "events" DROP COLUMN "kind"` {
		t.Errorf("statement = %s", got)
	}
}

// restoreShape puts the fixture's engine and keys back on a table
// rebuilt column by column, so that a test about columns is not also a
// test about the engine.
func restoreShape(t *clickhouse.Table) {
	t.Engine(clickhouse.ReplacingMergeTree("version")).
		OrderBy(t.Col("ts"), t.Col("id")).
		PartitionBy(clickhouse.ToYYYYMM(t.Col("ts"))).
		Setting("index_granularity", "8192")
}

// A default that goes needs its own REMOVE statement, and it has to
// run before the restatement: ClickHouse's MODIFY COLUMN takes a whole
// column declaration and drops does not claim to know whether a clause
// left out of one is preserved or reset. Removing first reaches the
// declared shape under either reading.
func TestDiffRemovesADefaultBeforeRestating(t *testing.T) {
	declared := cloneFixture(func(name string, col *clickhouse.Col[string]) bool {
		if name == "amount" {
			// Same type, no default: the only difference is the
			// clause that has to come off.
			col.Default("")
		}
		return true
	})
	plan := clickhouse.Diff(liveSnapshot(t), clickhouse.BuildSnapshot(clickhouse.NewSchema(declared)))
	// One statement, not two: the REMOVE says everything a
	// restatement would have.
	got := onlyStatement(t, plan)
	if got != `ALTER TABLE "events" MODIFY COLUMN "amount" REMOVE DEFAULT` {
		t.Errorf("statement = %s", got)
	}
}

func TestDiffWidensAColumnWithARestatement(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].Columns["version"].Type = "UInt32"
	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
	got := onlyStatement(t, plan)
	if got != `ALTER TABLE "events" MODIFY COLUMN "version" UInt64` {
		t.Errorf("statement = %s", got)
	}
}

// A LowCardinality wrapper someone added by hand is an encoding, not a
// value domain, and a declaration says nothing about encodings — so it
// is not drift and the diff leaves it alone.
func TestDiffLeavesAHandAddedLowCardinalityAlone(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].Columns["note"].Type = "LowCardinality(String)"
	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
	if len(plan.Statements) != 0 {
		t.Errorf("statements = %q", plan.Statements)
	}
}

func TestDiffSetsAndClearsAComment(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].Columns["note"].Comment = "stale"
	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
	if got := onlyStatement(t, plan); got != `ALTER TABLE "events" COMMENT COLUMN "note" 'free text'` {
		t.Errorf("statement = %s", got)
	}

	// And an empty literal is how a comment is removed — there is no
	// statement that leaves a column with no comment at all.
	declared := cloneFixture(func(name string, col *clickhouse.Col[string]) bool {
		if name == "note" {
			col.Comment("")
		}
		return true
	})
	plan = clickhouse.Diff(liveSnapshot(t), clickhouse.BuildSnapshot(clickhouse.NewSchema(declared)))
	if got := onlyStatement(t, plan); got != `ALTER TABLE "events" COMMENT COLUMN "note" ''` {
		t.Errorf("statement = %s", got)
	}
}

// A key column's storage is out of reach and its comment is not:
// COMMENT COLUMN is metadata on any column, so a comment that moved on
// a sorting-key column is still a statement rather than silence.
func TestDiffCommentsAKeyColumn(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].Columns["id"].Comment = "stale"
	declared := cloneFixture(func(name string, col *clickhouse.Col[string]) bool {
		if name == "id" {
			col.Comment("the event id")
		}
		return true
	})
	got := onlyStatement(t, clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(declared))))
	if got != `ALTER TABLE "events" COMMENT COLUMN "id" 'the event id'` {
		t.Errorf("statement = %s", got)
	}
}

func TestDiffCreatesAndDropsWholeTables(t *testing.T) {
	declared := clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable()))
	plan := clickhouse.Diff(clickhouse.EmptySnapshot(), declared)
	got := onlyStatement(t, plan)
	for _, want := range []string{
		`CREATE TABLE "events" (`,
		`"amount" Decimal(10,2) DEFAULT 0`,
		`"note" String COMMENT 'free text' CODEC(ZSTD(3))`,
		"ENGINE = ReplacingMergeTree(version)",
		"ORDER BY (ts,id)",
		"PARTITION BY (toYYYYMM(ts))",
		"SETTINGS index_granularity = 8192",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("CREATE TABLE missing %q:\n%s", want, got)
		}
	}
	// The primary key is the sorting key and is not written twice.
	if strings.Contains(got, "PRIMARY KEY") {
		t.Errorf("an implicit primary key must not be restated:\n%s", got)
	}

	back := clickhouse.Diff(declared, clickhouse.EmptySnapshot())
	if got := onlyStatement(t, back); got != `DROP TABLE "events"` {
		t.Errorf("statement = %s", got)
	}
}

func TestSafeAddsTheExistenceGuards(t *testing.T) {
	declared := clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable()))
	create := onlyStatement(t, clickhouse.Diff(clickhouse.EmptySnapshot(), declared, clickhouse.DiffOptions{Safe: true}))
	if !strings.HasPrefix(create, "CREATE TABLE IF NOT EXISTS ") {
		t.Errorf("statement = %s", create)
	}

	live := liveSnapshot(t)
	live.Tables["events"].Columns["version"].Type = "UInt32"
	got := onlyStatement(t, clickhouse.Diff(live, declared, clickhouse.DiffOptions{Safe: true}))
	if !strings.Contains(got, "MODIFY COLUMN IF EXISTS ") {
		t.Errorf("statement = %s", got)
	}
}

// --- the table's own TTL and settings --------------------------------

func TestDiffAddsATableTTLAndWithholdsAChangedOne(t *testing.T) {
	declared := schemaTable()
	declared.TTL("ts + INTERVAL 30 DAY")
	snap := clickhouse.BuildSnapshot(clickhouse.NewSchema(declared))

	got := onlyStatement(t, clickhouse.Diff(liveSnapshot(t), snap))
	if got != `ALTER TABLE "events" MODIFY TTL ts + INTERVAL 30 DAY` {
		t.Errorf("statement = %s", got)
	}

	// Once the server has it, it reads back in ClickHouse's own
	// spelling. drops cannot tell that apart from a real change
	// without parsing SQL, so it withholds the statement rather than
	// re-emitting it on every push for ever.
	live := liveSnapshot(t)
	live.Tables["events"].TTL = "ts + toIntervalDay(30)"
	plan := clickhouse.Diff(live, snap)
	if len(plan.Statements) != 0 {
		t.Fatalf("statements = %q", plan.Statements)
	}
	if len(plan.Notices) != 1 || plan.Notices[0].Rule != "table-ttl" {
		t.Fatalf("notices = %v", plan.Notices)
	}
	if !strings.Contains(plan.Notices[0].SQL, "MODIFY TTL") {
		t.Errorf("the withheld statement has to travel with the notice: %+v", plan.Notices[0])
	}
}

func TestDiffLeavesASettingNobodyDeclaredAlone(t *testing.T) {
	// ClickHouse materialises an engine's defaults into the table's
	// metadata, so a live setting with no declared counterpart is not
	// evidence that anyone removed it.
	live := liveSnapshot(t)
	live.Tables["events"].Settings["merge_with_ttl_timeout"] = "3600"
	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
	if len(plan.Statements) != 0 {
		t.Errorf("statements = %q", plan.Statements)
	}

	declared := schemaTable()
	declared.Setting("merge_with_ttl_timeout", "60")
	got := onlyStatement(t, clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(declared))))
	if got != `ALTER TABLE "events" MODIFY SETTING merge_with_ttl_timeout = 60` {
		t.Errorf("statement = %s", got)
	}
}

// --- refusals --------------------------------------------------------

func refusalFor(t *testing.T, plan clickhouse.Plan, kind clickhouse.RefusalKind) clickhouse.Refusal {
	t.Helper()
	for _, r := range plan.Refusals {
		if r.Kind == kind {
			return r
		}
	}
	t.Fatalf("no %s refusal in %v (statements %q)", kind, plan.Refusals, plan.Statements)
	return clickhouse.Refusal{}
}

func TestDiffRefusesASortingKeyThatMoved(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].SortingKey = "id"
	live.Tables["events"].PrimaryKey = "id"
	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))

	r := refusalFor(t, plan, clickhouse.RefuseSortingKey)
	for _, want := range []string{"MODIFY ORDER BY", "ReplacingMergeTree", "copy"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("refusal missing %q: %s", want, r.Detail)
		}
	}
	if !strings.Contains(r.Detail, `"the same row"`) {
		t.Errorf("a sorting key change on a ReplacingMergeTree changes what a row is; say so: %s", r.Detail)
	}
	for _, s := range plan.Statements {
		if strings.Contains(s, "ORDER BY") {
			t.Errorf("there is no statement for this and drops emitted one: %s", s)
		}
	}
}

func TestDiffRefusesTheEngineAndItsParameters(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].Engine = "MergeTree"
	live.Tables["events"].EngineParams = nil
	r := refusalFor(t,
		clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable()))),
		clickhouse.RefuseEngine)
	if !strings.Contains(r.Detail, "deduplicates nothing") {
		t.Errorf("refusal = %s", r.Detail)
	}

	// The engine is right and the parameter is not, which is its own
	// failure: without the version column the engine keeps whichever
	// part merged last.
	live = liveSnapshot(t)
	live.Tables["events"].EngineParams = nil
	r = refusalFor(t,
		clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable()))),
		clickhouse.RefuseEngine)
	if !strings.Contains(r.Detail, "parameters") {
		t.Errorf("refusal = %s", r.Detail)
	}
}

func TestDiffRefusesAPartitionKeyThatMoved(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].PartitionKey = ""
	r := refusalFor(t,
		clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable()))),
		clickhouse.RefusePartitionKey)
	if !strings.Contains(r.Detail, "only ever merges within one partition") {
		t.Errorf("refusal = %s", r.Detail)
	}
}

func TestDiffRefusesToTouchAKeyColumn(t *testing.T) {
	t.Run("type", func(t *testing.T) {
		live := liveSnapshot(t)
		live.Tables["events"].Columns["id"].Type = "String"
		plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
		r := refusalFor(t, plan, clickhouse.RefuseColumnType)
		if r.Column != "id" || !strings.Contains(r.Detail, "on-disk layout") {
			t.Errorf("refusal = %+v", r)
		}
		if len(plan.Statements) != 0 {
			t.Errorf("statements = %q", plan.Statements)
		}
	})

	t.Run("drop", func(t *testing.T) {
		live := liveSnapshot(t)
		live.Tables["events"].Columns["extra"] = &clickhouse.ColumnSnapshot{
			Name: "extra", Type: "String", Position: 7, InSortingKey: true,
		}
		plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
		r := refusalFor(t, plan, clickhouse.RefuseDropColumn)
		if r.Column != "extra" {
			t.Errorf("refusal = %+v", r)
		}
		for _, s := range plan.Statements {
			if strings.Contains(s, "DROP COLUMN") {
				t.Errorf("ClickHouse will not run this: %s", s)
			}
		}
	})
}

func TestRefusedErrorNamesEveryRefusal(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].SortingKey = "id"
	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
	err := plan.Refused()
	if !errors.Is(err, clickhouse.ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "sorting_key") {
		t.Errorf("err = %v", err)
	}
	if plan.Aligned() {
		t.Error("a plan with refusals is not aligned; it is stuck")
	}
}

// --- Push ------------------------------------------------------------

func pushDB(t *testing.T) (*clickhouse.DB, *schemaDriver) {
	t.Helper()
	tables, columns := schemaLiveRows()
	d := &schemaDriver{tables: tables, columns: columns}
	return clickhouse.New(d), d
}

func TestPushIsSilentWhenTheServerMatches(t *testing.T) {
	db, d := pushDB(t)
	res, err := clickhouse.Push(context.Background(), db, clickhouse.NewSchema(schemaTable()))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Statements) != 0 || len(d.execs) != 0 {
		t.Errorf("statements = %q, execs = %q", res.Statements, d.execs)
	}
}

func TestPushDryRunRunsNothing(t *testing.T) {
	db, d := pushDB(t)
	declared := schemaTable()
	clickhouse.Add(declared, clickhouse.String("source").Nullable())

	res, err := clickhouse.Push(context.Background(), db, clickhouse.NewSchema(declared),
		clickhouse.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Statements) != 1 {
		t.Fatalf("statements = %q", res.Statements)
	}
	if len(d.execs) != 0 {
		t.Errorf("a dry run executed %q", d.execs)
	}
	if res.Applied || res.AppliedCount != 0 {
		t.Errorf("result = %+v", res)
	}
	if len(res.Warnings) == 0 {
		t.Error("a dry run is a review, so it has to carry the grading")
	}
}

func TestPushAppliesAndTurnsOnTheGuards(t *testing.T) {
	db, d := pushDB(t)
	declared := schemaTable()
	clickhouse.Add(declared, clickhouse.String("source").Nullable())

	res, err := clickhouse.Push(context.Background(), db, clickhouse.NewSchema(declared))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !res.Applied || res.AppliedCount != 1 || len(d.execs) != 1 {
		t.Fatalf("result = %+v, execs = %q", res, d.execs)
	}
	// ClickHouse cannot roll a push back, so every statement has to
	// survive the re-run that follows a failure part-way.
	if !strings.Contains(d.execs[0], "IF NOT EXISTS") {
		t.Errorf("statement = %s", d.execs[0])
	}
}

func TestPushStopsOnARefusalAndCanBeToldNotTo(t *testing.T) {
	db, d := pushDB(t)
	d.tables[0][4] = "id" // the sorting key moved
	d.tables[0][5] = "id"
	declared := schemaTable()
	clickhouse.Add(declared, clickhouse.String("source").Nullable())

	res, err := clickhouse.Push(context.Background(), db, clickhouse.NewSchema(declared))
	if !errors.Is(err, clickhouse.ErrRefused) {
		t.Fatalf("err = %v", err)
	}
	if len(d.execs) != 0 {
		t.Errorf("a refused push must run nothing: %q", d.execs)
	}
	if len(res.Refusals) == 0 {
		t.Error("the result has to carry the refusals whatever the error says")
	}

	// The ADD COLUMN is unrelated to the key, and blocking it helps
	// nobody — but the push is still not a finished one.
	db, d = pushDB(t)
	d.tables[0][4] = "id"
	d.tables[0][5] = "id"
	res, err = clickhouse.Push(context.Background(), db, clickhouse.NewSchema(declared),
		clickhouse.PushOptions{AllowRefused: true})
	if !errors.Is(err, clickhouse.ErrRefused) {
		t.Fatalf("err = %v — a partial push must not look finished", err)
	}
	if len(d.execs) != 1 || !strings.Contains(d.execs[0], "ADD COLUMN") {
		t.Errorf("execs = %q", d.execs)
	}
	if !res.Applied {
		t.Errorf("the statements ran: %+v", res)
	}
}

func TestPushReportsHowFarItGot(t *testing.T) {
	db, d := pushDB(t)
	d.execErr = errors.New("Code: 47. DB::Exception: Missing columns")
	declared := schemaTable()
	clickhouse.Add(declared, clickhouse.String("source").Nullable())

	res, err := clickhouse.Push(context.Background(), db, clickhouse.NewSchema(declared))
	if err == nil {
		t.Fatal("want the server's error")
	}
	if res == nil {
		t.Fatal("the result comes back alongside the error, not instead of it — there is no rollback")
	}
	if res.AppliedCount != 0 || !strings.Contains(res.Failed, "ADD COLUMN") {
		t.Errorf("result = %+v", res)
	}
}

func TestPushLeavesUndeclaredTablesAlone(t *testing.T) {
	db, d := pushDB(t)
	d.tables = append(d.tables, []any{
		"dashboard_view", "View", "View", "", "", "", "",
	})
	res, err := clickhouse.Push(context.Background(), db, clickhouse.NewSchema(schemaTable()))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	for _, s := range res.Statements {
		if strings.Contains(s, "dashboard_view") {
			t.Errorf("a table the schema never named is not drops's to drop: %s", s)
		}
	}
}

func TestPushRefusesASchemaSpanningDatabases(t *testing.T) {
	db, _ := pushDB(t)
	other := clickhouse.NewDatabaseTable("analytics", "hits")
	clickhouse.Add(other, clickhouse.UInt64("id"))
	other.Engine(clickhouse.MergeTree()).OrderBy(other.Col("id"))

	_, err := clickhouse.Push(context.Background(), db, clickhouse.NewSchema(schemaTable(), other))
	if !errors.Is(err, clickhouse.ErrMixedDatabases) {
		t.Fatalf("err = %v", err)
	}
}

func TestPushNeedsASchema(t *testing.T) {
	db, _ := pushDB(t)
	if _, err := clickhouse.Push(context.Background(), db, nil); !errors.Is(err, clickhouse.ErrSchemaRequired) {
		t.Fatalf("err = %v", err)
	}
}

// --- the safety analyser ---------------------------------------------

func TestAnalyzeGradesTheStatements(t *testing.T) {
	cases := []struct {
		stmt string
		want clickhouse.Cost
	}{
		{`ALTER TABLE "events" ADD COLUMN "source" Nullable(String) AFTER "note"`, clickhouse.CostMetadata},
		{`ALTER TABLE "events" MODIFY COLUMN "version" UInt64`, clickhouse.CostRewrite},
		{`ALTER TABLE "events" MODIFY COLUMN "amount" REMOVE DEFAULT`, clickhouse.CostMetadata},
		{`ALTER TABLE "events" DROP COLUMN "kind"`, clickhouse.CostDestructive},
		{`ALTER TABLE "events" MODIFY SETTING index_granularity = 4096`, clickhouse.CostMetadata},
		{`ALTER TABLE "events" MODIFY TTL ts + toIntervalDay(30)`, clickhouse.CostDestructive},
		{`DROP TABLE "events"`, clickhouse.CostDestructive},
	}
	for _, c := range cases {
		if got := clickhouse.ClassifyStatement(c.stmt); got != c.want {
			t.Errorf("ClassifyStatement(%s) = %s, want %s", c.stmt, got, c.want)
		}
	}
}

func TestAnalyzeExplainsTheAsynchronousRewrite(t *testing.T) {
	warnings := clickhouse.AnalyzeStatements([]string{`ALTER TABLE "events" MODIFY COLUMN "version" UInt64`})
	if len(warnings) != 1 {
		t.Fatalf("warnings = %+v", warnings)
	}
	w := warnings[0]
	if w.Cost != clickhouse.CostRewrite || w.Rule != "modify-column-type" {
		t.Fatalf("warning = %+v", w)
	}
	// The thing that makes ClickHouse different from every other
	// dialect drops speaks: the statement returns before the work
	// starts, so "it succeeded" is not the same as "it is done".
	for _, want := range []string{"mutations_sync", "system.mutations"} {
		if !strings.Contains(w.Message+w.Suggestion, want) {
			t.Errorf("warning does not send the operator to the mutation: %+v", w)
		}
	}
}

func TestAnalyzeGradesARefusalAsTheCopyItIs(t *testing.T) {
	live := liveSnapshot(t)
	live.Tables["events"].SortingKey = "id"
	plan := clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(schemaTable())))
	var found bool
	for _, w := range clickhouse.Analyze(plan) {
		if w.Rule == "refused-sorting_key" {
			found = true
			if w.Cost != clickhouse.CostDestructive {
				t.Errorf("a refusal costs a full copy of the table: %+v", w)
			}
			if !strings.Contains(w.Suggestion, "RENAME TABLE") {
				t.Errorf("the suggestion has to name the remedy: %+v", w)
			}
		}
	}
	if !found {
		t.Error("Analyze must report the refusals, not only the statements")
	}
}

// Every keyword the analyser matches on is also an ordinary English
// word, and a column comment is the one string in a schema statement
// drops did not write. A MODIFY COLUMN whose comment says "remove"
// still queues a mutation over every part of the table, and a grading
// that reads the comment calls it a metadata change that touches no
// data — the opposite, said reassuringly.
func TestTheAnalyserDoesNotReadTheComment(t *testing.T) {
	rewrite := `ALTER TABLE "events" MODIFY COLUMN "note" String COMMENT 'we remove this in Q3'`
	if got := clickhouse.ClassifyStatement(rewrite); got != clickhouse.CostRewrite {
		t.Errorf("ClassifyStatement = %s, want rewrite — the comment is not the statement", got)
	}
	warnings := clickhouse.AnalyzeStatements([]string{rewrite})
	if len(warnings) != 1 || warnings[0].Rule != "modify-column-type" {
		t.Fatalf("warnings = %+v", warnings)
	}

	// And the other direction: a comment that mentions dropping a table
	// must not raise a destructive finding against an ADD COLUMN.
	add := `ALTER TABLE "events" ADD COLUMN "note" String COMMENT 'we drop table dumps in here' FIRST`
	if got := clickhouse.ClassifyStatement(add); got != clickhouse.CostMetadata {
		t.Errorf("ClassifyStatement = %s, want metadata", got)
	}
	for _, w := range clickhouse.AnalyzeStatements([]string{add}) {
		if w.Rule != "add-column" {
			t.Errorf("an ADD COLUMN raised %q from the text of its comment: %+v", w.Rule, w)
		}
	}

	// The escapes the comment writer cannot use to hide the closing
	// quote and spill the rest of the statement past the mask.
	for _, stmt := range []string{
		`ALTER TABLE "events" MODIFY COLUMN "note" String COMMENT 'it''s here' CODEC(LZ4)`,
		`ALTER TABLE "events" MODIFY COLUMN "note" String COMMENT 'C:\\' CODEC(LZ4)`,
	} {
		if got := clickhouse.ClassifyStatement(stmt); got != clickhouse.CostRewrite {
			t.Errorf("ClassifyStatement(%s) = %s, want rewrite", stmt, got)
		}
	}
}

func TestIgnoreSilencesARule(t *testing.T) {
	stmts := []string{`ALTER TABLE "events" DROP COLUMN "kind"`}
	if got := clickhouse.AnalyzeStatements(stmts, clickhouse.SafetyOptions{Ignore: []string{"drop-column"}}); len(got) != 0 {
		t.Errorf("warnings = %+v", got)
	}
}

// --- normalisation ---------------------------------------------------

func TestNormalizeKeyExprUnquotesOnlyWhatItCan(t *testing.T) {
	cases := map[string]string{
		`"ts", "id"`:       "ts,id",
		"`ts`, `id`":       "ts,id",
		`toYYYYMM("ts")`:   "toYYYYMM(ts)",
		`"needs quotes"`:   `"needs quotes"`,
		`concat(a, 'b,c')`: `concat(a,'b,c')`,
	}
	for in, want := range cases {
		if got := clickhouse.NormalizeKeyExpr(in); got != want {
			t.Errorf("NormalizeKeyExpr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeTypeKeepsWhatIsInsideQuotes(t *testing.T) {
	if got := clickhouse.NormalizeType("DateTime64(3, 'Europe/Rome')"); got != "DateTime64(3,'Europe/Rome')" {
		t.Errorf("got %q — a time zone name is a value, not syntax", got)
	}
	if got := clickhouse.NormalizeType("Enum8('a b' = 1, 'c' = 2)"); got != "Enum8('a b'=1,'c'=2)" {
		t.Errorf("got %q — an enum label can contain a space", got)
	}
}

func TestClassifyTypeChangeIsNarrowAboutWidening(t *testing.T) {
	if c := clickhouse.ClassifyTypeChange("Int32", "Int16", false); c.Verdict != clickhouse.TypeWidens {
		t.Errorf("Int16 to Int32 = %s", c.Verdict)
	}
	// Floats above 2^53 stop being exact, so this is not a widening
	// however much it looks like one.
	if c := clickhouse.ClassifyTypeChange("Float64", "Int64", false); c.Verdict != clickhouse.TypeUnprovable {
		t.Errorf("Int64 to Float64 = %s", c.Verdict)
	}
	if c := clickhouse.ClassifyTypeChange("Decimal(12,2)", "Decimal(10,2)", false); c.Verdict != clickhouse.TypeWidens {
		t.Errorf("Decimal precision growth = %s", c.Verdict)
	}
	if c := clickhouse.ClassifyTypeChange("Decimal(12,4)", "Decimal(10,2)", false); c.Verdict != clickhouse.TypeUnprovable {
		t.Errorf("a scale change moves the value, not just its room: %s", c.Verdict)
	}
}

func TestClassifyTypeChangeCarriesTheWrapperWhereItCan(t *testing.T) {
	c := clickhouse.ClassifyTypeChange("Nullable(String)", "LowCardinality(String)", false)
	if c.To != "LowCardinality(Nullable(String))" {
		t.Errorf("To = %q — the wrapper is a tuning decision to respect", c.To)
	}
	// And where ClickHouse will not take it, neither answer is drops's
	// to choose.
	c = clickhouse.ClassifyTypeChange("Int32", "LowCardinality(Int16)", false)
	if c.Cause != clickhouse.CauseLowCardinalityWrapper {
		t.Fatalf("change = %+v", c)
	}
	if !strings.Contains(c.Why, "allow_suspicious_low_cardinality_types") {
		t.Errorf("the reason is a server setting; name it: %s", c.Why)
	}
	if c.ValuesAtRisk {
		t.Error("Int16 to Int32 puts no value at risk, and saying so is crying wolf")
	}
}

// A comment is the one place a schema statement carries text drops did
// not write, and ClickHouse's lexer reads C-style escapes inside a
// string literal. So doubling the quote — which is all PostgreSQL
// needs, and where this dialect's quoting came from — is not enough:
// a comment ending in a backslash escapes its own closing quote and
// runs on into whatever follows, and "\\' OR 1=1 --" leaves the literal
// altogether.
func TestACommentCannotEscapeItsLiteral(t *testing.T) {
	for _, comment := range []string{`C:\\`, `\\' OR 1=1 --`, `it's fine`, "tab\tand\\slash"} {
		live := liveSnapshot(t)
		live.Tables["events"].Columns["note"].Comment = "stale"
		declared := cloneFixture(func(name string, col *clickhouse.Col[string]) bool {
			if name == "note" {
				col.Comment(comment)
			}
			return true
		})
		got := onlyStatement(t, clickhouse.Diff(live, clickhouse.BuildSnapshot(clickhouse.NewSchema(declared))))
		body, ok := strings.CutPrefix(got, `ALTER TABLE "events" COMMENT COLUMN "note" `)
		if !ok {
			t.Fatalf("statement = %s", got)
		}
		if unquoted, err := unescapeCHLiteral(body); err != nil {
			t.Errorf("comment %q rendered a literal ClickHouse cannot read: %v\n%s", comment, err, got)
		} else if unquoted != comment {
			t.Errorf("comment %q reads back as %q\n%s", comment, unquoted, got)
		}
	}
}

// unescapeCHLiteral reads a ClickHouse string literal back the way the
// server's lexer does — ” and a backslash escape both end a quote —
// and fails when the literal does not end exactly at the end of the
// text, which is what an unterminated one looks like from here.
func unescapeCHLiteral(text string) (string, error) {
	if len(text) < 2 || text[0] != '\'' {
		return "", errors.New("not a quoted literal")
	}
	var b strings.Builder
	for i := 1; i < len(text); i++ {
		switch text[i] {
		case '\\':
			i++
			if i >= len(text) {
				return "", errors.New("statement ends inside a backslash escape")
			}
			switch text[i] {
			case 't':
				b.WriteByte('\t')
			case 'n':
				b.WriteByte('\n')
			default:
				b.WriteByte(text[i])
			}
		case '\'':
			if i+1 < len(text) && text[i+1] == '\'' {
				b.WriteByte('\'')
				i++
				continue
			}
			if i != len(text)-1 {
				return "", errors.New("the literal closes early and " +
					strconv.Quote(text[i+1:]) + " is left outside it")
			}
			return b.String(), nil
		default:
			b.WriteByte(text[i])
		}
	}
	return "", errors.New("unterminated string literal")
}

// --- the duplication this file exists to pin -------------------------

// CREATE TABLE is rendered twice — once from a live *Table by
// CreateTable, once from a snapshot by CreateTableSQL — and the column
// clause order is not free to vary: ClickHouse's parser reads DEFAULT,
// then COMMENT, then CODEC, then TTL, and a comment after a codec is a
// syntax error for the whole statement.
func TestBothCreateTableRenderersAgree(t *testing.T) {
	tbl := clickhouse.NewTable("annotated")
	ts := clickhouse.Add(tbl, clickhouse.DateTime("ts", "UTC"))
	clickhouse.Add(tbl, clickhouse.Float64("amount").
		Default("0").
		Comment("what the customer paid").
		Codec("ZSTD(3)").
		TTL("ts + INTERVAL 30 DAY"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ts)

	fromTable, _ := drops.StringWithDialect(clickhouse.Dialect, clickhouse.CreateTable(tbl))
	fromSnapshot := clickhouse.CreateTableSQL(
		clickhouse.BuildSnapshot(clickhouse.NewSchema(tbl)).Tables["annotated"])
	if got, want := columnBlock(fromSnapshot), columnBlock(fromTable); got != want {
		t.Errorf("the two renderers have drifted:\n--- from *Table ---\n%s\n--- from snapshot ---\n%s", want, got)
	}
	// The clauses after the column block legitimately differ: the
	// snapshot holds its keys in the normalised, unquoted spelling the
	// server echoes back, and a *Table renders its own quoted
	// identifiers. Both parse; only the column block has an order that
	// does not.
	if !strings.Contains(fromTable, `ORDER BY ("ts")`) || !strings.Contains(fromSnapshot, "ORDER BY (ts)") {
		t.Errorf("the key clauses are no longer what this test allows for:\n%s\n%s", fromTable, fromSnapshot)
	}
}

// columnBlock returns the parenthesised column definitions of a CREATE
// TABLE — the part whose clause order ClickHouse's parser is strict
// about.
func columnBlock(stmt string) string {
	open := strings.Index(stmt, "(\n")
	close := strings.Index(stmt, "\n)")
	if open < 0 || close < 0 {
		return stmt
	}
	return stmt[open+2 : close]
}
