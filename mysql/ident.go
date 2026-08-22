package mysql

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bernardoforcillo/drops"
)

// ErrInvalidIdentifier is returned when a table or column name fails
// validation. errors.Is reports true for every wrapped instance.
var ErrInvalidIdentifier = errors.New("drops/mysql: invalid SQL identifier")

// validateIdent rejects names that cannot be safely rendered.
//
// Quoting handles backticks, so the check is about what quoting cannot
// save: an empty name, invalid UTF-8, a NUL byte (which MySQL rejects
// in identifiers outright), and a trailing space (which MySQL also
// forbids, and which is invisible in a diff).
func validateIdent(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%w: %s name is empty", ErrInvalidIdentifier, kind)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: %s name %q is not valid UTF-8", ErrInvalidIdentifier, kind, name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("%w: %s name contains a NUL byte", ErrInvalidIdentifier, kind)
	}
	if strings.HasSuffix(name, " ") {
		return fmt.Errorf("%w: %s name %q ends in a space, which MySQL forbids", ErrInvalidIdentifier, kind, name)
	}
	if len(name) > 64 {
		return fmt.Errorf("%w: %s name %q is %d bytes; MySQL's limit is 64", ErrInvalidIdentifier, kind, name, len(name))
	}
	return nil
}

func mustIdent(kind, name string) {
	if err := validateIdent(kind, name); err != nil {
		panic(err)
	}
}

// quoteIdent wraps a name in backticks for the raw SQL the migration
// layer assembles as strings rather than through a drops.Builder.
func quoteIdent(name string) string { return drops.BacktickQuoteIdent(name) }

// quoteIdents backtick-quotes each name in a list.
func quoteIdents(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return out
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
// MySQL resolves a column name case-insensitively on every platform
// and in every configuration: lower_case_table_names governs TABLE
// names, and column names are folded whatever it is set to. So a
// handle spelled TENANTID renders as the axis here too, and the byte
// comparison had the same shift-key bypass sqlite's did.
//
// The fold is ASCII, which is the part every configuration agrees on.
// What MySQL does with a NON-ASCII case pair is its identifier
// collation's answer rather than Unicode's — utf8mb3_general_ci gives
// U+0130 the weight of "I", the utf8mb4_0900_ai_ci of a newer server
// folds a wider set again, and neither of them is strings.ToLower —
// and this package has no MySQL to ask. It does not guess: an ASCII
// fold is never wider than the server's, which is the invariant above,
// and the pairs it therefore reads as two columns where the server may
// read one are written down in the "Where the automatic scoping stops"
// list in tenant.go rather than covered by a fold nobody verified.
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
