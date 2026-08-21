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
//
// # Multi-tenancy
//
// A table can declare who owns its rows, and drops carries that
// declaration into every statement it composes:
//
//	Posts.ContextFilter(sqlite.TenantFilter(PostTenantID)).
//	    ScopeWritesByTenant(PostTenantID)
//
//	ctx = sqlite.WithTenant(ctx, currentTenant)
//
// The predicate is resolved by the EXECUTORS rather than by the
// renderer, so one declaration covers a root query, a joined table, a
// CTE body, a subquery operand, an eager-loaded edge, an UPDATE and a
// DELETE — everything that goes through All / One / Rows / Exec. It
// fails closed: a ctx with no tenant is [ErrTenantMissing] and no
// statement at all. The cost is that [SelectBuilder.ToSQL] no longer
// shows the whole statement; ToSQLCtx is the ctx-aware twin, and the
// one to log and to assert on. [Table.ContextFilter], [TenantFilter]
// and [Entity.ScopeByTenant] carry the reasoning, and tenant.go lists
// what the predicates do not reach.
//
// It is the same mechanism drops/pg, drops/mysql and drops/clickhouse
// carry — normalise the dialect name and diff sqlite/resolve.go against
// any of theirs and the same file comes back. What differs here is
// surface, and it differs where the SQL does: this package exposes
// INNER and LEFT JOIN and nothing else, so the join-placement shapes
// the other three have to answer cannot arise.
//
// What does NOT come across from drops/pg is the boundary underneath.
// PostgreSQL row-level security is what those predicates sit on top of,
// and SQLite has no equivalent: no roles, no policies, and a process
// that can open the file reads every byte in it. Here the predicates
// are the whole of what there is, which makes tenant.go's list of where
// they stop load-bearing rather than a footnote.
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
