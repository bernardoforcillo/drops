# drops

A Drizzle-inspired, driver-agnostic SQL toolkit for Go.

`drops` does not wrap an existing driver — it defines its own minimal
`Driver` interface so it stays out of the way of however you connect to
your database. Plug in `database/sql`, `pgx`, or your own pool by
implementing four methods.

## Status

Early. Two dialects ship today:

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
- **`drops/qdrant`** — Qdrant vector database. Focused HTTP client
  (stdlib only): collections, upsert/delete/retrieve, search /
  recommend / scroll, and a Must/Should/MustNot filter DSL with
  Eq/In/Range/HasID/Geo conditions.
- **`drops/pg`** with **pgvector** — `Vector(name, dim)`,
  `HalfVec(name, dim)`, `SparseVec`, `BitVec` column types plus
  the distance operators (`<->` L2, `<#>` inner product, `<=>` cosine,
  `<+>` L1) for similarity search in Postgres. HNSW/IVFFlat indexes
  with the right operator class via `Index.OpClass(...)`.
- **`drops/clickhouse`** — ClickHouse. Engine-bound tables
  (MergeTree family + replicated/distributed via `Raw`), CH-specific
  types (`Array`, `Nullable`, `LowCardinality`, `Decimal`,
  `DateTime64`, `Tuple`, `Map`, `Enum8/16`), full SELECT (`PREWHERE`,
  `FINAL`, `SAMPLE`, `ASOF JOIN`, `SETTINGS`), batch INSERT, and
  the analytics-aggregate library (`uniq`, `uniqExact`, `quantile`,
  `argMax`, `groupArray`, `quantileTiming`, …).

Both dialects share the root `drops` package (driver interface,
`Expression`, `Builder`, `Hook`, transactions).

## Install

```sh
go get github.com/bernardoforcillo/drops
```

To use the bundled `database/sql` adapter (`drops/stdlib`) you also need
a driver — for PostgreSQL, `github.com/jackc/pgx/v5/stdlib`; for
ClickHouse, `github.com/ClickHouse/clickhouse-go/v2`.

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
    UserAge  = pg.Add(Users, pg.Integer("age"))                 // *Col[int32]
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

Each event is a `drops.QueryEvent{Kind, SQL, Args, Duration, Err}`.
`LoggerHook` is one convenience built on top — write your own for OTel,
Prometheus, Datadog, etc. in a few lines. `db.Ping(ctx)` issues
`SELECT 1` and is the natural shape for a Kubernetes readiness probe.

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

> Per-parent `LIMIT`/`OFFSET` (drizzle's `with: { posts: { limit } }`) is
> not yet supported — a single `LIMIT` would cap the whole batched result,
> not each parent's slice, which needs a window-function rewrite.

### Migrations

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

What `GenerateMigration` covers (today): CREATE TABLE, DROP TABLE, ADD/DROP COLUMN, ALTER COLUMN type/NOT NULL/DEFAULT, ADD/DROP single-column UNIQUE, ADD/DROP single-column FOREIGN KEY (with ON DELETE/ON UPDATE).

What it does not cover yet: indexes, composite primary keys, composite uniques/FKs, check constraints, enums, sequences, views, RLS policies. The generated snapshot leaves those collections empty, which matches drizzle-kit's "no such constructs declared" state — but if you mix drops's generator with hand-edited or drizzle-kit-authored snapshots that use these features, drops won't be aware of them.

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

`Push` introspects the live database via `information_schema`, builds a snapshot from your Go schema, diffs the two, and applies the SQL inside a single transaction. A `DryRun: true` option returns the statements without executing — useful for previewing in CI.

There is no migration history written: `Push` is convenient for prototyping and tests, not for production where you want reviewable, reproducible migrations. For those, use `GenerateMigration` + `DrizzleMigrator`.

Underneath, `Push` is just three reusable pieces you can also call separately:

```go
current, _ := pg.Introspect(ctx, db)                  // *Snapshot from the live DB
desired := pg.BuildSnapshot(pg.NewSchema(Users, Posts)) // *Snapshot from the Go schema
stmts := pg.Diff(current, desired, pg.DiffOptions{Safe: true})
// stmts is the SQL diff — execute, review, or pipe wherever
```

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
or `drops.Raw{SQL: "...", Args: ...}`.

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
drops/clickhouse/            ClickHouse schema, engines, query builder,
                             analytical aggregates
drops/qdrant/                Qdrant vector-database HTTP client
drops/cache/                 Cache interface + sentinels
drops/cache/memory/          in-process cache backend
drops/cache/redis/           Redis cache backend (own RESP2 client)
drops/stdlib/                database/sql adapter
drops/examples/sqlgen/       no-deps SQL-generation demo (pg)
drops/examples/generate/     drizzle-kit-style migration generation demo
drops/_examples/postgres/    full DB demo via pgx (excluded from build)
```

## What's not here

- Other dialects (MySQL, SQLite, MSSQL)
- Indexes, composite primary keys, composite uniques, check constraints, enums, sequences, views, RLS in the snapshot/diff generator
- Per-parent `LIMIT`/`OFFSET` on eager loads (drizzle's `with: { posts: { limit } }`) — `WithRel` supports per-relation `Where`/`OrderBy` and dot-path nesting, but a per-parent row cap needs a window-function rewrite
- Down-migration generation from the diff (only Go-native `pg.Migrator` supports `Down`, and only when the user writes the down SQL themselves)

The structure leaves room to add these later without churning the
existing surface.
