package clickhouse_test

import (
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

func TestCastAndCase(t *testing.T) {
	checkExpr(t, clickhouse.CastAs(eventDur, "UInt32"),
		`CAST("events"."durationMs" AS UInt32)`)

	searched := clickhouse.Case().
		When(eventDur.Gt(1.0), "slow").
		Else("fast").
		End()
	checkExpr(t, searched,
		`CASE WHEN ("events"."durationMs" > ?) THEN ? ELSE ? END`,
		1.0, "slow", "fast")

	simple := clickhouse.CaseOn(eventKind).When("click", 1).Else(0).End()
	checkExpr(t, simple, `CASE "events"."kind" WHEN ? THEN ? ELSE ? END`,
		"click", 1, 0)
}

func TestSubqueryHelpers(t *testing.T) {
	db := clickhouse.New(nil)
	sub := db.Select(eventUser).From(events)
	checkExpr(t, clickhouse.Exists(sub),
		`EXISTS (SELECT "events"."userId" FROM "events")`)
	checkExpr(t, clickhouse.NotExists(sub),
		`NOT EXISTS (SELECT "events"."userId" FROM "events")`)
	checkExpr(t, clickhouse.InSub(eventUser, sub),
		`("events"."userId" IN (SELECT "events"."userId" FROM "events"))`)
}

func TestWindowFunctions(t *testing.T) {
	w := clickhouse.WindowSpec().PartitionBy(eventUser).OrderBy(eventTS)
	checkExpr(t, clickhouse.Over(clickhouse.RowNumber(), w),
		`row_number() OVER (PARTITION BY "events"."userId" ORDER BY "events"."ts")`)
	checkExpr(t, clickhouse.Over(clickhouse.Sum(eventDur), clickhouse.WindowSpec().PartitionBy(eventUser)),
		`sum("events"."durationMs") OVER (PARTITION BY "events"."userId")`)
	checkExpr(t, clickhouse.Lag(eventDur, 1),
		`lagInFrame("events"."durationMs", ?)`, 1)
}

func TestCTEPrefix(t *testing.T) {
	db := clickhouse.New(nil)
	base := db.Select(eventUser).From(events)
	q := db.Select().From(events).With(clickhouse.CTEDef("recent", base))
	sql, _ := q.ToSQL()
	want := `WITH "recent" AS (SELECT "events"."userId" FROM "events") SELECT * FROM "events"`
	if sql != want {
		t.Errorf("CTE prefix:\n  got:  %s\n  want: %s", sql, want)
	}
}
