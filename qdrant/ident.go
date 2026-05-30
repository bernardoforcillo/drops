package qdrant

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrInvalidIdentifier is returned when a collection / vector name
// fails validation. errors.Is(err, ErrInvalidIdentifier) is true for
// every wrapped instance.
var ErrInvalidIdentifier = errors.New("qdrant: invalid identifier")

// validateName rejects names that are obviously unsafe to interpolate
// into a URL path: empty strings, non-UTF8 sequences, control bytes,
// embedded path separators or query markers. Qdrant itself has
// stricter rules; this layer catches the cases that would corrupt the
// request URL before the server gets a chance to.
func validateName(kind, name string) error {
	if name == "" {
		return fmt.Errorf("%w: %s name is empty", ErrInvalidIdentifier, kind)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("%w: %s name %q is not valid UTF-8", ErrInvalidIdentifier, kind, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == 0 || c == '/' || c == '?' || c == '#' || c == ' ' || c < 0x20 {
			return fmt.Errorf("%w: %s name contains forbidden byte 0x%02x", ErrInvalidIdentifier, kind, c)
		}
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "-") {
		return fmt.Errorf("%w: %s name %q must not start with %q",
			ErrInvalidIdentifier, kind, name, string(name[0]))
	}
	return nil
}
