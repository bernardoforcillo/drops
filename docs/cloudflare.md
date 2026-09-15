# Running drops on Cloudflare

Six Cloudflare products, six different relationships to drops. The
first thing to get straight is which is which, because only one of
them is a database drops speaks to.

| | package | what it is to drops |
|---|---|---|
| D1 | `cloudflare/d1` | a `drops.Driver`. The SQLite dialect runs on it unchanged. |
| Vectorize | `cloudflare/vectorize` | a `vector.Store`, like `qdrant`. |
| Workers KV | `cache/cloudflarekv` | a `cache.Cache`, like `cache/redis`. |
| R2 | `cloudflare/r2` | not a database — object storage, for the operations that produce a file: a D1 export, a schema dump. |
| Queues | `cloudflare/queues` | not a database either — the durable hop an outbox publishes to, and a `mirror.Sink`. |
| Hyperdrive | `cloudflare/hyperdrive` | not a backend at all — a pooler in front of *your* PostgreSQL, and a list of what stops working behind it. |

All six share one API client, `cloudflare`, which holds the token,
the account, the envelope decoding and the retry policy.

```go
cf, err := cloudflare.New(accountID, cloudflare.WithAPIToken(token))
```

Scope the token to what the backend needs — D1:Edit, Vectorize:Edit,
Workers KV Storage:Edit, Workers R2 Storage:Edit, Queues:Edit,
Hyperdrive:Edit — rather than reusing one across all of them. The
legacy global API key is deliberately not supported: it authenticates
as the whole account with no way to narrow it, so a leaked one is a
leaked account.

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

### Read replication, and the session that makes it safe

D1 can place read replicas around the world. Turning that on is one
call:

```go
admin := d1.NewAdmin(cf)
_, err := admin.SetReadReplication(ctx, databaseID, d1.ReplicationAuto)
```

Turning it on without reading the rest of this section is how a
read-your-writes bug gets deployed. A replica is allowed to be behind
the primary. So two consecutive reads may be served by two instances
and the second may see *less* than the first, and a read after a write
may not see the write at all. Nothing errors; the data is just old.

D1's answer is the bookmark, and drops' is `d1.Session`:

```go
sess, err := drv.Session(d1.FirstUnconstrained)
db := sqlite.New(sess)          // a Session is a drops.Driver
```

Every request in a session returns a bookmark, the next request
carries it, and D1 refuses to serve that request from an instance
that has not caught up to it. The reads stay local and cheap, and
they stop going backwards.

Scope one to the unit the consistency is wanted over — an HTTP
request, a job, a page render. They cost nothing: there is no
connection and nothing to close, so one per request is the intended
use rather than an extravagance.

The constraint applies to the first request only:

| | |
|---|---|
| `d1.FirstUnconstrained` | the first read may go anywhere. The fast default, and right for a session that only reads. |
| `d1.FirstPrimary` | the first read goes to the primary, so it sees every write committed before it. For a session that starts by reading something it is about to decide on. |

To carry the guarantee past the end of one session, hand the bookmark
on — a cookie, a header, a queue message:

```go
bookmark := sess.Bookmark()             // at the end of the write request
sess, err := drv.Resume(bookmark)       // at the start of the next one
```

`Resume("")` is an error rather than an unconstrained session: the
caller asked for a guarantee, and one that quietly is not provided is
worse than one that says so.

Two things to know before designing around it.

**Sessions need the bridge.** Cloudflare offers the Sessions API
through the Worker binding only, not over the REST API. `d1.New`
therefore answers `d1.ErrSessionsUnsupported` from `Session` and
`Resume` — at construction, not at the first query, so a deployment
pointed at the wrong transport by a changed environment variable fails
at boot rather than serving a stale read months later. Use
`d1.NewBridge`.

**A session is not a transaction.** It gives sequential consistency,
which is a different thing: everything above about `Driver.Begin`
still applies inside one.

`Meta.ServedByPrimary` on a result says which instance answered, which
is the flag to check when a read-your-writes bug is suspected.

