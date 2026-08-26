package pg

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

// The role in SET LOCAL ROLE is the one caller-supplied string in this
// feature that becomes SQL text rather than a bound parameter, because
// PostgreSQL's grammar has no placeholder slot for it. That makes it
// the only place where getting the escaping wrong ends a token early
// and hands the rest of the string to the parser — the same exposure
// pg/ident_fuzz_test.go was written for, and the reason this file
// reuses its lexer and its corpus instead of asserting against
// expected output.
//
// The property is the one that matters and it is not "does it look
// escaped": every role name Session.check accepts must appear in the
// statement as one closed quoted identifier with nothing after it, and
// must read back as exactly the bytes the caller supplied.
func FuzzPgSessionRoleCannotEscapeTheStatement(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, role string) {
		s := Session{Role: role}
		_, err := s.check()
		if role == "" {
			// An empty role is not a refusal about the name; it means
			// "leave the role alone", and with no settings beside it
			// the session is empty.
			if !errors.Is(err, ErrEmptySession) {
				t.Fatalf("empty role: err = %v, want ErrEmptySession", err)
			}
			return
		}
		if err != nil {
			if !errors.Is(err, ErrInvalidRole) {
				t.Fatalf("refused %q with %v, want ErrInvalidRole", role, err)
			}
			return
		}

		const prefix = "SET LOCAL ROLE "
		stmt := prefix + quoteIdent(role)
		if !strings.HasPrefix(stmt, prefix) {
			t.Fatalf("statement %q does not open with %q", stmt, prefix)
		}
		// The lexer from ident_fuzz_test.go: a token that opens with a
		// double quote, closes on the first quote not doubled, and
		// recognises no other escape.
		checkIdent(t, strings.TrimPrefix(stmt, prefix), role)
		if strings.ContainsRune(stmt, 0) {
			t.Fatalf("accepted role %q renders a NUL byte into the statement, which truncates it on the wire", role)
		}
	})
}

// A setting name is bound, so this target is not about escaping. It is
// about the boundary check.check draws being the one the doc claims:
// the three byte patterns a text parameter cannot carry, and nothing
// else. A name accepted here goes to the server verbatim as $1, so
// over-refusing would reject legal GUC names and under-refusing would
// hand the driver bytes it cannot encode.
func FuzzPgSessionSettingNameBoundary(f *testing.F) {
	for _, s := range nastyNames {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		s := Session{Settings: map[string]string{key: "v"}}
		keys, err := s.check()

		want := key == "" || strings.ContainsRune(key, 0) || !utf8.ValidString(key)
		if want {
			if !errors.Is(err, ErrInvalidSetting) {
				t.Fatalf("key %q: err = %v, want ErrInvalidSetting", key, err)
			}
			return
		}
		if err != nil {
			t.Fatalf("key %q was refused with %v, and is none of empty, NUL-bearing or invalid UTF-8", key, err)
		}
		if len(keys) != 1 || keys[0] != key {
			t.Fatalf("keys = %q, want exactly %q — the key must reach the placeholder unchanged", keys, key)
		}
	})
}

// Session.check runs before any transaction is opened, and its verdict
// is the whole of what InTxAs decides locally. The table states each
// verdict in its own terms so a change to one does not quietly become
// a change to another.
func TestSessionCheckVerdicts(t *testing.T) {
	tests := []struct {
		name     string
		session  Session
		wantErr  error
		wantKeys []string
	}{
		{
			name:    "empty",
			session: Session{},
			wantErr: ErrEmptySession,
		},
		{
			name:     "role alone is enough",
			session:  Session{Role: "app"},
			wantKeys: []string{},
		},
		{
			name:     "settings alone are enough",
			session:  Session{Settings: map[string]string{"app.x": "1"}},
			wantKeys: []string{"app.x"},
		},
		{
			name: "keys come back sorted",
			session: Session{Settings: map[string]string{
				"b": "2", "a": "1", "c": "3",
			}},
			wantKeys: []string{"a", "b", "c"},
		},
		{
			name:    "a bad role is refused even with good settings",
			session: Session{Role: "a\x00b", Settings: map[string]string{"app.x": "1"}},
			wantErr: ErrInvalidRole,
		},
		{
			name:    "a bad setting is refused even with a good role",
			session: Session{Role: "app", Settings: map[string]string{"": "1"}},
			wantErr: ErrInvalidSetting,
		},
		{
			name:     "a value is never inspected",
			session:  Session{Settings: map[string]string{"app.x": "a'b\"c;--\x00"}},
			wantKeys: []string{"app.x"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := tc.session.check()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if keys != nil {
					t.Errorf("keys = %q, want none alongside a refusal", keys)
				}
				return
			}
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if len(keys) != len(tc.wantKeys) {
				t.Fatalf("keys = %q, want %q", keys, tc.wantKeys)
			}
			for i := range keys {
				if keys[i] != tc.wantKeys[i] {
					t.Fatalf("keys = %q, want %q", keys, tc.wantKeys)
				}
			}
		})
	}
}
