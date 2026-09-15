# Running it

The parts of `drops/pg` that exist because something went wrong in
production once. Errors that mean something other than "the query was
bad", the migration failure that arrives an hour after the migration,
draining a node for failover, jobs that outlive the process, and
reading a result set that will not fit in memory.

## Errors that change what you do next

`pg/errors.go` classifies the eight SQLSTATEs an application handles by
hand — the constraint violations, the two rollbacks, the two
"undefined" ones. Those tell you what to say to a user.

The rest of the table is the codes an *operator* handles, and they do
not tell you to show a nicer message. They tell you to do something
else entirely:

| code | sentinel | what it means you should do |
|---|---|---|
| `25006` | `ErrReadOnlyTransaction` | you reached a standby or a demoted primary — **route elsewhere**, retrying is forever |
| `40003` | `ErrCompletionUnknown` | the write **may have committed**. Not retryable |
| `0A000` | `ErrFeatureNotSupported` | usually literal; also the cached-plan failure below |
| `57014` | `ErrQueryCanceled` | a timeout fired, or somebody cancelled you |
| `57P01` | `ErrAdminShutdown` | reconnect; do not retry on this connection |
| `53300` | `ErrTooManyConnections` | shed load. Retrying makes it worse |
| `53100` | `ErrDiskFull` | often the downstream symptom of [an abandoned replication slot](cdc.md#the-slot-is-the-dangerous-part) |
| `55P03` | `ErrLockNotAvailable` | a `NOWAIT` **answer**, not a failure. Move to the next row |
| `25P02` | `ErrInFailedTransaction` | this is not the error. The first one in the transaction was |
| `25P03` | `ErrIdleInTransactionTimeout` | a transaction was held open across an HTTP call |

`Condition` and `ClassName` give a code its canonical PostgreSQL name,
which is the difference between an alert that starts a search and one
that starts a diagnosis:

```go
var pe *pg.PgError
if errors.As(err, &pe) {
    log.Error("db", "sqlstate", pe.Code, "condition", pe.Condition())
    // → sqlstate=55P03 condition=lock_not_available
}
```

The table is deliberately partial rather than invented: a code it does
not know returns `""` rather than a plausible guess, because a wrong
condition name in a log line is harder to catch than a bare code.

### What is safe to retry

```go
db := pg.New(drv).WithRetry(pg.RetryPolicy{
    MaxAttempts: 3,
    Errors:      pg.RetryableErrors(),
    Backoff:     pg.ExponentialJitter(10*time.Millisecond, time.Second),
})
```

`RetryableErrors()` is short on purpose, and **what it leaves out is
the point**:

- `ErrCompletionUnknown` (40003) sits one digit from two codes that
  *are* safe. 40001 and 40P01 are guaranteed rolled back; 40003 is
  guaranteed unknown. Retrying a transfer that may already have moved
  the money is worse than failing it.
- `ErrConnectionFailure` (class 08) has the same problem whenever the
  break lands near the commit, and from the client the two are
  indistinguishable.
- `ErrTooManyConnections`, `ErrDiskFull`, `ErrOutOfMemory` are real,
  and retrying is what turns them into an outage.
- `ErrLockNotAvailable` is an answer. A worker that retries it spins on
  the lock it asked not to wait for.

A caller who *knows* their transaction is idempotent — no side effects
outside the database, or an idempotency key covering them — can add the
first two. That is a property of your transaction, which is why the
function will not assume it.

### Read routing corrects itself

`Replicated` decides where a statement goes by reading its leading
keyword, which is documented as fallible in one direction: a write it
cannot see is a write on the wrong node. `25006` from a standby is the
server saying exactly that, so the statement is re-issued on the
primary — safe, because a standby rejects such a statement before
executing it.

```go
repl.ReadOnlyRedirects()   // non-zero and growing is worth an alert
```

Growing means one of two things, and both want fixing: the keyword
scan is misreading a statement you issue often, or a node configured as
a replica is being handed traffic that belongs on the primary.

## The migration failure that arrives later

Add a column to a table that a pooled connection has a prepared
`SELECT *` against, and PostgreSQL invalidates that connection's plan.
Most of the time it re-plans. When the *result type* would change it
refuses, with `0A000` and the message `cached plan must not change
result type`.

What makes it nasty is the timing. **The migration succeeds. Nothing
fails at that moment.** The failures arrive afterwards, in ordinary
requests, one pooled connection at a time as each one next reaches for
a stale plan — so it looks unrelated to the deploy that caused it,
appears intermittently, and clears itself once every connection has
been through it.

```go
drv := pg.RetryCachedPlans(stdlib.New(sqlDB))
db  := pg.New(drv)
```

The retry is safe in a way retries usually are not: the statement was
rejected during *planning*, so nothing executed. There is no partial
write to reason about, which is why this wrapper retries exactly this
error and nothing else. Only one retry is attempted — a second
identical failure is a driver re-sending an invalidated prepared
statement, which is a bug to surface rather than loop on.

Compose it *under* `Replicated`, next to the connection whose plan went
stale:

```go
repl := pg.NewReplicated(
    pg.RetryCachedPlans(primary),
    pg.RetryCachedPlans(r1),
    pg.RetryCachedPlans(r2),
)
```

`DiscardPlans` exists too, and is the wrong tool on a pool: `DISCARD
PLANS` clears the plan cache of *the session it runs on* and no other,
so on a pool that is one arbitrary connection out of however many.
Reach for it when you hold a single connection — a migration worker, a
maintenance job.

## Draining a node

`Hook` answers what *ran*: every event reaches it after the operation
finished. That is the right shape for tracing and the wrong one for
the two questions asked during an incident — "is anything still
writing?" and "can I make it stop?" — because by then there is nothing
left to wait for or cancel.

A `StatementRegistry` wraps the driver instead, so it sees a statement
start, finish, and everything in between:

```go
reg := pg.NewStatementRegistry()
db  := pg.New(reg.Wrap(stdlib.New(sqlDB)))
```

```go
// A failover has been announced, or the process is shutting down.
ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
defer cancel()

if err := reg.Quiesce(ctx); err != nil {
    reg.CancelAll()      // it ran out of time
}
```

`Quiesce` stops admitting statements — new ones get `ErrQuiesced`, a
deliberately non-transient error so a retry loop sheds the request
rather than hammering a node on its way out — and waits for the
in-flight ones. It stays quiesced if it returns early, because a drain
that lets work in behind it never finishes. `Resume` re-admits.

`CancelWrites()` is the gentler failover move: a read still running
against a primary about to be demoted will finish and return correct
data, while a write against it cannot commit anywhere useful.

`Snapshot()` is what a `/debug` handler or a pre-shutdown log line
prints, and the difference between "the deploy hung" and "the deploy is
waiting on this 40-second UPDATE":

```go
for _, s := range reg.Snapshot() {
    log.Info("in flight", "kind", s.Kind, "age", s.Age(), "sql", s.SQL)
}
```

Two details the shape forces, both worth knowing:

- **A query stays in flight until its rows are closed or exhausted**,
  not until `Query` returns. That is when the server is done with it,
  and cancelling earlier would invalidate the cursor under the caller.
- **`Rollback` runs on the caller's context, not the transaction's.**
  The transaction's is what `CancelAll` cancels, and a rollback refused
  before it reaches the server leaves the transaction open on a
  connection nobody will clean up.

Compose the registry closest to the real driver, under `Replicated` and
under `RetryCachedPlans`: those decide *where* and *whether* a
statement is sent, and the registry should record what was actually
sent.

Cancellation is not magic. It cancels the context the driver was given,
and every driver anyone uses turns that into a cancel request on the
wire — which is what produces `57014`. A driver that ignores its
context will not be stopped, and no wrapper can fix that.

## Long jobs

`pg.Backfill` drives a chunked loop over a huge table and persists its
checkpoint, so a crash resumes rather than restarts. What it cannot do
is exist apart from the call: nothing else can start the work, ask how
far it has got, or stop it, and two deploys run it twice.

A `JobQueue` is the row that outlives the call.

```go
// Once, in a migration.
for _, e := range pg.CreateTableWithIndexes(pg.NewJobTable("jobs")) { /* ... */ }
```

```go
q := pg.NewJobQueue(db, "jobs")

id, err := q.Enqueue(ctx, pg.NewJob{
    Kind: "reindex", Key: "orders", Total: 4_200_000,
})
if errors.Is(err, pg.ErrJobAlreadyQueued) {
    // Not a failure. The work you asked for is going to happen.
}
```

```go
// On every worker.
job, ok, err := q.Claim(ctx, "worker-3", "reindex")
if ok {
    for chunk := range chunks {
        if err := q.Heartbeat(ctx, job.ID, done); errors.Is(err, pg.ErrJobNotRunning) {
            return nil          // somebody cancelled us, or we were reaped
        }
    }
    err = q.Complete(ctx, job.ID)
}
```

The load-bearing part is a **partial unique index** over `(kind, key)`
covering only the pending and running rows. It makes "start the
reindex" idempotent however many times the button is pressed or however
many machines are listening — the check is in the database and cannot
be raced, unlike the `SELECT`-then-`INSERT` this usually gets written
as. Finished rows fall out of the index, so history accumulates without
ever blocking a new run.

`Claim` is one statement with `FOR UPDATE SKIP LOCKED`: two workers
racing cannot both take a job, and the loser takes the next one instead
of waiting.

Two things are deliberately **not** exactly-once:

- **Cancel marks a column.** A running worker finds out at its next
  heartbeat; nothing is killed from outside, because a job halfway
  through a chunk should finish the chunk. A worker that never
  heartbeats never notices, which is the reason to heartbeat on a
  schedule rather than when convenient.
- **`Reap` cannot tell a dead worker from a partitioned one.** A job
  whose heartbeat stopped goes back to pending and may then be claimed
  by somebody else while the original is still running. So jobs must be
  safe to run twice — which a chunked backfill already is — and the
  staleness threshold must be comfortably longer than the heartbeat
  interval. Ten intervals is a reasonable start.

```go
// On a schedule, from anywhere. Nothing else recovers a dead worker's job.
n, err := q.Reap(ctx, 10*time.Minute)
```

`Live(ctx)` is the status endpoint — answerable from any process,
because the answer is in the database rather than in whichever one
happens to be running the loop.

## Result sets that will not fit

`CursorSpec` pages through a table for an API, and that is a different
problem: each page is a separate query and re-executes the `WHERE`.
This is one query whose result is too large to materialise, read start
to finish inside one process — an export, a migration, a
reconciliation pass.

The obstacle is that the PostgreSQL protocol sends the whole result
unless the client asks otherwise, and asking otherwise is a
driver-specific setting the three-method `drops.Driver` cannot reach.
`DECLARE` and `FETCH` need no driver support at all:

```go
err := pg.StreamRows(ctx, db, pg.StreamOptions{Batch: 5000},
    "SELECT id, email FROM users WHERE created_at > $1", []any{cutoff},
    func(row drops.Rows) error {
        var id int64
        var email string
        if err := row.Scan(&id, &email); err != nil { return err }
        return send(id, email)
    })
```

`StreamQuery` is the same thing per batch, when the consumer wants
them in batches. Return `pg.ErrStopStream` to stop early without
reporting a failure — which is the whole point of a cursor over a
materialised result.

**What it costs:** a cursor lives inside a transaction, so streaming a
large table holds one open for as long as it takes. An open
transaction pins the oldest snapshot the database must preserve, which
holds back vacuum on *every* table, not just this one. A full export is
the intended use and is fine; leaving the transaction open while
something else is decided is how a table's bloat doubles overnight.

`StreamOptions{Hold: true}` uses `WITH HOLD`, which frees the snapshot
— at the price of the server materialising the entire result into a
temporary file at commit time, before the first row is read. Worth it
for a slow consumer writing each batch to a remote API; not worth it
for a fast local one.

## Which driver you connect with

Four of the things on this page are optional interfaces `pg` probes the
driver for, not methods on `drops.Driver`. Whether they answer depends
entirely on what you passed to `pg.New`:

| | `drops/stdlib` over `*sql.DB` | `drops/pgxdriver` over `*pgxpool.Pool` |
|---|---|---|
| Queries, entities, transactions | ✅ | ✅ |
| `pg.CopyFrom` (bulk load) | `ErrCopyNotSupported` | ✅ |
| `pg.Subscribe`, LISTEN/NOTIFY change feed | `ErrListenNotSupported` | ✅ |
| `pg.StartPoolMetrics` | `database/sql`'s own `DBStats` | pgx's, for the pool the statements go through |
| `pg.ConnAcquirer` (queue-time, pinned work) | — | ✅ |
| SQLSTATE, constraint name, retry classes | ✅ | ✅ |
| Dependency | whatever driver you already registered | pgx v5 |

Nothing in that list is a translation layer: it is what `database/sql`
structurally cannot express. `COPY FROM STDIN` is not a statement it can
send. `LISTEN` needs one connection held open and read from, which is
the one thing a pool of interchangeable connections will not promise.
So the second column is not a faster first column — it is the reason
four shipped features have anything to run on.

```go
import (
    "github.com/jackc/pgx/v5/pgxpool"

    "github.com/bernardoforcillo/drops/pg"
    "github.com/bernardoforcillo/drops/pgxdriver"
)

pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
    return err
}
defer pool.Close()

db := pg.New(pgxdriver.New(pool))
```

`pgxdriver` is its own module, so importing `drops` does not pull pgx
into a build that does not want it:

```sh
go get github.com/bernardoforcillo/drops/pgxdriver
```

The pool stays yours. `pgxdriver.New` does not close it, does not
configure it, and does not touch its timeouts — pooling, TLS and
connection limits stay where you set them, in `pgxpool.Config`.

Errors are unchanged either way. `pg` reads a SQLSTATE from anything
exposing `SQLState()` and a constraint name from `*pgconn.PgError`,
which pgx returns natively, so `pg.IsUniqueViolation`, the retry
classes above and the constraint-name errors behave identically
through both drivers.

**A nested `Begin` is a savepoint.** pgx has real nested transactions,
so `db.InTx` inside `db.InTx` gives you `SAVEPOINT` / `ROLLBACK TO`
rather than a second top-level transaction. That is what you want, and
it is worth knowing when reading a query log.

## A note on composing the wrappers

Three of these are `drops.Driver` wrappers and the order matters:

```go
db := pg.New(
    pg.NewReplicated(                        // outermost: where does it go
        reg.Wrap(                            // then: record what was actually sent
            pg.RetryCachedPlans(primary)),   // innermost: next to the connection
        reg.Wrap(pg.RetryCachedPlans(r1)),
    ))
```

Each of them implements `Unwrap() drops.Driver`, so the duck-typed
capability probes — `Copier`, `Listener`, `PoolStatsProvider`,
`LogicalStreamer` — still reach a driver that implements them through
the whole stack.
