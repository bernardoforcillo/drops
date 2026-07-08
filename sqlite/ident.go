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
