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

Because `ALTER TABLE` cannot do most things, a migration that changes a
column, drops one, or changes a constraint becomes a **table rebuild**:
create the new shape, copy the rows, `DROP TABLE`, rename. `DROP TABLE`
takes every index and every trigger on the table with it, so `Diff`
reads them out of `sqlite_master` first and replays them after the
rename. Three cases behave differently, deliberately:

- An index keyed on a column the rebuild removes is **dropped with the
  column**, the way PostgreSQL drops a dependent index, and the
  migration carries a comment saying so. `AnalyzeMigration` reports it
  as `rebuild-drops-index`.
- An index that reaches a removed column some other way — a partial
  index's `WHERE`, an expression key — is replayed as stored, because
  `PRAGMA index_info` does not report what those name. SQLite rejects
  the `CREATE INDEX` and the migration **fails and rolls back**, rather
  than losing an index drops could not reason about.
- A trigger is always replayed verbatim. SQLite does not resolve the
  names in a trigger body until it fires, so one naming a removed
  column is accepted here and fails later; drops warns instead
  (`rebuild-stale-trigger`).

The schema DSL cannot declare an index, so `Diff` and `Push` never
create or drop one on their own — an index you made by hand survives a
push untouched.

## MySQL / MariaDB

The schema and query surface, plus entity CRUD with the drift check
and composite keys. None of the cross-cutting packages yet.

Three differences shape the API rather than the SQL:

- **No `RETURNING`.** `Entity.Create` issues the INSERT and reads the
  generated key back through the driver's `LastInsertId`. `CreateMany`
  deliberately does not backfill keys, because `LastInsertId` reports
  only the first row's and inferring the rest assumes a contiguous
  block `innodb_autoinc_lock_mode=2` does not guarantee.
- **Upsert keys on any unique index.** `ON DUPLICATE KEY UPDATE` has no
  conflict target, so it fires on a unique email as readily as on the
  primary key. Broader than PostgreSQL's `ON CONFLICT (id)`, and
  occasionally a surprise.
- **An aliased DELETE is written the long way.** MariaDB answers 1064
  to ``DELETE FROM t AS a``, so ``db.Delete(users.As("u"))`` emits the
  multi-table form, ``DELETE `u` FROM `users` AS `u` ``, which both
  families accept. That form takes no `ORDER BY` and no `LIMIT`, so a
  batched delete goes through the un-aliased handle; `Exec` returns
  `ErrAliasedDeleteBounded` rather than posting a statement the server
  is certain to reject.

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
