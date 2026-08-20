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
| Keyset pagination | ✅ | ✅ | ✅ | ✅ (via mirror) | ✅ (via vector) |
| Migrations, diff, snapshot | ✅ | ✅ | ✅ | diff + push² | — |
| Introspection reads back | most¹ | ✅ | ✅ | most² | — |
| Outbox, saga, event store | ✅ | ✅ | outbox, event store | event store | — |
| Typed driver errors, retry | ✅ | sentinels only | ✅ | — | — |
| Audit, tenancy, authz, cache | ✅ | ✅ | — | — | — |
| Vector search | ✅ pgvector | — | — | ✅ built-in | ✅ native |

² ClickHouse has `Introspect`, `BuildSnapshot`, `Diff` and `Push` but
no migration generator, and its `Diff` returns a plan rather than a
list of statements — a great deal of what a ClickHouse schema change
would mean has no `ALTER` behind it and comes back as a refusal. Column
TTLs are the one declared thing `system.columns` cannot report, so they
are left out of the comparison and reported as a notice.

¹ PostgreSQL introspection does not yet read back enums, sequences,
views, RLS or policies, though the diff generator can write all of
them. A schema declaring one enum therefore makes `Push` re-emit its
`CREATE TYPE` on every run and `DetectDrift` permanently noisy. See
`pg/introspect.go`.

Where a cell is empty the feature is not there yet, not disabled. The
package doc for each dialect says what it covers, and `## What's not
here` in the readme says what none of them do.

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
takes every index and every trigger on the table with it, so
`Introspect` reads them out of `sqlite_master` and the rebuild replays
them after the rename. Three cases behave differently, deliberately:

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
push untouched. For the same reason `DetectDrift` cannot see one: an
index added to production by hand is not reported as an unauthorised
change, because every index in every database would be.

**That replay only happens on the paths that read the database.**
`Push` and `DetectDrift` diff against a live `Introspect`, so they have
the stored DDL to put back. `GenerateMigration` diffs two snapshot
files, and a snapshot file records no index and no trigger — there is
nothing in the schema DSL for `BuildSnapshot` to record. A generated
migration that rebuilds a table therefore destroys its indexes and
triggers, and reading the database at generation time would not help:
the file is applied later, to servers the generator never saw, each
with its own indexes. So the generated SQL carries a comment above
every rebuild saying exactly that, `AnalyzeMigration` reports it as
`rebuild-loses-indexes`, and re-creating them is the reviewer's job.

One rebuild is impossible rather than lossy. `ALTER TABLE t_new RENAME
TO t` resolves every view and every trigger body that names `t`, and
between the `DROP` and the `RENAME` there is no `t` — so a table
referenced by a view, or by a trigger on another table, cannot be
rebuilt at all: the rename fails and the migration rolls back. Drop the
dependent object, rebuild, and re-create it.

## MySQL / MariaDB

The schema and query surface, entity CRUD with the drift check and
composite keys, migrations against `information_schema`, a
transactional outbox and event store, keyset pagination, typed driver
errors and the expression library. Not audit, tenancy, authz or cache,
and relations are declaration-only — there is no eager loader.

Four differences shape the API rather than the SQL:

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
- **No transactional DDL.** MySQL commits implicitly on every DDL
  statement, so a migration that fails half-way leaves the schema half
  changed — there is nothing to roll back to. That is the contract
  rather than a surprise: `Push` reports how far it got, and a single
  `ALTER TABLE` carrying several actions is atomic even though the
  migration around it is not.

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

`Introspect` reads a table back out of `system.tables` and
`system.columns`, `BuildSnapshot` derives the same shape from the Go
declaration, `Diff` puts them side by side and `Push` applies the
result. What makes this dialect different is what `Diff` cannot emit:
ClickHouse has no `ALTER` for a table's engine, its partitioning, its
primary key or — beyond appending columns the same statement adds — its
sorting key, and none for a column that takes part in any of them.
Those come back as refusals naming the remedy, which is always a new
table and a copy.

On a `ReplacingMergeTree` the sorting key deserves the emphasis it
gets: the engine collapses rows that share it, so the key is the
definition of "the same row" rather than a layout choice, and changing
it changes how many rows the table holds.

`clickhouse.Analyze` grades what a plan does carry — metadata, a
background rewrite, or a deletion with no way back. A ClickHouse
`ALTER` returns before its work is done (`mutations_sync` defaults to
0), so `system.mutations` is where a migration actually finishes.

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
