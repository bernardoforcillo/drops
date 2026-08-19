# Choosing a dialect

drops targets five backends. They are not interchangeable, and the
honest summary is that PostgreSQL is where the library is deepest.

| | PostgreSQL | SQLite | MySQL | ClickHouse | Qdrant |
|---|---|---|---|---|---|
| Schema, DDL, indexes | ✅ | ✅ | ✅ | ✅ | n/a |
| Query builders | ✅ | ✅ | ✅ | ✅ | n/a |
| Entity CRUD | ✅ | ✅ | ✅ | ✅ | n/a |
| Drift check | ✅ | ✅ | ✅ | ✅ | n/a |
| Composite keys | ✅ | ✅ | ✅ | n/a | n/a |
| Relations, eager loading | ✅ | partial | declaration only | — | — |
| Keyset pagination | ✅ | ✅ | — | ✅ (via mirror) | ✅ (via vector) |
| Migrations, diff, snapshot | ✅ | ✅ | — | ✅ | — |
| Outbox, saga, event store | ✅ | ✅ | — | event store | — |
| Audit, tenancy, authz, cache | ✅ | ✅ | — | — | — |
| Vector search | ✅ pgvector | — | — | ✅ built-in | ✅ native |

Where a cell is empty the feature is not there yet, not disabled. The
package doc for each dialect says what it covers.

## PostgreSQL

The reference implementation. Everything else is measured against it.
Beyond the basics it has the production patterns most services end up
writing by hand: transactional outbox, saga, event sourcing,
idempotency keys, change feeds, logical-replication lag tracking,
sharding, materialised views, online DDL, query-plan capture, PostGIS
helpers, money and PII types.

Use it unless you have a reason not to.

## SQLite

Close behind, and the right choice for tests, embedded use and small
deployments. Nearly all of pg's entity surface is there.

Differences that matter: types are affinities rather than declarations,
so a `Varchar(255)` is a `TEXT` and the length is not enforced.
`ALTER TABLE` cannot add a constraint, so constraints are declared
inline at create time. There is no zoned timestamp — store UTC.

## MySQL / MariaDB

The schema and query surface, plus entity CRUD with the drift check
and composite keys. None of the cross-cutting packages yet.

Two differences shape the API rather than the SQL:

- **No `RETURNING`.** `Entity.Create` issues the INSERT and reads the
  generated key back through the driver's `LastInsertId`. `CreateMany`
  deliberately does not backfill keys, because `LastInsertId` reports
  only the first row's and inferring the rest assumes a contiguous
  block `innodb_autoinc_lock_mode=2` does not guarantee.
- **Upsert keys on any unique index.** `ON DUPLICATE KEY UPDATE` has no
  conflict target, so it fires on a unique email as readily as on the
  primary key. Broader than PostgreSQL's `ON CONFLICT (id)`, and
  occasionally a surprise.

Smaller things worth knowing before you port a schema: `TEXT` cannot be
indexed without a prefix length (`Index.Prefix`), so a column you mean
to index wants `Varchar`; `Timestamp(name, false)` maps to
`DATETIME(6)` rather than `TIMESTAMP`, because `TIMESTAMP` silently
converts through the session time zone; index names are scoped to their
table, so `DropIndex` takes one.

`Dialect.SupportsReturning()` reports false even on MariaDB 10.5+,
which has `RETURNING` for INSERT and DELETE but not UPDATE. drops
targets the intersection rather than emitting a clause the server may
reject.

## ClickHouse

Analytical, not transactional. Engine-bound tables across the MergeTree
family, CH-specific types (`Array`, `Nullable`, `LowCardinality`,
`Decimal`, `DateTime64`, `Tuple`, `Map`, `Enum`), the full SELECT
surface (`PREWHERE`, `FINAL`, `SAMPLE`, `ASOF JOIN`, `SETTINGS`), batch
INSERT, materialised views, and the analytics aggregates.

There is no `UPDATE`/`DELETE` in the usual sense — mutations rewrite
whole parts asynchronously — so the shape of a ClickHouse workload is
append, and collapse on merge. [mirror.md](mirror.md) is built on that.

## Qdrant

Not SQL. A focused HTTP client (net/http and encoding/json only) for
collections, points, search, recommend and scroll, plus a filter DSL.

Through [`drops/vector`](vector-search.md) it also satisfies the same
portable search interface as pgvector and ClickHouse.

## Porting between them

The builder chain is dialect-agnostic — placeholders and identifier
quoting come from the `drops.Dialect` installed on the Builder — so
moving a query usually means swapping the import. Moving a *schema*
means checking the type mapping, since the constructors are named for
parity but the underlying types differ. The tables above are where the
surprises are.
