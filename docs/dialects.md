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
| Relations, eager loading | ✅ | partial | — | — | — |
| Keyset pagination | ✅ | ✅ | — | ✅ (via mirror) | ✅ (via vector) |
| Migrations, diff, snapshot | ✅ | ✅ | — | ✅ | — |
| Outbox, saga, event store | ✅ | ✅ | — | event store | — |
| Tenant scoping | ✅ | ✅ | ✅ | ✅ (narrower) | — |
| Audit, authz, cache | ✅ | ✅ | — | — | — |
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

The schema and query surface, plus entity CRUD with the drift check
and composite keys. None of the cross-cutting packages yet, and no
relations: `mysql` has no eager loader, so the declaration API that
used to be here compiled, ran, and loaded nothing. It is gone rather
than deprecated — an API that silently does nothing is worse than one
that is not there, because nothing tells the caller. Write the join.

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

`Push` is bounded by the same two consents as PostgreSQL's, and for a
sharper reason. `DropUnmanagedTables` answers "is this table drops's to
drop"; `Allow` answers "may this table lose its data". A destructive
change — `DROP TABLE`, `DROP COLUMN`, a `MODIFY COLUMN` that retypes —
against a table with rows in it is withheld, the whole push is refused
with `ErrDestructivePush` before a single statement is sent, and
`PushResult.DataLoss` names what it would have destroyed and the
`mysql.Destructive` value that would authorise it. An `Allow` entry that
matches nothing in the diff is reported as a `stale-consent` notice
rather than discarded, because a consent that has quietly stopped
applying reads at the call site exactly like one that has not.
PostgreSQL rolls a failed push back; MySQL has no transactional DDL, so
the refusal in front of the statement is the whole of the protection.

"Has rows" is read from `information_schema.TABLES.TABLE_ROWS`, which
for InnoDB is a sampled estimate rather than a count and can report 0
for a table that is not empty. Since 0 is exactly the value that would
let a DROP through, both 0 and NULL are settled with
`SELECT EXISTS (SELECT 1 FROM t LIMIT 1)` — one row read, on the only
tables in doubt, and only for a change nobody authorised. An
over-estimate needs no such care: it can only withhold a change that
did not need withholding.

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

## Tenant scoping

The one feature that is the same mechanism in four dialects, on
purpose. A table declares the axis; the *executors* resolve it; the
predicate reaches every statement drops composed, to any depth — a
joined table, a CTE body, a subquery operand, an eager-loaded edge, the
predicate another table's filter answers with — and a ctx with no
tenant is refused before anything is sent.

A nil is no tenant. `WithTenant` takes an `any`, so a `(*string)(nil)`
read out of a request struct arrives inside an interface that is not
itself nil; it is refused exactly as an absent tenant is, rather than
stamping `NULL` onto a row that then belongs to nobody. A zero that is
not a nil — an empty string, a zero int — is a tenant like any other:
the schema can store it and it addresses the same rows on the way back
out.

```go
Posts.ContextFilter(pg.TenantFilter(PostTenantID)).
    ScopeWritesByTenant(PostTenantID)

ctx = pg.WithTenant(ctx, currentTenant)
```

Read the same line with `sqlite.`, `mysql.` or `clickhouse.` in front
of it and it means the same thing. Each package holds a `resolve.go`
carrying the walk; normalise the dialect name and diff any two of them
and the same file comes back, which is how the next divergence is meant
to be caught rather than re-derived.

What differs is surface, and it differs where the SQL does:

- **PostgreSQL** is the reference, and the only one where the
  predicates are *not* the isolation boundary: row-level security is,
  and `EnableRLS` / `AddPolicy` / `DB.InTxAs` are how you declare it.
  Read the predicates as defence in depth there.
- **SQLite** has the whole mechanism minus what the dialect lacks: no
  `RIGHT` or `FULL JOIN`, so the join-placement shapes cannot arise.
  There is no row-level security to sit underneath — no roles, no
  policies, and a process that can open the file reads every byte.
- **MySQL** has the whole mechanism, including the aliased `UPDATE` and
  `DELETE` that must name their alias twice and the upsert whose
  `ON DUPLICATE KEY UPDATE` has no conflict target and no `WHERE`
  clause for a predicate to reach. Its nearest thing to RLS is a
  definer-rights view, which drops does not manage.
