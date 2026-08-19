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
// # Scope
//
// This is the schema and query surface: types, tables, DDL, SELECT /
// INSERT / UPDATE / DELETE, operators, and Entity CRUD with the drift
// check, composite keys and relations the other dialects have. The
// cross-cutting packages that pg and sqlite have grown — migrations,
// outbox, saga, event store, audit, tenancy — are not ported yet, and
// this doc will say so until they are.
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
