# Testing drops

There are two suites, and the split is the point.

## The unit suite

`go test ./...` from the repository root. No services, no network,
fast. It checks the SQL drops *generates*, by comparing it against what
a test author wrote down.

That is a real check and it catches a lot. It also has a blind spot,
and the blind spot has a track record.

## Why the integration suite exists

Six bugs have shipped in which drops generated SQL a server would
reject, or a schema comparison that could never converge. Every one of
them passed its unit tests, because the expected string was written by
the same reasoning that wrote the code:

| | |
|---|---|
| `CREATE INDEX` with table-qualified column names | PostgreSQL: syntax error |
| ClickHouse `CREATE TABLE` with qualified names in `ORDER BY` | cannot resolve a table that does not exist yet |
| PostGIS helpers emitting every placeholder twice | syntax error, unconditionally |
| Composite `PRIMARY KEY` rendered inline on each column | "multiple primary keys for table are not allowed" |
| SQLite `PRIMARY KEY` without `NOT NULL` | a `TEXT PRIMARY KEY` accepts and stores a NULL key |
| A table with no `CHECK` constraints diffing against itself | a full table rebuild on every deploy |

In three of those cases the unit test had pinned the *broken* output as
the expectation.

No amount of care fixes this from inside the same head. The only test
that cannot make the mistake is one where a server parses the
statement. That is `integration/`.

## Running it

`integration/` is a separate Go module, so the drivers it needs — pgx,
go-sql-driver/mysql, clickhouse-go — cannot reach a user's build. drops
itself still has no dependencies, and the CI `tidy` job still asserts
it. `cmd/drops` is a third module for the same reason: the binary
links pgx, and nothing a user imports can reach it.

SQLite's driver is pure Go, so a third of the suite runs with nothing
installed:

```sh
go test -C integration ./...
```

The rest needs servers:

```sh
docker compose -f integration/docker-compose.yml up -d

DROPS_PG_DSN='postgres://drops:drops@localhost:5433/drops?sslmode=disable' \
DROPS_MYSQL_DSN='drops:drops@tcp(localhost:3307)/drops?parseTime=true' \
DROPS_CLICKHOUSE_DSN='clickhouse://localhost:9001/default' \
DROPS_QDRANT_URL='http://localhost:6334' \
go test -C integration ./...
```

### Without Docker

A container with no Docker daemon is a normal place to work, and the
alternative there is running no integration tests at all. `make
servers-up` starts PostgreSQL and MySQL from distribution packages
instead, on the same offset ports, and prints the two DSNs:

```sh
. scripts/local-servers.sh
go test -C integration ./...
make servers-down
```

It needs `postgresql-16`, `postgresql-16-pgvector`,
`postgresql-16-postgis-3` and `mariadb-server`. Two things differ from
the compose services and both are worth knowing:

- It gives you **MariaDB**, not MySQL 8.4. Where the two disagree —
  `FOR SHARE` against `LOCK IN SHARE MODE`, CHECK enforcement, the
  rules for indexing a `TEXT` column — a test that passes here can
  still fail in CI. That is not a flaw in the setup; drops claims to
  serve both, so a difference between them is a thing to pin rather
  than to smooth over.
- pgvector's version is whatever the distribution ships. The `<+>`,
  `<~>` and `<%>` operators arrived in 0.7.0, so on an older package
  the tests that need them skip.

ClickHouse and Qdrant are not covered — neither ships a distribution
package worth depending on. Leave their DSNs unset and those tests
skip.

### Skipping

A test whose DSN is unset **skips**, with a message saying how to start
the servers. That is deliberate: a contributor without Docker should
still get real signal from the SQLite half rather than a wall of
failures about services they never asked for.

CI sets `DROPS_REQUIRE_ALL=1`, which turns a missing DSN into a
failure. A silently skipped integration job is worse than no
integration job, because it reports green.

## What belongs in which suite

Put it in the **unit** suite when the question is *what SQL should we
generate* — operator precedence, clause order, which columns end up in
a `SET`. These are cheap, and a string comparison states the intent
precisely.

Put it in the **integration** suite when the question is *would a
server accept this, and does it mean what we think*. Concretely:

- Any DDL. Grammar is not something to reason about from memory.
- Anything that reads the catalogue back — introspection, diffing,
  drift detection. A comparison against a live schema cannot be tested
  against a fake one.
- Any claim that a constraint is *enforced* rather than merely
  rendered. `UNIQUE` in the emitted string proves nothing.
- Anything that depends on the driver rather than the SQL — MySQL's
  `LastInsertId`, which stands in for the `RETURNING` it does not have.
- Anything whose correctness is a sequence rather than a statement:
  that keyset pagination walks every row exactly once, that an outbox
  event dies with its transaction, that a migration converges on a
  second run.
- Anything where a server's semantics differ from the obvious reading.
  MySQL's upsert fires on any unique index, not just the primary key.
  Qdrant's score is a similarity for some metrics and a raw distance
  for others. Neither is visible in a rendered string.

## Writing a test that fails usefully

Print the SQL. A failure that says only `pq: syntax error at or near
")"` costs an hour; one that prints the statement costs a minute. The
helpers in each backend file do this — follow them:

```go
func exec(t *testing.T, db *sqlite.DB, e drops.Expression) {
	t.Helper()
	if _, err := db.ExecExpr(context.Background(), e); err != nil {
		text, args := drops.StringWithDialect(sqlite.Dialect, e)
		t.Fatalf("SQLite rejected the statement: %v\n%s\nargs: %v", err, text, args)
	}
}
```

Name tables with `integration.UniqueName(t, prefix)`. Tests share one
database, and a leftover table poisons a later run — this way a
collision is impossible and a stray table names the test that left it.
