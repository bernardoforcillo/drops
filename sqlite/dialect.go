// Package sqlite is the SQLite dialect for drops. It mirrors drops/pg's
// API surface — Table / Column / DB / DDL / query builders — over the
// same swappable drops.Driver connector, but emits SQLite-flavoured SQL:
// "?" placeholders, SQLite type affinities, and constraints declared
// inline in CREATE TABLE (SQLite has no ALTER TABLE ADD CONSTRAINT).
//
// Because both pg and sqlite build on drops.Dialect, pointing the same
// schema at SQLite instead of PostgreSQL is a matter of swapping the
// package (and the underlying driver) — the builder chain is otherwise
// identical.
package sqlite

import "github.com/bernardoforcillo/drops"

// sqliteDialect implements drops.Dialect for SQLite.
type sqliteDialect struct{}

func (sqliteDialect) Name() string { return "sqlite" }

// Placeholder renders SQLite's positional bind marker. SQLite accepts
// "?" (anonymous positional) as well as "?N" and named forms; drops
// binds arguments positionally, so bare "?" is used.
func (sqliteDialect) Placeholder(int) string { return "?" }

// QuoteIdent double-quotes identifiers per the SQL standard, which
// SQLite honours (it also accepts backticks and [brackets], but double
// quotes are the portable choice).
func (sqliteDialect) QuoteIdent(name string) string { return drops.StdQuoteIdent(name) }

// SupportsReturning reports true: SQLite added RETURNING in 3.35.0
// (2021). Callers targeting an older SQLite can avoid RETURNING at the
// builder level.
func (sqliteDialect) SupportsReturning() bool { return true }

// Dialect is the SQLite dialect value. Pass it to drops.WithDialect (or
// rely on the sqlite.DB, which installs it on every builder).
var Dialect drops.Dialect = sqliteDialect{}

// Placeholder is the SQLite placeholder BuilderOption, provided for
// symmetry with clickhouse.Placeholder. Prefer drops.WithDialect(Dialect).
var Placeholder = drops.WithDialect(Dialect)
