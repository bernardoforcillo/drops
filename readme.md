# drops

A Drizzle-inspired SQL toolkit for Go, with no dependencies, across
five engines — and one table declared once for all of them.

The usual shape of a 2026 backend is Postgres for state, ClickHouse for
analytics, and a vector store for retrieval. That is normally three
schema declarations, three ingestion paths and three query vocabularies,
kept in step by hand. `drops/mirror` makes the analytics and vector
schemas a *function* of the transactional one, moves changes through a
durable outbox, and gives you the operations nobody ships: replay a
mirror that never had history, ask whether the copies are actually
equal, and walk the mirror's schema forward when the source's moves.

The rest of the toolkit is what that needs, and stands on its own: a
typed schema and query builder per dialect, entities, relations,
migrations with drift detection, and the production patterns most
services end up writing by hand.

`drops` does not wrap an existing driver — it defines its own minimal
`Driver` interface so it stays out of the way of however you connect to
your database. Plug in `database/sql`, `pgx`, or your own pool by
implementing four methods. The SQL core has **zero dependencies**; CI
asserts it.

## Status

Pre-1.0, and the API can still move. Five dialects ship today:

- **`drops/pg`** — PostgreSQL. Full surface: SELECT (joins, grouping,
  CTEs, set ops, window functions, subqueries), INSERT (`RETURNING`,
  `ON CONFLICT`), UPDATE, DELETE, transactions, DDL (schemas,
  extensions, sequences, enums, views, functions, triggers, indexes),
  file-based migrations (Go-native + drizzle-kit compatible), and
  eager-loaded relations (`HasMany`, `HasOne`, `BelongsTo`).
- **`drops/cache`** — driver-agnostic cache interface (`Get` / `Set` /
  `Delete` / `Exists` / `TTL` / `Ping` / `Close` plus a `MultiCache`
  batch extension). Sentinel errors (`ErrNotFound`, `ErrClosed`,
  `ErrInvalidKey`).
- **`drops/cache/memory`** — in-process LRU-ish backend with TTL,
  optional janitor goroutine, FIFO eviction on `MaxEntries`. Zero
  deps; ideal for tests and the local tier of a two-level cache.
- **`drops/cache/redis`** — Redis backend with a minimal RESP2 client
  + bounded connection pool. Zero deps. Supports AUTH (legacy and ACL
  forms), SELECT db, key prefix, and the same `drops.Hook` contract
  used elsewhere.
- **`drops/cache/memcached`** — Memcached backend speaking the classic
  ASCII protocol (get / set / delete, native multi-key get) over a
  bounded connection pool, with `TTL` via the meta-get command
  (`mg`, Memcached 1.6+). Zero deps; `MultiCache`, `KeyPrefix`, `Hook`.
- **`drops/cache/tiered`** — two-level L1 + L2 read-through /
  write-through cache composing any two `cache.Cache` backends (e.g.
  memory in front of redis). Backfills L1 on L2 hits, caps local
  staleness with `L1TTL`, and offers `GetOrLoad` with singleflight
  stampede protection. Zero deps; nests (an L2 can be another tiered).
- **`drops/otel`** — OpenTelemetry spans + RED metrics built from the
  `drops.Hook` stream via small OTel-shaped interfaces (no OTel import;
  a ~10-line adapter bridges the real SDK). One instrumentation covers
  every dialect and every cache backend.
- **`drops/vector`** — one portable vector-search vocabulary shared by
  pgvector, ClickHouse and Qdrant: a `Filter` predicate tree, a `Query`
  builder, `Hit`/`Results` with both distance and score, opaque
  cursors, and the `Store` interface all three backends implement.
- **`drops/qdrant`** — Qdrant vector database. Focused HTTP client
  (stdlib only): collections, upsert/delete/retrieve, search /
  recommend / scroll, and a Must/Should/MustNot filter DSL with
  Eq/In/Range/HasID/Geo conditions. `(*Client).Store` exposes a
  collection as a `vector.Store`.
- **`drops/pg`** with **pgvector** — `Vector(name, dim)`,
  `HalfVec(name, dim)`, `SparseVec`, `BitVec` column types plus
  the distance operators (`<->` L2, `<#>` inner product, `<=>` cosine,
  `<+>` L1) for similarity search in Postgres. HNSW/IVFFlat indexes
  with the right operator class via `Index.OpClass(...)`.
- **`drops/mysql`** — MySQL / MariaDB. Backtick identifiers, `?`
  placeholders, AUTO_INCREMENT, `ON DUPLICATE KEY UPDATE`, prefix
  indexes, and `ORDER BY`/`LIMIT` on UPDATE and DELETE for batched
  maintenance. Because MySQL has no `RETURNING`, `Entity.Create` reads
  a generated key back through the driver's `LastInsertId`. Beyond the
  builders: migrations that read `information_schema` and push a diff,
  a transactional outbox and event store, keyset pagination, typed
  errors keyed on the numbers people know (1062, 1213, 1205, 1451),
  and CTEs, subqueries, window functions, JSON, strings, math and
  dates. MySQL has no transactional DDL, so a failed migration leaves
  the schema half changed — that is the documented contract, and
  `Push` reports how far it got.
- **`drops/mirror`** — one Postgres table, mirrored into ClickHouse for
  analytics and Qdrant for search. The ClickHouse schema is *derived*
  from the pg one rather than declared twice, and changes flow through
  the durable outbox so the copies cannot silently diverge. It also
  covers what an operator has to do to a running mirror: `Reseeder`
  replays history into a mirror that never had it, `Verifier` answers
  whether the copies are actually equal, and `Evolver` walks the
  mirror's schema forward when the source's moves — adding and widening
  on its own, refusing a drop or a narrowing by name.
- **`drops/clickhouse`** — ClickHouse. Engine-bound tables
  (MergeTree family + replicated/distributed via `Raw`), CH-specific
  types (`Array`, `Nullable`, `LowCardinality`, `Decimal`,
  `DateTime64`, `Tuple`, `Map`, `Enum8/16`), full SELECT (`PREWHERE`,
  `FINAL`, `SAMPLE`, `ASOF JOIN`, `SETTINGS`), batch INSERT, and
  the analytics-aggregate library (`uniq`, `uniqExact`, `quantile`,
  `argMax`, `groupArray`, `quantileTiming`, …).

Every dialect shares the root `drops` package (driver interface,
`Expression`, `Builder`, `Hook`, transactions, and the generic
`All[T]` / `One[T]` result scanners).

## Install

```sh
go get github.com/bernardoforcillo/drops
```

To use the bundled `database/sql` adapter (`drops/stdlib`) you also need
a driver — for PostgreSQL, `github.com/jackc/pgx/v5/stdlib`; for
ClickHouse, `github.com/ClickHouse/clickhouse-go/v2`.

## Documentation

The [docs](docs/) directory has the explanations: a
[getting-started tutorial](docs/getting-started.md), how to
[declare a schema](docs/schema.md) without it drifting from your
structs, [entities and relations](docs/entities.md),
[which dialect gives you what](docs/dialects.md),
[portable vector search](docs/vector-search.md), and
[mirroring one table across all three engines](docs/mirror.md), and
[how the two test suites divide the work](docs/testing.md).

