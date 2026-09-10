# drops documentation

drops is a SQL toolkit for Go with no dependencies. It gives you typed
schema declarations, a query builder that checks your comparisons at
compile time, and entity CRUD — across PostgreSQL, MySQL, SQLite,
ClickHouse and Qdrant.

## Start here

| | |
|---|---|
| [Getting started](getting-started.md) | Install, connect, first query, first entity. Twenty minutes. |
| [Declaring a schema](schema.md) | Tables, columns, types — and how to keep the schema and the struct from drifting apart. |
| [Entities and CRUD](entities.md) | Get / Create / Update / Delete, composite keys, relations, pagination. |
| [The `drops` CLI](cli.md) | generate, migrate, push, drift, pull, baseline, status — and how a CLI reads a Go schema. |

## By topic

| | |
|---|---|
| [Choosing a dialect](dialects.md) | What each of the five backends gives you, and what it does not. Read this before porting a schema. |
| [Vector search](vector-search.md) | One query vocabulary over pgvector, ClickHouse and Qdrant. |
| [OLTP → OLAP → vector](mirror.md) | Keeping one table mirrored across all three, without three schema declarations. |
| [Change data capture](cdc.md) | Reading the write-ahead log instead of asking every writer to write twice — and the slot that fills your disk if you look away. |
| [Tenancy](tenancy.md) | The predicate that cannot be forgotten: where it is declared, which clause it lands in, and the four things the test suite enforces about it. |
| [Caching](caching.md) | Why a query cache needs more than a TTL, and the one rule that makes topic invalidation safe. |
| [Query plans](plans.md) | Measuring how selective a predicate is, hinting the planner, and holding the plan to a test. |
| [Running it](operations.md) | The SQLSTATEs that mean "do something else", draining a node for failover, durable jobs, results that will not fit in memory, and which driver you have to connect through for COPY and LISTEN to answer. |
| [Testing](testing.md) | The two suites, why the second exists, and which of your tests belongs in which. |
| [`drops lint`](lint.md) | Three query mistakes caught at build time, and the false-positive story for each. |

## Reference

Package documentation lives with the code, at
[pkg.go.dev/github.com/bernardoforcillo/drops](https://pkg.go.dev/github.com/bernardoforcillo/drops).
Every package has runnable examples; `go doc` works offline.

The [readme](../readme.md) is the tour — what exists, in one page. These
documents are the explanation.

## A note on what is not here

drops is pre-1.0 and the surface is not evenly deep. PostgreSQL has the
most (migrations, outbox, saga, event store, audit, tenancy, geo,
money); SQLite has most of it; MySQL has the schema and query surface,
relations and the tenancy scope layer, but not audit, authz or cache;
ClickHouse is analytical
rather than transactional; Qdrant is a focused HTTP client, not SQL at
all. [dialects.md](dialects.md) has the table. Where a page describes
something one dialect cannot do, it says so rather than leaving you to
find out.
