# Declaring a schema

## Columns are typed handles

```go
var (
    Users     = pg.NewTable("users")
    UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
    UserName  = pg.Add(Users, pg.Text("name").NotNull())
    UserAge   = pg.Add(Users, pg.Integer("age"))
    UserMeta  = pg.Add(Users, pg.JSONB("meta"))
)
```

Each constructor returns a `*pg.Col[T]` whose `T` is the Go type the
column holds — `int64` for `BigSerial`, `string` for `Text`, `int32`
for `Integer`, `json.RawMessage` for `JSONB`. Builder methods preserve
`T`, so the chain stays typed to the end and `UserAge.Gte(18)` is
checked at compile time.

`pg.Add` registers the column with the table and hands it back, which
is what lets the whole declaration be one `var` block.

## Constraints

```go
pg.Text("email").NotNull().Unique()
pg.Integer("views").NotNull().Default("0")
pg.BigInt("authorId").References(UserID, pg.OnDelete("CASCADE"))
pg.Integer("version").NotNull().OptimisticLock()
pg.Text("ssn").AsPII()
```

`OptimisticLock` marks the version column: `Entity.Update` then guards
on it and bumps it, returning `ErrStaleObject` when another
transaction got there first. `AsPII` marks the column so hooks and
logs see a redaction marker instead of the value.

## The two-declarations problem

A schema declared this way lives in two places: the column variables
above, and the struct rows scan into.

```go
type User struct {
    ID    int64
    Name  string
    Email string
}
```

Nothing in Go connects them. Rename `Email` to `Emial` and the code
still compiles, the schema still declares an `email` column — and every
INSERT and UPDATE silently stops writing it. That is the failure mode
this section is about, and drops addresses it from both ends.

### Drift

`pg.NewEntity` refuses to build an entity whose table has a column no
struct field binds to:

```
drops/pg: NewEntity[User]: table "users" has column(s) "email" with no
matching struct field, so they would be silently dropped from every
INSERT and UPDATE; unbound struct field(s): Emial — a typo in a field
name or `drop:` tag looks like the cause; if the database owns it, say
so with pg.AllowUnmappedColumns("email")
```

It panics rather than returning an error because schemas are declared
in package `init` blocks: startup is where bad configuration is cheap
to fix.

Some columns legitimately have no field — one a trigger maintains, a
generated column, a counter another service owns. Name them:

```go
pg.NewEntity[User](Users, pg.AllowUnmappedColumns("searchVector"))
```

An exemption you have to type is one you have thought about. Columns
drops itself writes — the soft-delete marker, mixin-maintained
timestamps — are exempt automatically.

The same check exists in `sqlite`, `mysql` and `clickhouse`.

### Generating the declaration instead

The check catches drift. Generating the schema from the struct makes
it unrepresentable:

```go
//drops:schema table=Users name=users
type User struct {
    ID    int64  `drop:"id,primaryKey,autoIncrement"`
    Email string `drop:"email,notNull,unique"`
    Name  string `drop:"name,notNull"`
    Age   *int32 `drop:"age"`
}
```

```sh
go run github.com/bernardoforcillo/drops/cmd/dropsgen -schema models.go
```

produces `models_drops_schema.go`:

```go
var (
    Users     = pg.NewTable("users")
    UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
    UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())
    UserName  = pg.Add(Users, pg.Text("name").NotNull())
    UserAge   = pg.Add(Users, pg.Integer("age"))
)
```

Typed handles, derived from the struct. Add `//go:generate` above the
struct and the two stay in step by construction.

`examples/schemagen` is a working instance, and its test checks both
halves: the entity builds with no exemptions, and the checked-in
generated file still matches what the generator produces today.

Two things the generator refuses rather than guesses: a Go type outside
the mapping needs an explicit `type=` in the tag, and a pointer field
tagged `notNull` is a contradiction.

### AutoTable

`pg.AutoTable[User]("users")` derives the table from the same tags at
runtime, with no generation step. Nothing can drift, but you get
untyped `*pg.Column` handles rather than `*pg.Col[T]`, so comparisons
are no longer checked. Use it for throwaway code and the generator for
code you will keep.

## Tags

| tag option | effect |
|---|---|
| `primaryKey` | PRIMARY KEY |
| `autoIncrement` | serial family / AUTO_INCREMENT |
| `notNull` | NOT NULL |
| `unique` | UNIQUE |
| `default=<sql>` | raw DEFAULT clause |
| `version` | optimistic-lock column |
| `pii` | redact in hooks and logs |
| `type=<sql>` | override the derived SQL type |
| `-` | skip the field entirely |

## DDL

```go
db.ExecExpr(ctx, pg.CreateTableIfNotExists(Users))
db.ExecExpr(ctx, pg.NewIndex("users_email_idx", Users, UserEmail).Unique())
db.ExecExpr(ctx, pg.DropTableIfExists(Users))
```

For anything beyond a fresh create, use the migration tooling —
`pg.Diff` against a snapshot, or the drizzle-kit interop. That is a
separate topic; see the package docs for `pg/migrate.go` and
`pg/diff.go`.
