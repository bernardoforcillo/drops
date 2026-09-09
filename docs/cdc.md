# Reading the write-ahead log

Three ways to learn that a row changed, and they are not
interchangeable.

| | costs the writer | survives a restart | replays | ordering |
|---|---|---|---|---|
| `pg.Subscribe` (trigger + `NOTIFY`) | a trigger per write | no | no | none |
| `mirror.OutboxSource` | a second row per write | yes | yes | per key |
| `pg.Stream` (logical replication) | nothing | yes | yes | total |

The first two are documented in [mirror.md](mirror.md). This page is
the third.

## Why bother, when the outbox works

The outbox works, and it asks the application for something real.
Every transaction that mutates a mirrored row must also write its own
change into the outbox table:

```go
err := db.InTx(ctx, func(tx *pg.DB) error {
    if err := DocEntity.Update(tx, ctx, &doc); err != nil {
        return err
    }
    return mirror.EmitChange(ob, tx, ctx, mirror.Change{ /* ... */ })
})
```

That is what buys the guarantee — both land or neither does — and it
has three costs. Every write path has to cooperate. A path that
nobody remembered to instrument is invisible until the mirror is
wrong. And the write volume doubles.

PostgreSQL is already writing every change down, in order, durably,
for its own replication. Logical decoding turns that record back into
rows. A consumer of it costs the writer nothing, cannot miss a change,
and can replay from any position the server still holds.

What it costs instead is **operational**, and the rest of this page is
mostly about that.

## What the server needs

Three things, none of which drops can do for you:

```ini
# postgresql.conf — needs a restart
wal_level = logical
max_replication_slots = 10      # one per consumer, plus room
max_wal_senders = 10
```

The connecting role needs `REPLICATION` (or superuser), and on a
managed provider the setting is usually a parameter-group flag rather
than a file. If `wal_level` is not `logical`, slot creation fails with
a message that says so.

## Declaring what to replicate

`pgoutput` — PostgreSQL's built-in decoder — replicates what a
*publication* names, and nothing else:

```go
for _, stmt := range pg.Publication("drops_mirror", Docs, DocTags) {
    if _, err := db.Exec(ctx, stmt); err != nil { return err }
}
```

> **The failure that is silent.** A table not in the publication
> produces no messages at all. Not an error, not a warning — the
> consumer simply never hears about it, and the first sign is a mirror
> missing a table's rows with nothing in the logs. Adding a table later
> means `ALTER PUBLICATION`; the slot does not need recreating.

The second gotcha costs data rather than time. An `UPDATE` or `DELETE`
decodes with the *old* row only as far as the table's replica identity
carries it, and the default carries the primary key and nothing else:

```go
db.Exec(ctx, pg.ReplicaIdentityFull(Docs))
```

Enough to address a mirrored row without this; not enough to see what
a column changed *from*. Needed by an audit trail, by a sink that keys
on something other than the primary key, and by
[precise cache invalidation](caching.md). Not free — it widens every
`UPDATE` and `DELETE` record in the WAL to a whole row — so set it
where it earns its keep.

## Slots

A slot is the server-side registration that says "I want these
changes, and do not throw away the WAL I have not read yet".

```go
created, err := pg.EnsureSlot(ctx, db, "mirror_docs", pg.PluginPgOutput)
```

`EnsureSlot` is idempotent, and deliberately so: two processes starting
at once both want the slot to exist, and the loser of the race gets
`ErrDuplicateObject` for a slot that is now present, which is the
outcome it asked for.

Slot names are restricted by PostgreSQL to `[a-z0-9_]`, at most 63
characters. drops checks that before the statement so the error names
the mistake rather than arriving from the server much later.

### The slot is the dangerous part

A temporary slot disappears with its connection. A permanent one
outlives the process — which is the point, since a consumer that
restarts resumes where it left off, and also the hazard:

**PostgreSQL retains every WAL segment a slot might still need,
forever, whether or not anybody ever comes back.** An abandoned slot
grows `pg_wal` without bound until the volume fills and the server
stops accepting writes. It is the most common way a logical
replication deployment takes an outage, and the only warning is a
number nobody is looking at.

So look at it:

```go
lag, err := pg.SlotLag(ctx, db, "mirror_docs")   // bytes of unconfirmed WAL
```

That is the one metric to alert on. A consumer keeping up holds it
near zero; one that has died holds a number that only grows, measured
against the disk `pg_wal` lives on. `SlotLag` returns `ErrNoSuchSlot`
for a slot that is gone rather than zero, because a monitor reading
zero for a slot that does not exist is worse than one that reports the
truth.

And clean up after the consumers that are not coming back:

```go
dead, err := pg.InactiveSlots(ctx, db, "mirror_")   // prefix filter, "" for all
dropped, err := pg.DropInactiveSlots(ctx, db, names(dead))
```

