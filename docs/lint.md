# `drops lint`

Three mistakes keep reaching production, and every one of them is
visible in the source before it runs:

```
drops lint ./...
```

```
db/users.go:118:9: [unfilteredwrite] DELETE on Users has no Where: it removes
  every row. Add a Where, or //drops:lint ignore unfilteredwrite if that is
  the intent.
```

Exit 0 when clean, 3 when it finds something — the same answer `drift`
gives, so it drops into a pipeline without a wrapper.

## Why this is not an ESLint plugin

Drizzle ships one, and its flagship rule is the same first rule as
here. It has to guess: to ESLint, any object with a `.delete()` and no
`.where()` looks alike, and telling a query builder from a file handle
means configuring the plugin into type information it does not have by
default.

Go hands a `go/analysis` pass the type checker's answers whether it
asks or not. The rules below therefore know that a value is a
`*pg.DeleteBuilder` and not something else that happens to have those
method names; they know which package-level `*pg.Table` a statement
targets; and they can attach a *fact* to that table — "this one is
small" — and read it back in another package that only imports the
schema.

The rules are ordinary analyzers, exported from
`github.com/bernardoforcillo/drops/cmd/drops/dropslint`. `drops lint`
is a convenience; they also run under `golangci-lint`, a
`unitchecker`, or `go vet -vettool`.

## The rules

### `unfilteredwrite` — a DELETE or UPDATE with no WHERE

```go
db.Delete(Users).Exec(ctx)                    // reported
db.Delete(Users).Where(UserID.Eq(id)).Exec(ctx)  // fine
```

A `Limit` bounds a write too, where the dialect allows one — MySQL's
`DELETE … ORDER BY … LIMIT 1000` is how a large table is emptied in
batches, and pushing the author off it towards the single enormous
transaction would be the wrong advice.

`Unscoped()` is not a bound. It *drops* the table's global filters; it
adds nothing.

One shape it cannot see: `Where` accepts nil predicates and discards
them, so `Where(maybeNil)` can render no WHERE at all. The call is
there in the source, and the rule takes it at its word.

### `unboundedread` — a read of every row

```go
UserEntity.Query(db).All(ctx)                 // reported
UserEntity.Query(db).Limit(100).All(ctx)      // fine
```

This is the rule most likely to cry wolf, and a linter that cries wolf
gets switched off entirely — taking the first rule with it. So it asks
for a great deal before it says anything:

- **Whole rows only.** An entity query, a `Find`, or a `Select` given
  no projection at all (which renders `SELECT *`). A hand-written
  `Select` with columns is as likely to be an aggregate or a
  subquery's driving table as a full scan, and nothing in the source
  says which.
- **`All` only.** `One` returns a row, `Count` returns a number,
  `Stream` and `Rows` hand rows back as they arrive. `All` is the call
  that pulls a whole table into memory — and it is exactly the call
  `pg.Budget`'s `MaxRows` renders a `LIMIT` for.
- **Only when the row count is not already capped.** An entity whose
  `Budget` sets `MaxRows` has its `LIMIT` written for it at run time;
  the linter has nothing to add.
- **Any predicate counts, not just `Where`.** `WhereHas`,
  `WhereHasRel`, `WhereDoesntHave` and `WhereDoesntHaveRel` render a
  `WHERE EXISTS (…)` around a subquery over a relation, which narrows
  the rows returned exactly as a column predicate does.
- **Only when it can name the table.** The target has to be a
  package-level `*Table` or `*Entity` — which is where a drops schema
  lives. A table built inside a function is one the analyzer knows
  nothing about, *including whether it is small*, and it is also the
  one kind of table nobody can attach a budget or a directive to. A
  finding there would be a finding with no fix.

A table that really is small says so once, on its declaration:

```go
// Currencies holds one row per ISO 4217 code and never many more.
//
//drops:lint lookup
var (
	Currencies   = pg.NewTable("currencies")
	CurrencyCode = pg.Add(Currencies, pg.Text("code").PrimaryKey())
)
```

