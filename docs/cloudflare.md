# Running drops on Cloudflare

Four Cloudflare products, four different relationships to drops. The
first thing to get straight is which is which, because only one of
them is a database drops speaks to.

| | package | what it is to drops |
|---|---|---|
| D1 | `cloudflare/d1` | a `drops.Driver`. The SQLite dialect runs on it unchanged. |
| Vectorize | `cloudflare/vectorize` | a `vector.Store`, like `qdrant`. |
| Workers KV | `cache/cloudflarekv` | a `cache.Cache`, like `cache/redis`. |
| Hyperdrive | `cloudflare/hyperdrive` | not a backend at all — a pooler in front of *your* PostgreSQL, and a list of what stops working behind it. |

All four share one API client, `cloudflare`, which holds the token,
the account, the envelope decoding and the retry policy.

```go
cf, err := cloudflare.New(accountID, cloudflare.WithAPIToken(token))
```

Scope the token to what the backend needs — D1:Edit, Vectorize:Edit,
Workers KV Storage:Edit — rather than reusing one across all of them.
The legacy global API key is deliberately not supported: it
authenticates as the whole account with no way to narrow it, so a
leaked one is a leaked account.

---

## D1

D1 is SQLite, so the whole of `drops/sqlite` applies: schema
declarations, query builders, entities, relations, migrations, the
tenant axis. This package supplies the missing half.

```go
drv := d1.New(cf, databaseID)
db := sqlite.New(drv)
```

### Two transports, because there are two places your code runs

`d1.New` goes to Cloudflare's public REST API over the internet,
authenticated by an API token. That is the transport for a migration
runner, a CI job, a laptop — anything outside Cloudflare's network,
where a service binding does not exist.

`d1.NewBridge` goes to a Worker of yours that holds the D1 binding:

```go
drv, err := d1.NewBridge("http://d1.internal")
```

That is the transport for a Container or a sidecar. It is faster and
cheaper — the request never leaves Cloudflare's network — and it needs
no API token, because the binding is the authorisation.

The Worker side is `cloudflare/d1/worker/handler.js`, a
dependency-free ES module you **mount** inside your own Worker rather
than deploy as-is:

```js
import { handleD1Request } from "./drops-d1-handler.js";

export default {
  async fetch(request, env) {
    if (new URL(request.url).pathname !== "/d1") return new Response("not found", { status: 404 });
    if (!authorized(request, env)) return new Response("forbidden", { status: 403 });
    return handleD1Request(request, env.DB);
  },
};
```

Your authentication, routing and logging stay yours. The only thing
that comes from drops is the wire format — which is the part whose
drift returns *wrong rows* rather than a build failure, and therefore
the part that should not be written twice.

`cloudflare/d1/protocol.go` is the normative description of every
field. `cloudflare/d1/worker/fixtures.json` is the conformance suite:
the Go side runs it in `conformance_test.go`, the Worker side runs it
in whatever test runner your Worker already has, and a change to one
half that the other has not made fails there rather than in
production.

### Transactions

Read this before shipping anything.

D1's HTTP API has no interactive transactions. There is no `BEGIN` to
send: the API is one request, one unit of work, and consecutive
requests are not guaranteed to reach the same connection. What D1
offers instead is the batch — several statements in one request,
which D1 documents as running in an implicit transaction.

`Driver.Begin` is built on that, and the shape it can honestly offer
is deferred:

- **Exec** inside the transaction is buffered, not sent. The
  `drops.Result` it returns is a promise: `ErrPending` until the
  commit fills it in, real afterwards.
- **Query** inside the transaction returns `ErrTxQuery`. The rows do
  not exist yet, and running the SELECT outside the buffer would
  silently break read-your-writes for every caller that reads back
  what it just wrote.
- **Commit** sends the buffer as one request. **Rollback** discards
  it, which costs nothing because nothing was sent.

So `InTx` works for write-only units of work — the common one, an
insert plus the rows that hang off it — and refuses the
read-modify-write ones rather than getting them subtly wrong.

Three shapes of `InTx` need changing when a codebase moves here:

**Reading back a generated id.** Generate the identifier in Go (a
UUID) so the write does not need to report it, or look the row up by
its natural key after the commit.

**Read, decide, write.** Put the guard in the statement. A conditional
`UPDATE … WHERE version = ?` tells you it lost by reporting zero rows
affected, which is the same answer a transaction would have given and
needs no session to hold it.

**Feeding one statement's row count into the next.** Rewrite it as one
statement — a CTE, or a subquery in the `WHERE`.

`d1.Batch` is the same mechanism without the `drops.Tx` clothing, and
the clearer spelling when the unit of work is already a list of
statements.

### The limits that change how you write queries

`d1.Published` carries them as values so a test can assert against a
number rather than read one in a comment.

**100 bound parameters per statement.** This is the one met by
accident. `WHERE id IN (?, ?, …)` over a slice reaches it at a hundred
ids, and a generated `VALUES` list reaches it sooner. The driver
refuses locally, without a round trip, and the error names the way
through:

