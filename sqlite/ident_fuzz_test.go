package sqlite

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// SQLite's lexer is the plainest of the four: a double-quoted token
// ends at the first quote that is not doubled, and there is no
// backslash escape anywhere in the grammar. That makes the invariant
// short to state and worth pinning anyway, because introspection here
// interpolates a table name into PRAGMA arguments and into a
// sqlite_master lookup — the two places in the package where a name
// becomes SQL text instead of a bound parameter.

var nastyNames = []string{
	"",
	"users",
	`"`,
	`""`,
	`a"b`,
	`a""b`,
	"'",
	"a'b",
	`\`,
	`a\`,
	`a\"`,
	"a\x00b",
	"a\nb",
	"a;b",
	`"); DROP TABLE users; --`,
	"a--b",
	"a/*b*/c",
	"ünïcode",
	"\xff\xfe",
	strings.Repeat("a", 100),
}

// readDoubleQuoted models SQLite's quoted-token lexer.
func readDoubleQuoted(s string) (body, rest string, ok bool) {
	if len(s) < 2 || s[0] != '"' {
		return "", "", false
	}
	var out strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			out.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '"' {
			out.WriteByte('"')
			i++
			continue
		}
		return out.String(), s[i+1:], true
	}
	return "", "", false
}

func checkDoubleQuoted(t *testing.T, quoted, want string) {
	t.Helper()
	body, rest, ok := readDoubleQuoted(quoted)
	if !ok {
		t.Fatalf("%q is not a closed quoted token", quoted)
	}
	if rest != "" {
		t.Fatalf("%q closed early: %q escaped the quotes", quoted, rest)
	}
	if body != want {
		t.Fatalf("round-trip = %q, want %q", body, want)
	}
}

func FuzzSQLiteQuoteIdent(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		q := quoteIdent(name)
		checkDoubleQuoted(t, q, name)

		b := drops.NewBuilder(drops.WithDialect(Dialect))
		b.WriteIdent(name)
		sql, _ := b.SQL()
		if sql != q {
			t.Fatalf("WriteIdent = %q, quoteIdent = %q; the DDL and query paths disagree", sql, q)
		}
	})
}

// quoteLiteral is the PRAGMA-argument helper: the same double-quoted
// token, used where the value is a table or index name rather than an
// identifier position. It has to survive the same inputs, because the
// name it is given comes from the same schema declaration.
func FuzzSQLiteQuoteLiteral(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		checkDoubleQuoted(t, quoteLiteral(text), text)
	})
}

// What validateIdent accepts, quoting must be able to carry — and must
// carry without a NUL, which would truncate the statement at the C
// string boundary and leave SQLite running the prefix.
func FuzzSQLiteMustIdentMatchesWhatQuotingCanRender(f *testing.F) {
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
		checkDoubleQuoted(t, q, name)
		if strings.ContainsRune(q, 0) {
			t.Fatalf("accepted name %q renders a NUL byte into the statement", name)
		}
	})
}

// SQLite's identifier comparison is sqlite3StrICmp: it folds A-Z onto
// a-z through a 256-entry table and leaves every other byte alone.
// [identKey] has to be that map and not a wider one, so the rule is
// written out here and the fuzz asks whether identKey is it.
//
// A wider fold is not the harmless over-approximation it looks like.
// The axis guard reads a match as the tenant column being bound
// already and appends no stamp, so folding a pair the server calls two
// columns produced an INSERT with the axis absent from it — verified
// against a real SQLite in the integration suite, where it comes back
// as a NOT NULL failure on a column the caller never named.
func asciiFoldModel(name string) string {
	out := []byte(name)
	for i, c := range out {
		if c >= 'A' && c <= 'Z' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}

func FuzzSQLiteIdentKeyFoldsExactlyWhatSQLiteFolds(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	// The pairs the two answers disagree about: U+0130 and U+212A are
	// case pairs to Unicode and ordinary bytes to SQLite.
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
		// shows: no lead or continuation byte falls in A-Z.
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
