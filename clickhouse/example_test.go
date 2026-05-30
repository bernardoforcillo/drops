package clickhouse_test

import (
	"context"
	"fmt"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// Schema fixtures used by the godoc examples — picked from a small
// analytics-style table so the rendered SQL is recognisable.
var (
	exEvents   = clickhouse.NewTable("events")
	exEventID  = clickhouse.Add(exEvents, clickhouse.UUID("id"))
	exEventTS  = clickhouse.Add(exEvents, clickhouse.DateTime("ts", "UTC"))
	exEventUID = clickhouse.Add(exEvents, clickhouse.UInt64("user_id"))
	exEventDur = clickhouse.Add(exEvents, clickhouse.Float64("duration_ms"))
)

func init() {
	exEvents.
		Engine(clickhouse.MergeTree()).
		OrderBy(exEventTS, exEventUID).
		PartitionBy(clickhouse.ToYYYYMM(exEventTS))
}

// ExampleAdd shows the canonical table+columns declaration pattern.
func ExampleAdd() {
	logs := clickhouse.NewTable("logs")
	id := clickhouse.Add(logs, clickhouse.UInt64("id"))
	msg := clickhouse.Add(logs, clickhouse.String("message").LowCardinality())
	fmt.Printf("%s, %s\n", id.Name(), msg.Name())
	// Output: id, message
}

// ExampleCreateTable shows the rendered DDL for a MergeTree table.
func ExampleCreateTable() {
	sql, _ := clickhouse.ToSQL(clickhouse.CreateTableIfNotExists(exEvents))
	// trim for godoc readability — only the first two clauses
	end := 0
	for n := 0; n < 2; n++ {
		end = nthIndex(sql, "\n", end+1)
		if end < 0 {
			break
		}
	}
	fmt.Println(sql[:end])
	// Output:
	// CREATE TABLE IF NOT EXISTS "events" (
	// 	"id" UUID,
}

// ExampleDB_Select shows a quantile + grouped analytics query.
func ExampleDB_Select() {
	db := clickhouse.New(nil)
	sql, _ := db.Select(
		clickhouse.As(clickhouse.ToStartOfDay(exEventTS), "day"),
		clickhouse.As(clickhouse.QuantileTiming(0.95, exEventDur), "p95"),
		clickhouse.As(clickhouse.CountAll(), "hits"),
	).
		From(exEvents).
		GroupBy(clickhouse.ToStartOfDay(exEventTS)).
		OrderBy(exEventTS.Asc()).
		ToSQL()
	fmt.Println(sql)
	// Output:
	// SELECT toStartOfDay("events"."ts") AS "day", quantileTiming(?)("events"."duration_ms") AS "p95", count() AS "hits" FROM "events" GROUP BY toStartOfDay("events"."ts") ORDER BY "events"."ts" ASC
}

// ExampleDB_WithHook shows attaching the dialect-neutral LoggerHook
// to a ClickHouse DB.
func ExampleDB_WithHook() {
	db := clickhouse.New(exampleNoopDriver{}).WithHook(
		drops.LoggerHook(func(format string, args ...any) {
			fmt.Printf("logged: "+format+"\n", args...)
		}),
	)
	_, _ = db.Exec(context.Background(), "SELECT 1")
	// Sample output omitted — duration is non-deterministic. The hook
	// produces one line per call with kind, status, elapsed and SQL.
}

// exampleNoopDriver lets the godoc examples render through DB without
// pulling in a real ClickHouse driver. Production code never embeds
// a no-op.
type exampleNoopDriver struct{}

func (exampleNoopDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return exampleNoopResult{}, nil
}
func (exampleNoopDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, fmt.Errorf("not implemented")
}
func (exampleNoopDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, fmt.Errorf("not implemented")
}

type exampleNoopResult struct{}

func (exampleNoopResult) RowsAffected() (int64, error) { return 0, nil }

// nthIndex returns the index of the nth occurrence of sep in s
// starting from start, or -1.
func nthIndex(s, sep string, start int) int {
	for i := start; i < len(s); i++ {
		if s[i] == sep[0] {
			return i
		}
	}
	return -1
}
