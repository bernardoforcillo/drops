# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once a 1.0 is cut.

## [Unreleased]

### Added
- **Two-level tiered cache** (`drops/cache/tiered`) — composes two
  `cache.Cache` backends into one read-through / write-through cache: a
  fast near tier (L1, typically `cache/memory`) in front of a shared
  network tier (L2, typically `cache/redis` or `cache/memcached`). Reads
  check L1, fall through to L2, and backfill L1 on an L2 hit; writes and
  deletes fan out to both tiers with an `L1TTL` cap that bounds local
  staleness. `GetOrLoad` adds read-through population from an origin
  function with built-in **singleflight** stampede protection (a burst of
  concurrent misses for a cold key triggers exactly one load). Implements
  `cache.Cache` and `cache.MultiCache`, so tiers nest. Zero deps.
- **Memcached backend** (`drops/cache/memcached`) — a `cache.Cache`
  implementation speaking the classic Memcached ASCII protocol (get /
  set / delete, native multi-key get) over a small bounded connection
  pool, with `TTL` served by the meta-get command (`mg`, Memcached 1.6+).
  Satisfies `cache.MultiCache`, honours `KeyPrefix`, and fires the shared
  `drops.Hook` contract. Standard library only.
- **OpenTelemetry instrumentation** (`drops/otel`) — turns the
  `drops.Hook` / `drops.QueryEvent` stream into OpenTelemetry spans and
  RED metrics (call counter, error counter, duration histogram) without
  importing OpenTelemetry: it talks to small OTel-shaped interfaces
  (`Tracer`, `Span`, `Meter`, `Int64Counter`, `Float64Histogram`) that a
  ~10-line adapter bridges to the real SDK — the same zero-dependency
  approach as `pg.Tracer`. Because it is driven purely by the Hook, one
  `Instrumentation` covers every dialect (pg / sqlite / clickhouse) and
  every cache backend. Spans are created retroactively with explicit
  start/end timestamps; `db.statement` recording is opt-in.
- **Keyset (cursor) pagination for SQLite** (`drops/sqlite`) — ports
  drops/pg's `Entity[T].Page` down to `drops/sqlite`: `Page` /
  `PageBuilder` with opaque URL-safe cursors, `Asc` / `Desc` ordering
  columns, and `HasMore` / `NextCursor`. Uses SQLite row-value comparison
  (`(a, b) > (?, ?)`) for homogeneous orderings and a tie-break
  disjunction for mixed asc/desc directions — O(limit) regardless of page
  depth, unlike OFFSET. `(*Column).Asc` / `.Desc` were added for parity.
- **Default filters + soft delete for SQLite** (`drops/sqlite`) — a new
  `Table.DefaultFilter` mechanism auto-applies predicates to every
  Select / Update / Delete (also handy for tenant scoping), with
  `Unscoped()` on each builder to opt out. `SoftDelete(t)` registers a
  nullable `deletedAt` column plus a `deletedAt IS NULL` default filter,
  and `Entity[T].SoftDeleteByID` / `Restore` mark and un-mark rows.
  Mirrors drops/pg's `SoftDeleteMixin` semantics. `UpdateBuilder.SetExpr`
  was added to assign raw SQL expressions (e.g. `CURRENT_TIMESTAMP`).

## [0.4.0] - 2026-07-08

### Added
- **Swappable SQL dialect abstraction** (`drops`) — a new `drops.Dialect`
  interface (`Name`, `Placeholder`, `QuoteIdent`, `SupportsReturning`)
  that a `Builder` carries. `drops.WithDialect(d)` reroutes placeholder
  rendering and identifier quoting through the dialect, so the same
  builder chain targets any SQL-like backend by swapping the dialect and
  driver. A Builder with no dialect keeps the previous PostgreSQL
  behaviour byte-for-byte (`$N` placeholders, `"…"` identifiers), so this
  is fully backward compatible. `pg.Dialect` and `sqlite.Dialect` are the
  two implementations; `drops.StringWithDialect` renders an expression a
  dialect's way.
