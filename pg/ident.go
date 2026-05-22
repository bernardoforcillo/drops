package pg

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// validateIdent rejects identifier values whose bytes would compromise
// quoting safety even after standard double-quote escaping. The two
// concerns are:
//
//  1. NUL bytes — many PostgreSQL drivers refuse them and a stray NUL
//     can silently truncate the column name reaching the wire.
//  2. Non-UTF8 sequences — PG identifiers are UTF-8; binary garbage in
//     a name is almost always a programming error and never intended.
//
// A bare empty string is also rejected because every place we use an
// identifier requires a non-empty token.
//
// Embedded double quotes ARE permitted (they round-trip safely through
// WriteIdent's doubling) but are vanishingly rare in real schemas, so
// callers usually misroute SQL into a name argument when they appear.
// We still allow them to avoid surprising legitimate (if eccentric)
// uses.
func validateIdent(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%w: %s name is empty", ErrInvalidIdentifier, kind)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: %s name %q is not valid UTF-8", ErrInvalidIdentifier, kind, name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: %s name contains NUL byte", ErrInvalidIdentifier, kind)
	}
	return nil
}

// mustIdent is the constructor-time helper that panics if validation
// fails. Schema declarations happen at process startup (in package
// init or var blocks), so a panic is the right way to surface a bad
// identifier — the program fails immediately rather than at the first
// query.
func mustIdent(kind, name string) {
	if err := validateIdent(kind, name); err != nil {
		panic(err)
	}
}
