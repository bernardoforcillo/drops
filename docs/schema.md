# Declaring a schema

## Columns are typed handles

```go
var (
    Users     = pg.NewTable("users")
    UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
    UserName  = pg.Add(Users, pg.Text("name").NotNull())
    UserAge   = pg.Add(Users, pg.Integer("age").Nullable())
    UserMeta  = pg.Add(Users, pg.JSONB("meta").NotNull())
)
```

Each constructor returns a `*pg.Col[T]` whose `T` is the Go type the
column holds — `int64` for `BigSerial`, `string` for `Text`, `int32`
for `Integer`, `json.RawMessage` for `JSONB`. Builder methods preserve
`T`, so the chain stays typed to the end and `UserAge.Gte(18)` is
checked at compile time.

`pg.Add` registers the column with the table and hands it back, which
is what lets the whole declaration be one `var` block.

## Nullability

Say which it is. `NotNull()` and `Nullable()` are counterparts and the
last one wins; `PrimaryKey()` says `NotNull` implicitly.

`Nullable()` renders nothing in PostgreSQL, SQLite or MySQL DDL — a
column there admits NULL unless it says otherwise, so saying so
changes no CREATE TABLE and produces no migration. What it changes is
what drops will let you bind the column to: `NewEntity` requires the
struct field behind a nullable column to be one that can receive a
NULL. In ClickHouse, where nullability is spelled in the type,
`Nullable()` also rewrites `String` to `Nullable(String)` — same
statement, different rendering.

The Go type parameter stays the *value* type either way. `Eq`, `In`,
`Between` and friends compare against a value and never against NULL,
so a nullable column's comparisons take the same plain `T`:

```go
UserAge.Gte(18)   // int32, whether or not the column admits NULL
```

Test for NULL with `IsNull()` / `IsNotNull()`. Write one with the
typed bindings:

```go
UserAge.SetNull()      // a bound NULL
UserAge.ValPtr(p)      // *int32: *p, or NULL when p is nil
UserAge.ValNull(n)     // sql.Null[int32]: n.V, or NULL when !n.Valid

db.Insert(Users).Row(UserName.Val("Ada"), UserAge.SetNull())
db.Update(Users).Set(UserAge.ValPtr(p)).Where(UserID.Eq(id))
```

All three bind a placeholder rather than splicing the literal token,
so a statement that sometimes writes NULL is the same statement as one
that writes a value — one plan-cache entry, not two — and the NULL
travels through the same argument path a value does, where hooks,
tracers and PII redaction can see it.

Read one into a `*T` or a **named** `sql.Null[T]` field. Never *embed*
an `sql.Null[T]`: that promotes its `Scan` onto the struct, which then
claims to be a single-column scan destination.

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

### Nullability drift

The second half of the same problem. A column the database will hand
back as NULL, bound to a field that cannot receive one, is a query
that works until the first NULL row and then fails inside a scan with
`converting NULL to string is unsupported` and a column index. Nothing
in Go connects the two types — a column's `T` is what its comparisons
take, the destination is a struct field drops reaches by reflection —
so `NewEntity` is the one place both are in scope, and it refuses:

```
drops/pg: NewEntity[User]: table "users": column "bio" is not declared
NOT NULL, so the database will accept NULL, but field Bio is string,
which cannot receive one — either add .NotNull() to the column (and a
migration to enforce it) or make the field *string; if NULL is
genuinely impossible there, say so with pg.AllowNullableColumns("bio")
```

It fires on whether the column admits NULL, not on whether it said so:
a bare `pg.Text("bio")` is exactly the shape that has been accepting
NULLs nobody declared. The rule is one-directional — a NOT NULL column
bound to a `*T` field is the legitimate "distinguish unset from zero"
idiom and passes in silence.

Primary-key columns are exempt however the key was declared. A
composite key written as `Users.PrimaryKey(tenantID, id)` records the
key on the table and leaves the columns' own flags alone, but the
server makes every key column NOT NULL, so the check reads the key
rather than the flag.

A field can receive NULL when `*field` knows how to take a column
value: `*T`, a named `sql.Null[T]`, the `sql.NullString` family, any
type with its own `Scan`, `[]byte`, `json.RawMessage`, `any`. It
cannot when it is a `string`, a number, a `bool`, a `time.Time` or an
array.

The escape hatches mirror the unmapped-column ones:
`pg.AllowNullableColumns("bio", "note")` names the exceptions,
`pg.AllowAnyNullableColumn()` turns the check off wholesale for a
codebase with too many to fix in one commit. Both exist in all four
dialects.

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
    UserAge   = pg.Add(Users, pg.Integer("age").Nullable())
)
```

Typed handles, derived from the struct. Every column states its
nullability, and the field's type is the statement: `*int32` and
`sql.Null[int32]` give `Nullable()`, everything else `NotNull()`. The
`null` tag is the escape hatch for a field whose type cannot say it —
a named type with its own `Scan` method, which a source-level
generator cannot recognise. Add `//go:generate` above the
struct and the two stay in step by construction.

`examples/schemagen` is a working instance, and its test checks both
halves: the entity builds with no exemptions, and the checked-in
generated file still matches what the generator produces today.

Three things the generator refuses rather than guesses: a Go type
outside the mapping needs an explicit `type=` in the tag, a pointer
field tagged `notNull` is a contradiction, and so is `notNull`
together with `null`.

### AutoTable

`pg.AutoTable[User]("users")` derives the table from the same tags at
runtime, with no generation step, and reads nullability off the field
type by the same rule the generator uses. Nothing can drift, but you
get untyped `*pg.Column` handles rather than `*pg.Col[T]`, so
comparisons are no longer checked. Use it for throwaway code and the generator for
code you will keep.

## Tags

| tag option | effect |
|---|---|
| `primaryKey` | PRIMARY KEY |
| `autoIncrement` | serial family / AUTO_INCREMENT |
| `notNull` | NOT NULL |
| `null` | the column admits NULL even though the field's type does not say so |
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
`pg.Diff` against a snapshot, or the drizzle-kit interop. The `drops`
binary drives all of it from the command line: see
[The `drops` CLI](cli.md), and the package docs for `pg/migrate.go`
and `pg/diff.go` for the library underneath.
