package sqlite

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// validateIdent rejects identifier values whose bytes would compromise
// quoting safety. Like drops/pg it forbids empty, non-UTF8 and NUL-byte
// names; embedded double quotes are permitted because they round-trip
// through the standard "" doubling.
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

// mustIdent panics if name is invalid. Schema declarations happen at
// process startup, so a panic surfaces the mistake immediately.
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
// SQLite resolves a column name case-insensitively however it is
// quoted: "TENANTID" and "tenantId" are one column, an INSERT may name
// it under either spelling, and an UPDATE's SET list assigns the same
// column whichever it writes. A comparison on the bytes therefore let
// a codegen'd OtherTable.TENANTID walk past the stamping and the
// upsert gate with nothing but a shift key between it and the refusal.
//
// The fold is ASCII and stops there, because SQLite's does:
// sqlite3StrICmp folds A-Z onto a-z through a 256-entry table and
// leaves every other byte alone. strings.ToLower is Unicode-wide and
// folds pairs SQLite does not — U+0130 onto "i", U+212A onto "k" — so
// a table declaring "tenantid" and "tenantİd", which SQLite creates as
// two columns and the integration suite asks it to, had the second
// read as the axis and the first left out of the INSERT altogether. Folding byte-wise is safe on UTF-8 for the same reason
// it is right here: no lead or continuation byte falls in A-Z, so a
// multi-byte sequence is left exactly as it was written.
//
// pg and clickhouse compare a quoted identifier byte for byte and
// their identKey returns the name itself. Asking the question in all
// four packages is what stops one dialect's answer from being carried
// into another by a reader who only saw the comparison.
func identKey(name string) string {
	var b []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 'A' || c > 'Z' {
			continue
		}
		if b == nil {
			b = []byte(name)
		}
		b[i] = c + ('a' - 'A')
	}
	if b == nil {
		return name
	}
	return string(b)
}
