package clickhouse

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidIdentifier is returned when a table / database / column
// name fails validation. The decorating wrappers use fmt.Errorf with
// %w so errors.Is(err, ErrInvalidIdentifier) succeeds.
var ErrInvalidIdentifier = errors.New("drops/clickhouse: invalid SQL identifier")

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

func mustIdent(kind, name string) {
	if err := validateIdent(kind, name); err != nil {
		panic(err)
	}
}
