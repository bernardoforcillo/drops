# Tenancy: the predicate that cannot be forgotten

Multi-tenant isolation fails one query at a time. The schema is right,
the entity is right, every call site anybody reviewed is right, and then
one report, one background job or one eagerly loaded relation is built
by hand and reads without the tenant predicate. Nothing errors. The rows
come back. They belong to somebody else.

drops answers that by moving the predicate off the call site and onto
the **table**, and by refusing when it cannot build one.

```go
var Posts = pg.NewTable("posts")
var PostID       = pg.Add(Posts, pg.BigSerial("id").PrimaryKey())
var PostTenantID = pg.Add(Posts, pg.BigInt("tenantId").NotNull())
var PostTitle    = pg.Add(Posts, pg.Text("title").NotNull())

var PostEntity = pg.NewEntity[Post](Posts).ScopeByTenant(PostTenantID)
```

From here on every statement against `posts` needs a tenant on its ctx:

```go
ctx := pg.WithTenant(r.Context(), currentTenant)

posts, err := PostEntity.Query(db).All(ctx)      // scoped
rows, err  := db.Select().From(Posts).All(ctx, &out) // scoped too
```

The second line is the point. `db.Select()` knows nothing about the
entity, and it still carries the axis, because the axis is registered on
the table rather than injected by the entity's methods.

## What "cannot be forgotten" means, precisely

Four claims, each of which the test suite enforces rather than asserts
in prose.

**Every executor carries it.** A statement reaches the server through
one of about thirty methods — `All`, `One`, `Rows`, `Exec`, `Stream`,
`Page`, `CopyFrom`, the entity CRUD, the factory, the seeder, the vector
store. The census in `pg/scopeexec_test.go` reads the package's own
source for every exported method that takes a `context.Context` and can
reach a driver, and requires each one either to run in a test that
proves it carries the axis, or to appear in a table of exemptions whose
reason has to say why carrying one would be *wrong*. A new executor
added without a case fails the build.

**Every door a relation comes through carries it.** A second census
(`pg/scoperelation_census_test.go`) does the same for the methods a
`*Table` can reach a statement through — `From`, `Join`, `Using`,
`As`, and every chainable setter that hands a table back.

**No statement hides inside an expression.** A predicate is a place a
statement can be written: `In(col, subquery)`, `Coalesce(subquery, 0)`,
`CASE WHEN EXISTS (…)`, a window's `PARTITION BY`, an `UPDATE`'s `SET`
value. Every composite expression in drops is a **node holding its
operands** rather than a closure over them, so the resolver can walk
into it and scope what it finds. A constructor written as a closure is
caught by a census of its own.

**A filter that cannot decide refuses.** With no tenant on ctx, a scoped
table does not render an unfiltered statement — `ErrTenantMissing` comes
back and nothing is sent. This is asserted separately from the carrying
half, because a guard that reported *after* the rows had gone would be a
leak with a stack trace attached.

## Reads and writes are two halves

A `ContextFilterFunc` answers with a predicate. A predicate scopes the
statements that have a `WHERE` clause — `SELECT`, `UPDATE`, `DELETE` —
and an `INSERT` has none. So the write side is a *column* rather than a
predicate:

```go
Posts.ContextFilter(pg.TenantFilter(PostTenantID)).
    ScopeWritesByTenant(PostTenantID)
```

`Entity.ScopeByTenant` declares both at once. With the write axis
declared, every `INSERT` into the table stamps the ctx tenant onto that
column and refuses without one — including an insert built straight from
`db.Insert()`.

A table scoped only with `ContextFilter` keeps its guarantee on every
read, `UPDATE` and `DELETE` while its `INSERT`s stay the caller's to
bind. That is a coherent position, not an oversight: nothing can ask a
closure which column owns a row, and drops will not guess one from the
predicate it rendered.

### The `SET` list is the half a predicate does not reach

An `UPDATE`'s `WHERE` clause decides which rows may be touched. Its
`SET` list decides what they become — including, if nobody checks,
somebody else's tenant:

```sql
UPDATE "gadgets" SET "tenantId" = 999 WHERE id = 7 AND "tenantId" = 3
```

That statement is correctly scoped in the half a reviewer checks. It
finds exactly one row, the caller's own, and gives it away. drops
refuses an assignment to the tenant axis unless it names the tenant the
ctx already carries; a restatement of the same value renders, another
tenant's value is `ErrTenantMismatch`, and an expression is refused
rather than trusted, because what it evaluates to is the server's answer
and a transfer written as arithmetic is still a transfer.

## Where the predicate lands

Not every clause is the right place for a guard, and getting this wrong
loses rows rather than leaking them — which is the failure nobody
reports as a security bug.

An outer join preserves rows that did not match. A predicate on the
preserved side, written in the `WHERE` clause, is evaluated *after* the
join has NULL-extended those rows, so it is false for exactly the rows
the outer join exists to keep and the join silently becomes an inner
one. So:

- a `LEFT JOIN`ed table's predicates go in **that join's `ON`**;
- the `FROM` table's go in the first `RIGHT JOIN`'s `ON`, for the
  mirror-image reason;
- everything else stays in the `WHERE` clause.