### Managing the databases themselves

`d1.Admin` is the other half of `drops/cloudflare/d1` — the database
as a resource rather than as a thing to run statements against.

```go
admin := d1.NewAdmin(cf)
db, err := admin.Create(ctx, d1.CreateOptions{
    Name:            "tenant-42",
    PrimaryLocation: d1.LocationWesternEurope,
})
drv := admin.Driver(db.UUID)
```

It is a separate type, needing a `cloudflare.Client`, because the two
halves are separately authorised and separately reachable: creating a
database is an account-level REST operation, while running a statement
can go through a Worker binding with no account credential at all. A
service that only queries should hold a `Driver` and no `Admin`.

**Database per tenant.** D1's 10 GB ceiling is what makes this a real
design rather than an eccentric one, and provisioning was the half
drops could not supply before. Names are unique within an account, so
`admin.FindByName(ctx, tenant)` is the lookup that means the UUID
never has to be stored anywhere.

**Time Travel** is D1's point-in-time restore, and it is worth taking
a bookmark before a migration:

```go
before, _ := admin.Bookmark(ctx, databaseID)
// … run the migration …
res, err := admin.Restore(ctx, databaseID, d1.RestoreOptions{Bookmark: before})
```

`admin.BookmarkAt(ctx, id, t)` answers for a moment nobody thought to
mark at the time — bookmarks are derivable from a timestamp. A restore
is in place and destructive: everything written after the restore
point is gone, and `res.PreviousBookmark` is the only way back, so
store it before doing anything else with the result. `Restore` refuses
a bookmark and a timestamp together rather than picking one, because
the half ignored decides which data survives.

Time Travel reaches back thirty days on a paid plan, seven on the free
one, and it dies with the database. It is not a backup.

**Export and import** are. Both are jobs D1 polls, and both are one
call here:

```go
exp, err := admin.ExportTo(ctx, databaseID, w, d1.ExportOptions{})
res, err := admin.ImportFile(ctx, databaseID, "dump.sql")
```

`ExportTo` writes the SQL to any `io.Writer` — the signed URL D1 hands
back is good for about an hour, which is exactly long enough to be
stale by the time a two-step API's caller uses it. `Import` hides the
three round trips D1 requires (hash, presigned upload to R2, ingest)
and `ImportFile` hashes by streaming, so a dump larger than memory
never enters it. `exp.Bookmark` names the state the dump captured, and
`res.FinalBookmark` names the state directly after the import
succeeded.

