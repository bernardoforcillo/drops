// Package drops is a Drizzle-inspired, driver-agnostic SQL toolkit for Go.
//
// The root package defines the Driver, Tx, Rows and Result interfaces that
// adapt the toolkit to any underlying database connection (database/sql,
// pgx, etc.) plus the building blocks for composing SQL: Expression and
// Builder.
//
// Dialect-specific schema declarations and query builders live in
// subpackages (currently only [github.com/bernardoforcillo/drops/pg] for
// PostgreSQL). A reference adapter for database/sql lives in
// [github.com/bernardoforcillo/drops/stdlib].
package drops
