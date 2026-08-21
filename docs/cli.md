# The `drops` CLI

The migration loop — declare in Go, diff, apply, detect drift — is a
set of library functions. This is the front end to them.

```
go install github.com/bernardoforcillo/drops/cmd/drops@latest
```

```
drops generate --schema ./db/schema --name add_articles
drops migrate
drops push --schema ./db/schema --dry-run
drops drift --schema ./db/schema
drops lint ./...
```

Every command takes `-h`. Connection strings come from `--dsn`, else
`$DROPS_PG_DSN`, else `$DATABASE_URL`; the binary carries pgx, so
there is no driver to choose and nothing to configure.

`cmd/drops` is a Go module of its own, which is how it can carry a
driver at all: drops the library has no dependencies and CI asserts
it, so the binary that needs one lives outside that promise. See
[the module, and what it costs](#the-module-and-what-it-costs) — one
command, `push`, needs a driver in *your* module too.

## The convention

The schema lives in your Go code, and a schema built out of
`pg.NewTable` and `pg.Add` is a *value* — there is nothing on disk to
parse. So the commands that need one write a small program into your
module, have it import your schema package and call a function by
name, run it with `go run`, and read the answer back. The compiler
that evaluates your declarations is the same compiler your application
uses, which is the only way the answer is the same answer.

The function is:

```go
// db/schema/schema.go
package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())
)

func Schema() *pg.Schema {
	return pg.NewSchema(Users)
}
```

A table you declare and do not register in `Schema()` is invisible to
drops — which is how a table another tool owns stays out of its way.
A package without the function is reported as such, with the function
to add.

Two consequences worth knowing before you script this:

- `generate`, `push` and `drift` need a Go toolchain and have to run
  from inside your module, as does `status --schema`. On a deploy host
  with only the binary, `migrate`, `status` without `--schema`,
  `baseline` and `pull` still work — they read the database and the
  migration directory, not your source.
- The program is written into a temporary directory inside your module
  and removed afterwards.
- `push` is the one mode where that program opens a database
  connection, so it is the one command that needs
  `github.com/jackc/pgx/v5` in your `go.mod`. It says so if it is
  missing. `generate`, `drift` and `status --schema` compile the same
  program without the connection and need nothing but drops.

`examples/cli` in this repository is a schema package in full, with a
foreign key, a check constraint and an index.

## The commands

### `drops generate`

Diffs the Go schema against the newest snapshot in the migration
directory and writes `<dir>/NNNN_<name>.sql`, its rollback script
`NNNN_<name>.down.sql`, `<dir>/meta/NNNN_snapshot.json` and an updated
journal. It touches no database. `--safe` wraps the DDL in
`IF [NOT] EXISTS`; `--no-down` skips the rollback script.

Destructive statements are reported, not refused: generating a
migration is the review step, and the file is there to be read.

#### Renames

A diff compares two snapshots. It can see that `email` is gone and
`emailAddress` has arrived; it cannot see whether that was one rename or
a drop and an add, and the two produce migrations that differ by the
whole contents of the column. So `generate` stops:

```
drops/pg: this schema change could be a rename or a drop-and-add and drops will not guess:
  column "email" on table "users" is gone and "emailAddress" (text) has appeared
    rename it:      --rename-column users.email=emailAddress
    or drop it:     --drop-column users.email
```

Stopping is the default, and it is not conditional on anything: a
script, a hook and CI all get the refusal and an exit code of 3, because
the answer drops would otherwise have to invent is the `DROP COLUMN`.
`--interactive` asks instead, one question per pair on stdin, and
anything that is not a `y` is a no.

State the answer on the command line and the run goes through without
either:

- `--rename-column users.email=emailAddress`
- `--rename-table users=people`
- `--drop-column users.email` — no, it really is going
- `--drop-table users`

Every answer, typed or prompted, is written to
`<dir>/meta/_renames.json` alongside the snapshots. The next run reads
it, so the question is asked once; committing the file is what makes a
colleague's `generate` — and CI's — produce the same migration as
yours. drizzle-kit reads the journal and the snapshots by name and
ignores the rest, so the two tools still share a directory.

A pair is only offered when the types are in the same family, so a
rename that widens `varchar(120)` to `text` is still recognised while an
unrelated dropped timestamp and added boolean are not. A rename that
also changes the type produces the `RENAME` and the type change, in that
order.

Two tables are paired when they agree on at least half their columns, so
a table renamed in the same commit that added a column to it — or
renamed a column inside it — is still offered rather than dropped and
rebuilt. A rename inside a renamed table is asked *after* the table
question is answered, because until then the two snapshots file the
table under different names and nothing in it pairs with anything. So a
commit that renames both asks twice; `--interactive` keeps asking until
there is nothing left, and a scripted run states `--rename-table` first,
gets the column question, and states `--rename-column` on the run after.

### `drops migrate`

Applies every migration in the journal that the database has not
recorded, each in its own transaction, using drizzle-orm's protocol —
the SHA-256 of each file, recorded in `drizzle.__drizzle_migrations`.

`drops migrate down` rolls the most recent one back using its
`.down.sql`, and removes its row in the same transaction. A migration
set that came from drizzle-kit has no rollback scripts, and the error
says so rather than inventing the inverse of somebody else's SQL.

### `drops push`

Introspects the live database, diffs it against the Go schema, and
applies the difference with no migration file in between —
drizzle-kit's `push`. Good for development, wrong for production,
where a reviewable file is the point.

`--dry-run` prints the plan and applies none of it. Push also reports
what it saw and declined to act on: an index the database has that
your schema never declared, an index it cannot describe, an expression
the server would not respell. See `pg/push.go` for the full list.

### `drops drift`

Prints the statements that would bring the database up to the schema,
and the ones that would bring the schema up to the database, and exits
**3** if there are any. That exit code is what makes it a CI gate.

Introspection does not read everything the schema layer can declare —
see the note the command prints — so a schema using an unread feature
reports a difference on every run.

### `drops pull`

Introspects a database into a Go schema file: `drops pull --out
./db/schema/schema.go`. The output is a starting point to edit and
commit, not a generated artefact to leave alone. It declares what it
can and comments on what it cannot, rather than rendering something
close: a partial index whose predicate exists only as catalogue text
would come back as an index over the whole table, which is a different
index under the same name.

drops derives foreign-key names itself, so a constraint PostgreSQL
named `<table>_<column>_fkey` is dropped and re-created under the
derived name the first time the pulled schema is pushed. The
constraint is the same constraint.

### `drops baseline`

Adopts a database that already has its tables: writes the migration
that would have created what is there, records the snapshot beside it,
and marks it applied without running a statement. After that
`generate` has a previous snapshot to diff against and `migrate` has
somewhere to start. It refuses a directory that already has a history.

### `drops status`

The journal, entry by entry, with what is applied and what is
pending — plus any migration hash in the database that no file in the
directory accounts for, which means a file was edited after it was
applied or came from another branch. Pass `--schema` and it reports
drift as well.

### `drops lint`

Reads the source rather than the database: a DELETE or UPDATE executed
with nothing to bound it, a read of every row of a table, a relation
eager-loaded once per iteration of a loop. Exits 3 on a finding.

```
drops lint ./...
```

The rules are `go/analysis` analyzers, so they also run under
golangci-lint or `go vet -vettool`. What each one will and will not
say — the false-positive story matters more than the true-positive one
— is in [lint.md](lint.md).

## Destructive changes

`push`, `migrate` and `migrate down` run their statements through
`pg.AnalyzeMigration` before applying anything. A statement that
destroys data or an object — `DROP TABLE`, `DROP COLUMN`, `TRUNCATE`,
`DROP TYPE`, `ALTER TYPE ... DROP VALUE` — stops the command, which
prints each statement it is holding back and exits 3.
`--allow-destructive` runs them.

The gate reads the whole run at once, so one destructive statement in
the last pending migration stops the harmless ones queued ahead of it
too: `drops migrate` is a single reviewable unit, and applying half of
it would leave the history describing a database that does not exist.

The classification is about the SQL, not its formatting — a statement
a generator wrapped across lines is the same statement.

Statements that are only expensive — a table rewrite, an index built
without `CONCURRENTLY`, a `SET NOT NULL` — are printed as warnings and
applied. Refusing those too would train everyone to pass the flag by
reflex, and then it would mean nothing.

## Schemas other than `public`

`push`, `drift`, `pull`, `baseline` and `status` take `--pg-schema` to
say which PostgreSQL schema to introspect. It defaults to `public`.

Introspection is schema-aware; the diff is not. `pg.Diff` writes
unqualified identifiers, so a `CREATE TABLE "users"` it produces lands
wherever `search_path` points no matter which schema was read — and
that is true whether the schema was named with `--pg-schema` or
declared in Go with `pg.NewSchemaTable("reporting", "users")`. **A
schema-qualified table declaration is not yet honoured by the DDL
side.** Until `Diff` qualifies, a non-`public` schema is reachable only
through `search_path`.

What that means per command:

- `push` and `baseline` **refuse** a `--pg-schema` other than `public`.
  Accepting it would introspect `reporting`, find it empty, create
  every table in `public`, report four statements applied, and wedge on
  "relation already exists" on the next run.
- `drift` and `status` accept it and are correct, *provided the Go
  schema declares the same schema* — `pg.NewSchemaTable("reporting",
  …)` against `--pg-schema reporting` reports in sync. An unqualified
  Go schema against a named `--pg-schema` compares two different keys
  and reports every table as both missing and extra.
- `pull` accepts it and renders what is there. The tables it writes are
  `pg.NewTable`, unqualified, so pushing the result puts them in
  `public`.

To manage a non-`public` schema today, leave `--pg-schema` alone and
connect with a DSN whose `search_path` is that schema:

```
drops push --schema ./db/schema \
  --dsn 'postgres://user@host/db?options=-csearch_path%3Dreporting'
```

That is checked against a live server: the tables land in `reporting`.

## Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 1 | failure — unreachable database, bad SQL, a file that would not parse |
| 2 | the command line was wrong |
| 3 | the command ran and the answer was no: drift found, or changes refused |

## The module, and what it costs

`cmd/drops` has its own `go.mod`. The library it front-ends has no
dependencies — that is drops's stated differentiator, and the CI
`tidy` job proves it by diffing the tree after `go mod tidy`. A binary
that migrates a database has to open a connection, though, and for a
year this one did it by speaking the PostgreSQL v3 wire protocol by
hand, SCRAM-SHA-256 included, because it could not link pgx from
inside the root module. Splitting the module deleted ~1,500 lines of
hand-written, security-critical network code and replaced them with
`pgx` behind `drops/stdlib`, the adapter drops already ships.

Three things follow, and all three are the bill:

- **`push` needs pgx in your module.** Evaluating a Go schema means
  running Go inside *your* module, and `go run` resolves that
  program's imports there rather than in the binary's own module. The
  binary's pgx is not in scope for it. `drops push` therefore checks
  first and tells you to `go get github.com/jackc/pgx/v5`; nothing
  else the CLI does is affected.
- **The CLI needs a newer toolchain than the library.** drops itself
  still builds on Go 1.22. `cmd/drops` follows pgx, which requires
  1.25.
- **Releasing it takes an extra step.** See below.

### Cutting a release

A nested module carries the directory in its tag. The version of
`github.com/bernardoforcillo/drops/cmd/drops` that corresponds to
`v0.7.0` of the library is tagged

```
cmd/drops/v0.7.0
```

and *not* `v0.7.0` — that tag names the root module, whose contents no
longer include the CLI. `go install …/cmd/drops@latest` finds the
nested module's own tags and nothing else.

Two things have to be true before that tag is pushed:

1. **`cmd/drops/go.mod` must not contain a `replace`.** In the
   checkout it replaces `github.com/bernardoforcillo/drops` with
   `../../`, so the binary under test is built against the library in
   the tree. `go install pkg@version` refuses a module whose `go.mod`
   replaces anything — the install would fail with *"the go.mod file
   for the module providing named packages contains one or more
   replace directives"*. Remove the line and require the version of
   the library you are releasing alongside it.
2. **Tag the root module first.** The require in step 1 has to name a
   version that exists, and `go mod tidy` has to be able to resolve it
   to fill in `go.sum`.

So: tag `v0.7.0`, swap the replace for `require
github.com/bernardoforcillo/drops v0.7.0`, run `go mod tidy` in
`cmd/drops`, commit, tag `cmd/drops/v0.7.0`. Then put the replace
back for development.

## drizzle-kit interoperability

`drops migrate` applies a migration set drizzle-kit generated, and
drizzle-orm applies one `drops generate` wrote. The directory layout,
`meta/_journal.json`, the SHA-256 of each file, the statement
breakpoints and the `drizzle.__drizzle_migrations` table are the same
on both sides, so the two runtimes can share a database without
either learning about the other.

The rollback direction is drops's addition. drizzle-kit has no
concept of one.
