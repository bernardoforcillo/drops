package drops

import "strings"

// Dialect abstracts the SQL-syntax differences between database
// backends so the same schema and query builders can target any
// SQL-like connector by swapping the Dialect a Builder carries.
//
// A Builder with no Dialect installed behaves exactly as it always
// has: PostgreSQL-style "$N" placeholders and standard double-quoted
// identifiers. Installing a Dialect (via WithDialect) reroutes
// placeholder rendering and identifier quoting through it, so pointing
// the same query code at SQLite instead of PostgreSQL is a matter of
// swapping the Dialect and the underlying Driver — nothing else in the
// builder chain changes.
//
// Implementations must be safe for concurrent use: a Dialect is shared
// across every Builder a DB creates.
type Dialect interface {
	// Name identifies the dialect, e.g. "postgresql" or "sqlite".
	Name() string

	// Placeholder renders the bind marker for the n-th (1-based) bound
	// argument — "$1" for PostgreSQL, "?" for SQLite/MySQL, "@p1" for
	// SQL Server, and so on.
	Placeholder(n int) string

	// QuoteIdent quotes a single identifier, escaping the dialect's
	// quote character. PostgreSQL and SQLite both use double quotes
	// with "" doubling; MySQL uses backticks.
	QuoteIdent(name string) string

	// SupportsReturning reports whether INSERT/UPDATE/DELETE ...
	// RETURNING is available. PostgreSQL and SQLite (>= 3.35) support
	// it; MySQL does not. Builders consult this to decide whether a
	// RETURNING clause can be emitted or must be emulated.
	SupportsReturning() bool
}

// dialectQuoteIdent applies dqt as the quote character to name,
// doubling any embedded occurrence. It is the shared implementation
// most dialects reuse from QuoteIdent (PostgreSQL/SQLite pass '"',
// MySQL passes '`').
func dialectQuoteIdent(name string, dqt byte) string {
	q := string(dqt)
	return q + strings.ReplaceAll(name, q, q+q) + q
}

// StdQuoteIdent quotes name with standard SQL double quotes, doubling
// embedded quotes. Exposed so dialect packages can build their
// QuoteIdent without re-implementing the escaping.
func StdQuoteIdent(name string) string { return dialectQuoteIdent(name, '"') }

// BacktickQuoteIdent quotes name with MySQL-style backticks.
func BacktickQuoteIdent(name string) string { return dialectQuoteIdent(name, '`') }