Package reference is on
[pkg.go.dev](https://pkg.go.dev/github.com/bernardoforcillo/drops);
every package ships runnable examples.

The rest of this page is the tour.

## Quick start

```go
import (
    "github.com/bernardoforcillo/drops/pg"
    "github.com/bernardoforcillo/drops/stdlib"
)

// Schema. Each pg.Add returns a typed *pg.Col[T] so subsequent
// comparisons and value bindings are checked at compile time.
var (
    Users    = pg.NewTable("users")
    UserID   = pg.Add(Users, pg.BigSerial("id").PrimaryKey())  // *Col[int64]
    UserName = pg.Add(Users, pg.Text("name").NotNull())         // *Col[string]
    UserAge  = pg.Add(Users, pg.Integer("age").Nullable())      // *Col[int32]
)

type User struct {
    ID   int64
    Name string
    Age  *int32
}

// Connection.
sqlDB, _ := sql.Open("pgx", dsn)
db := pg.New(stdlib.New(sqlDB))

// Insert + RETURNING — Val(v) is type-checked against the column.
var u User
db.Insert(Users).
    Row(UserName.Val("Alice"), UserAge.Val(30)).
    Returning(UserID, UserName, UserAge).
    One(ctx, &u)

// Select with typed predicates.
var users []User
db.Select().
    From(Users).
    Where(UserAge.Gte(18)).
    OrderBy(UserName.Asc()).
    All(ctx, &users)
```

A complete demonstration without a database is in
`examples/sqlgen/main.go` — it prints generated SQL. A real DB demo
(via pgx) is under `_examples/postgres/`.

### ClickHouse

The ClickHouse dialect is the same shape, with `?` placeholders and an
engine-bound table:

```go
import (
    _ "github.com/ClickHouse/clickhouse-go/v2"
    "github.com/bernardoforcillo/drops/clickhouse"
    "github.com/bernardoforcillo/drops/stdlib"
)

var (
    Events    = clickhouse.NewTable("events")
    EventID   = clickhouse.Add(Events, clickhouse.UUID("id"))
    EventTS   = clickhouse.Add(Events, clickhouse.DateTime("ts", "UTC"))
    EventUser = clickhouse.Add(Events, clickhouse.UInt64("user_id"))
    EventKind = clickhouse.Add(Events, clickhouse.String("kind").LowCardinality())
    EventDur  = clickhouse.Add(Events, clickhouse.Float64("duration_ms"))
)

func init() {
    Events.
        Engine(clickhouse.MergeTree()).
        OrderBy(EventTS, EventUser).
        PartitionBy(clickhouse.ToYYYYMM(EventTS)).
        Setting("index_granularity", "8192")
}

sqlDB, _ := sql.Open("clickhouse", "clickhouse://localhost:9000/default")
db := clickhouse.New(stdlib.New(sqlDB)).WithHook(
    clickhouse.LoggerHookOrSimilar(...), // any drops.Hook
)
defer db.Close()

// DDL.
db.ExecExpr(ctx, clickhouse.CreateTableIfNotExists(Events))

// Batch insert (small batches; for native columnar bulk loads, drop
// to the driver directly).
db.Insert(Events).
    Row(EventID.Val(uuid1), EventTS.Val(t1), EventUser.Val(42),
        EventKind.Val("click"), EventDur.Val(0.25)).
    Row(EventID.Val(uuid2), EventTS.Val(t2), EventUser.Val(43),
        EventKind.Val("view"), EventDur.Val(1.10)).
    Exec(ctx)

// Analytical query — PREWHERE + CH aggregates.
type bucket struct {
    Day   time.Time
    P95   float64
    Hits  int64
}
var rows []bucket
db.Select(
    clickhouse.As(clickhouse.ToStartOfDay(EventTS), "day"),
    clickhouse.As(clickhouse.QuantileTiming(0.95, EventDur), "p95"),
    clickhouse.As(clickhouse.CountAll(), "hits"),
).
    From(Events).
    Prewhere(EventKind.Eq("click")).
    Where(EventTS.Gte(weekAgo)).
    GroupBy(clickhouse.ToStartOfDay(EventTS)).
    OrderBy(clickhouse.ToStartOfDay(EventTS).Asc()).
    All(ctx, &rows)
```

The `clickhouse` package mirrors `pg`'s `Hook`/`Ping`/`Close`/`InTx`
contract, identifier validation, and `*Col[T]` type safety. The
differences are intentional: ClickHouse-flavoured SQL (PREWHERE,
FINAL, SAMPLE, SETTINGS, ASOF JOIN), engine-bound tables, no
RETURNING / ON CONFLICT / foreign keys.

`clickhouse.Introspect` reads a table's real shape back out of
`system.tables` and `system.columns` — columns and types, the engine
and its parameters, the sorting, primary and partition keys, the TTL,
the settings — `BuildSnapshot` derives the same from the Go
declaration, and `Diff` puts the two side by side. `Push` applies the
result.

`Diff` returns a `Plan` where the other dialects return `[]string`,
because ClickHouse is the dialect where a schema difference does not
always have a statement behind it. There is no `ALTER` for a table's
engine, its partitioning, its primary key, or — beyond appending
columns the same statement adds — its sorting key, and none for a
column that takes part in any of those. Those come back as `Refusal`
values naming the remedy, which is always a new table and a copy.

The sorting key is the one worth pausing on. A `ReplacingMergeTree`
collapses rows that share it, so it is not a layout choice that
happens to affect performance — it is the definition of "the same
row". Changing it changes which rows are one row.

`clickhouse.Analyze` grades the statements a plan does carry: metadata,
a background rewrite of every part, or a deletion with no way back. It
earns its place here more than elsewhere, because a ClickHouse `ALTER`
returns before its work is done — `mutations_sync` defaults to 0 — so a
statement the server accepted may have hours of rewriting still ahead
of it in `system.mutations`.

### Vectors: pgvector

The `pg` package speaks pgvector once the extension is installed.
Declare vector columns alongside ordinary ones; the distance
operators are first-class predicates and ORDER BY expressions.

```go
import "github.com/bernardoforcillo/drops/pg"

var (
    Items         = pg.NewTable("items")
    ItemID        = pg.Add(Items, pg.BigSerial("id").PrimaryKey())
    ItemEmbedding = pg.Add(Items, pg.Vector("embedding", 384)) // []float32
)

// One-time: install the extension and the HNSW index.
db.ExecExpr(ctx, pg.CreateExtensionIfNotExists("vector"))
db.ExecExpr(ctx, pg.CreateTable(Items))
db.ExecExpr(ctx, pg.CreateIndex(
    pg.NewIndex("items_embedding_idx", Items, ItemEmbedding).
        Using("hnsw").
        OpClass(pg.VectorCosineOps).
        With("m = 16, ef_construction = 64"),
))

// k-nearest-neighbours search.
type hit struct {
    ID       int64
    Distance float64
}
var top []hit
db.Select(
    ItemID,
    pg.As(ItemEmbedding.Cosine(query), "distance"),
).
    From(Items).
    OrderBy(ItemEmbedding.Cosine(query)).
    Limit(10).
    All(ctx, &top)
```

Available types: `Vector` (float32), `HalfVec` (float16-on-the-wire,
float32 in Go), `SparseVec`, `BitVec`. Distance operators: `L2Distance`
(`<->`), `InnerProduct` (`<#>`), `CosineDistance` (`<=>`), `L1Distance`
(`<+>`), `HammingDistance`, `JaccardDistance` — plus shorthand methods
`Embedding.L2 / .IP / .Cosine / .L1` on the column.

### Vector database: Qdrant

When pgvector isn't enough — billions of vectors, heavy filtering, or
you already run Qdrant — `drops/qdrant` is a focused HTTP client.
Zero external deps (net/http + encoding/json):

```go
import "github.com/bernardoforcillo/drops/qdrant"

cli, _ := qdrant.NewClient("http://localhost:6333",
    qdrant.WithAPIKey(os.Getenv("QDRANT_API_KEY")))

_ = cli.CreateCollection(ctx, "embeddings", qdrant.CollectionConfig{
    Vectors: qdrant.VectorParams{Size: 384, Distance: qdrant.DistanceCosine},
})

_ = cli.Upsert(ctx, "embeddings", []qdrant.Point{
    {ID: "doc-1", Vector: vec1, Payload: map[string]any{"topic": "go",  "draft": false}},
    {ID: "doc-2", Vector: vec2, Payload: map[string]any{"topic": "rust","draft": false}},
})

hits, _ := cli.Search(ctx, "embeddings", qdrant.SearchRequest{
    Vector:      query,
    Limit:       10,
    WithPayload: true,
    Filter: qdrant.Must(
        qdrant.Eq("topic", "go"),
        qdrant.Eq("draft", false),
        qdrant.Range("created_at", qdrant.RangeOpts{Gte: qdrant.F(1700000000)}),
    ),
})
```

Surface: `CreateCollection` / `DeleteCollection` / `CollectionInfo` /
`ListCollections`, `Upsert` / `DeleteByIDs` / `DeleteByFilter` /
`Retrieve` / `Count`, `Search` / `Recommend` / `Scroll`, plus a
`Must` / `Should` / `MustNot` filter DSL with `Eq` / `In` / `NotIn` /
`MatchText` / `Range` / `HasID` / `IsEmpty` / `IsNull` / `GeoIn` /
`Nest` conditions. `HTTPError` (with `Status`/`Body`) and
`ErrCollectionMissing` are exported for `errors.As` / `errors.Is`.

### One search, three backends: `drops/vector`

pgvector, ClickHouse and Qdrant all do similarity search, and until now
each wanted the query written its own way: a `drops.Expression`
predicate and a distance operator for pgvector, a JSON
Must/Should/MustNot tree for Qdrant, nothing at all for ClickHouse.
Moving a collection between them — or querying two at once — meant
rewriting the search.

`drops/vector` is that query written once. It has no SQL and no HTTP in
it: a portable `Filter` tree, a `Query`, a `Store` interface, and the
score/distance conventions the three backends are normalised onto.

```go
import "github.com/bernardoforcillo/drops/vector"

q := vector.Search(embedding).
    TopK(20).
    Metric(vector.Cosine).
    Where(vector.And(
        vector.Eq("lang", "it"),
        vector.Gte("published_at", 1700000000),
        vector.Not(vector.In("status", "draft", "archived")),
    )).
    WithPayload().
    Build()

res, err := store.Search(ctx, q)   // store is any vector.Store
```

`store` is whichever backend holds the vectors:

```go
// pgvector — a table with a vector(N) column
store := pg.NewVectorStore(db, Docs, DocID, DocEmbedding,
    pg.WithPayloadColumn(DocMeta),   // jsonb
    pg.WithField("lang", DocLang))   // a real column beats jsonb

// ClickHouse — an Array(Float32) column, no extension needed
store := clickhouse.NewVectorStore(chdb, Docs, DocID, DocEmbedding,
    clickhouse.WithPayloadColumn(DocMeta))

// Qdrant — a collection
store := cli.Store("embeddings", qdrant.WithMetric(vector.Cosine))
```

Filter fields are strings because Qdrant payloads have no schema. The
SQL stores resolve them in two steps: a `WithField` mapping compiles to
that column, otherwise the name becomes a JSON accessor into the
payload column (dotted names walk nested objects). A field that matches
neither is `vector.ErrUnknownField` rather than a predicate that
silently matches nothing.

**Distance and score.** Every `Hit` carries both. `Distance` is in the
metric's own units and smaller is closer; `Score` is a ranking value
where larger is better. The conversion lives in one place, so a
`MaxDistance(0.25)` means the same thing whether it becomes a `<=` on a
pgvector expression or Qdrant's `score_threshold` — Qdrant's score,
which is a similarity for Cosine/Dot but the raw distance for
Euclid/Manhattan, is normalised on the way out.

**Pagination.** One opaque cursor, whatever is underneath:

```go
for cursor := ""; ; {
    res, err := store.Search(ctx, vector.Search(v).TopK(50).After(cursor).Build())
    if err != nil {
        return err
    }
    handle(res.Hits)
    if !res.HasMore {
        break
    }
    cursor = res.NextCursor
}
```

The two SQL stores paginate by keyset on `(distance, id)` — the next
page is guarded by `(distance, id) > (lastDistance, lastID)`, so
concurrent inserts cannot shift a row across a page boundary the way
`OFFSET` does. Qdrant's search API has no keyset, so its cursor carries
an offset instead. Both encode to the same opaque string, and each is
stamped with the backend that issued it: replaying a Qdrant cursor
against a pgvector store returns `vector.ErrCursorMismatch` rather than
quietly serving the wrong page. `HasMore` costs no extra round trip —
every backend asks for `TopK+1` and trims.

Two honest caveats. pgvector's HNSW/IVFFlat indexes are approximate and
apply `WHERE` on top of what the index returned, so a selective filter
or a deep page can return fewer than `TopK` rows; widen the search with
`.Param("hnsw.ef_search", 200)`. ClickHouse without a vector index is
an exact brute-force scan — never lossy, but linear.

## Design

### Typed columns

Type constructors return `*pg.Col[T]`, where `T` is the Go value type
(`int32` for `integer`, `string` for `text`, `time.Time` for
`timestamp`, `[]byte` for `bytea`, etc). Builder methods preserve `T`,
so a chained declaration stays typed end-to-end:

```go
UserAge.Eq(30)           // OK
UserAge.Eq("thirty")     // compile error: cannot use string as int32
UserAge.Val(30)          // OK; binds as $N
```

For places that don't care about the value type — `JOIN ON`, `ON
CONFLICT (...)`, `EXCLUDED.col` — both `*Column` and `*Col[T]` satisfy
the `pg.ColRef` interface, so call sites accept either.

### Driver interface

The root package defines:

```go
type Driver interface {
    Exec(ctx context.Context, sql string, args ...any) (Result, error)
    Query(ctx context.Context, sql string, args ...any) (Rows, error)
    Begin(ctx context.Context) (Tx, error)
}
```

`drops` itself imports no concrete driver. The `stdlib` subpackage
adapts `*sql.DB`; you can write your own adapter for `pgx.Pool` or
anything else in a few dozen lines.

### Building queries

Every fragment of SQL — a column, an operator, a subquery — is a
`drops.Expression`:

```go
type Expression interface {
    WriteSQL(b *Builder)
}
```

The `Builder` handles parameter binding ($N) and identifier quoting.
Operators come in two flavours:

- **Typed methods on `*Col[T]`** — `UserAge.Gte(18)`, `UserName.Like("A%")`, `UserAge.In(18, 21, 25)`, `UserAge.Between(18, 65)`. Type-checked.
- **Untyped free functions** — `pg.Eq(a, b)`, `pg.And(...)`, `pg.Or(...)`, `pg.Not(p)`, `pg.In(col, slice)`. Useful for column-to-column comparisons, AND/OR composition, and slice expansion.

### Scanning

`All(ctx, &dest)` and `One(ctx, &dest)` scan rows into struct values.
Field-to-column mapping rules:

1. `drop:"name"` struct tag, if present (`drop:"-"` to skip)
2. exact field name match
3. snake_case of the field name (`UserID` → `user_id`)

Unmatched columns go to a discard sink, so projecting fewer columns
than the struct has is fine.

### Observability

Attach a `drops.Hook` to log every operation, time queries, raise
slow-query alerts, or feed a tracer. Hooks fire for `exec`, `query`,
`begin`, `commit`, `rollback`, and `ping` — including those issued
through query builders — and are propagated into transaction-bound
DBs returned by `Begin` / `InTx`.

```go
db := pg.New(stdlib.New(sqlDB)).WithHook(
    pg.LoggerHook(log.Printf, pg.LoggerOptions{
        SlowQuery: 100 * time.Millisecond,
    }),
)

// Compose multiple hooks (metrics + logging):
db = db.WithHook(drops.ChainHooks(metricsHook, loggerHook))
```

Each event is a `drops.QueryEvent{Kind, SQL, Args, Duration,
WaitDuration, WaitKnown, Err}`. `LoggerHook` is one convenience built
on top — write your own for OTel, Prometheus, Datadog, etc. in a few
lines. `db.Ping(ctx)` issues `SELECT 1` and is the natural shape for a
Kubernetes readiness probe.

#### Queue time is not query time

`Duration` is two measurements added together: the wait for a
connection from the pool, and the time the database took. They have
opposite remedies — the first says add connections or shed
concurrency, the second says fix the query or the index — so reporting
only their sum leaves neither actionable.

drops cannot time the acquisition itself; `drops.Driver` is three
methods and the pool lives behind them. A pool that can check out a
single connection can implement `pg.ConnAcquirer`, and
`pg.QueueTimed` then measures the checkout and nothing else:

```go
db := pg.New(pg.QueueTimed(myPool)).WithHook(hook)

// in the hook:
if e.WaitKnown {
    queueTime.Observe(e.WaitDuration.Seconds())
}
if serverTime, ok := e.QueryDuration(); ok {
    dbTime.Observe(serverTime.Seconds())
}
```

A driver that measures the wait internally can report it directly with
`drops.ReportConnWait`. A driver that reports nothing leaves
`WaitKnown` false and the split simply absent — a queue-time gauge
reading zero because nobody was counting is worse than no gauge at
all, so `drops/otel` records `db.client.connection.wait_time` and
`db.client.operation.query_time` only for events that carry a real
measurement.

### Replicas and the post-write window

`pg.NewReplicated(primary, replicas...)` is a `drops.Driver` that sends
writes to the primary and fans reads out over the replicas. The hard
part is not the fan-out, it is the moment right after a write, when the
replica a read would land on may not have replayed it yet.

```go
repl := pg.NewReplicated(primary, r1, r2).
    WithLSNTracking(50 * time.Millisecond). // route by replay position
    WithWriteDelay(1 * time.Second)         // ...but not for a second after a write
db := pg.New(repl)

ctx = pg.WithReadYourWrites(ctx, 2*time.Second)
```

- **`WithReadYourWrites(ctx, d)`** is the window: a write on this
  context arms it, and reads stay off the replicas while it is open.
  `d=0` clears it.
- **`WithLSNTracking(ttl)`** spends less of the primary's capacity on
  it: the write's WAL position is captured and a read may go to any
  replica that has replayed past it.
- **`WithWriteDelay(d)`** is the floor under that. A caught-up replay
  position proves the row just written is there and says nothing about
  what the write set in motion — a trigger, a view refresh, a row in
  another table. For `d` after a write, reads go to the primary
  whatever the replicas report. It widens a caller's shorter window and
  leaves one explicitly cleared with `d=0` cleared.

A commit is a write: a transaction that wrote arms the window when it
commits, and a rolled-back one arms nothing. So is a statement with a
`RETURNING` clause, even though it is issued through `Query` — that is
how `InsertBuilder.Scan` gets a generated key back — so `Query` routes
by what the statement does, not by the method it arrived on.

Routing decides where a statement goes; it does not decide what a
statement does. `db.InReadTx(ctx, fn)` makes "this only reads" a rule
the server keeps, running `fn` inside a read-only transaction that
PostgreSQL refuses writes in with SQLSTATE 25006, and routing it the
way a read is routed. `drops.Driver.Begin` takes a context and nothing
else, so drops issues `SET TRANSACTION READ ONLY` as the transaction's
first statement; a driver that implements the one-method
`pg.ReadOnlyBeginner` extension saves that round trip. If neither path
works the call returns `pg.ErrNotReadOnly` rather than a transaction
that can write.

### Query tagging

A hook can see a statement but cannot change it — it fires after the
operation and its return value is discarded. Attributing a statement
back to the code that issued it has to happen *before* it is sent, so
tagging is plumbed into the render path instead: put tags on the
context and every dialect appends them to the statement as a trailing
[SQLCommenter](https://google.github.io/sqlcommenter/) comment.

```go
ctx = drops.WithQueryTags(ctx,
    drops.Tag{Key: "controller", Value: "users"},
    drops.Tag{Key: "action", Value: "show"},
)
ctx = drops.WithQueryTag(ctx, "request_id", "7f3a")

rows, err := db.Query(ctx, `SELECT * FROM "users" WHERE "id" = $1`, id)
// SELECT * FROM "users" WHERE "id" = $1 /*action='show',controller='users',request_id='7f3a'*/
```

That comment is the only part of a statement that survives into
`pg_stat_statements`, MySQL's slow log and a proxy's logs, which is
what turns a slow query there back into a line of application code
here. Rails and EF Core both ship this; `context.Context` is a better
carrier for it than the thread-locals Rails uses, because it already
follows the request across goroutines.

The details that matter:

- **Trailing, not leading.** Too much tooling switches on a
  statement's first token — MySQL executes `/*!` and reads `/*+` as a
  hint, proxies split reads from writes by leading keyword.
- **Percent-encoded.** Tag values are application strings landing
  inside a comment, so a value containing `*/` would end it and
  continue as SQL. Everything outside RFC 3986's unreserved set is
  escaped, which makes that impossible rather than unlikely; there are
  live-server tests that feed exactly that payload.
- **Arguments are not taggable.** `Tag` holds two strings — no `any`,
  no `Stringer` — and the function that renders the comment never
  receives the argument slice. Bound values are user data and a
  tracing backend is not where they go.
- **Free when unused.** No tags on the context means no comment and no
  work beyond one context lookup.
- **Cardinality costs.** Tag values are part of the statement text, so
  a per-request id defeats statement caching for as long as it is set.
  Controller and action are free; request ids are for the requests
  worth tracing.

Works in `pg`, `mysql`, `sqlite` and `clickhouse` — a feature present
in one dialect only would be a footgun in the other three.

### Sentinel errors

Common failure modes are exported so callers can branch with
`errors.Is`:

| Sentinel | When |
|---|---|
| `pg.ErrReturningRequired` | `INSERT/UPDATE/DELETE.All` or `.One` called without `Returning(...)` |
| `pg.ErrNoRowsToInsert` | `Insert(t).Exec` with no `Row(...)` |
| `pg.ErrNoUpdateAssignments` | `Update(t).Exec` with no `Set(...)` |
| `pg.ErrSchemaRequired` | `Push` called with a nil `*Schema` |
| `pg.ErrInvalidIdentifier` | bad table / schema / column name (empty, NUL, non-UTF8) |
| `pg.ErrNoRows` | `Select.One` / `Find.One` matched zero rows |
| `pg.ErrNoMigrationsApplied` | `Migrator.Down` with empty history |

### Transactions

```go
db.InTx(ctx, func(tx *pg.DB) error {
    // tx is a DB bound to the transaction; nil commits, error rolls back.
    return nil
})
```

Or take an explicit handle with `db.Begin(ctx)` and call `Commit` /
`Rollback` yourself.

### Relations

Declare relations once, eager-load with `Find().With(...)`:

```go
pg.NewRelations(Users).
    HasMany("posts", Posts, UserID, PostUserID).
    ManyToMany("groups", Groups, UserGroups,
        UserGroupsUserID, UserGroupsGroupID, // junction FKs
        UserID, GroupID,                      // local + target keys
    )
pg.NewRelations(Posts).
    BelongsTo("author", Users, PostUserID, UserID)

type Post struct {
    ID     int64
    UserID int64 `drop:"user_id"`
    Title  string
}
type User struct {
    ID     int64
    Name   string
    Posts  []Post  `dropRel:"posts"`     // matched by tag
    Groups []Group `dropRel:"groups"`    // many-to-many through UserGroups
}

var users []User
db.Find(Users).
    With("posts", "groups").
    Where(UserAge.Gte(18)).
    All(ctx, &users)
```

Each kind takes a different shape:

| Kind | Field type | Queries fired |
|------|------------|---------------|
| `HasMany` | `[]Child` or `[]*Child` | parent + 1 child query |
| `HasOne` | `Child` or `*Child` | parent + 1 child query (takes the first match) |
| `BelongsTo` | `Parent` or `*Parent` | row + 1 parent query |
| `ManyToMany` | `[]Target` or `[]*Target` | parent + 1 junction + 1 target query |

Relation fields are matched by `dropRel:"<name>"` tag first, then by
case-insensitive name match.

#### Nested (deep) relations

Eager-load relations of relations with dot paths. Each edge runs exactly
one batched query — `With("posts.comments")` fetches every parent's posts,
then every comment of those posts, regardless of how many rows are
involved. Paths that share a prefix are merged, so the shared edge is
fetched only once:

```go
type Comment struct {
    ID     int64
    PostID int64 `drop:"post_id"`
    Body   string
}
type Post struct {
    ID       int64
    UserID   int64     `drop:"user_id"`
    Title    string
    Comments []Comment `dropRel:"comments"`
}

pg.NewRelations(Posts).HasMany("comments", Comments, PostID, CommentPostID)

var users []User
db.Find(Users).
    With("posts.comments", "posts.tags"). // posts fetched once, fans out
    All(ctx, &users)
// users[i].Posts[j].Comments is populated in place.
```

Unknown relations — at any depth — are reported before a single query
runs, so a typo in `With("posts.commnets")` fails fast.

#### Filtering and ordering an eager load

`WithRel` configures one relation with a `Where` filter, an `OrderBy`, and
any deeper relations — all applied to that edge's single batched query, so
filtering/sorting costs nothing extra:

```go
var users []User
db.Find(Users).
    WithRel("posts", func(p *pg.RelConfig) {
        p.Where(Published.Eq(true)).      // only published posts
            OrderBy(PostCreatedAt.Desc()). // newest first, per user
            With("comments")               // …and load their comments
    }).
    All(ctx, &users)
```

The `Where` is AND-ed onto the `IN (parent keys)` predicate; the `OrderBy`
sorts the batched result, and because rows are grouped in arrival order
each parent's slice comes out correctly sorted. For `ManyToMany`, `OrderBy`
re-sorts each parent's slice into the target query's order (the default,
without `OrderBy`, follows junction-row order). `WithRel` and `With` merge
when they name the same edge, so it is still fetched once.

`RelConfig.Limit` and `.Offset` cap the rows attached **per parent**,
which is drizzle's `with: { posts: { limit: 5 } }`. A plain `LIMIT`
would cap the whole batched result rather than each parent's slice, so
it compiles to a `ROW_NUMBER() OVER (PARTITION BY <fk> ORDER BY ...)`
window and filters on the rank:

```go
var users []User
db.Find(Users).
    WithRel("posts", func(p *pg.RelConfig) {
        p.OrderBy(PostCreatedAt.Desc()).Limit(5) // five newest, per user
    }).
    All(ctx, &users)
```

It applies to `HasMany`, `MorphMany` and `ManyToMany`; the single-row
relations already cap at one.

#### Filtering the parent by its relation

`With` and `Load` answer "these authors, and their books". `WhereHas`
answers the other question — "the authors who *have* a published book" —
where the relation is evidence about the parent rather than something to
load. It is Eloquent's `whereHas`, and it compiles to an `EXISTS` over
the relation's own join condition:

```go
var authors []Author
db.Find(Authors).
    WhereHas("books", func(q *pg.RelQuery) {
        q.Where(BookPublished.Eq(true))
    }).
    All(ctx, &authors)

// SELECT * FROM authors WHERE EXISTS (
//   SELECT 1 FROM books
//   WHERE books.author_id = authors.id
//     AND books.deleted_at IS NULL   -- the books table's own filters
//     AND books.published = $1
// )
```

The related table's global filters are **inside** the subquery, so a
soft-deleted book does not make its author match. Step around one by
name with `q.IgnoreFilters(pg.FilterSoftDelete)`, or all of them with
`q.Unscoped()`.

That means the filters the *table* registers. The tenant axis is not
one of them — `Entity.ScopeByTenant` builds it per query from the ctx
tenant, which a `*Table` cannot see — so it narrows the outer `SELECT`
and not the subquery: an author of yours whose only book belongs to
another tenant still matches, and `q.IgnoreFilters(pg.FilterTenant)`
has nothing to bypass. The eager loaders reach exactly as far. Where it
matters, exclude the other tenant in the callback:
`q.Where(BookOrg.Eq(org))`.

`WhereDoesntHave` is the same subquery under `NOT EXISTS` — "every order
with no shipment". `WhereHasRel` / `WhereDoesntHaveRel` take a relation
handle instead of a name, the way `LoadRel` does. Both are available on
the typed `Entity.Query` too, and both nest: `q.WhereHas(...)` inside the
callback puts an `EXISTS` inside the `EXISTS`.

Every kind but `MorphTo` works — a `ManyToMany` joins its junction to its
target inside the subquery, and both tables' filters apply.

A count bound replaces the `EXISTS` with a correlated `count(…)`
comparison, because there is nothing to short-circuit once a threshold is
involved:

```go
db.Find(Authors).WhereHas("books", func(q *pg.RelQuery) {
    q.Where(BookPublished.Eq(true)).CountGte(3)
})
```

`CountEq`, `CountGt`, `CountGte`, `CountLt`, `CountLte`. The number goes
in the method name so a mis-typed comparison is a compile error. For a
many-to-many it counts distinct targets, not junction rows.

### Seeding generated data

`pg.SeedAdd` takes rows you wrote out, which is right for a fixture with
three named users in it and wrong for a table that needs to look
populated. `pg.SeedMany` takes a count and a template instead, and
`pg.SeedRelated` fans out along a relation you already declared:

```go
seeder := pg.NewSeeder(db).WithSeed(42)

authors := pg.SeedMany(seeder, AuthorEntity, 10, func(g *pg.Gen, a *Author) {
    a.Name  = g.FullName()
    a.Email = g.Email()            // distinct within a run, so UNIQUE holds
    a.Bio   = pg.Maybe(g, 0.5, g.Sentence()) // sometimes NULL
})
pg.SeedRelated(authors, "books", BookEntity, 2, 5, func(g *pg.Gen, b *Book, a *Author) {
    b.Title     = g.Sentence()
    b.Published = pg.Weighted(g,
        pg.Choice[bool]{Value: true, Weight: 7},
        pg.Choice[bool]{Value: false, Weight: 3})
    b.WrittenAt = g.TimeBetween(start, end)
})

if err := seeder.Apply(ctx); err != nil { ... }
// authors.Rows() and the books handle report what was created, keys included.
```

Nothing in the second call restates how a book points at an author: the
relation was declared once with `NewRelations`, and that declaration is
what `SeedRelated` reads to write the foreign key. Plans nest to any
depth, and the per-parent count is drawn per parent — ten authors get ten
different numbers between two and five, not one number ten times.

Every value comes from the seeder's `pg.Gen`, a PRNG built from one
integer (`math/rand/v2`, no dependency). `Apply` rewinds it first, so the
same seed against a clean database produces the same rows — which is what
makes a test that fails on generated data re-runnable. Two rules keep
that true and are worth knowing: nothing in `pg.Gen` reads the clock
(timestamps come from a range you supply), and a `Gen` is not safe for
concurrent use.

The generators are `FullName`, `FirstName`, `LastName`, `Email`, `Word`,
`Words`, `Sentence`, `Paragraph`, `TimeBetween`, `IntN`, `IntRange`,
`Float64Range`, `Bool`, `Chance`, plus the free functions `Pick`,
`Weighted`, `Maybe` and `Shuffle`. That is the whole list on purpose:
this is not a faker library, and the name and word lists are short,
fixed and English rather than pretending to be localised data. Domain
values — statuses, currencies, SKUs — come from `Pick` and `Weighted`,
where the caller supplies them.

### Migrations

The whole loop has a front end — `drops generate`, `drops migrate`,
`drops push`, `drops drift`, `drops pull`, `drops baseline`,
`drops status` — described under [The `drops` CLI](#the-drops-cli).
Everything it does, it does by calling the four library pieces below,
so a project that would rather drive them from its own `main` loses
nothing.

Four pieces ship in the box:

1. `pg.GenerateMigration` — produces drizzle-kit-format migrations from a Go schema (diff against the previous snapshot).
2. `pg.Push` — introspects the live database, diffs vs the Go schema, applies the changes directly (drizzle-kit `push` equivalent; no file history).
3. `pg.DrizzleMigrator` — applies migrations written in drizzle-kit's format (either by `GenerateMigration` or by drizzle-kit itself).
4. `pg.Migrator` — a simpler standalone runner that uses its own file convention. Use this if you don't want any drizzle compatibility.

All four understand the `Safe` / `IF [NOT] EXISTS` mode, see [Idempotent DDL](#idempotent-ddl) below.

#### Generating migrations (`pg.GenerateMigration`)

Given a `*pg.Schema` describing your tables, `GenerateMigration`:

- reads `drizzle/meta/_journal.json` and the latest `meta/<idx>_snapshot.json` (if any)
- builds a fresh snapshot from your current Go schema declarations
- diffs the two and emits the SQL to evolve the database
- writes `<dir>/<NNNN>_<name>.sql`, `<dir>/meta/<NNNN>_snapshot.json`, and an updated `<dir>/meta/_journal.json`

The output is byte-for-byte identical between `drops` and `drizzle-kit` for the features we both support (tables, columns with PG types, `NOT NULL`, `DEFAULT`, single-column `UNIQUE`, single-column foreign keys with `ON DELETE` / `ON UPDATE`). Snapshots round-trip through both tools.

```go
schema := pg.NewSchema(Users, Posts, UserGroups, Groups)

res, err := pg.GenerateMigration(pg.GenerateOptions{
    Schema: schema,
    Dir:    "drizzle",
    Name:   "init", // omit for a random "ancient_forest"-style name
})
if err != nil {
    log.Fatal(err)
}
if res.NoOp {
    log.Println("schema unchanged")
} else {
    log.Printf("wrote %s", res.Tag)
}
```

Typical workflow: stash this in a `cmd/migrate/main.go` (or similar) and run `go run ./cmd/migrate` whenever the schema changes. The output is what drizzle-kit's `generate` command would produce, so the existing drizzle-orm runtime — or `pg.DrizzleMigrator` below — can apply it.

A runnable in-memory walkthrough is in [examples/generate/main.go](examples/generate/main.go).

What `GenerateMigration` covers (today): CREATE TABLE, DROP TABLE, ADD/DROP COLUMN, ALTER COLUMN type/NOT NULL/DEFAULT, UNIQUE and FOREIGN KEY (single- and multi-column, with ON DELETE/ON UPDATE), composite primary keys, CHECK constraints, indexes, enums, sequences, views and materialised views, row-level security (enabled and forced) and policies. `BuildSnapshot` fills all of those, and `Diff` renders DDL for them.

What it does not cover: renames, which a structural diff cannot tell from a drop plus an add; an index's operator class, storage parameters, column ordering or `NULLS NOT DISTINCT`, none of which reach the snapshot; an index over an expression rather than a column, which the snapshot cannot describe at all; which columns a view reads, so a migration that drops or retypes a column rebuilds every declared view rather than working out which ones were in the way; and removing or reordering an enum's labels, which PostgreSQL cannot do in place. `pg.Push`'s doc comment lists the same limits from the live-database side, along with the notices Push raises for the ones it can see but not act on.

#### Pushing directly (`pg.Push`)

For development loops where you'd rather skip the migration file and just sync the database to your current Go schema:

```go
res, err := pg.Push(ctx, db, pg.NewSchema(Users, Posts),
    pg.PushOptions{Safe: true})
if err != nil {
    log.Fatal(err)
}
log.Printf("applied %d statements", len(res.Statements))
```

`Push` introspects the live database — `information_schema` for the columns and constraints, `pg_catalog` for what it has no view of: enums, sequences, views, policies and the row-level security flags — builds a snapshot from your Go schema, diffs the two, and applies the SQL inside a single transaction. A `DryRun: true` option returns the statements without executing — useful for previewing in CI.

There is no migration history written: `Push` is convenient for prototyping and tests, not for production where you want reviewable, reproducible migrations. For those, use `GenerateMigration` + `DrizzleMigrator`.

`Push` does not remove something the Go schema never declared. `Diff` compares two declarations, so an object missing from the newer one was removed; `Push`'s "previous" side is a database, where an index, an enum, a sequence, a view or a policy missing from the Go schema was very likely never declared there at all. Those drops are held back and returned as `res.Notices`, each carrying the exact statement so you can run it by hand — set `DropUnmanagedIndexes` or `DropUnmanagedObjects` to let them through instead. Row-level security is held back the same way and for a sharper reason: a table with RLS enabled in the database and no `EnableRLS` in Go is one `ALTER TABLE` away from serving every row to every caller, so `Push` reports an `unmanaged-rls` notice rather than performing it.

Notices also cover what `Push` can see but cannot act on — an enum whose labels were reordered, an index it cannot represent, an expression the server would not parse. A push that returns no statements and no notices is a database that matches the schema.

Underneath, `Push` is just three reusable pieces you can also call separately:

```go
current, _ := pg.Introspect(ctx, db)                  // *Snapshot from the live DB
desired := pg.BuildSnapshot(pg.NewSchema(Users, Posts)) // *Snapshot from the Go schema
stmts := pg.Diff(current, desired, pg.DiffOptions{Safe: true})
// stmts is the SQL diff — execute, review, or pipe wherever
```

One thing `Push` does that this three-line version does not: it asks the server to respell the declared CHECK bodies, index predicates, policy clauses and view definitions before diffing them. `Introspect` reads those back in PostgreSQL's own spelling — `age >= 0` comes back `(age >= 0)` — so a bare `Diff` of the two reports a difference in every one of them and keeps reporting it. Use `Push` (with `DryRun: true` to preview) for anything holding an expression.

#### Idempotent DDL

`DiffOptions{Safe: true}` (and the matching `GenerateOptions.Safe` / `PushOptions.Safe`) wraps every destructive or creative DDL in `IF [NOT] EXISTS`:

| Operation | Default | `Safe: true` |
|-----------|---------|--------------|
| CREATE TABLE | `CREATE TABLE "users" (...)` | `CREATE TABLE IF NOT EXISTS "users" (...)` |
| DROP TABLE | `DROP TABLE "users" CASCADE;` | `DROP TABLE IF EXISTS "users" CASCADE;` |
| ADD COLUMN | `... ADD COLUMN "age" integer;` | `... ADD COLUMN IF NOT EXISTS "age" integer;` |
| DROP COLUMN | `... DROP COLUMN "age";` | `... DROP COLUMN IF EXISTS "age";` |
| DROP CONSTRAINT (FK / UNIQUE) | `... DROP CONSTRAINT "...";` | `... DROP CONSTRAINT IF EXISTS "...";` |
| ALTER COLUMN (type/NULL/default) | unchanged — PostgreSQL has no `IF EXISTS` form | unchanged |

#### Existence checks

If you need to branch on the live state of the database, four helpers query `information_schema`:

```go
ok, _ := pg.SchemaExists(ctx, db, "drizzle")
ok, _  = pg.TableExists(ctx, db, "", "users")             // "" → public
ok, _  = pg.ColumnExists(ctx, db, "", "users", "email")
ok, _  = pg.ConstraintExists(ctx, db, "", "users", "users_email_unique")
```

#### Drizzle-kit compatible (`pg.DrizzleMigrator`)

`drops` reads migrations written by [drizzle-kit](https://orm.drizzle.team/docs/migrations) verbatim. The on-disk layout, hashing, history table, and statement-splitting protocol all match drizzle-orm at apply time, so the same migration set can be applied by either runtime against the same database without conflict.

What it expects (drizzle-kit's default output):

```
drizzle/
├── 0000_warm_iron_man.sql
├── 0001_serious_jack_flag.sql
└── meta/
    ├── _journal.json
    ├── 0000_snapshot.json
    └── 0001_snapshot.json
```

What it does:

- Reads `meta/_journal.json` for ordering and per-entry `breakpoints` flag.
- Computes `sha256(<file bytes>)` for each `<tag>.sql` (same hash drizzle-orm computes).
- Tracks history in `drizzle.__drizzle_migrations(id serial pk, hash text, created_at bigint)`.
- Skips entries whose hash is already in the history table.
- Splits each file on `--> statement-breakpoint` when the entry has `breakpoints: true`; runs the file as one statement when `false`.
- Wraps each migration (statements + history insert) in a single transaction.

```go
//go:embed drizzle/*
var migrations embed.FS

m := pg.NewDrizzleMigrator(db, migrations, "drizzle")
if err := m.Up(ctx); err != nil {
    log.Fatal(err)
}
```

If your `drizzle.config.ts` overrides `migrationsSchema` / `migrationsTable`, mirror that:

```go
pg.NewDrizzleMigrator(db, migrations, "drizzle").
    WithSchema("public").
    WithTable("schema_migrations")
```

#### Go-native (`pg.Migrator`)

For projects that don't use drizzle-kit, a simpler file or code-driven runner. Migration files use `<version>_<name>.{up,down}.sql`; history is tracked in `_drops_migrations`. Supports rollbacks (drizzle's runtime does not).

```go
//go:embed migrations/*.sql
var migrations embed.FS

m := pg.NewMigrator(db)
if err := m.AddFS(migrations, "migrations"); err != nil {
    log.Fatal(err)
}
if err := m.Up(ctx); err != nil {
    log.Fatal(err)
}
```

Go-defined migrations work with the same Migrator:

```go
m.Add(pg.Migration{
    Version: "0003",
    Name:    "backfill_users",
    Up: func(ctx context.Context, db *pg.DB) error {
        _, err := db.Exec(ctx, `UPDATE users SET status = 'active' WHERE status IS NULL`)
        return err
    },
})
```

#### Data hooks (migrating data between schema migrations)

When a schema change needs a data migration alongside it — backfill a new
column, copy rows into a split-out table, rewrite a value before an old
column is dropped — register a `BeforeEach` / `AfterEach` hook. Hooks run
**inside the same transaction** as the migration, so the data change
commits atomically with the schema change (or rolls back with it on error).

On the Go-native `Migrator`, the hook receives the tx-scoped `*pg.DB`, the
`Migration`, and the direction, so a step can be scoped to one version and
one direction:

```go
m.AfterEach(func(ctx context.Context, tx *pg.DB, mig pg.Migration, dir pg.MigrationDirection) error {
    if dir == pg.DirectionUp && mig.Version == "0004" {
        // 0004 added full_name; backfill it from the split columns.
        _, err := tx.Exec(ctx, `UPDATE users SET full_name = first_name || ' ' || last_name`)
        return err
    }
    return nil
})
```

The `DrizzleMigrator` gets the same seam. Since drizzle files are pure SQL
with no place for Go logic, a `DrizzleHook` lets a backfill run atomically
with a file's statements, keyed by tag:

```go
m := pg.NewDrizzleMigrator(db, migrations, "drizzle")
m.AfterEach(func(ctx context.Context, tx *pg.DB, e pg.DrizzleEntry) error {
    if e.Tag == "0001_add_full_name" {
        _, err := tx.Exec(ctx, `UPDATE users SET full_name = first_name || ' ' || last_name`)
        return err
    }
    return nil
})
```

A hook that returns an error aborts that migration; the whole transaction
rolls back.

## The `drops` CLI

```
go install github.com/bernardoforcillo/drops/cmd/drops@latest
```

```
drops generate --schema ./db/schema --name add_articles   # write a migration
drops migrate                                             # apply the pending ones
drops migrate down --allow-destructive                    # roll the last one back
drops push --schema ./db/schema --dry-run                 # diff against the live DB
drops drift --schema ./db/schema                          # exit 3 if they disagree
drops pull --out ./db/schema/schema.go                    # introspect a database into Go
drops baseline                                            # adopt a database that already exists
drops status                                              # applied, pending, unaccounted for
drops lint ./...                                          # exit 3 on a query mistake
```

Connection: `--dsn`, else `$DROPS_PG_DSN`, else `$DATABASE_URL`. The
binary carries pgx behind `drops/stdlib`, so it needs no configuration
beyond the connection string.

It can carry a driver because `cmd/drops` is a Go module of its own,
outside the no-dependencies promise the library keeps — `go install`
resolves a nested module by tags that carry the directory, so a
release is tagged `cmd/drops/v0.7.0`, not `v0.7.0`. One command,
`push`, needs `github.com/jackc/pgx/v5` in *your* module as well: it
is the one whose generated program opens the connection, and `go run`
resolves that program's imports in your module rather than in the
binary's. It says so if it is missing. See
[docs/cli.md](docs/cli.md#the-module-and-what-it-costs).

### How the CLI reads a Go schema

It runs it. A schema built out of `pg.NewTable` and `pg.Add` is a Go
value, and no amount of parsing recovers a value — resolving it means
resolving the whole language. So `drops` writes a small program into
your module that imports your schema package, calls into it, and
prints the answer as JSON; `go run` compiles that against your real
package with the real compiler. The program is deleted afterwards.

The one thing it has to know is where to call, which is the whole
convention:

```go
// db/schema/schema.go
package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())
)