Two shapes have no correct answer and are refused instead of rendered
one of the two wrong ways: a `FULL JOIN` (both sides are preserved, so
neither clause works) and, in ClickHouse, an `ASOF JOIN` of a scoped
table (its `ON` is a grammar, not a predicate). `ANY JOIN` in ClickHouse
picks one arbitrary match while the join runs, so its guard goes in the
`ON` too — in the `WHERE` it would drop rows the caller is entitled to,
non-deterministically.

Both refusals name the way through: join a pre-filtered subquery, or say
`Unscoped()` and write the predicate at the query, where a reviewer
reads it next to the join.

## Aliases share the table's scope

An alias is a query-scope rename, so it carries the filters its table
has **now** rather than the ones it had when `As` was called:

```go
var Users = pg.NewTable("users")   // package scope
var U     = Users.As("u")          // package scope, before init

func init() { Users.ContextFilter(pg.TenantFilter(UserTenantID)) }
```

Go initialises package-level variables before it runs `init`, so `U` is
taken while the table has no tenant axis. If the alias had copied the
filter list it would never gain one, and a statement written against it
would render `DELETE FROM "users" AS "u"` — no predicate, every tenant's
rows, and no error. The filter lists, the write axis and the lifecycle
hooks live behind one shared pointer for that reason.

The consequence, stated plainly because it follows from sharing and is
not a bug: registering a filter on an alias registers it on the table,
and so on every other alias of it. Where two genuinely different
scopings are wanted, they are two tables.

## Stepping around one guard without stepping around the rest

`Unscoped()` drops **every** automatic predicate a table carries. That
makes it the wrong tool for "show me the deleted rows" on a table that
is also scoped by tenant — the caller asks for deleted rows and gets
another customer's. So filters carry names:

```go
Posts.AddFilter(pg.FilterSoftDelete, pg.IsNull(PostDeletedAt))

// deleted rows, still only this tenant's:
db.Select().From(Posts).IgnoreFilters(pg.FilterSoftDelete)
```

`IgnoreFilters` is statement-local: a nested statement installs its own
set, so it reaches no further than the statement that said it.

`EntityQuery.Unscoped()` is deliberately narrower than the builder's: it
drops the table's declaration-time filters and keeps the request-time
ones, because "include the soft-deleted rows" must not also mean "and
every other tenant's". The raw builder's `Unscoped()` stays the blunt
instrument it documents itself as — reach for it when you mean *no
scoping at all*, and for nothing narrower.

## `ToSQL` is not what gets sent

A context filter is built from a ctx, and `ToSQL()` has none. So on a
scoped table `ToSQL()` renders the declaration-time filters and **none**
of the request-time ones — it is not the statement the server would see.
`ToSQLCtx(ctx)` is:

```go
sql, args, err := db.Select().From(Posts).ToSQLCtx(ctx)
```

This matters most in tests. A round-trip against a single-tenant fixture
passes while leaking, because there is no second tenant's row to come
back; the rendered SQL is the only place the guard is visible. Assert on
`ToSQLCtx`.

## A filter that reads its own table

A context filter is arbitrary code and may run a query. A filter on
`notes` whose predicate selects from `notes` — directly, or through
another table whose filter comes back — has no fixed point to converge
on: each turn asks the filter for a fresh predicate. Left alone it is
not a wrong answer, it is a goroutine that never returns.

drops refuses it with `ErrContextFilterCycle` and spells the cycle out:
`notes -> authors -> notes`. The way through is the shape the author
meant anyway — say `Unscoped()` on the statement the filter embeds,
since the inner read selects the rows the filter is about to restrict.

## Where the guarantee stops

Worth knowing before you rely on it:

- **`DB.Exec` and `DB.Query` take raw SQL.** drops is handed a string
  and never learns which relations it names, so there is nothing to
  scope. They are the escape hatch, documented as one.
- **`Push` reconciles the schema, not the rows.** Its emptiness probe
  deliberately asks whether a table is empty *for everybody*, because
  that is what a `DROP` turns on; scoped, one tenant's consent would
  destroy another's rows.
- **A change feed publishes primary keys.** `Subscribe` sends `LISTEN`
  and reads no rows; what crosses tenants is the key of a changed row,
  which is what a change feed is. A consumer that must not see other
  tenants' keys filters the stream, or reads it behind row-level
  security.
- **These predicates are not row-level security.** They are drops'
  statements being correct. RLS is the server refusing regardless of
  what the client sends, and it is the layer beneath this one:
  `Table.EnableRLS`, `AddPolicy` and `DB.InTxAs` are how you reach it.
  Use both if the data warrants it.

## The same answer in four dialects

pg, MySQL, SQLite and ClickHouse answer this the same way, deliberately:
a rule that differs per dialect is no rule at all, because the
boilerplate it removes is written by people who use more than one. A
test in the root package compares the tenancy code across all four,
declaration by declaration, and fails when they drift apart.

Where a dialect *must* differ, it differs where the server does:

- **"the same column name"** is byte-for-byte in pg and ClickHouse, and
  an ASCII case-fold in MySQL and SQLite, because that is how each
  server resolves an identifier.
- **ClickHouse merging engines** fold rows that share a sorting key, in
  the background, comparing by that key and nothing else. A tenant
  column outside the sorting key is not part of the comparison, so two
  tenants' rows that agree on the key become one row — no statement, no
  error. drops refuses the `INSERT` and points at `ORDER BY`.
- **SQLite has no row-level security**, so the guard it offers under a
  rebuild is a trigger; see `sqlite.TenantGuard`.
