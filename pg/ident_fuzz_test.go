package pg

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// pg assembles two kinds of SQL text that a bound parameter cannot
// carry: identifiers, and the literals that utility statements demand
// (COMMENT ... IS, CREATE TYPE ... AS ENUM, INTERVAL '...'), because
// PostgreSQL's grammar has no placeholder slot there. Those two
// functions are therefore the only places in the package where a
// caller-supplied string is turned into SQL text, and the only places
// where getting the escaping wrong ends a token early and hands the
// rest of the string to the parser.
//
// The targets below assert round-trip identity against a reference
// lexer rather than string equality against an expected output: the
// question is never "does it look escaped", it is "does the server
// read back exactly the bytes we were given, and nothing after them".

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
	`\\`,
	"a\x00b",
	"a\nb",
	"a;b",
	`"; DROP TABLE users; --`,
	"a--b",
	"a/*b*/c",
	"ünïcode",
	"\xff\xfe",
	strings.Repeat("a", 100),
}

// readDoubleQuoted models PostgreSQL's identifier lexer: a token that
// opens with a double quote, closes with the first double quote not
// doubled, and recognises no other escape — a backslash inside a
// quoted identifier is an ordinary byte.
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

// readStringLiteral models PostgreSQL's string-literal lexer. It
// understands the E'...' prefix, and takes backslashEscapes to model a
// session with standard_conforming_strings turned off — which is the
// case a literal escaped by quote-doubling alone does not survive,
// because there \' does not close anything and the literal runs on
// into the rest of the statement.
//
// An escape other than \\ or \' is reported as a failure rather than
// decoded: quoteLiteral never emits one, so seeing one means a byte of
// the input reached the server with a meaning it did not have here.
func readStringLiteral(s string, backslashEscapes bool) (body, rest string, ok bool) {
	if len(s) > 1 && (s[0] == 'E' || s[0] == 'e') && s[1] == '\'' {
		s = s[1:]
		backslashEscapes = true
	}
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
			switch s[i+1] {
			case '\\':
				out.WriteByte('\\')
			case '\'':
				out.WriteByte('\'')
			default:
				return "", "", false
			}
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

func checkIdent(t *testing.T, quoted, want string) {
	t.Helper()
	body, rest, ok := readDoubleQuoted(quoted)
	if !ok {
		t.Fatalf("%q is not a closed quoted identifier", quoted)
	}
	if rest != "" {
		t.Fatalf("%q closed early: %q escaped the quotes", quoted, rest)
	}
	if body != want {
		t.Fatalf("round-trip = %q, want %q", body, want)
	}
}

// quoteIdent (the migration layer's string-concatenating helper) and
// Builder.WriteIdent (the query builders' path) must agree byte for
// byte, or a name is one identifier in a CREATE TABLE and a different
// one in the SELECT that reads it back.
func FuzzPgQuoteIdent(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		q := quoteIdent(name)
		checkIdent(t, q, name)

		b := drops.NewBuilder(drops.WithDialect(Dialect))
		b.WriteIdent(name)
		sql, _ := b.SQL()
		if sql != q {
			t.Fatalf("WriteIdent = %q, quoteIdent = %q; the DDL and query paths disagree", sql, q)
		}
	})
}

// The boundary validateIdent draws has to be at least as strict as
// what quoting can carry: every name it accepts must round-trip, and
// must not smuggle a NUL into the statement, because the wire protocol
// terminates a query string at the first one and the server would run
// the truncated prefix. mustIdent is the same predicate with a panic
// on it — schema declarations run at init, where a returned error has
// nobody to read it — so the two must never disagree.
func FuzzPgMustIdentMatchesWhatQuotingCanRender(f *testing.F) {
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
		checkIdent(t, q, name)
		if strings.ContainsRune(q, 0) {
			t.Fatalf("accepted name %q renders a NUL byte into the statement", name)
		}
	})
}

// A literal has to close in a session with standard_conforming_strings
// either way round. The setting is a session GUC drops does not set
// and cannot see, so an escaping that only works under the default is
// an escaping that works until someone changes a connection string.
func FuzzPgQuoteLiteral(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		q := quoteLiteral(text)
		for _, scsOff := range []bool{false, true} {
			body, rest, ok := readStringLiteral(q, scsOff)
			if !ok {
				t.Fatalf("%q is not a closed literal (backslash escapes = %v)", q, scsOff)
			}
			if rest != "" {
				t.Fatalf("%q closed early: %q escaped the literal (backslash escapes = %v)", q, rest, scsOff)
			}
			if body != text {
				t.Fatalf("round-trip = %q, want %q (backslash escapes = %v)", body, text, scsOff)
			}
		}
	})
}

// PostgreSQL compares a quoted identifier byte for byte, and drops quotes
// every identifier it writes, so [identKey] is the name itself. That
// is worth fuzzing precisely because it is nothing: the two dialects
// that DO fold reach the same guard through the same [namesAxis], and
// a fold copied across from one of them would read a schema's
// deliberately distinct "tenantId" and "TenantId" as one column — at
// which point the INSERT stamp is suppressed for a column the server
// never resolved.
func FuzzPgIdentKeyFoldsNothing(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Add("TENANTID")
	f.Add("tenantİd")
	f.Fuzz(func(t *testing.T, name string) {
		if key := identKey(name); key != name {
			t.Fatalf("identKey(%q) = %q; PostgreSQL resolves neither onto the other", name, key)
		}
	})
}
