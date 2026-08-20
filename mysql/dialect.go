// Package mysql is the MySQL / MariaDB dialect for drops.
//
// It mirrors drops/pg and drops/sqlite — Table, Col[T], DB, the four
// statement builders, Entity CRUD — over the same swappable
// drops.Driver, and emits MySQL syntax: "?" placeholders, backtick
// identifiers, AUTO_INCREMENT, and INSERT … ON DUPLICATE KEY UPDATE
// where PostgreSQL writes ON CONFLICT.
//
// # What MySQL makes different
//
// Two differences are not cosmetic and shape the API rather than just
// the rendered SQL.
//
// MySQL has no RETURNING clause. PostgreSQL and SQLite read a
// generated primary key straight back out of the INSERT; here the key
// arrives through the driver's LastInsertId, which is why
// [Entity.Create] issues one statement and then reads the id from the
// result rather than from a row. MariaDB 10.5+ does support RETURNING
// on INSERT and DELETE, but not on UPDATE, and drops targets the
// intersection — [Dialect] reports SupportsReturning as false so no
// builder emits a clause the server may reject.
//
// MySQL's upsert keys on *any* unique index, not on a named conflict
// target. ON DUPLICATE KEY UPDATE therefore fires for a collision on a
// unique email as readily as on the primary key, which is a broader
// promise than PostgreSQL's ON CONFLICT (id). [Entity.UpsertMany]
// documents that where it matters.
//
// MySQL has no transactional DDL. Every CREATE, ALTER and DROP commits
// itself, so a migration that fails half-way stays half-applied and no
// ROLLBACK will take it back. That is the contract the whole migration
// layer is written around: see [Push], whose result says how many
// statements committed before the failure, and [AnalyzeMigration],
// which grades a statement by what it costs when there is nothing
// standing behind it.
//
// # Migrations
//
// [Schema] collects the tables an application owns. [BuildSnapshot]
// turns it into a [Snapshot], [Introspect] reads one back out of
// information_schema, [Diff] renders the SQL between two of them,
// [Push] applies that against a live server, [GenerateMigration]
// writes it to a reviewable file, and [AnalyzeMigration] says which
// statements will hurt.
//
// # Messaging and event sourcing
//
// [Outbox] is the transactional outbox: an event written in the same
// transaction as the business row, drained by [OutboxWorker]. Where
// PostgreSQL takes SKIP LOCKED for granted, here it is a server
// version question the outbox faces explicitly — see [DrainLocking].
// [EventStore] is the append-only log, whose optimistic-concurrency
// contract has to survive MySQL's REPEATABLE READ default rather than
// PostgreSQL's READ COMMITTED; its doc explains what that changes.
// [IdempotencyStore] caches a response per request key.
//
// # Pagination and failure
//
// [Entity.Page] and the [CursorSpec] form of a SELECT page by keyset
// rather than by OFFSET, over an opaque cursor this package stamps and
// only this package accepts. Both refuse a sort key they cannot walk
// safely, which on MySQL is not pedantry: its default collation is
// case- and accent-insensitive, so a text key without a unique
// tiebreaker ties far more rows than the same schema would in
// PostgreSQL — see [NewCursorSpec].
//
// Failures from the server arrive as a [ServerError] carrying the
// error number, so callers branch with errors.Is against
// [ErrUniqueViolation] and its siblings rather than on message text.
// [RetryPolicy] re-runs a transaction the server rolled back under
// contention — a deadlock or a lock wait timeout, and nothing else.
//
// # Expressions
//
// [SelectBuilder.With] attaches common table expressions (cte.go),
// [Exists] and friends wrap subqueries (subquery.go), and [Over]
// carries window functions (window.go) — all of which need MySQL 8.0
// or MariaDB 10.2, which [ServerInfo.SupportsCTE] and
// [ServerInfo.SupportsWindowFunctions] report. The function library
// mirrors drops/pg's: json.go, strings.go, math.go, datetime.go,
// funcs.go. [Entity.Patch] applies server-side assignments — a
// counter increment, a high-watermark — to one row by key.
//
// Those files are ports of pg's, and each carries at its head a list
// of what could not be ported and why: MySQL has one JSON type and no
// jsonb operator set, no aggregate FILTER clause, no date_trunc, no
// interval *value*, and a || that means OR. The three that change an
// answer rather than a spelling are worth knowing before writing a
// query: MySQL's LOG is the natural logarithm where PostgreSQL's is
// base 10, its LENGTH counts bytes where PostgreSQL's counts
// characters, and its CONCAT returns NULL where PostgreSQL's would
// have skipped the NULL. drops renders LOG10, CHAR_LENGTH, and plain
// CONCAT respectively — the first two keeping PostgreSQL's meaning,
// the third keeping MySQL's, because there is no rewrite of CONCAT
// that does not also change what a caller asked for.
//
// One divergence has no safe default at all: both servers cap a
// recursive CTE at 1000 iterations, and past it MySQL raises an error
// while MariaDB returns a short answer and no warning. See cte.go and
// [SetRecursionLimit].
//
// # Scope
//
// This is the schema, query and migration surface: types, tables, DDL,
// SELECT / INSERT / UPDATE / DELETE, operators, Entity CRUD with the
// drift check, composite keys and relations, and schema migrations;
// plus the outbox, the event store, the idempotency store, keyset
// pagination, the typed error surface, and the expression library
// above. The remaining cross-cutting
// packages that pg and sqlite have grown — saga, audit, tenancy — are
// not ported yet, and this doc will say so until they are.
package mysql

import "github.com/bernardoforcillo/drops"

// mysqlDialect implements drops.Dialect for MySQL and MariaDB.
type mysqlDialect struct{}

func (mysqlDialect) Name() string { return "mysql" }

// Placeholder renders MySQL's positional bind marker. The protocol has
// no numbered form, so arguments must be bound in the order they are
// written — which the Builder does by construction.
func (mysqlDialect) Placeholder(int) string { return "?" }

// QuoteIdent wraps an identifier in backticks, doubling any embedded
// one. MySQL also accepts double quotes, but only when ANSI_QUOTES is
// in the session's sql_mode, which is not the default and not
// something a library should assume.
func (mysqlDialect) QuoteIdent(name string) string { return drops.BacktickQuoteIdent(name) }

// SupportsReturning reports false. See the package doc: MySQL has no
// RETURNING at all and MariaDB's is partial, so drops targets the
// intersection and reads generated keys through LastInsertId instead.
func (mysqlDialect) SupportsReturning() bool { return false }

// Dialect is the MySQL dialect value. A mysql.DB installs it on every
// builder it creates; pass it to drops.WithDialect to render MySQL SQL
// from a bare Builder.
var Dialect drops.Dialect = mysqlDialect{}
