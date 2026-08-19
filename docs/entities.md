# Entities and CRUD

An entity binds a struct to a table:

```go
var UserEntity = pg.NewEntity[User](Users)
```

It precomputes the column-to-field mapping once, at startup, and checks
it — see [schema.md](schema.md#drift).

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