```go
encoded, _ := json.Marshal(ids)          // 1,500 ids, one parameter
rows, err := db.Query(ctx,
    "SELECT id, title FROM tenders WHERE id IN (SELECT value FROM json_each(?))",
    string(encoded))
```

`d1.WithMaxBoundParams` changes or disables the check, because the
limit is Cloudflare's to raise.

**1 MB per query response.** A D1 result set is not streamed — it
arrives whole, inside an HTTP response — so a SELECT over a large
table fails at the service rather than arriving a page at a time. Page
with `LIMIT`, or with `drops/sqlite`'s keyset cursors.

**100 KB per statement, 30 s per query, 2 MB per row, 10 GB per
database.** The last one is what decides database-per-tenant against
table-per-tenant.

And the absent features, which are not limits a bigger plan lifts: no
`ATTACH` (so no cross-database query), no `PRAGMA` (foreign keys are
always enforced, and deferred inside a batch), no extensions (no FTS5,
no R*Tree), no user-defined functions. D1 also has no schemas, so
`tenders.documents` becomes `tenders_documents`.

### Values on the wire

D1 answers in JSON, which is lossier than SQLite's own protocol. The
driver handles all four of the ways that bites:

- **Integers past 2^53.** The response is decoded with `UseNumber`, so
  a rowid keeps its low bits instead of being rounded through a
  `float64`.
- **Booleans.** SQLite has no boolean type; a `BOOLEAN` column holds 1
  and 0, and scans into a `*bool` correctly.
- **Times.** `RFC 3339`, SQLite's own `datetime()` text, a bare date
  and Unix seconds all scan into a `time.Time`.
- **JSON columns.** They arrive as text, which is what SQLite stores.
  Scan into a `*string` or a `*json.RawMessage`.
- **Column order.** The driver uses D1's `/raw` endpoint, not
  `/query`. `/query` returns rows as JSON objects, and an object has
  no order: a positional `Scan(&id, &name)` would have to guess which
  key a destination meant, and a join projecting two columns of the
  same name would lose one.

### Errors

D1 passes SQLite's own message through, so a violated constraint
arrives as `UNIQUE constraint failed: users.email` inside a Cloudflare
envelope. The driver turns that back into a sentinel:

```go
if errors.Is(err, d1.ErrUniqueViolation) {
    _, columns := d1.ConstraintColumns(err)   // ["email"]
}
```

A statement issued through `sqlite.DB` is classified twice — once by
the driver, once by the dialect, which reads the same message out of
the error string — so `errors.Is(err, sqlite.ErrUniqueViolation)` and
`drops.AsFieldError` both answer on that path. The entity layer never
learns which driver it was talking to.

Retries follow the same split: a read may be repeated, a write may
not, because the client cannot tell a request that never arrived from
a reply that never came back. A statement SQLite refused is never
retried at all — it will fail identically next time.

---

## Vectorize

```go
store := vectorize.New(cf, "product-embeddings", vectorize.WithMetric(vector.Cosine))
res, err := store.Search(ctx, q)   // the same q that runs against pgvector
```

Vectorize is a smaller query language than the other vector stores
drops speaks to, and the gaps are not ones an adapter can paper over.
Each fails loudly rather than quietly returning the wrong page.

**The filter language is conjunctive.** `$eq`, `$ne`, `$in`, `$nin`,
`$lt`, `$lte`, `$gt`, `$gte`, combined by listing them, which means
AND. There is no `$or` and no `$not`. So `vector.Or` and most
`vector.Not` compile to `ErrUnsupportedOp`, as do `MatchText`,
`IsNull`, `HasID` and `GeoWithin`. A negated leaf with a direct
opposite is rewritten into it — `Not(Eq)` is `$ne`, `Not(In)` is
`$nin`, the ranges invert — because that is a rewrite rather than an
approximation. Negating a conjunction is not: De Morgan turns it into
a disjunction, and there is no disjunction. Move the disjunction into
the metadata at write time and filter on it with `Eq`.

**A filter needs a metadata index.** Filtering on a property with no
metadata index is not an error at Vectorize — it simply matches
nothing, which is the worst way for this to go wrong. Declare the
indexes with `CreateMetadataIndex` *before* the first write; an index
only covers vectors written after it existed. `ListMetadataIndexes` is
the first thing to check when a filter returns nothing.

Whether a filter over an **array-valued** metadata property matches
element-wise is not something this package can promise. Verify it
against your own index before relying on it; if it does not, denormalise
the array into one property per value you filter on.

**There is no pagination.** The query endpoint takes `topK` and
nothing else. The `vector.Cursor` contract is honoured by
over-fetching and slicing client-side, which works exactly as far as
the `topK` ceiling and then returns `ErrPageBeyondTopK` rather than a
page that silently repeats rows. At the ceiling `HasMore` goes false,
which is the honest reading: no further page exists through this API
however many vectors are in the index.

The ceilings themselves are Cloudflare's to change and differ by plan,
so `WithTopKCeilings` overrides them without waiting for a release
here.

