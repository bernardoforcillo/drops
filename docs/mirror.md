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

## Seed it

A pump only carries changes that happen after it starts. Everything
already in the table is invisible to it, and so is everything that
happened while a sink was down and the outbox was truncated out from
under it. `Reseeder` walks the source and replays it.

```go
seed, _ := mirror.NewFillReseeder(db, docs, chSink, qSink)
seed = seed.Named("docs-into-clickhouse").ChunkSize(5000)

if err := seed.Run(ctx); err != nil {
    // interrupted; Run again and it resumes at the next unseeded key
}
```

It is cursored, not offset — progress is committed after every chunk
and keyed by the job name, so an interrupted seed of a large table
resumes where it stopped rather than starting over. A chunk that fails
leaves the cursor alone, so the chunk is replayed rather than skipped.

There are two modes, and the difference is which rows they can correct.

`NewFillReseeder` writes straight to the sinks at `SeedVersion`, below
every version a live write can produce. That makes it safe to run
against a live mirror: a row the pump already delivered wins, and only
the holes get filled. It cannot fix a row that is present but wrong,
because that row's version is in the live space and beats the seed.

`NewRepairReseeder` emits into the outbox instead, so the seed travels
the ordinary path and a running pump delivers it. That is what
overwrites a wrong value — and it is expensive, since every row passes
through every sink and a Qdrant mirror re-embeds the whole table. It
locks each chunk `FOR SHARE` before reading, which is what makes a
concurrent writer beat the seed rather than the other way around.

Two things a reseed cannot do, both for the same reason — it reads the
source, so it only learns about rows that are there:

- A key deleted from the source while the mirror was behind is
  invisible to it. Its stale mirror row survives untouched.
- A soft delete is a live row to the walk, so a repair pass reinserts
  it over the tombstone the application emitted. Give `Where` the same
  predicate the application means by "present" and the two agree.

## Check it

You cannot tell by looking whether a mirror is right. `Verifier`
compares the two.

```go
v, _ := mirror.NewVerifier(db, docs, chDB, chDocs)
rep, err := v.RangeSize(10_000).Verify(ctx)

if !rep.Aligned() {
    for _, d := range rep.Divergences {
        log.Printf("%s %s: %s", d.Direction(), d.Key, d.Kind)
    }
}
```

It compares in ranges by digest and only narrows the ranges that
disagree, so a clean mirror costs one scan of each side and a dirty one
costs detail exactly where the dirt is. Divergences are classified
rather than counted: a row the mirror never received reads differently
from one it received wrong, which reads differently again from one it
tombstoned while the source still has it.

Neither engine computes the checksum. Both return rows, and every value
from either side is encoded in Go against the mirror column's declared
ClickHouse type. That is not fastidiousness — PostgreSQL and ClickHouse
disagree about the text of a value often enough that an engine-side
checksum reports divergence on a mirror that is perfectly correct.
Trailing zeros on a numeric, the rendering of a float, the timezone a
timestamp prints in. Encoding both sides against one schema is what
makes "12.30" and "12.3" in a `Decimal(10,2)` compare equal.

A pass proves less than it looks. Two engines admit no consistent cut,
so a row that changes between the two reads differs legitimately. The
mirror is read first and the source second, so every difference is at
least consistent with the mirror lagging — the normal, self-healing
state. `Recheck` is how a candidate is discharged: re-read the named
keys, and the ones that still disagree are real.

## Evolve it

The source table gains a column. The mirror does not, and the sink
keeps writing the old shape.

```go
ev, _ := mirror.NewEvolver(chDB, docs)
plan, _ := ev.Plan(ctx)

for _, s := range plan.Steps {
    fmt.Println(s.SQL)
}
if err := plan.Refused(); err != nil {
    log.Fatal(err)
}

_, err := ev.Apply(ctx)
sink, _ := mirror.NewClickHouseSink(chDB, ev.Target())
```

`Plan` reads the live mirror and compares it against the table the
source derives. It adds what the source gained and widens a type where
the widening provably cannot lose data. Everything else it refuses by
name — a drop, a narrowing, a key column, a cast it cannot prove — and
a refusal carries both the reason and the opt-in that would allow it,
so the plan is a conversation rather than a wall.

```go
ev.AllowDrop("legacyRef").AllowTypeChange("score")
```

The opt-in is per column on purpose. A single "yes, drop things" flag
would sweep along whatever drift appeared between the review and the
apply, which is the moment you least want a blanket permission.

A rename arrives as a drop plus an add, and no comparison of two
schemas can tell that apart from an unrelated pair. `Evolver` flags the
pair rather than guessing, and leaves the choice where it belongs.
