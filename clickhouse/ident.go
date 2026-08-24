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

// ErrInvalidSetting reports a SETTINGS pair that would not render as
// one setting. The decorating wrappers use %w, so
// errors.Is(err, ErrInvalidSetting) succeeds.
var ErrInvalidSetting = errors.New("drops/clickhouse: invalid table setting")

// validateSettingKey checks the name half of a SETTINGS pair. Every
// setting ClickHouse has is a plain snake_case name, and the name is
// rendered bare, so anything else is either a typo or an attempt to
// write SQL through the key.
func validateSettingKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: the name is empty", ErrInvalidSetting)
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return fmt.Errorf("%w: %q is not a setting name", ErrInvalidSetting, key)
		}
	}
	return nil
}

// validateSettingValue checks that value is one value.
//
// SETTINGS is a comma-separated list and ALTER TABLE … MODIFY SETTING
// is the same list of one, so a comma the renderer did not write is a
// second setting nobody declared. The check is deliberately narrow: it
// tracks string literals and bracketing and objects only to a comma
// that sits outside both, so 'tier_a,tier_b' and
// disk(name = 'd', type = cache) — both of which ClickHouse takes —
// go through, and "8192, allow_nullable_key = 1" does not.
//
// An unterminated literal is refused for the same reason: it would
// swallow the ", " the renderer writes next and the setting after it.
// So is a comment opener, which swallows the same thing without
// looking like it does: SETTINGS a = 8192 --, b = 'gold' is a
// well-formed statement that sets a and never sets b.
func validateSettingValue(key, value string) error {
	v := strings.TrimSpace(value)
	if v == "" {
		return fmt.Errorf("%w: %s has no value", ErrInvalidSetting, key)
	}
	depth := 0
	inLit := false
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case inLit && c == '\\':
			i++ // an escaped character, whatever it is
		case inLit && c == '\'':
			if i+1 < len(v) && v[i+1] == '\'' {
				i++ // a doubled quote is one quote, not the end
				continue
			}
			inLit = false
		case inLit:
		case c == '\'':
			inLit = true
		case isCommentOpener(v, i):
			return fmt.Errorf("%w: the value of %s opens a comment, "+
				"which hides every setting the renderer writes after it: %s",
				ErrInvalidSetting, key, v)
		case c == '(' || c == '[':
			depth++
		case c == ')' || c == ']':
			depth--
			if depth < 0 {
				return fmt.Errorf("%w: the value of %s closes a bracket it never opened: %s", ErrInvalidSetting, key, v)
			}
		case c == ',' && depth == 0:
			return fmt.Errorf("%w: the value of %s carries a comma outside any literal, "+
				"so it is not one value but this setting and whatever follows it: %s",
				ErrInvalidSetting, key, v)
		}
	}
	if inLit {
		return fmt.Errorf("%w: the value of %s opens a string literal it never closes: %s", ErrInvalidSetting, key, v)
	}
	if depth != 0 {
		return fmt.Errorf("%w: the value of %s opens a bracket it never closes: %s", ErrInvalidSetting, key, v)
	}
	return nil
}

// isCommentOpener reports whether v starts a comment at i. ClickHouse
// takes all three spellings — the SQL "--", the C "/*" and the MySQL
// "#" — and each of them ends the SETTINGS list as far as the server
// is concerned while leaving the statement valid, which is what makes
// the loss silent.
func isCommentOpener(v string, i int) bool {
	if v[i] == '#' {
		return true
	}
	if i+1 >= len(v) {
		return false
	}
	return v[i] == '-' && v[i+1] == '-' || v[i] == '/' && v[i+1] == '*'
}

func mustSettingKey(key string) {
	if err := validateSettingKey(key); err != nil {
		panic(err)
	}
}

func mustSettingValue(key, value string) {
	if err := validateSettingValue(key, value); err != nil {
		panic(err)
	}
}

// ValidateSetting reports whether key and value would render as one
// SETTINGS pair, wrapping [ErrInvalidSetting] when they would not.
//
// [Table.Setting] and [SelectBuilder.Setting] panic on exactly these
// terms, which is right where a schema is being declared. This is the
// spelling for code that has to answer for a pair it did not write —
// settings read from a configuration file, or handed in as an option
// by a caller of some other package — and that already reports its
// problems as errors.
func ValidateSetting(key, value string) error {
	if err := validateSettingKey(key); err != nil {
		return err
	}
	return validateSettingValue(key, value)
}
