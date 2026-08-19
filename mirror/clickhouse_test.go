package mirror_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// A source table exercising every type the mapping has to handle.
func sourceTable() *pg.Table {
	t := pg.NewTable("docs")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("title").NotNull())
	pg.Add(t, pg.Text("body"))
	pg.Add(t, pg.Integer("views").NotNull())
	pg.Add(t, pg.Boolean("draft").NotNull())
	pg.Add(t, pg.Timestamp("created_at", true).NotNull())
	pg.Add(t, pg.UUID("author_id"))
	pg.Add(t, pg.Numeric("price", 10, 2))
	pg.Add(t, pg.JSONB("meta"))
	return t
}

func derive(t *testing.T, src *pg.Table, opts ...mirror.DeriveOption) *clickhouse.Table {
	t.Helper()
	out, err := mirror.DeriveClickHouse(src, opts...)
	if err != nil {
		t.Fatalf("DeriveClickHouse: %v", err)
	}
	return out
}

func ddl(t *testing.T, tbl *clickhouse.Table) string {
	t.Helper()
	sql, _ := dropsString(clickhouse.CreateTable(tbl))
	return sql
}

// dropsString renders an expression, shared with the examples.
func dropsString(e drops.Expression) (string, []any) { return drops.String(e) }

func TestDeriveProducesMirroredSchema(t *testing.T) {
	got := ddl(t, derive(t, sourceTable()))

	for _, want := range []string{
		`"id" Int64`,
		`"title" String`,
		`"body" Nullable(String)`,
		`"views" Int32`,
		`"draft" Bool`,
		`"created_at" DateTime64(6, 'UTC')`,
		`"author_id" Nullable(UUID)`,
		`"price" Nullable(Decimal(10,2))`,
		`"meta" Nullable(String)`,
		`"` + mirror.VersionColumn + `" UInt64`,
		`"` + mirror.DeletedColumn + `" UInt8`,
		`ReplacingMergeTree("` + mirror.VersionColumn + `")`,
		`ORDER BY ("id")`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("derived DDL missing %s\ngot %s", want, got)
		}
	}
}

// NOT NULL at the source means non-Nullable in the mirror; everything
// else is wrapped, because a column that can be absent in Postgres can
// be absent here.
func TestDeriveNullabilityFollowsTheSource(t *testing.T) {
	got := ddl(t, derive(t, sourceTable()))
	if strings.Contains(got, `"title" Nullable`) {
		t.Errorf("NOT NULL column was made nullable: %s", got)
	}
	if !strings.Contains(got, `"body" Nullable`) {
		t.Errorf("nullable column was not wrapped: %s", got)
	}
	// The key must stay non-Nullable — ClickHouse cannot sort by a
	// Nullable column, and the key is the sorting key.
	if strings.Contains(got, `"id" Nullable`) {
		t.Errorf("primary key was made nullable: %s", got)
	}
}

