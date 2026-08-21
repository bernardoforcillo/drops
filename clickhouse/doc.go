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
// # Tenant scoping
//
// A table can declare an axis that every statement against it carries:
//
//	Events.ContextFilter(clickhouse.TenantFilter(EventTenantID)).
//	    ScopeWritesByTenant(EventTenantID)
//
//	ctx := clickhouse.WithTenant(ctx, currentTenant)
//
// From there a SELECT takes the predicate and an INSERT takes the
// stamp, whether it was built through an [Entity], through db.Select /
// db.Insert, or by [VectorStore.Search] — because the axis lives on the
// TABLE and is resolved by the executors rather than injected by one
// entry point. A ctx with no tenant is [ErrTenantMissing] and no
// statement at all. See tenant.go for the whole of it, including the
// list of places the predicates do not reach.
//
// One consequence changes an existing habit: since a table's context
// filters are resolved against a ctx, [SelectBuilder.ToSQL] no longer
// necessarily shows the whole statement. [SelectBuilder.ToSQLCtx] does,
// and it is what to log and what to assert on in a test.
//
// What this dialect's version of the feature does NOT have, because the
// surface it would attach to does not exist here:
//
//   - no UPDATE or DELETE to carry a predicate, so the write side is
//     stamping and refusal only. A mutation is an
//     ALTER TABLE … UPDATE/DELETE, asynchronous and not transactional,
//     and this package does not model one.
//   - no upsert to gate. PostgreSQL's cross-tenant overwrite needs an
//     ON CONFLICT DO UPDATE; ClickHouse's needs no statement at all,
//     because a merging engine folds rows sharing a sorting key in the
//     background. That is where the check went instead — see
//     [ErrTenantNotInSortingKey].
//   - no relations and no eager loader, so there is no child query to
//     carry the axis into and no per-parent LIMIT rewrite to get wrong.
//   - no query cache and no primary-key cache, so there is no cache key
//     that could answer one tenant's question with another's rows.
//   - no set operations, so a UNION operand cannot lose its scoping.
//     CTE bodies and subquery operands are resolved recursively.
//   - no bulk path of drops' own beyond the batch INSERT, which is
//     stamped like any other. The native columnar protocol this
//     package's doc points very large batches at is a driver API drops
//     is not in: rows written that way are the caller's to stamp.
//
// Entity[T] (see entity.go) binds a Go struct to a Table and exposes
// Create / CreateMany / Query — the narrow subset of CRUD that maps
// to ClickHouse's Insert + Select builders. Entity.Validate registers
// per-row validators that run before Create / CreateMany; on
// CreateMany the first failing row aborts the whole batch.
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