That travels as an analysis fact, so every package that imports the
schema knows it, and an entity declared over the table inherits it.

### `loopload` — an eager load inside a loop

```go
for _, org := range orgs {
	users, err := UserEntity.Query(db).      // reported
		Where(UserOrgID.Eq(org)).
		With("posts").
		All(ctx)
}
```

The N+1, caught at build time.
`pg.WithN1Detector` catches the same shape at run time
by counting repeated SQL; this asks a narrower question so it can
answer without running anything — not "did the same statement fire
repeatedly", which only the traffic knows, but "is a relation loaded on
every pass of a loop", which the source knows. Run both.

Both halves have to be inside the loop: the eager load *and* the call
that executes it. Assembling a load list in a loop and running the
query once afterwards is one query, and the rule says nothing about it.
Neither does it complain about a paging loop: the relation load runs
once per page, which is what paging is. An `Offset` says so outright,
and so does a `Limit`, because keyset paging — the way `AfterCursor`
walks a table — moves a `Where` rather than an `Offset` and would
otherwise be indistinguishable from the mistake.

One shape it gets wrong, and the reason the directive exists: a retry
loop that eager-loads runs the query once on the happy path, but the
source cannot tell that from an iteration over rows. Say so on the
query — `//drops:lint ignore loopload — retry, not iteration`.

## How far a value is followed

All three rules follow a builder **within one function body and no
further**, and inside that body they are flow-insensitive: a `Where`
called anywhere on the value counts, even under an `if` the value may
not take. That misses

```go
q := db.Delete(Users)
if narrow {
	q = q.Where(pred)
}
q.Exec(ctx)
```

which is a real bug — in exchange for never mis-reading the same shape
written correctly. The rules also go silent the moment a builder leaves
the function: returned, passed to a call, stored in a struct, or
aliased to a second name. A helper that hands back a half-built
`*pg.DeleteBuilder` has already done its `Where` somewhere this
analysis cannot see.

A statement is only ever reported where it is **executed** — `Exec`,
`All`, `One`, `Stream`. Rendering one with `ToSQL` is not running it,
which is why the dialect packages' own tests, which render unfiltered
deletes by the thousand, are not findings.

## Saying it on purpose

```go
//drops:lint ignore unfilteredwrite — the queue is drained wholesale
db.Delete(Jobs).Exec(ctx)
```

The directive is honoured on the reported line, on the line above it,
and in the enclosing function's doc comment. Name several rules with
commas; name none to silence every rule at that position. Everything
after the rule list is the reason, in your own words — which is where a
reviewer will look for it, and the reason this is a comment rather than
a method on the builder. A method would put a linter's vocabulary into
the runtime API of four dialect packages and could not carry a
sentence.

## Flags

| flag | |
|---|---|
| `-off <rules>` | comma-separated rules to skip. An unknown name is an error, not a silent no-op. |
| `-json` | findings as JSON, one object per finding |
| `-context <n>` | lines of source either side of each finding; `-1` prints none, `0` (the default) prints the offending line |

Operands are package patterns and default to `./...`. A package that
does not type-check is reported as such and nothing is analysed — the
rules read types, not text, so a clean run over code that never
compiled would be a lie.

## What it says about drops itself

Across `pg`, `sqlite`, `mysql`, `clickhouse`, `examples`, the CLI and
the integration suite: clean, with two directives in the integration
suite where a test deliberately rewrites a one-row table.

Getting there moved the rules three times. The first run reported a
MySQL batched `UPDATE … LIMIT 2` — the rule was wrong, and `Limit` now
bounds a write. It also reported 108 unit tests that read three-row
fixture tables built inside the test function — the rule was wrong
again, in a more interesting way: it was answering a question about
table size for tables whose size it had no way to know. Requiring a
nameable target fixed both that and the noise.

The third came from queries the tree does not yet contain: a
`WhereHas` read and a keyset-paging loop were both reported, and both
are correct code. Predicates other than `Where` now count, and a
`Limit` marks a page the way an `Offset` does.