func TestMapType(t *testing.T) {
	cases := map[string]string{
		"smallint":         "Int16",
		"integer":          "Int32",
		"serial":           "Int32",
		"bigint":           "Int64",
		"bigserial":        "Int64",
		"real":             "Float32",
		"double precision": "Float64",
		"boolean":          "Bool",
		"uuid":             "UUID",
		"date":             "Date32",
		"timestamp":        "DateTime64(6)",
		"timestamptz":      "DateTime64(6, 'UTC')",
		"numeric(12,4)":    "Decimal(12,4)",
		"numeric":          "String",
		"text":             "String",
		"varchar(255)":     "String",
		"jsonb":            "String",
		"bytea":            "String",
		"interval":         "String",
		"time":             "String",
	}
	for in, want := range cases {
		got, err := mirror.MapType(in, false)
		if err != nil {
			t.Errorf("MapType(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("MapType(%q) = %q want %q", in, got, want)
		}
	}
	if got, _ := mirror.MapType("text", true); got != "Nullable(String)" {
		t.Errorf("nullable wrap = %q", got)
	}
	if _, err := mirror.MapType("tsvector", false); err == nil {
		t.Error("expected an error for a type with no ClickHouse equivalent")
	}
}

// Date, not Date32, would silently wrap dates past 2149 — worth
// pinning so nobody "simplifies" it later.
func TestDateMapsWideEnough(t *testing.T) {
	got, _ := mirror.MapType("date", false)
	if got == "Date" {
		t.Error("date must map to Date32: Date stops at 2149 and wraps")
	}
}

func TestDeriveRejectsBadSources(t *testing.T) {
	t.Run("no primary key", func(t *testing.T) {
		tbl := pg.NewTable("nokey")
		pg.Add(tbl, pg.Text("a"))
		if _, err := mirror.DeriveClickHouse(tbl); err == nil {
			t.Error("expected an error: mirrored rows need a key to converge on")
		}
	})

	t.Run("composite primary key", func(t *testing.T) {
		tbl := pg.NewTable("composite")
		pg.Add(tbl, pg.BigInt("a").PrimaryKey())
		pg.Add(tbl, pg.BigInt("b").PrimaryKey())
		if _, err := mirror.DeriveClickHouse(tbl); err == nil {
			t.Error("expected an error for a composite key")
		}
	})

	t.Run("bookkeeping column collision", func(t *testing.T) {
		tbl := pg.NewTable("collide")
		pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
		pg.Add(tbl, pg.BigInt(mirror.VersionColumn))
		_, err := mirror.DeriveClickHouse(tbl)
		if err == nil || !strings.Contains(err.Error(), mirror.VersionColumn) {
			t.Errorf("expected a collision error, got %v", err)
		}
	})

	t.Run("unmappable type", func(t *testing.T) {
		tbl := pg.NewTable("weird")
		pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
		pg.Add(tbl, pg.Custom[string]("v", "tsvector"))
		if _, err := mirror.DeriveClickHouse(tbl); err == nil {
			t.Error("expected an error for an unmappable column type")
		}
	})
}

func TestDeriveOptions(t *testing.T) {
	tbl := derive(t, sourceTable(),
		mirror.WithName("docs_analytics"),
		mirror.WithDatabase("analytics"),
		mirror.WithOrderBy("id", "created_at"),
		mirror.WithSetting("index_granularity", "8192"),
	)
	got := ddl(t, tbl)
	for _, want := range []string{
		`"analytics"."docs_analytics"`,
		`ORDER BY ("id", "created_at")`,
		`index_granularity = 8192`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DDL missing %s\ngot %s", want, got)
		}
	}
}

func TestNotDeletedPredicate(t *testing.T) {
	tbl := derive(t, sourceTable())
	sql, args := drops.String(mirror.NotDeleted(tbl))
	if !strings.Contains(sql, mirror.DeletedColumn) {
		t.Errorf("predicate = %s", sql)
	}
	if len(args) != 1 || args[0] != 0 {
		t.Errorf("args = %v want [0]", args)
	}
}

// --- sink ------------------------------------------------------------

type recDriver struct {
	sql  []string
	args [][]any
}

