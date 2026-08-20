package drops_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// The whole of drops' safety story is one sentence: a value never
// reaches the wire as text. Two mechanisms carry it — identifier
// quoting, which turns an arbitrary Go string into exactly one SQL
// token, and AddArg, which turns a Go value into a placeholder and a
// slot in the argument list. Everything else in the library is built
// on the assumption that neither can be talked out of it.
//
// A table test can only assert that on the inputs someone thought of,
// and the inputs nobody thinks of are precisely the ones an attacker
// supplies. So these are fuzz targets, and they assert round-trip
// invariants rather than "did not panic": a quoted identifier that
// does not parse back to the exact bytes it was built from has either
// lost data or, far worse, ended its own token early and handed the
// remainder of the string to the parser as SQL.
//
// They run as ordinary unit tests over the seed corpus — which is
// where the nasty cases are pinned by name — unless -fuzz is passed,
// so CI pays milliseconds for them.

// nastyIdents is the corpus of bytes that have historically escaped
// one quoting scheme or another. Each is seeded into every target
// below, so a regression in any dialect's quoting fails a normal
// `go test` run rather than waiting for someone to run the fuzzer.
var nastyIdents = []string{
	"",
	"users",
	`"`,
	`""`,
	`a"b`,
	`a""b`,
	"`",
	"a`b",
	"``",
	"'",
	"a'b",
	`\`,
	`a\`,
	`\"`,
	`a\"`,
	`\\`,
	"a\x00b",
	"\x00",
	"a\nb",
	"a;b",
	"a; DROP TABLE users; --",
	`"; DROP TABLE users; --`,
	"a--b",
	"a/*b*/c",
	"/*",
	"*/",
	"a b",
	"a ",
	" a",
	"ünïcode",
	"日本語",
	"\xff\xfe",
	strings.Repeat("a", 200),
}

// unquoteDoubled models the lexer every standard-SQL backend runs over
// a quoted token: the token opens with q, closes with the first q that
// is not immediately followed by another q, and knows no other escape.
// PostgreSQL, SQLite and MySQL all read identifiers this way, which is
// why StdQuoteIdent and BacktickQuoteIdent can share one reference
// parser.
//
// It returns the decoded body and whatever text follows the closing
// quote. A caller asserting round-trip safety must check both: a body
// that matches with a non-empty rest means the quoting closed the
// token early and everything after it would have been parsed as SQL.
func unquoteDoubled(s string, q byte) (body, rest string, ok bool) {
	if len(s) < 2 || s[0] != q {
		return "", "", false
	}
	var out strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != q {
			out.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == q {
			out.WriteByte(q)
			i++
			continue
		}
		return out.String(), s[i+1:], true
	}
	// Ran off the end without a closing quote.
	return "", "", false
}

// assertOneToken fails t unless quoted is a single complete token that
// reads back as want.
func assertOneToken(t *testing.T, quoted, want string, q byte) {
	t.Helper()
	body, rest, ok := unquoteDoubled(quoted, q)
	if !ok {
		t.Fatalf("quoted %q is not a closed %c-quoted token", quoted, q)
	}
	if rest != "" {
		t.Fatalf("quoted %q closed early: %q escaped the quotes", quoted, rest)
	}
	if body != want {
		t.Fatalf("round-trip = %q, want %q (from %q)", body, want, quoted)
	}
}

func FuzzStdQuoteIdent(f *testing.F) {
	for _, s := range nastyIdents {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		assertOneToken(t, drops.StdQuoteIdent(name), name, '"')
	})
}

func FuzzBacktickQuoteIdent(f *testing.F) {
	for _, s := range nastyIdents {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		assertOneToken(t, drops.BacktickQuoteIdent(name), name, '`')
	})
}

// WriteIdent is the path every query builder takes, and with no
// dialect installed it re-implements the doubling rather than calling
// StdQuoteIdent — so it gets its own target instead of being assumed
// equivalent.
func FuzzBuilderWriteIdent(f *testing.F) {
	for _, s := range nastyIdents {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		b := drops.NewBuilder()
		b.WriteIdent(name)
		sql, args := b.SQL()
		if len(args) != 0 {
			t.Fatalf("WriteIdent bound %d args, want 0", len(args))
		}
		assertOneToken(t, sql, name, '"')
		if got := drops.StdQuoteIdent(name); got != sql {
			t.Fatalf("WriteIdent = %q, StdQuoteIdent = %q; the two quoting paths disagree", sql, got)
		}
	})
}

