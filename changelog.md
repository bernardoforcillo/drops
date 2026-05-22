# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once a 1.0 is cut.

## [Unreleased]

### Added
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
