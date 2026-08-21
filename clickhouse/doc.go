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
// Entity[T] (see entity.go) binds a Go struct to a Table and exposes
// Create / CreateMany / Query — the narrow subset of CRUD that maps
// to ClickHouse's Insert + Select builders. Entity.Validate registers
// per-row validators that run before Create / CreateMany; on
// CreateMany the first failing row aborts the whole batch.
//
// # Schema introspection, diffing and Push
//
// Introspect reads a table's real shape out of system.tables and
// system.columns — columns and types, the engine and its parameters,
// the sorting, primary, partition and sampling keys, the table's TTL
// and SETTINGS, and which columns take part in which key.
// BuildSnapshot derives the same shape from a Go [Schema], Diff
// produces the statements between the two, and Push applies them.
//
// Diff returns a [Plan] rather than the []string the other dialects
// return, because ClickHouse is the dialect where a schema difference
// does not always have a statement behind it. There is no ALTER that
// changes a table's engine, its partitioning, its primary key or —
// beyond appending columns the same statement adds — its sorting key,
// and no ALTER touches a column that takes part in any of those keys.
// Those differences come back as [Refusal] values naming the remedy,
// which is always a new table and a copy.
//
// The sorting key is worth singling out. A ReplacingMergeTree
// collapses rows that share it, so it is not a layout choice that
// happens to affect performance — it is the definition of "the same
// row". Changing it changes which rows are one row, and every group
// the old key merged comes back apart.
//
// [Analyze] grades the statements a plan does carry: metadata, a
// rewrite of every part in a background mutation, or a deletion with
// no way back. It matters more here than elsewhere because a
// ClickHouse ALTER returns before its work is done — mutations_sync
// defaults to 0 — so a statement the server accepted may have hours of
// rewriting still ahead of it in system.mutations.
//
// drops/mirror builds on the same analysis for its own evolution
// planner, which layers per-column opt-ins over it and knows things
// about a mirror the dialect cannot know — see mirror.Evolver.
//
// What this package does NOT try to mirror from drops/pg:
//
//   - per-row UPDATE/DELETE (ClickHouse mutations are asynchronous and
//     fundamentally different; use ALTER TABLE … UPDATE/DELETE via raw
//     SQL when you need them)
//   - ON CONFLICT (handled by engine choice, e.g. ReplacingMergeTree)
//   - Foreign keys / referential integrity (ClickHouse has none)
//   - a DiffDown. Reversing a ClickHouse migration is mostly not
//     something a statement can do, and a function whose name promises
//     it would be lying.
package clickhouse