- **SQLite dialect** (`drops/sqlite`) — a new package mirroring
  `drops/pg`'s API surface (Table / Column / DB / DDL / Select / Insert /
  Update / Delete) over the shared `drops.Driver`, emitting SQLite SQL:
  `?` placeholders, SQLite type affinities, `INSERT OR IGNORE/REPLACE`,
  and — the key dialect difference — **all constraints rendered inline in
  `CREATE TABLE`** (SQLite has no `ALTER TABLE ADD CONSTRAINT`). Type
  constructors share pg's names (`Text`, `BigInt`, `Timestamp`, …) so a
  schema ports with a package swap.
- **Composite (N-column) foreign keys** (`drops/pg`, `drops/sqlite`) —
  `Table.ForeignKeyN(cols, target, targetCols, opts…)` declares a
  multi-column FK (`FOREIGN KEY (a,b) REFERENCES t (x,y)`). In pg it is
  wired through the snapshot/diff generator and emitted as a separate
  `ALTER TABLE ADD CONSTRAINT`; in sqlite it is emitted inline. Column
  counts must match (panics at declaration otherwise).
- **Shared reflection row-scanner** (`drops`) — `drops.ScanOne` /
  `drops.ScanAll` moved the dialect-agnostic struct↔column mapping into
  the root package so every dialect scans rows identically. `drops.StructFields`
  exposes the column→field map for entity binding. `drops/sqlite` uses
  both; `drops/pg` keeps its own wrappers.
- **SQLite full ORM parity** (`drops/sqlite`) — the dialect now covers:
  - **Entities** — `Entity[T]` typed CRUD (`Get` / `Create` / `Update` /
    `Delete`) and a fluent `Query` (`Where` / `OrderBy` / `Limit` /
    `Offset` / `All` / `One`).
  - **Relations & eager loading** — `NewRelations(t).HasMany / HasOne /
    BelongsTo / ManyToMany`, loaded via `db.Find(t).With(names…)` with one
    batched query per edge (no N+1) stitched into `dropRel` struct fields.
  - **Migrations** — a versioned `Migrator` (`Add` / `AddSQL` / `AddFS` /
    `Up` / `Down` / `Status`) with `BeforeEach` / `AfterEach` in-transaction
    data hooks, mirroring `pg.Migrator`.
  - **Snapshot & diff** — `BuildSnapshot` / `Diff` generate SQLite
    migration SQL, honouring SQLite semantics: `ALTER TABLE ADD COLUMN`
    where possible, and the standard **table-rebuild sequence**
    (`CREATE t_new` → `INSERT … SELECT` → `DROP` → `RENAME`) for column
    type changes, drops, and constraint changes that SQLite cannot alter
    in place.
  - **Introspection** — `Introspect(ctx, db)` reconstructs a `Snapshot`
    from a live database via `sqlite_master` and the `table_info` /
    `foreign_key_list` / `index_list` PRAGMAs.

## [0.3.0] - 2026-07-04

### Added
- **Migration data hooks** (`drops/pg`) — both migrators now expose
  `BeforeEach` / `AfterEach` hooks that run inside each migration's
  transaction, the seam for data migrations that must run between
  schema migrations (backfilling a new column, copying rows into a
  split-out table, rewriting a value before an old column is dropped).
  On the native `Migrator`, `MigrationHook` receives the tx-scoped
  `*DB`, the `Migration`, and a `MigrationDirection` (`DirectionUp` /
  `DirectionDown`) so a data step can be scoped to a specific version
  and direction; hooks fire around both `Up` and `Down`. On
  `DrizzleMigrator` — where migration files are pure SQL and there is
  otherwise no place for Go logic — `DrizzleHook` receives the
  tx-scoped `*DB` and the `DrizzleEntry`, letting a backfill run
  atomically with the file's statements. A hook that returns an error
  aborts the migration and the whole transaction rolls back.
