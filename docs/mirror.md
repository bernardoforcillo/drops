# One table, three engines

Three questions, three engines that answer them well:

- *Give me this row, and change it.* — PostgreSQL.
- *Give me a count by month across two years.* — ClickHouse.
- *Find me things like this one.* — Qdrant.

drops is the only Go ORM that speaks all three. That counted for
nothing while keeping one table in agreement across them was the
application's problem: three schema declarations to keep in step by
hand, and three ingestion paths to write.

`drops/mirror` is that problem solved.

## Derive the analytics schema

```go
chDocs, err := mirror.DeriveClickHouse(Docs,
    mirror.WithDatabase("analytics"),
    mirror.WithPartitionBy(clickhouse.Func("toYYYYMM", DocCreatedAt)),
)
```

```sql
CREATE TABLE "analytics"."docs" (
    "id" Int64,
    "title" String,
    "lang" Nullable(String),
    "created_at" DateTime64(6, 'UTC'),
    "_drops_version" UInt64,
    "_drops_deleted" UInt8
) ENGINE = ReplacingMergeTree("_drops_version")
ORDER BY ("id")
PARTITION BY (toYYYYMM("created_at"))
```

The analytics schema is a *function* of the transactional one. Add a
column in Postgres and it appears here; the two cannot drift the way
two hand-written declarations do.

The type mapping is deliberately lossless-or-wider rather than exact:
`date` becomes `Date32` because `Date` stops at 2149 and wraps;
timestamps become `DateTime64(6)` because Postgres stores microseconds
and `DateTime` holds seconds; a bare `numeric` becomes `String` rather
than a silently chosen `Decimal` width.

## Read it back

Two additions come with the mirroring, and reads have to account for
both:

```go
db.Select().From(chDocs).Final().Where(mirror.NotDeleted(chDocs))
```

`FINAL` forces the merge that collapses superseded versions — without
it a row updated twice appears twice. `NotDeleted` drops the
tombstones. Neither is free, which is the honest cost of mirroring a
mutable table into an append-only store. For a table that is only ever
inserted into, pass `WithEngine` a plain `MergeTree` and skip both.

## Why the outbox and not the changefeed

drops offers two ways to observe row changes, and only one can back a
mirror.

`pg.Subscribe` is a LISTEN/NOTIFY feed. It is low-latency and it
**drops events when the consumer falls behind** — a deliberate choice
that keeps a slow subscriber from growing memory without bound. For a
cache warmer that is correct. For a mirror it is not: a dropped event
is a row that is silently wrong in ClickHouse until something else
happens to touch it, and nothing tells you.

The outbox is durable. Rows are written in the same transaction as the
mutation, drained with retries, and marked published only once a sink
has accepted them:

```go
err := db.InTx(ctx, func(tx *pg.DB) error {
    if err := DocEntity.Update(tx, ctx, &doc); err != nil {
        return err
    }
    return mirror.EmitChange(ob, tx, ctx, mirror.Change{
        Op:  mirror.OpUpdate,
        Key: strconv.FormatInt(doc.ID, 10),
        Row: map[string]any{"id": doc.ID, "title": doc.Title, "lang": doc.Lang},
    })
})
```

Either both land or neither does. The mirror can never be told about a
row that rolled back, nor miss one that committed.

Delivery is therefore at-least-once, which is why every sink here is
idempotent: the ClickHouse mirror is a `ReplacingMergeTree` keyed on
the primary key with a version column, so replaying a change converges
rather than duplicating, and Qdrant's upsert is idempotent by
construction.

## Run it

```go
src, _ := mirror.NewOutboxSource(ob)

chSink, _ := mirror.NewClickHouseSink(chDB, chDocs)
qSink, _ := mirror.NewQdrantSink(qc, "docs", embed)

pump, _ := mirror.NewPump(src, chSink, qSink)
go pump.Run(ctx)
```

`embed` is yours — it is the one step drops cannot do for you, being a
call to whichever model you chose over whichever fields you consider
the document. Returning a nil vector skips the row, which is how you
say "this change does not touch the index".

The pump is deliberately small, and the interesting decisions are what
it refuses to do:

- It does not acknowledge a batch any sink rejected.
- It stops at the first failing sink rather than widening the window in
  which the two disagree.
- It retries a failing batch instead of skipping it. Falling behind is
  the correct failure mode for a mirror; diverging is not.

Use `Step` instead of `Run` to drive it from your own scheduler.

## What a delete looks like

ClickHouse has no cheap row delete, so a mirrored delete is an ordinary
insert with the tombstone marker set and a version above the row it
retires. Qdrant gets a real point delete. Within a batch, changes are
applied in runs of the same kind rather than reordered into
all-upserts-then-all-deletes — a batch that creates a key and then
removes it has to end with the point gone.
