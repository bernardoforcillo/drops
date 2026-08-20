# Entities and CRUD

An entity binds a struct to a table:

```go
var UserEntity = pg.NewEntity[User](Users)
```

It precomputes the column-to-field mapping once, at startup, and checks
it — see [schema.md](schema.md#drift). Two things make it panic there:
a column no field binds to, and a column that admits NULL bound to a
field that cannot receive one. Both name the column, the likely cause
and the escape hatch —
[`AllowUnmappedColumns`](schema.md#drift) and
[`AllowNullableColumns`](schema.md#nullability-drift).

## The operations

```go
err := UserEntity.Create(db, ctx, &u)          // INSERT, populates generated keys
u, err := UserEntity.Get(db, ctx, id)          // SELECT by primary key
err = UserEntity.Update(db, ctx, &u)           // UPDATE by primary key
err = UserEntity.Save(db, ctx, &u)             // INSERT or UPDATE, on whether the key is set
res, err := UserEntity.Delete(db, ctx, id)     // DELETE by primary key
res, err = UserEntity.CreateMany(db, ctx, us)  // one multi-row INSERT
res, err = UserEntity.UpsertMany(db, ctx, us)  // INSERT … ON CONFLICT UPDATE
```

`Get` returns `pg.ErrNoRows` when nothing matches.

## Querying

```go
adults, err := UserEntity.Query(db).
    Where(UserAge.Gte(18)).
    OrderBy(UserName.Asc()).
    Limit(50).
    All(ctx)
```

`Query` returns `[]User` rather than scanning into a slice you supply.
Everything a `SelectBuilder` can do it can do; it just knows the type.

## Queries no entity describes

`Query` can be typed because the entity already says what row comes
back. A join, an aggregate, or a projection onto three of twelve
columns produces a row no table has — so there is no entity to hang the
type on, and `SelectBuilder.All(ctx, dest)` falls back to an untyped
pointer it can only complain about at run time.

`drops.All` and `drops.One` take the type from the call site instead.
They are generic over anything with a `Rows(ctx)` method, which every
dialect's `SelectBuilder` has:

```go
type authorPosts struct {
    Author string
    Posts  int64
}

top, err := drops.All[authorPosts](ctx, db.
    Select(UserName.As("author"), pg.As(pg.Count(PostID), "posts")).
    From(Users).
    Join(Posts, PostUserID.EqCol(UserID)).
    GroupBy(UserName).
    OrderBy(UserName.Asc()))
```

A single-column query needs no struct at all:

```go
ids, err := drops.All[int64](ctx, db.Select(UserID).From(Users))
n, err := drops.One[int64](ctx, db.Select(pg.Count(UserID)).From(Users))
nicks, err := drops.All[*string](ctx, db.Select(UserNick).From(Users))
```

`T` may be a struct, a `*struct`, or a scalar — including a pointer to
one, which is how a nullable column comes back. It may not be a pointer
to a struct: `One` already reports emptiness as an error, and a nil
`*T` would be a second, quieter way to say the same thing.

Which to reach for:

- the entity's `Query` when the rows *are* the table — it goes through
  the fast-scan path, the entity cache, tenant scoping and eager
  loading, none of which an ad-hoc query has;
- `drops.All` / `drops.One` when they are not.

The two report an empty `One` with different sentinels today —
`pg.ErrNoRows` from the entity, `drops.ErrNoRows` from `drops.One` —
and the dialect ones do not wrap the shared one, so check against the
sentinel of whichever you called.

`pg`, `mysql` and `clickhouse` builders satisfy `drops.RowSource`;
`sqlite`'s does not yet expose `Rows(ctx)`.

## Composite primary keys

A table with more than one `PrimaryKey()` column works, and the key
travels as several values:

```go
var (
    Memberships = pg.NewTable("memberships")
    MemberOrg   = pg.Add(Memberships, pg.BigInt("orgId").PrimaryKey())
    MemberUser  = pg.Add(Memberships, pg.BigInt("userId").PrimaryKey())
    MemberRole  = pg.Add(Memberships, pg.Text("role").NotNull())
)

m, err := MembershipEntity.Get(db, ctx, orgID, userID)
_, err = MembershipEntity.Delete(db, ctx, orgID, userID)
```

Passing the wrong number of values is `pg.ErrKeyArity`, not a partial
match. That matters: `Get(db, ctx, orgID)` on a two-column key would
otherwise render `WHERE orgId = $1` and address every row in the
organisation — which `Delete` would then remove.

`Patch` cannot take a variadic key because its operations already are
variadic, so the composite form is `PatchKey` with the key as a slice:

```go
MembershipEntity.PatchKey(db, ctx, []any{orgID, userID}, pg.Set(MemberRole, "admin"))
```

## Relations

Declare them once, against the table:

```go
pg.NewRelations(Users).
    HasMany("posts", Posts, UserID, PostUserID).
    HasOne("profile", Profiles, UserID, ProfileUserID)

pg.NewRelations(Posts).
    BelongsTo("author", Users, PostUserID, UserID)
```

The struct holds them in a `dropRel`-tagged field:

```go
type User struct {
    ID    int64
    Name  string
    Posts []Post `dropRel:"posts"`
}
```

Then eager-load. Two spellings, for two situations:

```go
// Checked: the handle is a Go identifier, so a typo will not compile
// and a rename refactors.
var UserPosts = Users.Rel("posts")
users, err := UserEntity.Query(db).Load(UserPosts).All(ctx)

// Dynamic: the set comes from outside the program — an
// ?include=posts,author parameter, a GraphQL selection.
users, err = UserEntity.Query(db).With(req.Include...).All(ctx)
```

`Users.Rel("posts")` resolves at declaration time and panics — listing
what *is* declared — if the name is wrong, so a mistake surfaces at
startup rather than on the first request that eager-loads.

Loading is batched. One extra query per relation edge, whatever the
number of parents, using `WHERE fk IN (…)` over the deduplicated
parent keys. Nested paths and per-edge constraints:

```go
UserEntity.Query(db).LoadRel(UserPosts, func(p *pg.RelConfig) {
    p.Where(PostPublished.Eq(true)).
        OrderBy(PostCreatedAt.Desc()).
        Load(PostComments)
})
```

### The relation you forgot to load

Go has no lazy loading, which is a feature — a field read never fires a
query behind your back. But forget `Load(UserPosts)` and `user.Posts`
is `nil`, which reads exactly like "this user has no posts". The wrong
answer is silent.

Nothing in Go can intercept a struct field read, so drops refuses the
*query* instead:

```go
db := pg.New(drv)
if devMode {
    db = db.StrictLoading()
}

users, err := UserEntity.Query(db).All(ctx)
// error: relation not loaded: "posts" on struct main.User — this query
// never loaded it … Load it with .Load(users.Rel("posts")) …, or say
// the query does not need it with .NoLoad(users.Rel("posts")) …
```

The check is structural: it walks the destination struct against the
table's declared relations and refuses before the SELECT runs, so it
costs no round trip. A query that genuinely does not need the relation
says so, and is let through:

```go
UserEntity.Query(db).Load(UserPosts).NoLoad(UserProfile).All(ctx)
```

Turn it on in development and in tests, where the mistake surfaces as a
failing test rather than a failing request. It is off by default and
changes nothing when off. `Entity.Get` is exempt: it addresses a row by
primary key and has no way to load a relation at all.

### Catching N+1 anyway

If a query loop slips through, the detector will say so:

```go
ctx, finish := pg.WithN1Detector(ctx)
db := db.WithHook(pg.N1Hook)
// ... run the handler ...
if report := finish(2); !report.IsClean() {
    log.Printf("N+1: %v", report.Patterns)
}
```

## Pagination

Keyset, not OFFSET:

```go
page, err := UserEntity.Page(db).
    OrderBy(pg.Asc(UserID)).
    Limit(20).
    After(prevCursor).
    All(ctx)

for _, u := range page.Items { ... }
if page.HasMore {
    next = page.NextCursor
}
```

The cursor is opaque and carries the ordering columns' values, so rows
inserted between two requests cannot shift a result across a page
boundary — which `OFFSET` does routinely on a busy table.

The last `OrderBy` column should be unique (usually the primary key)
so every row has a distinct cursor.

## Cross-cutting concerns

These attach to an entity and then apply to every operation:

```go
UserEntity.
    WithCache(cache, time.Minute).   // read-through, single-flight on misses
    WithAudit(auditLog).             // who-changed-what, same transaction
    ScopeByTenant(UserTenantID).     // every query filtered by ctx tenant
    AuthorizeWith(guard).            // every query filtered by ctx subject
    WithBudget(budget)               // caps rows, args and duration
```

Each is documented in its own file in the `pg` package. They are
PostgreSQL and SQLite only today.

## Stepping around a global filter

A table can carry filters drops AND-s into every statement without the
call site asking — a soft-delete guard, a tenancy axis. They are named,
and a query bypasses them one at a time:

```go
Posts.AddFilter("archived", PostArchived.Eq(false))

// deleted rows too, and still only this tenant's, still not archived
PostEntity.Query(db).IgnoreFilters(pg.FilterSoftDelete).All(ctx)
```

`pg.FilterSoftDelete` is the name `SoftDeleteMixin` registers its guard
under; `pg.FilterTenant` names the predicate `ScopeByTenant` injects,
which a deliberate cross-tenant report can drop the same way.

`Unscoped()` still exists and is the blunt instrument: it drops every
filter the table carries at once, which is what a migration or a
backfill wants and almost never what a query does. It does not reach
the `ScopeByTenant` guard — that one comes from the context rather than
the table, and losing customer isolation as a side effect of asking for
soft-deleted rows is exactly the accident `IgnoreFilters` exists to
prevent.

### How far a filter reaches

A statement carries the filters of the table it is *about* — the
`FROM` of a SELECT, the target of an UPDATE or DELETE. Two consequences
are worth knowing before you rely on one:

- **A joined table contributes nothing.** `db.Select().From(Authors).
  Join(Books, …)` applies `Authors`' filters and not `Books`', so a
  join onto a soft-deleted table sees the deleted rows. A filter is a
  statement about which rows of a table are *the* rows; on the far side
  of a join the query is already saying which rows it wants, and drops
  will not quietly narrow it further. Say it yourself in the `ON`
  clause or a `Where`.
- **An eager-loaded relation does.** Each edge of a `Load` /`With` tree
  is its own SELECT against the related table, so it carries that
  table's filters — including when `RelConfig.Limit` caps the rows per
  parent. A relation is loaded *as* that table, not joined onto this
  one, which is why the two answers differ.

`IgnoreFilters` and `Unscoped` speak only for the statement they are
called on. Neither reaches into an eager-loaded relation's query, and
there is no per-edge bypass: a relation's guards always apply. Load the
related rows with their own query when you need to step around one.