// Schema is what drops manages. A table you declare and do not
// register here is invisible to it — which is how a table another
// tool owns stays out of drops's way.
func Schema() *pg.Schema {
	return pg.NewSchema(Users)
}
```

`examples/cli` is that package, in full, with a foreign key, a check
constraint and an index.

### Destructive changes

`push`, `migrate` and `migrate down` run every statement they are
about to apply through `pg.AnalyzeMigration` first. A statement that
destroys data or an object — `DROP TABLE`, `DROP COLUMN`, `TRUNCATE`,
`DROP TYPE`, `ALTER TYPE ... DROP VALUE` — stops the command, which
prints each statement it is holding back and exits 3. `--allow-destructive`
runs them. Statements that are merely expensive — a rewrite, a
non-concurrent index build, a `SET NOT NULL` — are printed as warnings
and applied.

### Exit codes

| code | meaning |
| ---- | ------- |
| 0 | success |
| 1 | failure — unreachable database, bad SQL, a file that would not parse |
| 2 | the command line was wrong |
| 3 | the command ran and the answer was no: drift found, or changes refused |

`drops drift` returning 3 is what makes it a CI gate.

### `drops lint`

The one subcommand that reads Go source rather than a schema or a
database. Three `go/analysis` rules: a DELETE or UPDATE executed with
nothing to bound it, a read of every row of a table, a relation
eager-loaded once per iteration of a loop — the N+1
`pg.WithN1Detector` reports at run time, reported before it runs.

The equivalent ESLint plugin has to guess from a method name. A
`go/analysis` pass is handed the type checker's answers, so it knows
the value is a `*pg.DeleteBuilder`, knows which package-level
`pg.Table` the statement targets, and can carry a fact about that
table — "this one is small" — into every package that imports the
schema. `//drops:lint lookup` on the declaration is how a table says
so; `//drops:lint ignore <rule> — reason` is how a deliberate offender
does.

