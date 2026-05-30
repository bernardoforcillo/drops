// Package drops is a Drizzle-inspired, driver-agnostic SQL toolkit for Go.
//
// The root package defines the [Driver], [Tx], [Rows] and [Result] interfaces
// that adapt the toolkit to any underlying database connection (database/sql,
// pgx, or your own pool) plus the building blocks for composing SQL:
// [Expression] and [Builder].
//
// Observability is provided by [Hook], [ChainHooks], and [CallHook] — a
// single contract shared by every dialect. A ready-made structured hook
// is available via [LoggerHook].
//
// # Dialect packages
//
//   - [github.com/bernardoforcillo/drops/pg] — PostgreSQL. Full surface:
//     SELECT / INSERT / UPDATE / DELETE, DDL (schemas, enums, sequences,
//     views, functions, triggers, indexes), file-based migrations, and
//     eager-loaded relations (HasMany, HasOne, BelongsTo, ManyToMany).
//
//   - [github.com/bernardoforcillo/drops/clickhouse] — ClickHouse. Typed
//     columns (Array, Nullable, LowCardinality, Decimal, DateTime64, Tuple,
//     Map, Enum8/16), full SELECT (PREWHERE, FINAL, SAMPLE, ASOF JOIN,
//     SETTINGS), batch INSERT, and the analytics-aggregate library.
//
//   - [github.com/bernardoforcillo/drops/qdrant] — Qdrant vector database.
//     Stdlib-only HTTP client: collection management, upsert/delete/retrieve,
//     search / recommend / scroll, and a Must/Should/MustNot filter DSL.
//
// # Cache packages
//
//   - [github.com/bernardoforcillo/drops/cache] — driver-agnostic cache
//     interface (Get / Set / Delete / Exists / TTL / Ping / Close) plus a
//     MultiCache batch extension and sentinel errors.
//
//   - [github.com/bernardoforcillo/drops/cache/memory] — in-process LRU
//     cache with TTL and an optional janitor goroutine. Zero deps; ideal
//     for tests and the local tier of a two-level cache.
//
//   - [github.com/bernardoforcillo/drops/cache/redis] — Redis backend with
//     a minimal RESP2 client and a bounded connection pool. Zero deps.
//
// # Adapter
//
//   - [github.com/bernardoforcillo/drops/stdlib] — wraps a *sql.DB as a
//     drops.Driver so any database/sql driver works with drops out of the box.
package drops
