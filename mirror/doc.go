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
// [OutboxSource] is therefore the supported source, and the only one
// this package ships. A feed with weaker guarantees can be wrapped as
// a [Source] by hand — the interface is one method — but read what
// that doc says about versions first: a source that cannot number its
// changes cannot promise that the mirror keeps the newer of two
// writes to one row.
//
// # Versions
//
// [Change.Version] is the most consequential field in the package. A
// ReplacingMergeTree keeps the highest version per key and throws the
// rest away at merge time, so whatever wins here wins for good: there
// is no later read that re-examines the decision, and no error
// anywhere if it was wrong. Two changes to one row that are versioned
// in the wrong order leave the mirror holding the older value,
// permanently and silently.
//
// So the version has to come from something that is monotone with
// commit order per key. The uint64 line is cut into three bands, and
// which band a version is in is decided by which of three functions
// produced it:
//
//	0                MaxSeedVersion              LiveVersionBase            1<<64
//	|-- seed band --------|-- clock band --------------|-- live band ----------|
//	  [SeedVersionAt]        [ClockVersion]               [LiveVersion]
//	  a reseed pass          a wall clock                 a source sequence
//
// The live band is the one live traffic runs in. [OutboxSource]
// stamps [LiveVersion] of the outbox event id, and the id is the
// right number for three reasons: it comes from one BigSerial in one
// database, so no clock skew can reach it; it is dense and totally
// ordered; and it agrees with source commit order per key, because an
// emitter that mutates a row and emits in one transaction makes a
// second writer of that row wait on the row lock until the first
// commits — so the second writer cannot take an id ahead of the
// first. Per-key commit order is the only order a mirror needs, and
// this is the one mechanism in reach that actually provides it.
//
// Per key and no wider. An id is assigned when the outbox row is
// inserted, not when its transaction commits, so a long transaction
// can leave a lower id in flight while a higher one is drained and
// acknowledged. Between two keys that costs nothing — nothing
// compares versions across keys. Within one key it cannot happen,
// because the row lock that orders the two emitters also orders their
// inserts.
//
// The id is offset into the top half of the space rather than used
// raw, and that offset is what pays for the other two bands.
//
// Below the live band is the seed band, which exists because a
// fill-mode reseed needs a version strictly below everything live: a
// seeded row must lose every race so it can populate keys the change
// stream never covered and overwrite nothing else (see [Reseeder]).
// Raw outbox ids start at 1 and leave no room under them. Offset,
// they leave half the number line, and a seed takes the millisecond
// count of the pass that wrote it — low enough to lose to everything
// live, but increasing from pass to pass so two passes never write
// different values at the same version.
//
// Between them is the clock band: nanoseconds since the epoch, which
// is what [Change.Normalized] falls back to for a source that has no
// sequence of its own. It orders such a feed only as well as the
// hosts' clocks agree, which is the honest answer for a source that
// has nothing better; what the band buys is that it cannot outrank
// the durable stream, so running both against one mirror resolves
// every disagreement in favour of the outbox.
//
// The offset is applied in Go, by [OutboxSource.Fetch]. Nothing in
// the database changes: an outbox already in production keeps its
// sequence, its rows and its ids, and no migration is required.
//
// What it costs:
//
//   - A version is no longer a timestamp. Before the bands, a
//     mirrored row's version was a UnixNano stamp an operator could
//     read as "when"; now it is base plus a sequence number and means
//     nothing outside its outbox. Mirror a timestamp column if you
//     want to query on time.
//   - Ids are only comparable within one outbox. Two databases
//     feeding one mirrored table — a sharded or per-tenant
//     deployment — hand out ids from two independent sequences, so
//     their versions interleave meaninglessly. Mirror each shard into
//     its own table, or give each shard a disjoint slice of the id
//     space with ALTER SEQUENCE.
//   - The ordering holds only for rows that are mutated and emitted
//     in one transaction. A path that writes the row without calling
//     [EmitChange] produces no version at all, which is exactly the
//     hole a reseed exists to fill.
//   - Seed passes are ordered by the reseeding machine's clock. A
//     clock that jumps backwards between two passes leaves the second
//     one unable to overwrite the first. It still cannot overwrite
//     live data — the band boundary is structural, not a comparison
//     between two clock readings.
//
// One property comes free and is worth stating, because it is what
// makes the change safe to deploy: every version this package stamped
// before the bands existed was a UnixNano reading, and the whole
// int64 nanosecond space lies below LiveVersionBase. Every row a
// running mirror already holds is therefore below every sequenced
// change, so the first change to any key after the upgrade outranks
// what is there, and no migration step is needed to stop a stale row
// winning forever. (Those old readings are in the clock band proper
// as long as the change was dated more than five hours after the
// epoch, which every real one is; a fixture dated 1970 could sit low
// enough for a seed to reach it. [ClockVersion] lifts such a reading
// now, so only rows written by an older release can be there.)
//
// Finally, a version only protects a mirror if the sink reads it.
// ClickHouse compares versions in the engine; Qdrant has no
// conditional upsert and cannot. That is why a fill-mode reseed —
// the one path that writes to sinks without going through the
// ordered stream — accepts only sinks that declare themselves
// [VersionAwareSink], and why the way to seed a Qdrant collection is
// a repair reseed, which is ordered by delivery instead.
//
// # Deletes
//
// ClickHouse has no cheap row delete, so a mirrored delete is written
// as a normal row with the `_deleted` marker set. Reads filter it out;
// [NotDeleted] renders that predicate, and the derived table's doc
// comment explains the FINAL requirement that comes with
// ReplacingMergeTree.
//
// A delete is a change like any other, so it is ordered like one: a
// row deleted and then re-inserted comes back, because the insert
// carries the higher outbox id and wins the merge. Under a clock
// stamp that was not reliable, which is the sharpest form the old
// defect took — a re-inserted row could stay invisible in analytics
// for good.
package mirror