func (d *recDriver) Exec(_ context.Context, s string, a ...any) (drops.Result, error) {
	d.sql = append(d.sql, s)
	d.args = append(d.args, a)
	return recResult{}, nil
}
func (d *recDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, errors.New("unexpected Query")
}
func (d *recDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

type recResult struct{}

func (recResult) RowsAffected() (int64, error) { return 0, nil }

func newSink(t *testing.T) (*mirror.ClickHouseSink, *clickhouse.Table, *recDriver) {
	t.Helper()
	tbl := derive(t, sourceTable())
	d := &recDriver{}
	s, err := mirror.NewClickHouseSink(clickhouse.New(d), tbl)
	if err != nil {
		t.Fatalf("NewClickHouseSink: %v", err)
	}
	return s, tbl, d
}

func TestSinkRejectsUnderivedTable(t *testing.T) {
	plain := clickhouse.NewTable("plain")
	clickhouse.Add(plain, clickhouse.Int64("id"))
	if _, err := mirror.NewClickHouseSink(clickhouse.New(&recDriver{}), plain); err == nil {
		t.Error("expected a rejection: the table has no version/deleted columns")
	}
}

func TestSinkWritesOneInsertPerBatch(t *testing.T) {
	s, _, d := newSink(t)
	now := time.Unix(1700000000, 0).UTC()

	err := s.Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpInsert, Key: "1", Version: 10, At: now,
			Row: map[string]any{"id": int64(1), "title": "a", "views": int32(0), "draft": false, "created_at": now}},
		{Op: mirror.OpUpdate, Key: "1", Version: 11, At: now,
			Row: map[string]any{"id": int64(1), "title": "b", "views": int32(3), "draft": false, "created_at": now}},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(d.sql) != 1 {
		t.Fatalf("expected one statement for the batch, got %d", len(d.sql))
	}
	if !strings.HasPrefix(d.sql[0], "INSERT INTO") {
		t.Errorf("statement = %s", d.sql[0])
	}
	// An update is an append, not a mutation: ClickHouse collapses
	// it at merge time on the version column.
	if strings.Contains(d.sql[0], "ALTER") || strings.Contains(d.sql[0], "UPDATE") {
		t.Errorf("update should append, not mutate: %s", d.sql[0])
	}
	var sawV10, sawV11 bool
	for _, a := range d.args[0] {
		if a == uint64(10) {
			sawV10 = true
		}
		if a == uint64(11) {
			sawV11 = true
		}
	}
	if !sawV10 || !sawV11 {
		t.Errorf("both versions should be bound: %v", d.args[0])
	}
}

func TestSinkWritesDeleteAsTombstone(t *testing.T) {
	s, _, d := newSink(t)
	err := s.Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpDelete, Key: "42", Version: 99, At: time.Unix(1, 0)},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(d.sql) != 1 || !strings.HasPrefix(d.sql[0], "INSERT INTO") {
		t.Fatalf("delete should be an insert: %v", d.sql)
	}
	var sawMarker, sawKey bool
	for _, a := range d.args[0] {
		if a == uint8(1) {
			sawMarker = true
		}
		// The key has to carry the real value or the tombstone
		// sorts somewhere other than the row it retires.
		if a == int64(42) {
			sawKey = true
		}
	}
	if !sawMarker {
		t.Errorf("tombstone marker not set: %v", d.args[0])
	}
	if !sawKey {
		t.Errorf("tombstone did not carry the key: %v", d.args[0])
	}
}

func TestSinkRejectsKeylessChange(t *testing.T) {
	s, _, _ := newSink(t)
	err := s.Apply(context.Background(), []mirror.Change{{Op: mirror.OpInsert}})
	if !errors.Is(err, mirror.ErrNoKey) {
		t.Errorf("err = %v want ErrNoKey", err)
	}
}

func TestSinkEmptyBatchIsNoOp(t *testing.T) {
	s, _, d := newSink(t)
	if err := s.Apply(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(d.sql) != 0 {
		t.Errorf("empty batch issued %d statements", len(d.sql))
	}
}

func TestChangeNormalized(t *testing.T) {
	now := time.Unix(1700000000, 500).UTC()
	got := mirror.Change{Op: mirror.OpInsert, Key: "1"}.Normalized(now)
	if !got.At.Equal(now) {
		t.Errorf("At = %v want %v", got.At, now)
	}
	if got.Version != uint64(now.UnixNano()) {
		t.Errorf("Version = %d want %d", got.Version, now.UnixNano())
	}
	// An explicit version must survive.
	got = mirror.Change{Op: mirror.OpInsert, Key: "1", Version: 7}.Normalized(now)
	if got.Version != 7 {
		t.Errorf("Version = %d, want the caller's 7", got.Version)
	}
}
