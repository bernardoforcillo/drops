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

### `sqlite.Push` refuses a destructive change (breaking change)

A rebuild is also where SQLite's destructive changes hide, and until
now `Push` had no guard against them at all. Pushing a schema with a
column removed applied cleanly, the rebuild copied the columns both
sides named, the data was gone and `err` was `nil`.

The reason it went unnoticed is worth stating, because it shapes the
fix. On PostgreSQL, `drops push` reads the plan it is about to run and
refuses the destructive statements in it — `DROP COLUMN`, `DROP TABLE`,
`TRUNCATE`. On SQLite there is no such statement to find. The plan is a
`CREATE`, an `INSERT … SELECT`, a `DROP TABLE` and a `RENAME`, and
those four are identical whether the rebuild widens the table or empties
half of it; the column that is going is simply absent from the
`INSERT`'s column list. **No guard that reads the SQL can see this.**
The fact lives in the diff — this column is in the previous snapshot
and not in the next one — so that is where it is computed, and it is
carried out to the caller rather than recovered afterwards.

`sqlite.Push` now returns `*sqlite.DestructiveChangeError` and applies
nothing when the change would destroy any of:

| Rule | What it is |
|---|---|
| `drop-column` | the column is in the database and not in the schema |
| `alter-column-type` | the column moves from `TEXT` affinity to a numeric one, and the copy converts as it goes: `'007'` arrives as `7` |
| `alter-column-set-not-null` | the column gains `NOT NULL` and rows still hold `NULL`, which the copy will not accept |
| `add-unique-constraint` | the column or column list gains `UNIQUE` and the rows already hold a value twice |
| `add-check-constraint` | the table gains a `CHECK` and a row already breaks it |
| `rebuild-drops-index` | the rebuild drops an index keyed on a departing column and does not put it back |
| `rebuild-stale-trigger` | the rebuild puts a trigger back naming a departing column; SQLite accepts it and it fails when it fires |

Three of them — `alter-column-set-not-null`, `add-unique-constraint`
and `add-check-constraint`, the tightenings — cost no data at all:
SQLite refuses the copy and the whole transaction rolls back. They are
in the list because without them the push dies partway through a
rebuild on a constraint message, which is the thing this guard exists
to turn into a readable refusal.

The names are `drops/pg`'s names wherever the meaning is the same, so
`drop-column` means here what it means there. The last two are what
`GenerateMigration` has always warned about through `AnalyzeMigration`
and `Push`, which prints no migration for anyone to read, did not.

There is one more rule, `drop-table`, and `Push` never reaches it. It
diffs only over the tables the schema declares — a table drops was
never told about belongs to somebody else — so deleting a `NewTable`
from the schema does not drop the table and does not raise a finding
either; it is left alone, silently, exactly as before. The rule is for
callers of `sqlite.DestructiveChanges` (below), which diffs whatever
two snapshots it is handed. Dropping a table still means writing the
`DROP` into a migration.

`PushOptions.AllowDestructive` applies them anyway — the same
permission `drops push --allow-destructive` carries on PostgreSQL, and
the same meaning: not that the change is safe, but that somebody has
read what it destroys. It does **not** answer a rename question, and a
rename answer does not permit a drop; a column being renamed rather
than dropped is a question about what the change means, and the two
must not collapse into one option. `sqlite.DestructiveChanges(prev,
cur, opts)` computes the same list from any pair of snapshots.

Two things deliberately do not trigger it. A rebuild that only widens a
table loses nothing and is not refused — otherwise the option would be
needed on almost every push and would stop meaning anything. And the
three tightenings are put to the rows before they are reported:
tightening a column that holds no `NULL`, declaring one unique that has
held distinct values all along, or adding a `CHECK` every row satisfies
all go through untouched.

The probe reads the table as the rebuild's `INSERT … SELECT` will
present it, not as it stands: a column this push renames is supplied
under its new name and one it adds under its default. SQLite reads a
double-quoted identifier matching no column as a string literal rather
than failing, so a probe against the raw table would ask a different
question and answer it confidently — which for a `CHECK` means a
silent acquittal and a push that then dies on that very constraint.

`PushOptions.DryRun` does not refuse. It returns the plan with
`PushResult.Destructive` filled in, because a preview that will not show
you what you would have to permit is no preview.

**This is a breaking change** for anyone who relied on the old silence:
a push that used to drop a column now stops. The migration path is one
option, or one flag.

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

### What a dropped column takes with it

A migration that drops a column and something naming it has one working
order — the dependent goes first — and getting there is not a port of
PostgreSQL's rule, because MySQL answers a different way for each kind.
Read off MariaDB 10.11: a secondary index over the dropped column alone
is **removed with the column**, so a `DROP INDEX` afterwards is 1091;
one over several columns is **narrowed** to the columns that remain and
stays, so the drop is not stale and is the only thing that gets rid of
it. A `UNIQUE` key over the column alone goes with it, but MariaDB will
not narrow a multi-column one and refuses the column drop with 1072. A
`CHECK` naming only that column goes with it on MariaDB and is refused
on MySQL 8.0.16+ with 3959; one naming a surviving column too is
refused on both with 1054. Either side of a foreign key — the column
the key is on, the column it points at, and the index it needs —
refuses with 1553. A single-column `PRIMARY KEY` goes with its column;
a composite one refuses with 1072.

`Diff` emits the foreign-key drops first, across every table, then the
indexes, `CHECK`s and primary keys on each, and only then the columns.
One hazard is not an ordering problem and remains: `DROP PRIMARY KEY`
on a table whose key covers an `AUTO_INCREMENT` column is 1075 wherever
it is put.

`Push` reaches the drop of an index the Go schema never declared only
under `PushOptions.DropUnmanagedIndexes`; under the default it withholds
it as an `unmanaged-index` notice, because it cannot tell an index the
schema stopped declaring from one somebody made by hand. The one
exception is an index keying a column the same push drops, which MySQL
will not leave as it is whatever Push does — so withholding the drop
there preserves nothing: it leaves a narrowed index nobody asked for, or
stops the push on 1072 or 1553. That drop goes through by default, under
a notice naming the index and the departing column.

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