**There is no delete-by-filter.** `DeleteWhere` composes one out of a
query and a delete-by-ID, and refuses with
`ErrDeleteByFilterUnbounded` when the matching set may be larger than
one query can see — rather than deleting an arbitrary prefix and
reporting success. It is a convenience for a bounded delete, not a
substitute for the missing operation. The shape that scales is to
derive the IDs from what you would have filtered on:

```go
func chunkID(docID string, n int) string { return fmt.Sprintf("%s:%d", docID, n) }
```

so the set can be regenerated without asking the index what is in it.
Adopt it before the collection grows past the ceiling, not after.

**Writes are asynchronous.** An upsert returns a mutation id, and a
query issued immediately afterwards may not see it.

---

## Workers KV

```go
c, err := cloudflarekv.New(cf, namespaceID, cloudflarekv.WithKeyPrefix("embeddings:"))
```

KV is a read-optimised, eventually consistent store, and two of its
properties decide what it is good for.

**A write takes up to sixty seconds to be visible everywhere.** Not
"usually fast, occasionally slow" — the propagation is the design. So
KV is right for something expensive to compute and safe to serve
slightly stale: an embedding, a rendered page, a compiled
configuration. It is wrong for anything invalidated by a write that a
subsequent read must not miss, which includes the topic invalidation
in `drops/cache` and any query cache over a table being written. Use
the in-process cache or Redis for those.

**A TTL cannot be shorter than sixty seconds.** A cache asked for five
seconds and given sixty is not a slow cache, it is a wrong one, so
`Set` returns `ErrTTLTooShort` rather than rounding. `WithRoundUp`
makes the opposite choice explicitly, for callers who have decided the
staleness is acceptable.

`TTL` and `Exists` answer from a marker this package writes into each
key's KV metadata — the absolute expiry, recorded at `Set`. That is
why they can answer at all, since the value endpoint reports no
expiry, and why a key written by something else reads back as `-1`,
"no expiry", rather than as a wrong number.

`GetMulti` uses KV's bulk read, which is a real batch rather than a
loop — that is why this backend satisfies `cache.MultiCache` at all.
It has one wrinkle: the bulk endpoint answers in JSON, so a value that
is not valid UTF-8 (msgpack, protobuf, a compressed blob) loses the
offending bytes on the way. Those keys are detected and re-read
byte-exact through the single-value endpoint, so the common case stays
one round trip and the binary case is correct rather than fast.

---

## Hyperdrive

Hyperdrive is not a database. It is a connection pooler and query
cache in front of PostgreSQL or MySQL that you already run, so the
dialect stays `drops/pg` or `drops/mysql` and the driver stays
whatever it was. Nothing in this package talks to Cloudflare.

It does the two things that go wrong when a schema written for a
direct connection is pointed at a pooler.

`Config.DSN` builds the connection string from a binding's parts —
percent-encoding the password, which matters more than it sounds: a
generated password containing a `/` or an `@` produces a URL that
parses as a different host, and the failure that follows names neither
the password nor the escaping.

And `Check` is the part nobody thinks of until production. A pooler
does not hold a session, and a great deal of PostgreSQL is session
state. drops uses several of those, and the features that depend on
them do not degrade behind Hyperdrive — they silently do the wrong
thing:

| feature | symbols | what the pooler does |
|---|---|---|
| LISTEN/NOTIFY | `pg.Listen`, `pg.Notify` | returns the connection between statements, so the subscription is never delivered to |
| logical replication | `pg.Stream`, `pg.CreateSlot`, `mirror.LogicalSource` | cannot carry the replication protocol, which is a startup packet rather than a query |
| session advisory locks | `pg.WithAdvisoryLock` | releases the lock when the connection returns to the pool |
| statement registry | `pg.StatementRegistry` | cancels the backend PID the registry recorded, which is not the one running the statement |
| session state | `SET`, `CREATE TEMP TABLE`, `PREPARE` | none of it survives the connection going back to the pool |
| COPY | `pg.Copier` | a protocol mode rather than a statement |

```go
if err := hyperdrive.Check(
    hyperdrive.FeatureListenNotify,
    hyperdrive.FeatureLogicalReplication,
); err != nil {
    log.Fatal(err)   // at boot, not at the first NOTIFY nobody receives
}
```

`hyperdrive.Report()` prints the whole table, marking the ones that
fail silently — the dangerous ones, since a feature that errors is
found in testing.

`hyperdrive.Works()` is the other half, because a list of what breaks
invites the assumption that everything else is suspect. It is most of
drops: every query builder, every entity operation, every DDL
statement, the file-based migrations, and — because a transaction pins
a connection for its lifetime even behind a pooler — the outbox, the
job queue, the saga runner, and transaction-scoped advisory locks.

Two things stay outside drops. Logical replication wants a direct
connection to the origin, past Hyperdrive; it is one long-lived
connection, which is what a pooler is least useful for anyway. And
Cloudflare's rate-limiting binding only offers 10- and 60-second
windows, so a limiter with 5-minute or hourly rules is not a drops
concern and not a Hyperdrive one — it needs its own store.