A linter is judged on false positives, so each rule documents where it
stops: within one function body, flow-insensitively, at the point a
statement executes rather than where `ToSQL` renders it. Run over
drops itself — `pg`, `sqlite`, `mysql`, `clickhouse`, the examples,
the CLI and the integration suite — it is quiet, and getting it there
corrected two rules. [docs/lint.md](docs/lint.md) has the details.

### drizzle-kit interoperability

`drops migrate` applies a migration set drizzle-kit generated, and
drizzle-orm applies one `drops generate` wrote: the directory layout,
the journal, the SHA-256 of each file and the
`drizzle.__drizzle_migrations` history table are the same on both
sides. The one addition is the rollback direction — `drops generate`
writes a `<tag>.down.sql` beside each migration, which drizzle-kit has
no concept of, and `drops migrate down` uses it.

## PostgreSQL feature surface

In addition to the schema/query/migration story above, the `pg` package
exposes the rest of PostgreSQL's catalog of object types and built-in
operators/functions as plain Go helpers. Each returns a `drops.Expression`
that composes anywhere a SQL fragment is expected.

### DDL objects beyond tables

```go
pg.CreateSchema("analytics")
pg.CreateExtensionIfNotExists("pgcrypto")
pg.CreateSequenceIfNotExists("user_id_seq", pg.SequenceOptions{Start: ptr(int64(100))})
pg.CreateView("active_users", db.Select(UserID, UserName).From(Users))
pg.CreateMaterializedView("mv_users", q, /*withData*/ true)
pg.RefreshMaterializedView("mv_users", /*concurrently*/ true)
pg.CreateFunction("touch_updated_at", pg.FunctionOptions{
    Returns: "trigger",
    Body:    "BEGIN NEW.updated_at = now(); RETURN NEW; END;",
})
pg.CreateTrigger("users_touch", pg.TriggerOptions{
    Timing: "BEFORE", Events: "UPDATE", Table: Users,
    Execute: "touch_updated_at()",
})
pg.CommentOnColumn(UserName, "display name")
```

