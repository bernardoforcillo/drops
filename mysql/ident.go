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
