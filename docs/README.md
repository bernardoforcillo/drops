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
| [Testing](testing.md) | The two suites, why the second exists, and which of your tests belongs in which. |

## Reference

Package documentation lives with the code, at
[pkg.go.dev/github.com/bernardoforcillo/drops](https://pkg.go.dev/github.com/bernardoforcillo/drops).
Every package has runnable examples; `go doc` works offline.

The [readme](../readme.md) is the tour — what exists, in one page. These
documents are the explanation.

## A note on what is not here

drops is pre-1.0 and the surface is not evenly deep. PostgreSQL has the
most (migrations, outbox, saga, event store, audit, tenancy, geo,
money); SQLite has most of it; MySQL has the schema and query surface
but none of the cross-cutting packages yet; ClickHouse is analytical
rather than transactional; Qdrant is a focused HTTP client, not SQL at
all. [dialects.md](dialects.md) has the table. Where a page describes
something one dialect cannot do, it says so rather than leaving you to
find out.