// A qualified reference is where early termination pays best for an
// attacker: "schema"."table" has a separator between two tokens, so a
// name that closes its own quotes can inject a whole extra qualifier.
// The invariant is that splitting on the top-level dots gives back the
// parts, in order, with nothing left over.
func FuzzBuilderWriteQualified(f *testing.F) {
	for _, s := range nastyIdents {
		f.Add(s, "id")
		f.Add("users", s)
	}
	f.Fuzz(func(t *testing.T, schema, table string) {
		b := drops.NewBuilder()
		b.WriteQualified(schema, table)
		sql, _ := b.SQL()

		want := []string{}
		for _, p := range []string{schema, table} {
			if p != "" {
				want = append(want, p)
			}
		}
		rest := sql
		for i, w := range want {
			if i > 0 {
				if rest == "" || rest[0] != '.' {
					t.Fatalf("part %d of %q is not separated by a dot", i, sql)
				}
				rest = rest[1:]
			}
			body, after, ok := unquoteDoubled(rest, '"')
			if !ok {
				t.Fatalf("part %d of %q is not a closed token", i, sql)
			}
			if body != w {
				t.Fatalf("part %d = %q, want %q (from %q)", i, body, w, sql)
			}
			rest = after
		}
		if rest != "" {
			t.Fatalf("%q has %q after the last identifier", sql, rest)
		}
	})
}

// AddArg's invariant is stronger than "the value is escaped": the
// value must not be in the SQL at all. The way to assert that without
// tripping over a value that happens to look like a placeholder is to
// render the same statement twice with different values and require
// the two SQL strings to be byte-identical — if the argument can move
// a single byte of the statement, it can move all of it.
func FuzzAddArgNeverReachesTheSQL(f *testing.F) {
	for _, s := range nastyIdents {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, val string) {
		render := func(v string) (string, []any) {
			b := drops.NewBuilder()
			b.WriteString("SELECT * FROM ")
			b.WriteIdent("users")
			b.WriteString(" WHERE name = ")
			b.AddArg(v)
			b.WriteString(" AND blob = ")
			b.AddArg([]byte(v))
			return b.SQL()
		}
		sql, args := render(val)
		const want = `SELECT * FROM "users" WHERE name = $1 AND blob = $2`
		if sql != want {
			t.Fatalf("sql = %q, want %q", sql, want)
		}
		if fixed, _ := render("x"); fixed != sql {
			t.Fatalf("the bound value changed the SQL: %q vs %q", sql, fixed)
		}
		if len(args) != 2 {
			t.Fatalf("len(args) = %d, want 2", len(args))
		}
		if got, _ := args[0].(string); got != val {
			t.Fatalf("args[0] = %q, want %q", got, val)
		}
		if got, _ := args[1].([]byte); string(got) != val {
			t.Fatalf("args[1] = %q, want %q", got, val)
		}
	})
}

// The same invariant has to hold for the dialects that renumber or
// re-spell the placeholder, because a placeholder function is the one
// hook a caller can install between a value and the statement.
func FuzzAddArgUnderQuestionMarkPlaceholders(f *testing.F) {
	for _, s := range nastyIdents {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, val string) {
		b := drops.NewBuilder(drops.WithPlaceholder(func(int) string { return "?" }))
		b.WriteString("VALUES (")
		b.AddArg(val)
		b.WriteString(", ")
		b.AddArg(val)
		b.WriteString(")")
		sql, args := b.SQL()
		if sql != "VALUES (?, ?)" {
			t.Fatalf("sql = %q, want %q", sql, "VALUES (?, ?)")
		}
		if len(args) != 2 || args[0] != any(val) || args[1] != any(val) {
			t.Fatalf("args = %v, want two copies of %q", args, val)
		}
	})
}

// Raw is the deliberate hole in all of the above, and a hole nobody
// tests is a hole that quietly acquires escaping later — at which
// point the callers that were relying on verbatim SQL break. So the
// contract is pinned from the other side: Raw writes its text byte for
// byte and binds nothing, whatever it contains.
func FuzzRawIsVerbatimAndBindsNothing(f *testing.F) {
	for _, s := range nastyIdents {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		sql, args := drops.String(drops.Raw(text))
		if sql != text {
			t.Fatalf("Raw rendered %q, want the input verbatim %q", sql, text)
		}
		if len(args) != 0 {
			t.Fatalf("Raw bound %d args, want 0", len(args))
		}
	})
}
