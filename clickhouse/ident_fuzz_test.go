package clickhouse

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// ClickHouse's lexer is the one that does not behave like the others.
// Its quoted tokens — single-quoted strings, double-quoted identifiers
// and backticked identifiers alike — go through one routine that
// accepts *both* escaping conventions: a doubled quote closes nothing,
// and a backslash consumes the byte after it. Every other backend
// drops targets treats a backslash inside a quoted token as data.
//
// That difference is invisible until a name or a comment ends in a
// backslash, at which point quoting by doubling alone produces "a\"
// — where the server reads the backslash as escaping the quote, never
// closes the token, and reads the rest of the statement into it. These
// targets exist to hold that shut.

var nastyNames = []string{
	"",
	"events",
	`"`,
	`""`,
	`a"b`,
	`a""b`,
	"'",
	"a'b",
	`\`,
	`a\`,
	`a\"`,
	`\\`,
	`a\\`,
	"`",
	"a\x00b",
	"a\nb",
	"a;b",
	`"; DROP TABLE events; --`,
	`a\", x String) ENGINE = Log; --`,
	"a--b",
	"a/*b*/c",
	"ünïcode",
	"\xff\xfe",
	strings.Repeat("a", 100),
}

// readQuoted models ClickHouse's Lexer::quotedString: the token opens
// with q, a backslash escapes the following byte, a doubled q is one q,
// and anything else is data. An escape other than a doubled quote or a
// doubled backslash is reported as a failure rather than decoded —
// neither quoteIdent nor quoteLiteral has any business emitting one,
// and if one appears it means a byte reached the server carrying a
// meaning it did not have in Go.
func readQuoted(s string, q byte) (body, rest string, ok bool) {
	if len(s) < 2 || s[0] != q {
		return "", "", false
	}
	var out strings.Builder
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			if i+1 >= len(s) {
				return "", "", false
			}
			switch s[i+1] {
			case '\\':
				out.WriteByte('\\')
			case q:
				out.WriteByte(q)
			default:
				return "", "", false
			}
			i++
		case s[i] == q:
			if i+1 < len(s) && s[i+1] == q {
				out.WriteByte(q)
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

func checkQuoted(t *testing.T, quoted, want string, q byte) {
	t.Helper()
	body, rest, ok := readQuoted(quoted, q)
	if !ok {
		t.Fatalf("%q is not a closed %c-quoted token", quoted, q)
	}
	if rest != "" {
		t.Fatalf("%q closed early: %q escaped the quotes", quoted, rest)
	}
	if body != want {
		t.Fatalf("round-trip = %q, want %q", body, want)
	}
}

func FuzzClickHouseQuoteIdent(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		q := quoteIdent(name)
		checkQuoted(t, q, name, '"')

		// The DDL helpers concatenate strings and the query builders go
		// through the Dialect; a name that is one identifier on one
		// path and two on the other is the bug this catches.
		b := drops.NewBuilder(drops.WithDialect(Dialect))
		b.WriteIdent(name)
		sql, _ := b.SQL()
		if sql != q {
			t.Fatalf("WriteIdent = %q, quoteIdent = %q; the DDL and query paths disagree", sql, q)
		}
	})
}

func FuzzClickHouseQuoteLiteral(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		checkQuoted(t, quoteLiteral(text), text, '\'')
	})
}

// What validateIdent accepts, quoting must be able to carry.
func FuzzClickHouseMustIdentMatchesWhatQuotingCanRender(f *testing.F) {
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
		checkQuoted(t, q, name, '"')
		if strings.ContainsRune(q, 0) {
			t.Fatalf("accepted name %q renders a NUL byte into the statement", name)
		}
	})
}
