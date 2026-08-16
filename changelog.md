# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once a 1.0 is cut.

## [Unreleased]

## [0.6.0] - 2026-08-16

### Added
- **Portable vector search** (`drops/vector`) — one search vocabulary
  shared by pgvector, ClickHouse and Qdrant, replacing three
  backend-specific ones:
  - **`Filter`** — a portable predicate tree (`And`/`Or`/`Not` over
    `Eq`, `Ne`, `In`, `NotIn`, `Lt`/`Lte`/`Gt`/`Gte`, `Between`,
    `IsNull`, `MatchText`, `HasID`, `GeoWithin`), compiled to each
    backend's own representation through a generic `Compile`/`Visitor`
    pair so the traversal exists once.
  - **`Query` / `QueryBuilder`** — query vector, `TopK`, `Metric`,
    filter, `MaxDistance` (always in the metric's units), payload and
    vector inclusion, cursor, and a `Params` bag for backend-specific
    tuning that other backends ignore.
  - **`Hit` / `Results`** — every hit carries both `Distance` (lower is
    closer) and `Score` (higher is better), converted in one place;
    `HasMore` is decided by a `TopK+1` probe, never a second query.
  - **`Cursor`** — one opaque, URL-safe cursor across backends, stamped
    with the issuing backend so a cross-store replay is
    `ErrCursorMismatch` rather than a wrong page. IDs travel as text
    plus a kind tag, so an `int64` past 2^53 round-trips exactly.
  - **`Store`** — the one-method interface the three adapters implement.
- **Vector-store adapters** for the three backends:
  - **`pg.NewVectorStore`** — pgvector distance operators for all six
    metrics, filter fields resolved to mapped columns or a jsonb
    payload accessor, keyset pagination on `(distance, id)`, PostGIS
    `ST_Within` for geo filters via `WithGeoColumn`. `FormatVector` /
    `ParseVector` / `FormatBitVector` are exported for hand-written
    statements.
  - **`clickhouse.NewVectorStore`** — `cosineDistance` / `L2Distance` /
    `L1Distance` / negated `dotProduct` over `Array(Float32)` (no
    extension required), `JSONExtract*` payload accessors with
    `JSONHas` null tests, SETTINGS forwarded from `Params`, and the
    same keyset pagination. The query vector is rendered once and
    referenced by alias in `WHERE` and `ORDER BY`.
  - **`(*qdrant.Client).Store`** — portable filters compiled to Qdrant's
    Must/Should/MustNot tree (negations routed through `must_not`,
    `IsNull` covering both `is_null` and `is_empty`), offset
    pagination, and score-to-distance normalisation that accounts for
    Qdrant's per-metric score semantics. `qdrant.CompileFilter` is
    exported so portable filters can also drive `Scroll`,
    `Recommend` and `DeleteByFilter`.

### Fixed
- **Every PostGIS helper emitted invalid SQL** (`drops/pg`, `geo.go`) —
  `Within`, `DistanceFrom`, `NearestFrom` and `WithinRadius` each wrote
  `$1`, `$2`, … into the SQL text by hand *and* called `AddArg`, which
  writes the placeholder itself. Each helper therefore emitted its
  placeholders twice, the second set dangling after the closing
  parenthesis:

      ST_Within(…, ST_MakeEnvelope($1, $2, $3, $4, 4326))$1$2$3$4

  That is a syntax error unconditionally, not merely mis-numbered
  parameters, so PostGIS support has never worked. All four now bind
  through `AddArg` alone, which also makes their numbering follow the
  Builder — a geo predicate that is not first in a statement now binds
  `$2`, `$3`, … correctly. The existing tests missed this because they
  asserted on substrings and argument counts, which the broken output
  satisfied; the new regression test pins the whole rendered string.

## [0.5.0] - 2026-07-25

### Added
- **Tiered cache** (`drops/cache/tiered`) — two-level L1+L2 read-through /
  write-through cache with `GetOrLoad` singleflight stampede protection.
- **Memcached cache backend** (`drops/cache/memcached`) — stdlib-only
  backend implementing `cache.Cache` / `cache.MultiCache`.
- **OpenTelemetry hook instrumentation** (`drops/otel`) — spans + RED
  metrics adapter for all backends without importing OTel in core packages.
- **SQLite keyset pagination and soft delete parity** (`drops/sqlite`) —
  `Entity.Page`, `Table.DefaultFilter`, `SoftDelete` helpers, and
  `UpdateBuilder.SetExpr`.
- **drizzle-kit interop for SQLite** (`drops/sqlite`) —
  - **DrizzleMigrator** (`drizzle.go`) — applies a drizzle-kit migration
    directory (journal + hashed `.sql` files, statement-breakpoint
    splitting, `BeforeEach`/`AfterEach` hooks). Adapted to SQLite: the
    `__drizzle_migrations` history table is unqualified (no schema), and
    the journal dialect must be `sqlite`.
  - **GenerateMigration** (`generate.go`) — diffs the Go schema against
    the latest snapshot and writes a new drizzle-kit migration set
    (`<tag>.sql`, `meta/<idx>_snapshot.json`, updated `_journal.json`),
    with optional `WithDown` rollback SQL. No-op when the schema is
    unchanged.
- **Query-plan capture for SQLite** (`drops/sqlite`, `explain.go`) —
  `Explain` runs `EXPLAIN QUERY PLAN`, parsing it into `PlanStep`s with
  `SeqScans` (full-table scans), `UsedIndexes`, a stable `Fingerprint`
  for regression detection, and a tree `String`.
- **Audit, authorization and caching for SQLite** (`drops/sqlite`) — the
  cross-cutting Entity concerns from pg, wired into SQLite's Entity CRUD:
  - **Audit** (`audit.go`) — `NewAuditLog`/`NewAuditTable`/`WithAudit`,
    `WithActor`/`ActorFrom`; Create/Update/Delete write an audit row in
    the same transaction as the mutation.
  - **Authorization** (`authz.go`) — `Guard` + `OwnerGuard`/
    `MembershipGuard`/`CustomGuard` + `AnyOf`/`AllOf`, `WithSubject`/
    `SubjectFrom`, `(*Entity).AuthorizeWith`; the guard predicate is
    AND-ed into Get/Query/Update/Delete and fails closed with
    `ErrSubjectMissing`.
  - **Cache** (`cache.go`) — `(*Entity).WithCache` read-through cache
    over the `drops/cache` backend, with a single-flight group,
    PK-entry invalidation on write/delete, and gob-encoded entries.
- **Schema push & diagram for SQLite** (`drops/sqlite`) —
  - **Push** (`push.go`) — `Push` introspects the live DB, diffs it
    against a Go `Schema`, and applies (or `DryRun`-previews) the diff in
    one transaction; `PushResult`/`PushOptions`/`ErrSchemaRequired`.
  - **Mermaid ER diagram** (`diagram.go`) — `MermaidDiagram` renders a
    schema (tables + relations) as a Mermaid `erDiagram`.

  `objects.go` (sequences, RLS policies, materialized views) is not
  ported — those are Postgres-specific.
- **Reflection, PII and drift for SQLite** (`drops/sqlite`) —
  - **AutoTable** (`autotable.go`) — `AutoTable[T]` / `NewAutoEntity[T]`
    derive a Table from `drop` struct tags (primaryKey, autoIncrement,
    notNull, unique, pii, default), mapping Go types to SQLite affinities
    (`sqlite.Money` → INTEGER).
  - **PII redaction** (`pii.go`) — `PII`/`IsPII`/`(*Col).AsPII`; Exec and
    Query unwrap the marker for the driver while hooks/loggers see
    `<redacted>`, and entity bindings wrap PII columns automatically.
  - **Drift detection** (`drift.go`) — `DetectDrift` computes the two-way
    Snapshot diff into a `DriftReport` (`PendingMigrations`,
    `UnauthorizedChanges`, `InSync`).
- **Dev & schema tooling for SQLite** (`drops/sqlite`) —
  - **Factory** (`factory.go`) — `NewFactory`/`Build`/`BuildN`/`Create`/
    `CreateN`/`With`/`Reset` test-data factories (backed by the new
    `Entity.CreateMany` batch insert).
  - **Seeder** (`seed.go`) — `NewSeeder` + `SeedAdd`/`SeedAddCreate`/
    `SeedDo` + transactional `Apply`.
  - **Test transaction** (`testing.go`) — `TestTx` runs a test body in a
    rolled-back transaction via the `TB` interface.
  - **N+1 detector** (`n1.go`) — `WithN1Detector` + `N1Hook` +
    `N1Report`/`N1Pattern` to flag repeated query skeletons.
  - **Keyset cursor** (`cursor.go`) — `CursorSpec`/`OrderKey`,
    `EncodeCursor`/`Cursor.Decode`, and `SelectBuilder.OrderByCursor`/
    `AfterCursor`/`BeforeCursor` (NULLS defaults documented for SQLite).
  - **Enum** (`enum.go`) — `NewEnum`/`AddTo`/`EnumCol` emulate a
    PostgreSQL enum as a `TEXT` column plus an `IN (...)` CHECK
    constraint (SQLite has no enum type).
  - `Entity.CreateMany` — multi-row batch insert with tenant stamping.
- **Transactional outbox for SQLite** (`drops/sqlite`, `outbox.go`) —
  `Outbox` / `NewOutboxTable` / `Emit` / `EmitWith`, `Drain`,
  `MarkPublished`, `MarkFailed`, `Cleanup`, and `OutboxWorker`
  (`OnEvent`/`OnBatch`, `WithInterval`/`WithBatch`/`WithMaxAttempts`/
  `WithBackoff`, `Run`/`Tick`). SQLite has no LISTEN/NOTIFY, SKIP LOCKED
  or advisory locks, so it is a poll-based single-worker outbox with
  INTEGER Unix-second timestamps; the pg per-aggregate advisory-lock
  ordering mode is omitted. Delivery is at-least-once.
- **Event sourcing & saga for SQLite** (`drops/sqlite`) —
  - **Event store** (`eventstore.go`) — `EventStore` / `NewEventStoreTable`
    with `Append` (optimistic concurrency via the UNIQUE
    aggregate/version constraint → `ErrConcurrencyConflict`, detected
    from SQLite's "UNIQUE constraint failed" message), `Load`, `Stream`,
    `LatestVersion`, plus snapshots (`NewSnapshotTable`, `SaveSnapshot`
    via `ON CONFLICT DO UPDATE`, `LoadSnapshot`).
  - **Saga** (`saga.go`) — `NewSaga`/`Step`/`Run` orchestration with
    reverse-order compensation, typed `SagaState` (`SagaStateGet[T]`),
    and `SagaError`/`IsSagaError`.
- **Transactional store patterns for SQLite** (`drops/sqlite`) —
  - **Idempotency keys** (`idempotency.go`) — `IdempotencyStore` /
    `NewIdempotencyTable` / `Run` / `RunJSON` / `Cleanup` / `SweepEvery`.
    SQLite has no `SELECT ... FOR UPDATE`; concurrent `Run` calls
    serialise on the write-transaction lock instead. Time comparisons
    bind Go times (not `CURRENT_TIMESTAMP`) to avoid datetime-format
    mismatch.
  - **Chunked backfill** (`backfill.go`) — `NewBackfill` with
    `ChunkSize`/`Throttle`/`Fetch`/`Process`/`OnProgress`, resumable via a
    persisted state table (`NewBackfillStateTable`, timestamps stored as
    INTEGER Unix seconds), upserting through `ON CONFLICT DO UPDATE`. The
    pg replica-lag gate is omitted (SQLite has no replication).
- **Lifecycle hooks, templates and mixins for SQLite** (`drops/sqlite`) —
  the pg hook/mixin subsystem, adapted to SQLite:
  - **Hooks** (`hooks.go` + builder wiring) — `Table.OnInsert` /
    `OnUpdate` / `OnDelete` and `DefaultFilter`, applied by the INSERT /
    UPDATE / DELETE / SELECT builders; `Unscoped()` on Select/Update/
    Delete bypasses default scopes. User-supplied values always win over
    hook-supplied ones.
  - **Templates** (`template.go`) — `Timestamps`, `SoftDelete`, `Audit`,
    `UUIDPrimaryKey` column groups returning typed handles. SQLite
    adaptations: `CURRENT_TIMESTAMP` defaults, and a `randomblob()`-based
    RFC-4122 v4 UUID default for `UUIDPrimaryKey` (SQLite has no
    `gen_random_uuid()`).
  - **Mixins** (`mixin.go`) — `ApplyMixins` + `TimestampsMixin`
    (bumps `updatedAt` on UPDATE), `SoftDeleteMixin` (default-scopes
    queries and rewrites DELETE into UPDATE `deletedAt`), `AuditMixin`,
    `UUIDPrimaryKeyMixin`.
- **Higher-level pg feature parity for SQLite** (`drops/sqlite`) — the
  portable feature patterns that previously lived only in `drops/pg` are
  now available on SQLite, adapted to SQLite semantics:
  - **Money** (`money.go`) — precision-safe integer-cents monetary type
    (`Money`, `MoneyFromString`/`MoneyFromCents`/`MoneyFromUnits`, `Add`,
    `Sub`, `MulRate` with banker's rounding, JSON string round-trip,
    `driver.Valuer`/`sql.Scanner`).
  - **Cursor pagination** (`page.go`) — `Entity.Page` with opaque
    keyset cursors (`Asc`/`Desc`, `Page[T]`, `HasMore`/`NextCursor`),
    using SQLite row-value comparison for the keyset guard.
  - **Patch** (`patch.go`) — `Entity.Patch` with SQL-side ops `Inc`,
    `Dec`, `Set`, `SetIfGreater`/`SetIfLess` (via `max`/`min`) and
    `SetIfChanged` (via NULL-safe `IS NOT`).
  - **Tenant scoping** (`tenant.go`) — `Entity.ScopeByTenant` +
    `WithTenant`/`TenantFrom`; Get/Query/Update/Delete auto-apply the
    tenant predicate and Create stamps it, failing closed with
    `ErrTenantMissing` / `ErrTenantMismatch`.
  - **Typed JSON path** (`jsonpath.go`) — `JSONField[T]` typed accessor
    over `json_extract` with comparison/`In`/`IsNull`/`Like` operators,
    plus `JSONHasKey` via `json_type`.
  - **Retry** (`retry.go`) — `RetryPolicy` + `DB.WithRetry`;
    transaction-level retry on `SQLITE_BUSY`/`SQLITE_LOCKED`
    (`ErrBusy`/`ErrLocked`, matched by `errors.Is` or driver message),
    `ExponentialJitter`, `DefaultRetryPolicy`.
  - **Tracing** (`tracing.go`) — `Tracer`/`Span` contract + `WithTracer`
    wired into every Exec/Query span (dependency-free OTel-shaped API).
  - **Existence checks** (`exists.go`) — `TableExists`, `ColumnExists`,
    `IndexExists`, `TriggerExists` over `sqlite_master` / `pragma_table_info`.
  - **Migration safety analyzer** (`safety.go`) — `AnalyzeMigration`
    with SQLite-tuned rules (drop-table, drop/rename-column,
    add-NOT-NULL-without-default, non-constant ADD COLUMN default,
    DELETE/UPDATE without WHERE).
  - **Logger hook alias** (`hook_logger.go`) — `sqlite.LoggerHook` for
    symmetry with the pg/clickhouse dialects.

  Postgres-specific features remain pg-only where SQL cannot express
  them (LISTEN/NOTIFY, pgvector, materialized views, COPY, PostGIS,
  advisory locks, streaming replication, `CREATE INDEX CONCURRENTLY`,
  table-partitioned time series).
- **Portable SQL expression layer for SQLite** (`drops/sqlite`) — the
  SQLite dialect gains the full set of standard-SQL expression builders
  that previously lived only in `drops/pg`, so anything expressible in
  portable SQL is now available on SQLite too. New helpers:
  - **Operators / predicates** (`op.go`): free-standing `Eq`, `Ne`, `Gt`,
    `Gte`, `Lt`, `Lte`, `Not`, `In`, `NotIn`, `IsNull`, `IsNotNull`,
    `Between`, `NotBetween`, `Like`, `NotLike`, `LikeEscape`, plus the
    SQLite-native `Glob`, `Regexp`, and the NULL-safe `IsDistinctFrom` /
    `IsNotDistinctFrom` (rendered via SQLite `IS` / `IS NOT`).
  - **Aggregates / scalars** (`funcs.go`): `Count`, `CountAll`,
    `CountDistinct`, `Sum`, `Avg`, `Min`, `Max`, `SumDistinct`,
    `AvgDistinct`, `Filter`, plus SQLite's `Total`, `GroupConcat`,
    `Coalesce`, `IfNull`, `NullIf`, `Lower`, `Upper`, `As`, `Func`.
  - **Math** (`math.go`): `Abs`, `Round`, `Ceil`, `Floor`, `Trunc`,
    `Mod` (via `%`), `Power` (`pow`), `Sqrt`, `Sign`, `Exp`, `Ln`, `Log`,
    `Greatest`/`Least` (via multi-arg `max`/`min`), trig functions,
    `Random`, and the `Plus`/`Minus`/`Mul`/`Div` operators.
  - **Strings** (`strings.go`): `ConcatOp` (`||`), `Concat`, `ConcatWS`,
    `Length`, `OctetLength`, `Substr`, `Trim`/`LTrim`/`RTrim`, `Replace`,
    `Instr`, `Hex`/`Unhex`, `Quote`, `Chr`, `Unicode`, `Format`/`Printf`.
  - **Cast / Case** (`cast.go`): `CastAs`/`Cast` (SQLite has only the
    `CAST(x AS T)` form) and the `Case`/`CaseOn` builder.
  - **Subqueries** (`subquery.go`): `Exists`, `NotExists`, `Subquery`,
    `InSub`, `NotInSub`.
  - **CTEs** (`cte.go`): `With` / `WithRecursive` on the SELECT builder
    plus `CTEDef` (WITH / WITH RECURSIVE, supported since SQLite 3.8.3).
  - **Window functions** (`window.go`): `Over`, `WindowSpec`,
    `RowNumber`, `Rank`, `DenseRank`, `PercentRank`, `CumeDist`, `Ntile`,
    `Lag`, `Lead`, `FirstValue`, `LastValue`, `NthValue`.
  - **JSON1** (`json.go`): `JSONExtract`, `JSONGet` (`->`),
    `JSONGetText` (`->>`), `JSONArrayLength`, `JSONType`, `JSONValid`,
    `JSONQuote`, `JSONObject`, `JSONArray`, `JSONSet`/`JSONInsert`/
    `JSONReplace`, `JSONRemove`, `JSONPatch`, `JSONGroupArray`,
    `JSONGroupObject`.
  - **Date/time** (`datetime.go`): `Now`, `CurrentDate`, `CurrentTime`,
    `CurrentTimestamp`, `DateOf`, `TimeOf`, `DateTime`, `JulianDay`,
    `UnixEpoch`, `StrfTime`.
- **Portable SQL expression layer for ClickHouse** (`drops/clickhouse`)
  — the standard-SQL structural helpers ClickHouse supports with
  identical syntax, mirroring the SQLite/pg surface: `CastAs`/`Cast` and
  `Case`/`CaseOn` (`cast.go`); `Exists`, `NotExists`, `Subquery`,
  `InSub`, `NotInSub` (`subquery.go`); `With` / `CTEDef` (`cte.go`); and
  window functions `Over`, `WindowSpec`, `RowNumber`, `Rank`,
  `DenseRank`, `FirstValue`, `LastValue`, `NthValue`, plus `Lag`/`Lead`
  emitting ClickHouse's `lagInFrame`/`leadInFrame` (`window.go`).

## [0.4.1] - 2026-07-14

### Fixed
- **Qdrant missing-collection 404 classification** (`drops/qdrant`) — the
  `Client.Do` 404 check used a case-sensitive substring match on
  `"not found"`, which does not match real Qdrant's response body
  (``Not found: Collection `x` doesn't exist!`` — capital "Not found",
  "doesn't exist"). A missing collection therefore surfaced as a plain
  `HTTPError` instead of `ErrCollectionMissing`, so `CollectionExists`
  returned an error rather than `(false, nil)` and callers never reached
  their auto-create branch — collections were silently never created.
  The 404 is now classified case-insensitively and also accepts
  `"doesn't exist"` / `"does not exist"`. The test mock, which previously
  matched a lowercase body no live server emits, now uses Qdrant's real
  format, and a table-driven test pins the variants.

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
