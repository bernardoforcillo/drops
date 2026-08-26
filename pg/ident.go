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

// identKey returns the form in which two rendered column names are one
// column to the server that reads the statement. [namesAxis] asks its
// question through it, which is where "the same name" stops being Go's
// question and becomes the server's.
//
// The invariant is one-directional: identKey never reports two names
// as one column unless the server does. Both ways of being wrong are
// silent, and neither is a refusal.
//
// A key too NARROW is the defect the function was extracted to fix —
// the guard answering "not the axis" for a handle the renderer answers
// yes for, so the statement goes out carrying it. A key too WIDE costs
// more than the spurious refusal it looks like: stampTenantColumn
// reads a match as the axis being bound already and appends no stamp,
// so an INSERT naming some ordinary column renders with the tenant
// column absent from it and the row lands under whatever the schema
// defaults to — section 1's "belonged to nobody" reached through
// section 4.
//
// Here the key is the name itself. Every identifier drops writes goes
// out quoted — see drops.Builder.WriteIdent — and PostgreSQL compares a
// quoted identifier byte for byte, so "tenantId" and "TenantId" are
// two columns: a handle spelled the second way names a column the
// table does not have, the server answers 42703, and folding here would instead
// refuse a schema that legitimately declares both.
//
// It exists as a named function returning its argument because the
// answer differs by dialect and the difference is a leak. sqlite and
// mysql resolve a quoted column name case-insensitively, so there
// "TENANTID" IS the axis to the server while an exact-bytes comparison
// calls the two strangers; their identKey folds ASCII, and ASCII only,
// for that reason. Asking the question in all four packages is what
// stops one dialect's answer from being carried into another by a
// reader who only saw the comparison.
func identKey(name string) string { return name }