`DropInactiveSlots` re-checks activity in the same statement as the
drop. That is not decoration: between a listing and a drop, a consumer
that was merely *restarting* can have reattached, and dropping its slot
loses its position — it comes back, finds nothing, and either starts
from now (silently missing everything in between) or has to be
reseeded. The filter and the drop are one command so there is no
window at all.

Give it a grace period anyway. A restarting consumer and a dead one
look identical in one reading.

## Streaming

Slot management is plain SQL and goes through the ordinary
`drops.Driver`. Streaming is not: replication is a connection *mode*,
requested at startup, which three methods cannot express. So it
arrives the way `COPY` and `LISTEN` already do here — an interface a
driver may implement, found by duck typing:

```go
type LogicalStreamer interface {
    CreateReplicationSlot(ctx context.Context, name, plugin string, opts SlotOptions) (SlotHandle, error)
    StartReplication(ctx context.Context, slot string, start uint64, pluginArgs map[string]string) (ReplicationStream, error)
}
```

With pgx the adapter is short, because `pglogrepl` ships the protocol:

```go
type pgxStreamer struct{ conn *pgconn.PgConn }   // dialled with replication=database

func (s pgxStreamer) StartReplication(ctx context.Context, slot string,
    start uint64, args map[string]string) (pg.ReplicationStream, error) {
    err := pglogrepl.StartReplication(ctx, s.conn, slot,
        pglogrepl.LSN(start), pglogrepl.StartReplicationOptions{
            PluginArgs: pluginArgs(args),
        })
    return &pgxStream{conn: s.conn}, err
}
```

Compose it with your `drops.Driver` (embedding works) and drops finds
it through the wrapper stack — `RetryCachedPlans`, a
`StatementRegistry`, `Replicated`:

```go
stream, err := pg.Stream(db, ctx, "mirror_docs", 0, nil)
```

Passing `0` as the start position means "resume from the slot's
confirmed position", which is what a restarting consumer wants and
what makes the slot worth having. A driver without the interface gets
`ErrStreamNotSupported`.

## Whole transactions, not loose changes

A logical stream arrives as `BEGIN`, changes, `COMMIT`. Only the whole
group is meaningful: half of a transfer is not a smaller transfer, it
is a wrong balance. `Reassemble` enforces that framing once, so no
consumer has to:

```go
err := pg.Reassemble(ctx, stream, func(ctx context.Context, tx pg.Transaction) error {
    for _, ch := range tx.Changes {
        // ch.Schema, ch.Table, ch.Op, ch.New, ch.Old
    }
    return applyDurably(ctx, tx)
})
```

The callback is called only after the commit is seen. **When it
returns nil the commit position is acknowledged**, which is what
releases the WAL behind it — so it must not return nil until it has
durably handled the transaction. Return an error and nothing is
acknowledged, the loop stops, and the next run replays from the last
confirmed position. Delivery is therefore at-least-once and consumers
must be idempotent, which is the same contract `mirror.Source` states
for the same reason.

Two cases are handled quietly. An empty transaction — one that touched
nothing in the publication — is acknowledged without calling the
callback, or an idle publication would hold the slot back
indefinitely. A keepalive is acknowledged when no transaction is open
and ignored inside one, because confirming a position mid-transaction
would tell the server the consumer is done with changes it has not
seen.

### When the consumer buffers

`ReassembleDeferred` is the same framing with no acknowledgement at
all: the caller confirms, later. It exists for a consumer that cannot
say "handled" at the moment it is handed a transaction — which is
exactly the shape of a mirror, where the change is buffered here and
only durable once the sinks have taken it.

Two differences follow, and both matter:

- The callback is called for *every* commit, including the empty ones.
- A keepalive arrives as a transaction with no changes.

Both travel rather than being confirmed on the spot, because **an
acknowledgement is cumulative and takes everything before it**.
Confirming an empty transaction at position 50 while a real one at
position 40 is still in your buffer releases the WAL behind 40.

So a caller must confirm in the order it received, and never confirm a
position it has not finished with.

## Feeding a mirror from it

```go
stream, err := pg.Stream(db, ctx, "mirror_docs", 0, nil)

src, err := mirror.NewLogicalSource(stream, mirror.LogicalOptions{
    Tables: map[string]string{"docs": "id"},   // relation → key column
})
go src.Run(ctx)                                 // pumps the stream

pump, _ := mirror.NewPump(src, chSink, qSink)   // drained like any other source
go pump.Run(ctx)
```

`LogicalSource` has one moving part more than the other sources: `Run`
must be running for `Fetch` to see anything. A replication stream is
push-shaped and a `Source` is pull-shaped, and something has to hold
the boundary.

Three things it does that are worth knowing:

- **Transactions are never split across batches.** The `max` a pump
  asks for is a floor rather than a ceiling: once it is reached the
  batch ends, but the transaction that reached it is carried entire.
- **Relations not in `Tables` are dropped**, because a publication is
  usually wider than one mirror and a change to a table the mirror
  does not hold has no key to address.
