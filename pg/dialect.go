package pg

import (
	"strconv"

	"github.com/bernardoforcillo/drops"
)

// pgDialect is the PostgreSQL implementation of drops.Dialect: "$N"
// placeholders, standard double-quoted identifiers, and RETURNING
// support. pg's builders emit this syntax by default, so installing it
// on a drops.Builder is a no-op behaviourally — it exists so callers
// that parametrise generic code by drops.Dialect can name PostgreSQL,
// and so the swap between pg and another SQL-like dialect (e.g. sqlite)
// is a single value change.
type pgDialect struct{}

func (pgDialect) Name() string                  { return "postgresql" }
func (pgDialect) Placeholder(n int) string      { return "$" + strconv.Itoa(n) }
func (pgDialect) QuoteIdent(name string) string { return drops.StdQuoteIdent(name) }
func (pgDialect) SupportsReturning() bool       { return true }

// Dialect is the PostgreSQL dialect value.
var Dialect drops.Dialect = pgDialect{}
