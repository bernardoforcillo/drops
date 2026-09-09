# Caching, and knowing when to stop

A read-through cache on an entity is four lines:

```go
var Docs = pg.NewAutoEntity[Doc]("docs").
    WithCache(redis, 5*time.Minute)
```

`Get` serves from the cache and dedupes concurrent misses through a
single-flight group, so a thundering herd of "give me user 42"
resolves to one query. `Update`, `Save` and `Delete` drop the entry.
That part is easy, because a primary-key entry has exactly one thing
that can invalidate it.

Query results are the hard part, and until recently drops was honest
about giving up on them:

> Query entries rely on TTL alone — invalidation across an arbitrary
> WHERE/JOIN topology is intractable in the general case, and TTL is
> usually the right trade for read-heavy services.

That is true of the general case. It is not true of the cases services
actually cache.

## Topics

A lookup by a foreign key, a list scoped to a tenant, a row by a
unique column: each has a shape narrow enough to say what a change
would have to touch to affect it. A **topic** is that shape.

A query declares the topics it read. A write announces the topics it
touched. An overlap evicts. The idea is
[InstantDB's](https://github.com/instantdb/instant), whose reactive
queries are kept correct by exactly this matching; it transfers to a
relational cache without the triple store underneath it.

```go
idx := pg.NewTopicIndex(redis, 24*time.Hour)

var Orders = pg.NewAutoEntity[Order]("orders").
    WithCache(redis, 5*time.Minute).
    WithTopics(idx, "customer_id", "tenant_id")
```

```go
rows, err := Orders.Query(db).
    Where(pg.Eq(OrderCustomer, custID)).
    DependsOn(
        pg.UnscopedTopic("orders"),
        pg.ValueTopic("orders", "customer_id", custID),
    ).
    All(ctx)
```

Now a change to customer 9's orders leaves customer 7's cached list
alone, and a change to customer 7's drops it immediately.

## The three shapes, and the one rule

There are three kinds of topic, and the difference between the first
two is the whole design.

| topic | touched by | declared by |
|---|---|---|
| `TableTopic("orders")` | **every** change to the table | queries with no filter |
| `UnscopedTopic("orders")` | only changes that cannot name their values | queries **with** a filter |
| `ValueTopic("orders", "customer_id", 7)` | changes where that column had or now has that value | queries filtered on it |

The rule, in one sentence: **a query must depend on a topic that every
change able to alter its result will touch.**

An unfiltered query — a `COUNT`, a full list, anything a row *anywhere*
in the table can change — declares `TableTopic`. A filtered query
declares `UnscopedTopic` *instead of* `TableTopic`, plus its
`ValueTopic`s. That substitution is what keeps an ordinary row change
from evicting every query on the table.

Getting it wrong has two very different outcomes, and it is worth
naming both:

- A filtered query that declares `TableTopic` is **correct but
  pointless** — it is evicted by everything, exactly as before.
- An unfiltered query that declares `UnscopedTopic` is **silently
  wrong** — a row change will not evict it and it will serve a stale
  count.

When in doubt, `TableTopic` is always correct and never precise.

## How it works, and what that costs

There is no reverse index from topic to cache key. Maintaining one
over a plain `Get`/`Set`/`Delete` backend cannot be done without
races, and the cache interface is deliberately that small.

Instead every topic has a **generation** — an opaque random value in
the same cache — and a query's key embeds the generations of the
topics it declared. Invalidating a topic deletes its generation; the
next reader mints a fresh one; every key that embedded the old one
becomes unreachable.

The consequences, stated plainly:

- **Immediate for correctness.** A stale entry is never served after
  its topic is invalidated, because nothing computes its key any more.
- **Not immediate for memory.** The unreachable entries occupy the
  backend until their own TTL expires. Query entries need a TTL for
  that reason, and this is the one real cost of the scheme.
- **One extra round trip per query**, batched into a single `GetMulti`
  when the backend implements `cache.MultiCache` — which
  `cache/redis`, `cache/memcached` and `cache/memory` all do.
- **Generations must outlive query entries.** A generation that
  expires first only causes misses, never wrong answers, but it causes
  them for every query on the table at once. Give the index a long TTL,
  or zero for none.

If the backend cannot answer, the query is **not** served from cache
and falls through to the database. Serving from a key whose freshness
cannot be established is the one outcome worse than not caching.

## Why the topics are declared, not derived

drops could not read them off the query even if it wanted to. A
`drops.Expression` is a closure that writes SQL, not a tree to walk, so
between `Where` and the statement there is nothing to inspect.

The failure modes settle it anyway. A derivation that quietly missed a
predicate would produce a cache that serves stale rows and reports
nothing. A declaration that is too broad produces extra misses —
visible in a hit rate, harmless in a result.

## Writes are precise in one direction only

Of the entity write paths, only `Create` can name exactly what it
touched:

```go
// Create → InvalidateRow(nil, after) — precise.
// Update / Save / Delete / Patch → InvalidateTable — the whole table.
```

The reason is the same for each of the four: **drops does not read a
row before writing it**, so it knows what the row became and not what
it was. An update that moves an order from customer 7 to customer 9
changes the answer to both "orders of 7" and "orders of 9", and an
invalidation built from the new value alone would leave customer 7's
cached list holding a row that has left it. Reading the old row first
would make every write two round trips to buy precision most callers
do not need.

So those writes are correct and blunt. Two ways to get the precision
back where it matters.

**Call it yourself, with the before-image you already have:**

```go
err := idx.InvalidateRow(ctx, "orders", before, after, "customer_id")
```

`InvalidateRow` touches the value topic on *both* sides of the change,
which is the part that is easy to get wrong by hand.

**Or drive invalidation from the change stream instead.** A
[logical replication](cdc.md) consumer is handed both images by the
server, so it can be exact without any extra read:

```go
err := pg.Reassemble(ctx, stream, func(ctx context.Context, tx pg.Transaction) error {
    for _, ch := range tx.Changes {
        if err := idx.InvalidateRow(ctx, ch.Table, ch.Old, ch.New, "customer_id"); err != nil {
            return err
        }
    }
    return nil
})
```

This is how InstantDB does it, and the reason its invalidation is
precise where a write-path hook cannot be. It needs
[`REPLICA IDENTITY FULL`](cdc.md#declaring-what-to-replicate) on the
tables whose old values you are scoping by — the default identity
carries the key and nothing else, so `ch.Old` would have no
`customer_id` in it.

A column absent from a row map is **skipped rather than treated as
NULL**: a change feed that reports only the columns it has must not be
read as asserting the others were empty.

## Anything that bypasses the entity layer owes the cache a call

A raw `db.Exec`, a bulk `UPDATE`, a migration, a `TRUNCATE` — none of
them announce anything:

```go
_, err := db.Exec(ctx, `UPDATE orders SET status = 'archived' WHERE created_at < $1`, cutoff)
_ = idx.InvalidateTable(ctx, "orders")   // owed
```

`InvalidateTable` touches `TableTopic` and `UnscopedTopic` together,
which between them reach every query on the table however it was
scoped. It is the call to make when in doubt.

## When not to use this

- **A cache with a short TTL and a cheap query.** The extra round trip
  per read may cost more than the misses it prevents.
- **A query whose topics you cannot state confidently.** A wrong
  declaration in the unsafe direction serves stale data. Leave
  `DependsOn` off and keep the TTL: that is the behaviour drops had
  before, and it is still a defensible one.
- **Aggregates over a whole table under heavy write load.** They
  declare `TableTopic`, so they are evicted by every row change, and
  the cache never warms. A materialised view refreshed on a schedule
  is the better tool.
