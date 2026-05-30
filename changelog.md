# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once a 1.0 is cut.

## [Unreleased]

### Added
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

### Removed
- `drops.MustString` and `drops.Errorf` re-exports (unused).
