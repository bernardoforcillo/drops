package mysql

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// MySQL quotes identifiers with backticks and doubles an embedded one,
// which is the same shape as the standard's double quote — but its
// string literals are not: by default a backslash is an escape
// character, and under NO_BACKSLASH_ESCAPES it is not. quoteLiteral
// commits to doubling both, and the doc comment on it says what that
// trades away. These targets hold it to exactly that trade, on every
// input rather than on the handful anybody thought to write down.

var nastyNames = []string{
	"",
	"users",
	"`",
	"``",
	"a`b",
	`"`,
	`a"b`,
	"'",
	"a'b",
	`\`,
	`a\`,
	"a\\`",
	`\\`,
	"a\x00b",
	"a\nb",
	"a;b",
	"`; DROP TABLE users; --",
	"a--b",
	"a/*b*/c",
	"a ",
	"ünïcode",
	"\xff\xfe",
	strings.Repeat("a", 100),
}

// readBackQuoted models MySQL's identifier lexer: backtick-delimited,
// a doubled backtick is one backtick, and nothing else is an escape —
// a backslash inside a quoted identifier is an ordinary byte, which is
// why backticks alone are enough here and are not enough for a literal.
func readBackQuoted(s string) (body, rest string, ok bool) {
	if len(s) < 2 || s[0] != '`' {
		return "", "", false
	}
	var out strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '`' {
			out.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '`' {
			out.WriteByte('`')
			i++
			continue
		}
		return out.String(), s[i+1:], true
	}
	return "", "", false
}

// readSingleQuoted models MySQL's string-literal lexer under both
// sql_modes. With backslashEscapes (the default mode) a backslash
// consumes the next byte; under NO_BACKSLASH_ESCAPES it is data and
// only a doubled quote escapes.
func readSingleQuoted(s string, backslashEscapes bool) (body, rest string, ok bool) {
	if len(s) < 2 || s[0] != '\'' {
		return "", "", false
	}
	var out strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case backslashEscapes && s[i] == '\\':
			if i+1 >= len(s) {
				return "", "", false
			}
			out.WriteByte(s[i+1])
			i++
		case s[i] == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				out.WriteByte('\'')
				i++
				continue
			}
			return out.String(), s[i+1:], true
		default:
			out.WriteByte(s[i])
		}
	}
	return "", "", false
}

func FuzzMySQLQuoteIdent(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		q := quoteIdent(name)
		body, rest, ok := readBackQuoted(q)
		if !ok {
			t.Fatalf("%q is not a closed quoted identifier", q)
		}
		if rest != "" {
			t.Fatalf("%q closed early: %q escaped the backticks", q, rest)
		}
		if body != name {
			t.Fatalf("round-trip = %q, want %q", body, name)
		}

		b := drops.NewBuilder(drops.WithDialect(Dialect))
		b.WriteIdent(name)
		sql, _ := b.SQL()
		if sql != q {
			t.Fatalf("WriteIdent = %q, quoteIdent = %q; the DDL and query paths disagree", sql, q)
		}
	})
}

// validateIdent has to be no more permissive than quoting can carry.
// MySQL rejects more than quoting strictly requires — a trailing space,
// a name over 64 bytes — so the implication runs one way only: what it
// accepts must round-trip and must carry no NUL, because the protocol
// would truncate the statement there and run the prefix.
func FuzzMySQLMustIdentMatchesWhatQuotingCanRender(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		err := validateIdent("column", name)

		panicked := func() (p bool) {
			defer func() {
				if recover() != nil {
					p = true
				}
			}()
			mustIdent("column", name)
			return false
		}()
		if panicked != (err != nil) {
			t.Fatalf("mustIdent panicked = %v, validateIdent err = %v", panicked, err)
		}
		if err != nil {
			return
		}
		q := quoteIdent(name)
		body, rest, ok := readBackQuoted(q)
		if !ok || rest != "" || body != name {
			t.Fatalf("accepted name %q renders %q, which does not read back as one identifier", name, q)
		}
		if strings.ContainsRune(q, 0) {
			t.Fatalf("accepted name %q renders a NUL byte into the statement", name)
		}
	})
}

// The literal invariant is deliberately asymmetric, because
// quoteLiteral's contract is. Under either sql_mode the literal must
// close in the right place — that is the property that keeps the rest
// of the DDL out of the string, and it is not negotiable. Reading back
// as the same bytes is only promised under the default mode; under
// NO_BACKSLASH_ESCAPES a doubled backslash arrives doubled, which
// corrupts the comment and not the statement.
func FuzzMySQLQuoteLiteral(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		q := "'" + quoteLiteral(text) + "'"
		for _, backslashEscapes := range []bool{true, false} {
			body, rest, ok := readSingleQuoted(q, backslashEscapes)
			if !ok {
				t.Fatalf("%q is not a closed literal (backslash escapes = %v)", q, backslashEscapes)
			}
			if rest != "" {
				t.Fatalf("%q closed early: %q escaped the literal (backslash escapes = %v)", q, rest, backslashEscapes)
			}
			if backslashEscapes && body != text {
				t.Fatalf("round-trip = %q, want %q", body, text)
			}
		}
	})
}

// MySQL folds a column name's case on every platform and in every
// configuration, and folds ASCII in all of them. What it does with a
// non-ASCII case pair is its identifier collation's answer, and no
// MySQL was reachable to ask, so identKey folds the ASCII and stops.
// [identKey] has to be that map and not a wider one, so the rule is
// written out here and the fuzz asks whether identKey is it.
//
// A wider fold is not the harmless over-approximation it looks like.
// The axis guard reads a match as the tenant column being bound
// already and appends no stamp, so folding a pair the server calls two
// columns produces an INSERT with the axis absent from it. sqlite is
// where that was verified against a real server; the rendering here is
// the same rendering.
func asciiFoldModel(name string) string {
	out := []byte(name)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}

func FuzzMySQLIdentKeyFoldsExactlyWhatMySQLFolds(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	// The pairs the two answers disagree about: U+0130 and U+212A are
	// case pairs to Unicode and ordinary bytes to MySQL.
	f.Add("TENANTID")
	f.Add("tenantİd")
	f.Add("tenantKey")
	f.Fuzz(func(t *testing.T, name string) {
		key := identKey(name)
		if want := asciiFoldModel(name); key != want {
			t.Fatalf("identKey(%q) = %q, the server's own fold gives %q", name, key, want)
		}
		// Matching happens more than once per statement and on both
		// sides of the comparison, so the key has to be a fixed point:
		// a fold that moved again would answer differently depending
		// on how many times it had been asked.
		if again := identKey(key); again != key {
			t.Fatalf("identKey(%q) = %q, folding that again gives %q", name, key, again)
		}
		// Folding bytes rather than runes is what keeps every
		// multi-byte sequence intact, and the byte count is how that
		// shows: no lead or continuation byte falls in A-Z. It is also what keeps the
		// name inside MySQL's 64-byte identifier limit, which a fold
		// that resized a name could carry a declaration past.
		if len(key) != len(name) {
			t.Fatalf("identKey(%q) is %d bytes, the name is %d", name, len(key), len(name))
		}
		// And a name that could be rendered still can be. The fold is
		// asked about names that reached a column declaration, so it
		// must not turn one into something quoting cannot carry.
		if validateIdent("column", name) == nil {
			if err := validateIdent("column", key); err != nil {
				t.Fatalf("identKey(%q) = %q is no longer renderable: %v", name, key, err)
			}
		}
	})
}
