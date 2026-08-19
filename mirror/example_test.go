package mirror_test

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// The schema is declared once, in Postgres terms.
var (
	Docs      = pg.NewTable("docs")
	DocID     = pg.Add(Docs, pg.BigSerial("id").PrimaryKey())
	DocTitle  = pg.Add(Docs, pg.Text("title").NotNull())
	DocLang   = pg.Add(Docs, pg.Text("lang"))
	DocViews  = pg.Add(Docs, pg.Integer("views").NotNull())
	DocMadeAt = pg.Add(Docs, pg.Timestamp("created_at", true).NotNull())
)

// The analytics table is derived from it rather than declared again,
// so the two cannot drift.
func ExampleDeriveClickHouse() {
	chDocs, err := mirror.DeriveClickHouse(Docs,
		mirror.WithDatabase("analytics"),
		mirror.WithPartitionBy(clickhouse.Func("toYYYYMM", DocMadeAt)),
	)
	if err != nil {
		panic(err)
	}
	sql, _ := clickhouseDDL(chDocs)
	fmt.Println(sql)
	// Output:
	// CREATE TABLE "analytics"."docs" (
	// 	"id" Int64,
	// 	"title" String,
	// 	"lang" Nullable(String),
	// 	"views" Int32,
	// 	"created_at" DateTime64(6, 'UTC'),
	// 	"_drops_version" UInt64,
	// 	"_drops_deleted" UInt8
	// ) ENGINE = ReplacingMergeTree("_drops_version")
	// ORDER BY ("id")
	// PARTITION BY (toYYYYMM("created_at"))
}

// Reading the mirror needs FINAL, to collapse superseded versions,
// and the tombstone filter.
func ExampleNotDeleted() {
	chDocs, _ := mirror.DeriveClickHouse(Docs)
	db := clickhouse.New(&recDriver{})

	sel := db.Select().From(chDocs).Final().Where(mirror.NotDeleted(chDocs))
	sql, _ := sel.ToSQL()
	fmt.Println(sql)
	// Output:
	// SELECT * FROM "docs" FINAL WHERE ("docs"."_drops_deleted" = ?)
}

// A change is written to the outbox in the same transaction as the
// mutation, so the mirror can never learn about a row that was rolled
// back — nor miss one that committed.
func ExampleEmitChange() {
	type doc struct {
		ID    int64
		Title string
		Lang  string
	}
	d := doc{ID: 7, Title: "Vector search in Go", Lang: "en"}

	change := mirror.Change{
		Op:  mirror.OpUpdate,
		Key: strconv.FormatInt(d.ID, 10),
		Row: map[string]any{"id": d.ID, "title": d.Title, "lang": d.Lang},
	}
	fmt.Printf("%s %s -> %v\n", change.Op, change.Key, change.Row["title"])
	// Output:
	// update 7 -> Vector search in Go
}

// One pump, both mirrors: ClickHouse answers the aggregations and
// Qdrant answers "find me things like this", from the same durable
// stream of changes.
func ExamplePump() {
	var applied []string
	sink := func(name string) mirror.Sink {
		return mirror.SinkFunc{
			SinkName: name,
			Fn: func(_ context.Context, cs []mirror.Change) error {
				applied = append(applied, fmt.Sprintf("%s got %d", name, len(cs)))
				return nil
			},
		}
	}

	src := &fakeSource{batches: [][]mirror.Change{{
		{Op: mirror.OpInsert, Key: "1"},
		{Op: mirror.OpUpdate, Key: "2"},
	}}}

	pump, err := mirror.NewPump(src, sink("clickhouse:docs"), sink("qdrant:docs"))
	if err != nil {
		panic(err)
	}
	n, err := pump.Step(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(n, applied)
	// Output:
	// 2 [clickhouse:docs got 2 qdrant:docs got 2]
}

func clickhouseDDL(t *clickhouse.Table) (string, []any) {
	return dropsString(clickhouse.CreateTable(t))
}

// The source gained a column; the mirror lost one to somebody's ALTER.
// A plan says what it will run and what it will not touch without being
// told.
func ExampleEvolver_PlanAgainst() {
	ev, err := mirror.NewEvolver(clickhouse.New(&recDriver{}), Docs)
	if err != nil {
		panic(err)
	}

	// What ClickHouse actually holds, read back with InspectMirror in
	// anything but an example: no "views", and a "legacy" column the
	// source has never had.
	live := []mirror.MirrorColumn{
		{Name: "id", Type: "Int64", InKey: true},
		{Name: "title", Type: "String"},
		{Name: "lang", Type: "Nullable(String)"},
		{Name: "created_at", Type: "DateTime64(6, 'UTC')"},
		{Name: "legacy", Type: "String"},
		{Name: "_drops_version", Type: "UInt64"},
		{Name: "_drops_deleted", Type: "UInt8"},
	}

	plan := ev.PlanAgainst(live)
	for _, s := range plan.Steps {
		fmt.Println(s.SQL)
	}
	for _, r := range plan.Refusals {
		fmt.Println("refused:", r)
	}
	// Output:
	// ALTER TABLE "docs" ADD COLUMN IF NOT EXISTS "views" Int32 AFTER "lang"
	// refused: drop_column "legacy" (needs_opt_in): the source no longer has this column, and dropping it discards the data ClickHouse still holds; pass "legacy" to AllowDrop to accept that
}
