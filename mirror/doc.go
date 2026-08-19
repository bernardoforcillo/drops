// Package mirror keeps a copy of an OLTP table in the stores that
// answer the other two kinds of question: ClickHouse for analytics,
// Qdrant for similarity search.
//
// The pitch drops can make and no other Go ORM can is "one schema,
// three engines" — Postgres takes the writes, ClickHouse answers the
// aggregations, Qdrant answers "find me things like this". What
// stands in the way is that each store wants its own schema
// declaration and its own ingestion path, so keeping three copies of
// one table in agreement becomes the application's problem.
//
// This package is that problem solved in three pieces:
//
//   - [DeriveClickHouse] turns a [github.com/bernardoforcillo/drops/pg.Table]
//     into the ClickHouse table that mirrors it. The analytics schema
//     is derived, never re-declared, so it cannot drift from the
//     source.
//   - [Sink] is where changes go. [NewClickHouseSink] and
//     [NewQdrantSink] implement it.
//   - [Pump] moves changes from a [Source] to the sinks, in batches,
//     with the delivery guarantee the source provides.
//
// # Why the outbox and not the changefeed
//
// drops/pg offers two ways to observe row changes, and only one of
// them can back a mirror.
//
// [github.com/bernardoforcillo/drops/pg.Subscribe] is a LISTEN/NOTIFY
// feed. It is low-latency and it drops events when the consumer falls
// behind — a deliberate choice that keeps a slow subscriber from
// growing memory without bound. For a cache warmer that is correct.
// For a mirror it is not: a dropped event is a row that is silently
// wrong in ClickHouse until something else happens to touch it, and
// nothing tells you it happened.
//
// The outbox is durable. Rows are written in the same transaction as
// the mutation, drained with retries, and marked published only once
// a sink has accepted them. Delivery is at-least-once, which is why
// every sink here is idempotent: ClickHouse mirrors use
// ReplacingMergeTree keyed on the primary key with a version column,
// so replaying a change converges rather than duplicating, and
// Qdrant's upsert is idempotent by construction.
//
// [OutboxSource] is therefore the supported source. A changefeed
// source is available as [UnreliableChangeFeedSource] for the cases
// where approximate freshness genuinely is enough — the name is the
// documentation.
//
// # Deletes
//
// ClickHouse has no cheap row delete, so a mirrored delete is written
// as a normal row with the `_deleted` marker set. Reads filter it out;
// [NotDeleted] renders that predicate, and the derived table's doc
// comment explains the FINAL requirement that comes with
// ReplacingMergeTree.
package mirror
