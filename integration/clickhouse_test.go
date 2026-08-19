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