- **Nested (deep) relation eager-loading** (`drops/pg`) — `Find().With`
  now accepts dot paths such as `With("posts.comments")` to load
  relations of relations to arbitrary depth. Each relation edge still
  costs exactly one batched query (no N+1), and paths sharing a prefix
  are merged so the shared edge is fetched once
  (`With("posts.comments", "posts.tags")` runs three queries, not four).
  Nested rows are stitched in place onto the live result structs via
  pointers into the parent data. Works across `HasMany`, `HasOne`,
  `BelongsTo`, and `ManyToMany` intermediates. The entire `With` graph
  is validated against the schema before any query runs, so a typo at
  any depth fails fast with an `unknown relation` error; malformed
  paths (e.g. `"posts..comments"`) report `invalid relation path`.
- **Per-relation filtering & ordering on eager loads** (`drops/pg`) —
  new `Find().WithRel(name, func(*pg.RelConfig))`. The `RelConfig`
  callback exposes `Where` (AND-ed onto the relation's batched query),
  `OrderBy` (sorts each parent's loaded slice), and `With`/`WithRel`
  for configuring deeper relations — mirroring drizzle's
  `with: { posts: { where, orderBy } }`. Still one query per edge.
  For `ManyToMany`, `OrderBy` re-sorts each parent's slice into target
  order (default remains junction-row order). `WithRel` and `With`
  merge when they name the same edge, so it is fetched once. Per-parent
  `LIMIT`/`OFFSET` is intentionally not yet offered (a single `LIMIT`
  caps the whole batch, not each parent — needs a window-function
  rewrite).
- **`drops.CallHook(h, ctx, e)`** — the safe entrypoint every dialect
  now uses to emit observability events. Tolerates nil hooks and
  recovers panics, so a buggy user-supplied `Hook` (nil deref in a
  formatter, out-of-bounds in a metric label, …) can no longer crash
  the caller's request goroutine. `drops.ChainHooks` also continues
  to the next hook after a panicking one. Wired into pg, clickhouse,
  qdrant, cache/memory, cache/redis.
- `.gitignore` — coverage / profile / OS / editor / env / build
  artefacts kept out of the tree.
- **Cache abstraction** (`drops/cache`) — driver-agnostic interface
  (`Get` / `Set` / `Delete` / `Exists` / `TTL` / `Ping` / `Close`) with
  `MultiCache` for batch operations. Sentinels: `ErrNotFound`,
  `ErrClosed`, `ErrInvalidKey`.
- **In-memory cache** (`drops/cache/memory`) — concurrent-safe,
  TTL-aware, with an optional janitor goroutine and FIFO eviction once
  `MaxEntries` is reached. Defensive copies on Get/Set so callers can't
  mutate stored bytes.
- **Redis cache** (`drops/cache/redis`) — production backend with a
  bundled minimal RESP2 client and a bounded connection pool. Zero
  external dependencies (`net.Conn` + `bufio` only). Supports legacy
  and ACL `AUTH`, `SELECT db`, key prefixes, context-deadline
  propagation onto the wire, and the `drops.Hook` contract for
  observability. `Cache` and `MultiCache` interfaces both implemented.
- **Redis production hardening**:
  - Channel-based pool replaces the spin-wait loop; `Get` honours ctx
    cancellation natively, no CPU burn under contention.
  - `MinIdleConns` pre-dials connections at startup so the first
    request after a cold start doesn't pay a full TCP+AUTH RTT.
  - `MaxLifetime` recycles connections past an age cap regardless of
    idle status — critical when AUTH tokens rotate or a load balancer
    wants to drain old conns.
  - `ReadTimeout` / `WriteTimeout` (defaults: 3s each) apply when the
    caller's ctx has no deadline so a hung server can't stall the
    goroutine forever. Set negative to disable.
  - `MaxRetries` (default 1) retries on transient transport errors
    (EOF, `net.Error`, `ErrProtocol`) with a fresh connection;
    app-level `-ERR` replies are never retried.
  - `ShutdownTimeout` (default 5s) lets `Close` drain in-flight ops
    before forcing socket closure.
  - `ClientName` (default `"drops"`) is sent via `CLIENT SETNAME` on
    connect so the connection is identifiable in `CLIENT LIST` /
    `SLOWLOG` / `MONITOR`.
  - `Cache.Stats()` returns a `PoolStats` snapshot for metrics
    emitters: `TotalConns`, `Hits`, `Misses`, `Timeouts`,
    `StaleClosed`, `WaitCount`, `WaitDuration`, `Retries`.
- **Redis auth & transport**:
  - `redis.CredentialsProvider func(ctx) (Credentials, error)` is
    called per new connection so short-lived tokens (AWS ElastiCache
    IAM, Azure AAD, OIDC, Vault leases) can be refreshed without
    restarting the cache. Provider errors fail the dial cleanly.
  - `redis.StaticCredentials(user, pass)` helper for the simple case.
  - `Options.TLS *tls.Config` enables in-transit encryption; the
    default dialer is wrapped with a `tls.Dialer` so callers don't
    have to plumb their own.
  - `redis.ParseURL("redis[s]://[user:pass@]host[:port][/db]")` lifts
    a connection string into Options — and rediss:// pre-populates a
    sensible `tls.Config` (`ServerName` = host, MinVersion = TLS1.2).
  - Existing `Username`/`Password` fields are kept as the static
    shorthand; if `Credentials` is non-nil it wins.
- **Qdrant client** (`drops/qdrant`) — focused HTTP client for the Qdrant
  vector database. Zero external deps (net/http + encoding/json only):
  - `Client` with `WithAPIKey` / `WithHTTPClient` / `WithTimeout` options;
    Qdrant Cloud (`api-key`) and self-hosted (`Authorization: Bearer`)
    auth headers are set in lock-step
  - Collections: `CreateCollection`, `DeleteCollection`,
    `CollectionExists`, `CollectionInfo`, `ListCollections`
  - Points: `Upsert`, `DeleteByIDs`, `DeleteByFilter`, `Retrieve`, `Count`
  - Search: `Search` (single vector), `Recommend` (positive/negative
    examples), `Scroll` (deterministic pagination cursor)
  - Filter DSL: `Must` / `Should` / `MustNot` blocks with
    `Eq` / `In` / `NotIn` / `MatchText` / `Range` / `HasID` / `IsEmpty` /
    `IsNull` / `GeoIn` / `Nest` conditions
  - `HTTPError` carries `Status` / `StatusText` / `Body`; missing
    collections wrap `ErrCollectionMissing` so `errors.Is` works
- **pgvector** support in `drops/pg`:
  - Column types: `Vector(name, dim) *Col[[]float32]`,
    `HalfVec(name, dim) *Col[[]float32]`, `SparseVec(name, dim) *Col[string]`,
    `BitVec(name, dim) *Col[string]`
  - Distance operators: `L2Distance` (`<->`), `InnerProduct` (`<#>`),
    `CosineDistance` (`<=>`), `L1Distance` (`<+>`), `HammingDistance` (`<~>`),
    `JaccardDistance` (`<%>`); plus convenience methods
    `c.L2(v)` / `c.IP(v)` / `c.Cosine(v)` / `c.L1(v)` on `*Col[T]`
  - Index op-class hints (`VectorL2Ops`, `VectorCosineOps`, `HalfVecIPOps`,
    `BitHammingOps`, …) plus `Index.OpClass(...)` / `Index.With(...)` so
    HNSW and IVFFlat indexes render with the correct operator class
    and tuning parameters
  - The existing `CreateExtensionIfNotExists("vector")` is the install
    step — no new helper needed

### Added (still under [Unreleased])
- **ClickHouse dialect** (`drops/clickhouse`):
  - Typed columns: `String`, `FixedString`, `Int{8,16,32,64}`, `UInt{8,16,32,64}`,
    `Float{32,64}`, `Decimal`, `Bool`, `Date`, `Date32`, `DateTime(tz)`,
    `DateTime64(prec, tz)`, `UUID`, `JSON`, `Custom[T]`
  - Type wrappers: `TypeArray`, `TypeNullable`, `TypeLowCardinality`,
    `TypeMap`, `TypeTuple`, `TypeEnum8/16` plus chainable `.Nullable()` /
    `.LowCardinality()` / `.Default(sql)` / `.Codec(...)` / `.TTL(...)` /
    `.Comment(...)` on `*Col[T]`
  - Engines: `MergeTree`, `ReplacingMergeTree`, `SummingMergeTree`,
    `AggregatingMergeTree`, `CollapsingMergeTree`,
    `VersionedCollapsingMergeTree`, `ReplicatedMergeTree`, `Memory`, `Log`,
    `TinyLog`, `StripeLog`, `Null`, plus `Raw` for distributed / kafka /
    custom engines
  - `Table.Engine(...) / OrderBy / PartitionBy / PrimaryKey / SampleBy /
    TTL / Setting(...)` builder
  - DDL: `CreateTable[IfNotExists]`, `DropTable[IfExists]`, `TruncateTable`,
    `OptimizeTable(final)`, `CreateDatabase[IfNotExists]`,
    `DropDatabase[IfExists]`; `CreateTableErr` returns `ErrEngineRequired`
  - Query builder: `Select` with `From`, `Final`, `SampleBy`, joins
    (`Join` / `LeftJoin` / `AnyJoin` / `AllJoin` / `AsofJoin` / `FullJoin`),
    `Prewhere`, `Where`, `GroupBy`, `Having`, `OrderBy`, `Limit/Offset`,
    `Distinct`, `Setting`, plus `Count(ctx)`
  - `Insert(t).Row(...).Rows(...).Columns(...).Exec(ctx)` for batch INSERTs
  - Aggregates: `Uniq`, `UniqExact`, `UniqHLL12`, `AnyAgg`, `AnyLast`,
    `AnyHeavy`, `Quantile`, `QuantileExact`, `QuantileTiming`, `GroupArray`,
    `GroupUniqArray`, `ArgMax`, `ArgMin`, plus the usual `Count/Sum/Avg/Min/Max`
  - Date helpers: `ToDate`, `ToDateTime`, `ToStartOf{Day,Hour,Minute,Month}`,
    `ToYYYYMM`, `ToYYYYMMDD`, `DateDiff`
  - `DB` with `Hook` / `WithHook` / `Ping` / `Close` / `Begin` / `InTx`
    (context-safe rollback) — same surface as `pg.DB`
  - `Placeholder` exported so callers can render any drops expression
    with `?` placeholders via `clickhouse.ToSQL(expr)`
  - Identifier validation (`ErrInvalidIdentifier`) on construction
- `drops.BuilderOption` / `drops.WithPlaceholder` lets dialects override
  the `$N` placeholder rendering — used by ClickHouse to emit `?` and
  available to anyone building another dialect.
- `DB.Close()` releases the underlying driver if it implements `io.Closer`.
  The bundled `stdlib` adapter implements `Close` so `defer db.Close()`
  in user code now propagates to `*sql.DB.Close()`.
- `SelectBuilder.Count(ctx)` returns `int64` for the current SELECT,
  wrapping the existing query as a subquery — paginated UIs and admin
  dashboards usually need a total alongside their listing.
- `LoggerOptions.Redact func(args []any) []any` lets `LoggerHook` strip
  passwords, tokens and PII before logging when `LogArgs: true`. The
  redactor receives a copy so it can't mutate the caller's args.
- Go example tests (`ExampleAdd`, `ExampleDB_Select`, `ExampleDB_Insert`,
  `ExampleDB_WithHook`, `ExampleCol_Eq`) render in pkg.go.dev.
- `drops.Hook` interface + `drops.QueryEvent` for per-operation observability
  (kind, SQL, args, duration, error). Compose via `drops.ChainHooks`.
- `DB.WithHook(h)` to attach a hook; the hook is propagated into the
  transaction-bound DBs returned by `Begin` / `InTx`. `InTx` emits
  `begin` / `commit` / `rollback` events automatically.
- `pg.LoggerHook(log, opts)` convenience that wires any `LoggerFunc`
  (e.g. `log.Printf`, `slog.Info`) into the hook surface with
  `SlowQuery` threshold and `LogArgs` / `MaxSQLLength` options.
- `DB.Ping(ctx)` health check that issues `SELECT 1` and emits a
  `ping` event.
- Sentinel errors checkable with `errors.Is`:
  `ErrReturningRequired`, `ErrNoRowsToInsert`, `ErrNoUpdateAssignments`,
  `ErrSchemaRequired`, `ErrInvalidIdentifier`.
- Identifier validation at construction time (`NewTable`,
  `NewSchemaTable`, every column constructor) — rejects empty strings,
  non-UTF8 sequences and NUL bytes. Bad identifiers fail fast at
  startup rather than at the first query.
- GitHub Actions CI workflow: `go vet`, `go build`, `go test`,
  `go test -race`, `staticcheck`, `govulncheck` across Go 1.22 / 1.23 /
  1.24.
- MIT license (`license.md`).

### Changed
- **Migration diff generator never inlines constraints into `CREATE
  TABLE`** (`drops/pg`) — `Diff` now emits every composite primary
  key, UNIQUE, FOREIGN KEY and CHECK constraint as its own raw SQL
  `ALTER TABLE … ADD CONSTRAINT` statement, and enums as a separate
  `CREATE TYPE`. Previously UNIQUE constraints were rendered inline
  in the `CREATE TABLE` body; new tables now produce a bare column-only
  `CREATE TABLE` followed by the constraint statements (matching how
  composite PKs, FKs and CHECKs were already handled). This keeps each
  constraint independently diffable and re-orderable across migrations.
- `InTx` (both the root `drops.InTx` helper and `pg.DB.InTx`) now uses a
  detached context with a 5-second timeout for the deferred `Rollback`,
  so a cancelled or expired caller-ctx no longer prevents the cleanup
  path from running. The detached ctx still inherits values (trace IDs,
  request IDs) from the parent.
- All query builders (`Select`, `Insert`, `Update`, `Delete`) now route
  through `DB.Exec` / `DB.Query` so hook events fire uniformly,
  whether the SQL came from a builder or from raw `Exec`/`Query` calls.
- Errors that used to be unique `fmt.Errorf("…")` instances are now the
  sentinel values above. `errors.Is` works as expected.
- `drops.Raw` is now `type Raw string` (was a struct with a misleading
  `Args` field that never renumbered placeholders). Pure SQL text.
- Empty `In(col)` / `NotIn(col)` no longer emits the invalid
  `(col IN ())`. `In` returns `(false)`, `NotIn` returns `(true)` —
  matching set-theoretic semantics.

### Fixed
- **`CREATE INDEX` rendered table-qualified column names, producing
  invalid DDL** (`drops/pg`) — `NewIndex(...)` built from column handles
  emitted its column list as `("table"."column")`, which PostgreSQL
  rejects inside an index column list with `syntax error at or near
  ")"` (SQLSTATE 42601). Column references in the index column list now
  render as bare identifiers (`("column")`); functional/expression
  indexes are unaffected, and `WHERE` predicates (ordinary expressions)
  stay qualified. This also corrects pgvector `USING hnsw/ivfflat`
  index DDL. The bug was latent because the builder's tests only
  string-compared the rendered SQL and never executed it.

### Removed
- `drops.MustString` and `drops.Errorf` re-exports (unused).