- **ClickHouse** is narrower because the dialect is. There is no
  `UPDATE` or `DELETE` to carry a predicate, so the write side is
  stamping and refusal only; no upsert to gate, because a merging
  engine folds rows sharing a sorting key in the background — which is
  where the check went instead, as `ErrTenantNotInSortingKey`; no
  relations and so no eager-loaded edge; no cache to key by tenant; and
  no set operations. A materialised view evaluates its stored body on
  INSERT with no ctx anywhere near it.

`Unscoped` has one meaning per level in all four: **statement-wide** on
a raw builder, where the caller is describing the whole statement's
authority, and **defaults-only** on an entity query, which drops the
declaration-time filters (a soft-delete guard) and keeps the tenant
axis and the authorization guard. A query that genuinely has to span
tenants is written on the raw builder, where a reviewer reads the whole
of what was given up. On an **INSERT** it additionally means the ctx
tenant is neither stamped nor required and the dialect's upsert branch
is left as written — the escape hatch a migration or a backfill needs.

At every level it stops at the edge of the statement it was said on. A
CTE body, a subquery operand, a subquery bound as an INSERT value is a
statement of its own and keeps its own scoping, and an inner statement
with no tenant to name still refuses. That is also how one part of a
query is unscoped and no other. The wide misreading is the dangerous
one: a caller who expects `Unscoped` on the INSERT to widen the
subquery bound as its value will write a row computed from one
tenant's data while believing it spans them all.

The rules are written down once rather than per dialect. Each package's
`tenant.go` carries a block delimited `THE TENANT POLICIES —
NORMATIVE`, byte-identical in all four and pinned by a root-level test
that fails when one drifts by a word, by whitespace, or by reordering.
The set of dialects it pins is derived from the source — every package
declaring `WithTenant` — rather than listed, so a fifth dialect that
carried no block would fail rather than pass unnoticed.
It states what counts as the same tenant (a round-trip conversion, so a
truncating pair cannot compare equal), what may assign the axis
(`Create` and `Update` stamp and refuse a mismatch; `Patch` refuses any
op naming it), and what `Unscoped` means at each level. Dialect
differences are named inside the shared text — `clickhouse` models
neither `UPDATE` nor `DELETE`, `RelConfig.Unscoped` is `pg`'s alone —
so the same words are true in four packages.

What the mechanism does *not* reach is listed per dialect, under
"Where the automatic scoping stops" — in `tenant.go` for `sqlite`,
`mysql` and `clickhouse`, and in `doc.go` for `pg`, where it continues
the package overview that opened the subject. It is not a footnote: in
three of the four the predicates are the whole of what there is,
because none of those three has row-level security to put underneath
them.

The four lists are not one list repeated, and reading one is not
reading them all. `pg`'s is written against `pg`'s own surface and
shares no entry's wording with the other three: eleven entries,
covering among others the FULL JOIN refusal, the `DeleteHook` rewrite
path, what `ToSQL` renders without a ctx, how far `Unscoped` reaches,
and what the axis column's collation and type do to two tenant values
Go calls different. The other three share a spine of six — a raw
statement, a `drops.Raw` or `ExprFunc` body, a view body, a statement
that said `Unscoped()`, an INSERT into a table with a read filter and
no write column, and the RIGHT JOIN placement gap — and depart from it
where the dialect does. `mysql` carries three more: its hand-written
outbox, event-store and idempotency SQL, the identifier fold it cannot
settle without a server, and the tenant value its default
case-insensitive collation folds onto another tenant's. `sqlite`
carries the RIGHT JOIN entry to record a gap the dialect cannot have,
so that adding a join kind is known to bring it, and one entry of its
own: a `COLLATE NOCASE` axis column is two tenants to drops and one to
the server. `clickhouse` carries no entry of that kind: it has no
per-column collation for `=`, its `String` comparison being binary,
and no ClickHouse was reachable to probe what its drivers do with a
tenant value of the wrong Go type. Read the list for the dialect you
are writing against.

## Porting between them

The builder chain is dialect-agnostic — placeholders and identifier
quoting come from the `drops.Dialect` installed on the Builder — so
moving a query usually means swapping the import. Moving a *schema*
means checking the type mapping, since the constructors are named for
parity but the underlying types differ. The tables above are where the
surprises are.
