# Getting started

## Install

```sh
go get github.com/bernardoforcillo/drops
```

drops has no dependencies. It also ships no database driver: it talks
to whatever you already use through a small interface, so you bring
your own.

```sh
go get github.com/jackc/pgx/v5          # PostgreSQL
go get github.com/go-sql-driver/mysql   # MySQL
go get modernc.org/sqlite               # SQLite, cgo-free
```

## Connect

Any `*sql.DB` becomes a drops connection through the `stdlib` adapter:

```go
import (
    "database/sql"

    _ "github.com/jackc/pgx/v5/stdlib"
    "github.com/bernardoforcillo/drops/pg"
    "github.com/bernardoforcillo/drops/stdlib"
)

sqlDB, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
if err != nil {
    return err
}
db := pg.New(stdlib.New(sqlDB))
```

`pg.New` takes a `drops.Driver`, which is three methods — `Exec`,
`Query`, `Begin`. The `stdlib` adapter implements it over
`database/sql`; a pgx pool or a custom connection can implement it
directly.

Connection pooling, timeouts and TLS stay where they already are, in
the driver. drops does not wrap them.

## Declare a schema

A table is a variable, and so is every column:

```go
var (
    Users     = pg.NewTable("users")
    UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
    UserName  = pg.Add(Users, pg.Text("name").NotNull())
    UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())
    UserAge   = pg.Add(Users, pg.Integer("age").Nullable())
)
```

`pg.Add` returns a `*pg.Col[T]` — a column handle that knows its Go
type. `UserAge` is a `*pg.Col[int32]`, which is what makes the next
section work.

Every column says whether it can be NULL, and saying nothing is not an
option once an entity is declared over the table: `NewEntity` refuses a
column that admits NULL bound to a field that cannot hold one, because
that program works until the first NULL row and then fails on the read
path. `age` is `.Nullable()` above and `Age` is a `*int32` below; they
agree, so it passes.

Create the table:

```go
_, err := db.ExecExpr(ctx, pg.CreateTableIfNotExists(Users))
```

## Query

```go
type User struct {
    ID    int64
    Name  string
    Email string
    Age   *int32
}

var users []User
err := db.Select().
    From(Users).
    Where(UserAge.Gte(18)).
    OrderBy(UserName.Asc()).
    Limit(50).
    All(ctx, &users)
```

`UserAge.Gte(18)` compiles. `UserAge.Gte("18")` does not — the column
handle carries its type, so the comparison is checked before the query
ever runs. That is the difference between this and a string-based
builder.

Rows scan into your struct by field name (`Name` ↔ `name`), or by an
explicit `drop:"..."` tag when the two differ.

When the result is not a table's row — a join, an aggregate, a
projection — name the type at the call instead of declaring a
destination:

```go
type authorPosts struct {
    Author string
    Posts  int64
}

top, err := drops.All[authorPosts](ctx, db.
    Select(UserName.As("author"), pg.As(pg.Count(PostID), "posts")).
    From(Users).
    Join(Posts, PostUserID.EqCol(UserID)).
    GroupBy(UserName))

n, err := drops.One[int64](ctx, db.Select(pg.Count(UserID)).From(Users))
```

See [entities.md](entities.md#queries-no-entity-describes) for how this
sits next to the entity's own typed `Query`.

## Entities

An entity binds a struct to a table and gives you CRUD without writing
statements:

```go
var UserEntity = pg.NewEntity[User](Users)

u := User{Name: "Ada", Email: "ada@example.com"}
if err := UserEntity.Create(db, ctx, &u); err != nil {
    return err
}
// u.ID is now populated.

u.Name = "Ada Lovelace"
if err := UserEntity.Update(db, ctx, &u); err != nil {
    return err
}

found, err := UserEntity.Get(db, ctx, u.ID)
_, err = UserEntity.Delete(db, ctx, u.ID)
```

`NewEntity` checks at startup that every column has a struct field to
bind to, and panics with a specific message if one does not — see
[schema.md](schema.md#drift) for why that check exists and how to
waive it deliberately.

## Transactions

```go
err := db.InTx(ctx, func(tx *pg.DB) error {
    if err := UserEntity.Create(tx, ctx, &u); err != nil {
        return err   // rolls back
    }
    return AccountEntity.Create(tx, ctx, &acct)
})
```

The callback receives a `*pg.DB` bound to the transaction. Returning an
error rolls back; so does a panic, which is then re-raised.

## Where to go next

- [Declaring a schema](schema.md) — types, constraints, and generating
  the declaration from your struct so it cannot drift.
- [Entities and CRUD](entities.md) — relations, composite keys,
  pagination.
- [Choosing a dialect](dialects.md) — if you are not on PostgreSQL.