[R2](#r2) is where the dump goes.

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

The bridge transport has one wrinkle the REST one does not, and it is
worth knowing before a join goes through it. D1's Worker API exposes
the projected column names exactly through `raw()` and the statement's
metadata through `all()`, and there is no call that returns both. The
handler takes `all()`, because losing the metadata is the silent and
severe half: `changes()` is what `RowsAffected()` answers, so the
conditional `UPDATE … WHERE version = ?` that stands in for the
transaction D1 does not have would report that it lost the race every
single time. The cost is that a projection with two columns of the
same name loses the second — which fails loudly, a positional `Scan`
coming up a column short, and is fixed by aliasing:
`SELECT a.id AS a_id, b.id AS b_id`. The REST transport returns both
and needs neither the compromise nor the caveat.

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

## R2

R2 is object storage, and drops runs no queries against it. What it is
to a library like this one is where the artefacts go: a D1 export that
has to outlive Time Travel's thirty days, a schema dump kept beside a
migration, a large document whose row holds only the key to it.

```go
backups := r2.New(cf).Bucket("backups")
_, err := backups.PutFile(ctx, "d1/2026-09-15/tenants.sql", path, r2.PutOptions{
    ContentType:  "application/sql",
    StorageClass: r2.InfrequentAccess,
})
```

### Which R2 API this is, and what it costs

R2 has three front doors and this package uses the least famous of
them: Cloudflare's own REST API, at
`/accounts/{account}/r2/buckets/…`. That is a deliberate choice with a
real cost, so both halves are worth stating.

What it buys is that R2 becomes one more thing the account's API token
reaches. No SigV4 signing, no second set of credentials to mint and
rotate, no second SDK — the same `cloudflare.Client` carries it, with
the same retry policy and the same hook, so an R2 write appears in a
log next to the D1 statement that produced it.

What it costs is the ceiling. This endpoint takes objects up to
**300 MB** and has **no multipart upload**, so there is no resuming a
failed one and no going above the limit by splitting it.
`r2.MaxObjectSize` is the number, `r2.ErrObjectTooLarge` is what
`PutFile` returns *before* it starts sending, and R2's S3-compatible
API — with an S3 SDK and R2 access keys rather than an API token — is
the answer for anything bigger. A database dump reaches 300 MB long
before a 10 GB D1 database does, so check rather than assume.

### The backup that D1 actually needs

Time Travel reaches back thirty days and dies with the database. This
is the archive that does not:

```go
f, _ := os.Create(path)
exp, err := admin.ExportTo(ctx, databaseID, f, d1.ExportOptions{})
f.Close()

key := fmt.Sprintf("d1/%s/%s.sql", time.Now().UTC().Format("2006-01-02"), exp.Bookmark)
_, err = backups.PutFile(ctx, key, path, r2.PutOptions{StorageClass: r2.InfrequentAccess})
```

Through a file rather than in memory, because `PutFile` streams and
takes the length from the file — which is also what makes the size
check possible before a byte is sent. `InfrequentAccess` is the class
for this: cheap to keep, charged to read, and a backup is read
approximately never.

### Keys are paths

An object key may contain slashes and they are not escaped away, so
`"d1/2026-09-15/tenants.sql"` addresses that key and `List` with a
prefix and a delimiter walks it as though it were a directory.

Everything else in a key *is* escaped, including the two segments
`url.PathEscape` leaves alone. `.` and `..` are legal path characters,
so a key segment of `..` would travel as a dot segment, and anything
between the process and R2 is entitled by RFC 3986 to resolve it away
— a key of `../../secrets` would then address a different URL than the
one it names. They are percent-encoded, which still names the same key
because a percent-encoded dot is not a dot segment. A key built from a
tenant's name cannot climb out of the prefix it was put under.

### Consistency

R2 is strongly consistent for reads after a write of an object, which
is what makes it usable as the durable half of a pipeline — unlike
Workers KV, whose sixty-second propagation is the reason that package
is a cache and this one is not.

---

## Queues

drops already has an outbox (`pg.Outbox`): the pattern where a change
and the intent to publish it are written in one transaction, so the
two cannot disagree. What the outbox needs on the other side is
somewhere durable to publish *to*, and on Cloudflare that is a Queue.

```go
q := queues.New(cf).Queue(queueID)
err := q.Publish(ctx, queues.JSON(event))
```

### Only the pull consumer is reachable from Go

A Queue is consumed either by a Worker — Cloudflare pushes batches
into it — or by a pull consumer, which asks over HTTP. Only the second
works from a Go process outside Cloudflare, so it is the one this
package implements. The queue has to be configured for it first: a
pull consumer is a property of the queue, set with wrangler or in the
dashboard, not something a client turns on per request. A queue
without one answers `Pull` with an empty batch, which is
indistinguishable from an empty queue —
`QueueInfo.ConsumersTotalCount` is where that shows.

### Leases, not deletes

A pulled message is not removed, it is leased: it carries a lease ID
and stays invisible to other consumers for the visibility timeout,
then comes back if nobody acknowledged it. A consumer that crashes
mid-batch loses nothing, and one that succeeds must say so.

```go
batch, err := q.Pull(ctx, queues.PullOptions{BatchSize: 50, VisibilityTimeout: 2 * time.Minute})
for _, m := range batch.Messages {
    if err := handle(m); err != nil {
        _ = batch.Retry(m, queues.DefaultBackoff(m.Attempts))
        continue
    }
    _ = batch.Ack(m)
}
settled, err := batch.Settle(ctx)
```

Marking first and settling once is what turns a hundred decisions into
one request. `settled.Warnings` carries the expired leases, which is
how a visibility timeout that is too short for the work announces
itself.

`q.Consume` is that loop written once, for the case where the
handler's error is the only decision. Returning `queues.ErrStop` from
a handler acknowledges the message and ends the loop, which is the
clean way out on a shutdown signal — cancelling the context abandons
the batch mid-flight instead.

At-least-once is the guarantee, so a handler *will* eventually see the
same message twice. Making it idempotent is not optional.

### As a mirror sink

`mirror.QueuesSink` puts the change stream on a Queue, which is the
mirror whose far end is not a store: a Worker that invalidates a
cache, a job that re-indexes a document, a webhook to a system drops
knows nothing about.

```go
sink, err := mirror.NewQueuesSink(q)
pump := mirror.NewPump(source, sink)
```

It is deliberately **not** a `VersionAwareSink`, and the reason is
structural rather than an omission. Version-awareness means a store
can be asked to ignore a write older than what it already holds;
ClickHouse can, because ReplacingMergeTree does the comparison in the
engine. A queue holds nothing to compare against. So a fill-mode
reseed refuses this sink, and the consumer has to deduplicate for
itself — which is what `Change.Key` and `Change.Version` travel in
every message for. A consumer that keeps the highest version it has
seen per key is both deduplicated and protected against a redelivery
arriving late.

`WithQueuesEncoder` is where a row too wide for a message becomes a
reference, and where columns that have no business leaving the
database are dropped. Returning `nil` from an encoder drops the change
entirely.

### The limits that change a design

`queues.Published` carries them as values. Two of them matter here:

**128 KB per message.** A change event holding a row with a large text
column will not fit. The shape that does is the key plus enough to act
on it, with the consumer reading the row back. The check is local, and
the error says so.

**100 messages per request,** for both a batch publish and a pull, and
256 KB per batch publish. `QueuesSink` chunks at the first and falls
back to publishing one at a time when it meets the second — the byte
ceiling cannot be predicted from the change count, and a sink that
gave up there would stop the mirror on a wide table rather than on a
row.

---

## Hyperdrive

Hyperdrive is not a database. It is a connection pooler and query
cache in front of PostgreSQL or MySQL that you already run, so the
dialect stays `drops/pg` or `drops/mysql` and the driver stays
whatever it was.

It does three things: it builds the connection string, it creates the
configuration, and it tells you what stops working behind a pooler.

### Creating the configuration

The Hyperdrive in front of a database has to exist before a Worker can
bind it, and creating it from the code that owns the database beats
creating it by hand and writing the ID down somewhere:

```go
hc := hyperdrive.NewClient(cf)
cfg, err := hc.Create(ctx, hyperdrive.NewConfig{
    Name:     "app-primary",
    Origin:   hyperdrive.Origin{Engine: hyperdrive.PostgreSQL, Host: host, Database: "app", User: "hyperdrive"},
    Password: os.Getenv("ORIGIN_PASSWORD"),
    Caching:  hyperdrive.Caching{MaxAge: 30, StaleWhileRevalidate: 5},
})
// bind cfg.ID in wrangler.toml
```

`Origin` carries no password, and that is deliberate: Cloudflare
stores the credential and never returns it, so keeping it off the type
that round-trips is what stops a read-modify-write from quietly
blanking it. It is a separate argument on the two calls that can set
one — `Create` and `Replace` — and `Replace` needs it again for the
same reason. `SetCaching` is the one edit that does not, because it
touches no origin at all.

Creating a configuration does not make any of the features below work.
`Check` is still the call that belongs at boot, and
`Configuration.DirectConfig` builds the connection that goes *past*
Hyperdrive to the origin, which is what logical replication and the
rest of the list need.

### The connection string, and what breaks behind the pooler

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