- **Every change in a transaction takes the transaction's commit
  position as its version.** They are simultaneous as far as any
  observer is concerned, and giving them one version keeps a sink from
  inventing an order among them.

The commit LSN maps into `mirror.Change.Version` through
`pg.LSNVersion`, landing in the same live band `mirror.LiveVersion`
uses. It is a *better* number than an outbox id: an outbox id orders
commits per key, an LSN orders them outright.

Which is also the reason not to mix them. **Do not feed one mirror
from both a logical source and an outbox source** — both land in the
same band with unrelated numbering, so the higher number wins
regardless of which is newer.

## The seam a reseed usually gets wrong

A mirror has to be filled with what is already there and then kept up
to date by the stream, and the two have to meet exactly.

- Copy first, start the stream after → everything in between is lost.
- Start the stream first, copy after → the copy overwrites changes the
  stream already delivered.

Either way nothing reports it. `mirror`'s version bands make the
second case survivable (a seeded row loses to anything the stream
carried) but they are a guard rather than a join.

A slot created over a replication connection can hand back **both
halves of one instant**: an LSN, and the name of a snapshot showing the
database exactly as it stood at that LSN.

```go
streamer, ok := pg.Streamer(db)

handle, err := streamer.CreateReplicationSlot(ctx, "mirror_docs",
    pg.PluginPgOutput, pg.SlotOptions{ExportSnapshot: true})

// Fill the mirror from the database as it was at handle.ConsistentPoint.
err = pg.WithSnapshot(ctx, db, handle, func(snap *pg.DB) error {
    return fillMirror(ctx, snap)
})

// Then stream from exactly there. No gap, no overlap.
stream, err := streamer.StartReplication(ctx, handle.Name,
    handle.ConsistentPoint, nil)
```

`WithSnapshot` runs the callback inside a `REPEATABLE READ, READ ONLY`
transaction with `SET TRANSACTION SNAPSHOT` applied, and rolls back on
the way out — there is nothing to commit.

Three constraints:

- The snapshot must be imported on a **different connection** from the
  one that created the slot, and stays importable only while that
  connection is open. Keep the streamer alive for the copy.
- A long snapshot holds back vacuum on everything it can see. Copying
  a large table is the intended use; leaving the transaction open
  while something else is decided is not.
- `pg.CreateSlot` cannot do this. It is `pg_create_logical_replication_slot`,
  an ordinary function, and snapshot export belongs to the replication
  protocol. It returns a handle with an empty `SnapshotName`, and
  `WithSnapshot` refuses it with `ErrNoSnapshot` rather than silently
  reading the present.

The snapshot name is the one value this package concatenates into SQL
— `SET TRANSACTION SNAPSHOT` will not take a parameter — so its format
is checked against PostgreSQL's own exactly rather than trusted.

## Operating it

A short checklist, all of it drawn from the failure modes above.

- [ ] `wal_level = logical`, and enough `max_replication_slots`.
- [ ] Every mirrored table is in the publication. Adding one later is
      an `ALTER`, and forgetting is silent.
- [ ] `REPLICA IDENTITY FULL` on the tables whose old values a consumer
      needs.
- [ ] `pg.SlotLag` on a dashboard, with an alert. This is the one.
- [ ] `pg.InactiveSlots` on a schedule, with a grace period before
      `DropInactiveSlots`.
- [ ] The consumer is idempotent, because delivery is at-least-once.
- [ ] One source per mirror.

## What the integration suite covers

`integration/pglogical_test.go` runs against a real PostgreSQL with
`wal_level = logical`:

- slot lifecycle — create, ensure (twice), status, list, drop
- `SlotLag` growing across unread writes
- `InactiveSlots` and `DropInactiveSlots`, including the re-check that
  spares an active slot
- decoded output from real DML: transaction framing, whole
  multi-statement transactions, insert/update/delete
- `REPLICA IDENTITY FULL` changing what a delete carries
- acknowledgement actually moving `confirmed_flush_lsn`, and
  `ReassembleDeferred` moving nothing
- `mirror.LogicalSource` driving a `Pump` end to end, with versions
  ordered by commit position
- the slot advancing past a table the mirror does not hold
- `WithSnapshot` refusing to see a row committed after the instant,
  and refusing a write

Two things it does not cover, both because they need the replication
sub-protocol rather than SQL:

- **`StartReplication` over a replication connection.** The suite
  implements `pg.ReplicationStream` over
  `pg_logical_slot_peek_changes` and `pg_replication_slot_advance`,
  which is the same decoding through a different door. The
  [`LogicalStreamer`](#streaming) adapter a driver supplies is
  yours to test.
- **Snapshot *export*.** `pg_create_logical_replication_slot` cannot
  do it, so the suite uses `pg_export_snapshot()` to produce a
  snapshot of the same kind and holds `WithSnapshot`'s statements to
  the property that matters.

Set the server up with `wal_level = logical` or those tests skip —
`docker-compose.yml` and `scripts/local-servers.sh` both do.