Every constructor has an `IfNotExists` / `IfExists` variant.

### Indexes

```go
idx := pg.NewIndex("users_email_lower_idx", Users, pg.Lower(UserName)).
    Unique().
    Using("btree").
    Include(UserID.Column).
    Where(UserAge.Gte(18))
db.ExecExpr(ctx, pg.CreateIndex(idx))
```

### Enums

```go
status := pg.NewEnum("user_status", "active", "pending", "banned")
db.ExecExpr(ctx, pg.CreateEnum(status))
var UserStatus = pg.Add(Users, status.Col("status").NotNull().Default("'pending'"))
db.ExecExpr(ctx, pg.AlterEnumAddValue("user_status", "archived", "", "banned"))
```

### Built-in functions

| Category | Highlights |
|----------|-----------|
| Aggregates | `Count`, `CountDistinct`, `CountAll`, `Sum`, `SumDistinct`, `Avg`, `AvgDistinct`, `Min`, `Max`, `StringAgg`, `BoolAnd`, `BoolOr`, `Filter(agg, pred)` |
| String | `Concat`, `ConcatWS`, `ConcatOp` (||), `Length`, `Substring`, `Trim`/`LTrim`/`RTrim`, `Lower`, `Upper`, `Initcap`, `Replace`, `RegexpReplace`, `RegexpMatch`, `Position`, `Format`, `ToChar`, `Md5`, `Encode`, `Decode` |
| Math | `Abs`, `Ceil`, `Floor`, `Round`, `Mod`, `Power`, `Sqrt`, `Sign`, `Exp`, `Ln`, `Log`, `Greatest`, `Least`, `Random`, `Sin`/`Cos`/`Tan`/`Asin`/`Acos`/`Atan`, `Plus`/`Minus`/`Mul`/`Div` |
| Date/time | `CurrentDate`, `CurrentTime`, `CurrentTimestamp`, `LocalTime`, `LocalTimestamp`, `Now`, `DateTrunc`, `Extract`, `DatePart`, `Age`, `IntervalLit`, `Day`/`Hour`/`Minute`/`Second`/`Week`/`Month`/`Year`, `MakeDate`/`MakeTime`/`MakeTimestamp[TZ]`, `ToDate`/`ToTimestamp`/`ToNumber`, `AtTimeZone` |
| JSON/JSONB | `JSONGet` (->), `JSONGetText` (->>), `JSONPath` (#>), `JSONPathText` (#>>), `JSONBContains` (@>), `JSONBContainedIn` (<@), `JSONBHasKey` (?), `JSONBHasAnyKey` (?\|), `JSONBHasAllKeys` (?&), `JSONBConcat`, `JSONBDelete`, `ToJSON`/`ToJSONB`, `JSON[B]ArrayLength`, `JSON[B]Typeof`, `JSON[B]BuildObject`/`Array`, `JSONBSet`, `JSONBInsert`, `JSONBStripNulls`, `JSONBPretty`, `JSON[B]Agg`, `JSON[B]ObjectAgg` |
| Array | `ArrayContains` (@>), `ArrayContainedIn` (<@), `ArrayOverlaps` (&&), `ArrayConcat`, `Any`, `All`, `ArrayAgg`, `Unnest`, `Cardinality`, `ArrayLength`/`Upper`/`Lower`, `ArrayAppend`/`Prepend`/`Remove`/`Replace`, `ArrayPosition`/`Positions`, `ArrayToString`, `StringToArray`, `ArrayLit` |
| Sequences | `NextVal`, `CurrVal`, `SetVal` |
| Coercion | `Cast(e, "text")` (e::text), `CastAs(e, "text")` |
| Control flow | `Case().When(...).When(...).Else(...).End()`, `CaseOn(value).When(...).End()`, `Coalesce` |

If something isn't covered, fall back to `pg.Func("any_pg_function", args...)`
or `drops.Raw("...")` — but note that `Raw` is a bare string and carries
no arguments, so a fragment with a value in it needs `drops.Param` or an
`ExprFunc` that calls `Builder.AddArg`.

### Query constructs

```go
// CTEs (WITH / WITH RECURSIVE).
adults := pg.CTEDef("adults", db.Select(UserID).From(Users).Where(UserAge.Gte(18)))
db.Select(UserID).
    FromExpr(adults.Ref()).
    With(adults).
    All(ctx, &dest)

// Set operations.
a.UnionAll(b).Intersect(c).Except(d)

// DISTINCT ON.
db.Select(UserID, UserName).From(Users).DistinctOn(UserName).OrderBy(UserName.Asc())

// Window functions.
db.Select(
    UserName,
    pg.As(pg.Over(pg.RowNumber(),
        pg.WindowSpec().PartitionBy(UserDept).OrderBy(UserAge.Desc())), "rn"),
).From(Users)

// EXISTS / NOT EXISTS subqueries.
db.Select(UserID).From(Users).Where(pg.Exists(
    db.Select(PostID).From(Posts).Where(PostUserID.EqCol(UserID)),
))

// CASE.
status := pg.Case().
    When(UserAge.Lt(18), "minor").
    When(UserAge.Lt(65), "adult").
    Else("senior").
    End()

// Cast.
pg.Cast(UserAge, "text")  // ("users"."age")::text
```

## Cache

A driver-agnostic cache interface with two ready backends.

```go
type Cache interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
    Delete(ctx context.Context, keys ...string) (int, error)
    Exists(ctx context.Context, key string) (bool, error)
    TTL(ctx context.Context, key string) (time.Duration, error)
    Ping(ctx context.Context) error
    Close() error
}
```

`cache.MultiCache` extends it with `GetMulti` / `SetMulti` for backends
that can serve batches in one round-trip. Sentinels:
`cache.ErrNotFound`, `cache.ErrClosed`, `cache.ErrInvalidKey`.

### In-memory (`drops/cache/memory`)

Zero deps, safe for concurrent use, optional janitor goroutine and
FIFO eviction once `MaxEntries` is reached. Ideal for tests and the
local tier of a two-level cache.

```go
import "github.com/bernardoforcillo/drops/cache/memory"

mc := memory.New(memory.Options{
    MaxEntries: 10_000,
    SweepEvery: time.Minute,
})
defer mc.Close()

_ = mc.Set(ctx, "user:42", payload, 5*time.Minute)
got, err := mc.Get(ctx, "user:42")
```

### Redis (`drops/cache/redis`)

Production Redis backend with its own minimal RESP2 client and a
bounded connection pool. Zero deps. Supports AUTH (legacy + ACL),
`SELECT db`, key prefixes, and the same `drops.Hook` contract used
elsewhere.

```go
import "github.com/bernardoforcillo/drops/cache/redis"

rc := redis.New(redis.Options{
    Addr:        "127.0.0.1:6379",
    Password:    os.Getenv("REDIS_PASSWORD"),
    DB:          0,
    MaxConns:    25,
    IdleTimeout: 5 * time.Minute,
    KeyPrefix:   "app:",
    Hook:        drops.LoggerHook(log.Printf),
})
defer rc.Close()

if err := rc.Ping(ctx); err != nil { /* health-check failed */ }

_ = rc.Set(ctx, "user:42", payload, 5*time.Minute)
got, err := rc.Get(ctx, "user:42")
if errors.Is(err, cache.ErrNotFound) {
    // miss
}
```

#### Authentication

Three shapes, pick whichever fits:

```go
// 1. Static (back-compat shorthand). Set Username + Password (or
//    Password alone for legacy single-arg AUTH).
redis.Options{Password: os.Getenv("REDIS_PASSWORD")}

// 2. Explicit static credentials via the provider helper.
redis.Options{Credentials: redis.StaticCredentials("user", "pw")}

// 3. Dynamic credentials — short-lived tokens (AWS ElastiCache IAM,
//    Azure AAD, OIDC, HashiCorp Vault). The provider is called once
//    per new connection, receiving the caller's context so it can
//    honour deadlines and cancellation.
redis.Options{
    Credentials: func(ctx context.Context) (redis.Credentials, error) {
        tok, err := iam.MintAuthToken(ctx, "my-redis-cluster")
        if err != nil { return redis.Credentials{}, err }
        return redis.Credentials{Username: "iam-user", Password: tok}, nil
    },
}
```

If `Credentials` is set it overrides `Username` / `Password`. If both
are empty, the connection skips AUTH entirely (Redis without
`requirepass`).

#### TLS

```go
// Self-managed: pass any *tls.Config you like (custom RootCAs,
// client certs for mTLS, pinned cipher suites).
rc := redis.New(redis.Options{
    Addr: "redis.example.com:6380",
    TLS:  &tls.Config{ServerName: "redis.example.com", MinVersion: tls.VersionTLS12},
})

// Or pull a sensible default out of a rediss:// URL:
opts, _ := redis.ParseURL("rediss://user:pw@redis.example.com:6380/0")
rc := redis.New(opts) // opts.TLS already populated
```

#### Connection URL

```go
opts, err := redis.ParseURL("rediss://iam-user:" + token + "@cluster.example.com:6380/0")
if err != nil { /* malformed */ }
rc := redis.New(opts)
```

Accepted shapes: `redis://[user[:pass]@]host[:port][/db]` and
`rediss://...` (same but with TLS).

#### Production tuning

Every numeric `Options` field has a sensible default; override when
your workload says otherwise:

| Field | Default | What it does |
|---|---|---|
| `MaxConns` | 10 | Hard cap on simultaneous connections |
| `MinIdleConns` | 0 | Pre-dial this many connections at startup |
| `IdleTimeout` | 5 min | Close conns idle longer than this |
| `MaxLifetime` | 0 (off) | Close conns past this age regardless of idle status — important when AUTH tokens rotate or a load balancer drains |
| `DialTimeout` | 5 s | Cap on the TCP+TLS+AUTH+SELECT+SETNAME dance |
| `ReadTimeout` / `WriteTimeout` | 3 s each | Per-op deadlines applied when the caller's ctx has none. Set negative to disable |
| `MaxRetries` | 1 | Retry-once on transient I/O errors (EOF, network timeout, protocol corruption). App-level `-ERR` replies are never retried |
| `ShutdownTimeout` | 5 s | How long `Close` waits for in-flight ops to drain before forcing socket closure |
| `ClientName` | `"drops"` | Sent via `CLIENT SETNAME` on connect so the conn shows up in `CLIENT LIST` / `SLOWLOG` / `MONITOR` |

#### Pool metrics

```go
s := rc.Stats()
fmt.Printf("conns=%d hits=%d misses=%d timeouts=%d stale=%d retries=%d wait=%s/%d\n",
    s.TotalConns, s.Hits, s.Misses, s.Timeouts, s.StaleClosed,
    s.Retries, s.WaitDuration, s.WaitCount)
```

`PoolStats` is a snapshot; safe to read concurrently from a metrics
emitter. Counters are monotonic across the cache's lifetime.

For richer Redis usage (pub/sub, streams, scripts, cluster, sentinel)
reach for a full-featured client like `github.com/redis/go-redis/v9` —
this package's scope is the `cache.Cache` contract plus a few utility
commands.

## Layout

```
drops/                       driver interface + SQL primitives + Hook
drops/pg/                    Postgres schema, query builders, relations,
                             migrations, snapshot/diff/generate
drops/sqlite/                SQLite schema, query builders, entities,
                             relations, migrations, pagination, soft delete
drops/clickhouse/            ClickHouse schema, engines, query builder,
                             analytical aggregates, introspection + push
drops/mysql/                 MySQL / MariaDB schema, query builders, entities
drops/qdrant/                Qdrant vector-database HTTP client
drops/vector/                portable vector search shared by pg/CH/Qdrant
drops/mirror/                keeps a pg table mirrored into ClickHouse + Qdrant
integration/                 separate module: the suite that runs against real servers
drops/cache/                 Cache interface + sentinels
drops/cache/memory/          in-process cache backend
drops/cache/redis/           Redis cache backend (own RESP2 client)
drops/cache/memcached/       Memcached cache backend (own ASCII client)
drops/cache/tiered/          two-level L1+L2 read-through cache
drops/otel/                  OpenTelemetry spans + metrics from Hook
drops/stdlib/                database/sql adapter
drops/cmd/drops/             the CLI: generate, migrate, push, drift, pull, baseline, status (own module — links pgx)
drops/examples/cli/          a schema package shaped the way the CLI expects
drops/examples/sqlgen/       no-deps SQL-generation demo (pg)
drops/examples/generate/     drizzle-kit-style migration generation demo
drops/_examples/postgres/    full DB demo via pgx (excluded from build)
```

## What's not here

Kept honest deliberately — an evaluator should be able to trust this
list, and each line says what it costs you.

**In the migration loop**

- Postgres introspection reads enums (labels in order), sequences,
  views, materialised views, RLS (enabled and forced) and policies, so
  `Push` reaches a steady state for a schema declaring them. One gap
  remains inside those objects: an enum whose labels were reordered or
  removed is reported as a notice and left alone, because PostgreSQL
  cannot do either in place. A sequence whose attributes moved does get
  its `ALTER SEQUENCE`, but where the sequence has got to is not part
  of the declaration and no push restarts a live one — so raising
  `MINVALUE` above the value it is sitting on, or lowering `MAXVALUE`
  below it, is refused by PostgreSQL (22023) and rolls the push back
  rather than resetting the sequence for you. `pg/push.go`'s "What Push
  cannot see" lists the rest.
- `DetectDrift` does not reach that steady state, and neither does
  `drops drift`. Both are pure snapshot arithmetic with no server to
  ask, so every expression — a CHECK body, a partial index's
  predicate, a policy's USING clause, a view's definition — is
  compared as the text each side happens to spell it in, and the
  database always answers in PostgreSQL's own. A schema carrying any
  of them reports drift on every run against a database that matches
  it exactly. `pg.Push` with `DryRun: true` has a connection and
  respells the declared side first; it is the accurate preview until
  drift detection grows the same step.
- The CLI evaluates your schema by generating a program that imports
  it and running it with `go run`, so it needs a Go toolchain on the
  machine and it needs to be run from inside your module. A prebuilt
  binary on a deploy host can `migrate`, `status`, `baseline` and
  `pull` — those read the database and the migration directory — but
  not `generate`, `push` or `drift`, which have to read your Go.
- `pg.Diff` renders every statement unqualified, so a table declared
  with `pg.NewSchemaTable("reporting", ...)` is created wherever the
  connection's `search_path` points. Declare non-public tables with
  their schema in the name, or push them with a `search_path` set.
- No rename detection. A structural diff cannot tell `RENAME COLUMN`
  from a drop plus an add, so drops generates the destructive pair.
  `pg.AnalyzeMigration` flags it, which is a backstop, not a fix.
- `clickhouse.Introspect` cannot read a column's TTL — `system.columns`
  does not report one — so `clickhouse.Diff` leaves column TTLs out of
  the comparison and reports a declared one as a notice rather than
  re-emitting the same statement on every push. A table TTL the server
  has re-spelled from its own parse tree (`ts + INTERVAL 30 DAY` reads
  back as `ts + toIntervalDay(30)`) is withheld the same way. Neither
  is a difference drops can settle without parsing SQL.
- `clickhouse.Push` acts on one database per call, and the schema has
  to agree on which: mixing `clickhouse.NewTable` with
  `clickhouse.NewDatabaseTable` in one `Schema` is `ErrMixedDatabases`,
  because an unqualified statement lands wherever the connection points
  and there is no single database to read back.
- ClickHouse settings are compared only where the declaration names
  them. The server materialises an engine's defaults into the table's
  metadata — every MergeTree reports `index_granularity` whether or not
  anyone asked for it — so a live setting with no declared counterpart
  is not evidence that it was removed, and drops never resets one.

**In the type system**

- Nullability is not in the type. `Col[T]` is compile-checked in every
  respect except the one that causes the most bugs, and a nullable
  column has no typed way to write NULL.
- `drops.Raw` is a bare string and carries no arguments. A fragment
  with a value in it has to go through `drops.Param` or an `ExprFunc`
  calling `Builder.AddArg` — so the escape hatch and the
  parameterisation are two different decisions when they should be one.
- An unloaded relation is indistinguishable from an empty one. Forget
  `With("posts")` and `user.Posts` is `nil`, which reads exactly like
  "this user has no posts".

**Around the edges**

- No test double. Unit-testing a repository function needs a live
  database — `pg.TestTx` and `pg.NewFactory` both connect, and the
  pure-Go SQLite driver lives in `integration/go.mod` where a user's
  build cannot reach it.
- No isolation level, read-only transaction or savepoint in the
  `Driver` interface, so `pg/retry.go` documents behaviour under
  `SERIALIZABLE` that the API cannot ask for.
- MySQL relations are declaration-only — no eager loading — and it has
  no saga, audit, tenancy, authz or cache. SQLite has sentinel errors
  but no driver-error classifier, where pg and MySQL have one.

**Deliberately absent, and staying that way**

- Unit of work, identity map, change tracking. Go has value semantics:
  a `User` copied into a slice is a different object, so "one instance
  per primary key" is not enforceable. The honest substitute is
  explicit dirty-field tracking, which is what `pg/patch.go` is.
- Lazy loading, and entity lifecycle callbacks tied to implicit
  persistence. Both presuppose a `SaveChanges` to hook.
- More dialects. Five is already more than one author keeps at parity.
