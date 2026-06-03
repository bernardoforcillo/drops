// Package clickhouse provides a ClickHouse dialect for drops.
//
// The shape mirrors the drops/pg package — typed columns via Col[T],
// table declarations, fluent query builders, and a DB wrapper with
// the same Hook / Ping / Close / InTx surface — but the SQL it emits
// is ClickHouse-flavoured:
//
//   - "?" positional placeholders (matches the clickhouse-go database/sql driver)
//   - Engine-bound tables (CREATE TABLE … ENGINE = MergeTree() ORDER BY …)
//   - Native CH types: Array(T), Nullable(T), LowCardinality(T),
//     Decimal(P,S), Map(K,V), Tuple(...), Enum8/16, DateTime[64],
//     fixed-width integers (UInt8/UInt64/Int8/Int64/…), UUID
//   - Aggregates such as uniq, uniqExact, quantile, anyAgg
//
// Driver-agnostic: like drops/pg, the package imports no concrete
// driver. The bundled drops/stdlib adapter works against any database/
// sql-compatible ClickHouse driver — clickhouse-go's stdlib bridge is
// the standard choice:
//
//	import (
//	    _ "github.com/ClickHouse/clickhouse-go/v2"
//	    "github.com/bernardoforcillo/drops/clickhouse"
//	    "github.com/bernardoforcillo/drops/stdlib"
//	)
//
//	sqlDB, _ := sql.Open("clickhouse", "clickhouse://localhost:9000/default")
//	db := clickhouse.New(stdlib.New(sqlDB))
//
// Templates (Timestamps, SoftDelete, Audit, UUIDPrimaryKey) provide
// reusable column groups — see template.go for the function-style
// pattern and mixin.go for the richer Mixin interface. ClickHouse has
// no foreign keys, so Audit emits plain scalar columns mirroring the
// target's type. Lifecycle hooks are limited to OnInsert (no
// builder-side UPDATE/DELETE); default filters on SelectBuilder
// honour Unscoped() for opt-out.
//
// What this package does NOT try to mirror from drops/pg:
//
//   - per-row UPDATE/DELETE (ClickHouse mutations are asynchronous and
//     fundamentally different; use ALTER TABLE … UPDATE/DELETE via raw
//     SQL when you need them)
//   - ON CONFLICT (handled by engine choice, e.g. ReplacingMergeTree)
//   - Foreign keys / referential integrity (ClickHouse has none)
//   - Schema introspection and Push (planned)
package clickhouse
