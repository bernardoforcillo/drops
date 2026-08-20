package pg

import (
	"strings"
	"testing"
)

// queryKey is the only thing keeping one tenant's cached result set
// apart from another's — the tenant predicate is resolved by the
// executor, so two tenants asking the same question send the same SQL
// and differ only in a bound argument. These are the properties that
// makes it fit to hold that boundary; each of them was false when the
// key was sha256(sql + "%v" per arg) truncated to sixteen hex digits.
func TestQueryKeyIsBuiltForIsolation(t *testing.T) {
	const table = "users"
	sql := `SELECT * FROM "users" WHERE ("users"."tenantId" = $1)`
	seven := int64(7)
	otherSeven := int64(7)

	tests := []struct {
		name string
		a    []any
		b    []any
		same bool
	}{
		{
			// The one that leaked: an app reading the tenant off an
			// HTTP header on one path and out of a database column on
			// another holds both spellings of one customer id.
			name: "string and int64 tenant are different keys",
			a:    []any{"7"},
			b:    []any{int64(7)},
		},
		{
			name: "int64 and float64 tenant are different keys",
			a:    []any{int64(7)},
			b:    []any{float64(7)},
		},
		{
			// Without a length prefix a value that contains the
			// separator byte spells out its neighbours.
			name: "an argument cannot impersonate an argument pair",
			a:    []any{"a\x00b"},
			b:    []any{"a", "b"},
		},
		{
			name: "argument order matters",
			a:    []any{int64(1), int64(2)},
			b:    []any{int64(2), int64(1)},
		},
		{
			// Pointers are followed: an address is not stable across
			// runs, and worse, it is reused — a freed tenant value's
			// address hands its cache entry to whatever lands there
			// next.
			name: "equal values behind different pointers are one key",
			a:    []any{&seven},
			b:    []any{&otherSeven},
			same: true,
		},
		{
			name: "a pointer and its pointee are different keys",
			a:    []any{&seven},
			b:    []any{seven},
		},
		{
			name: "identical arguments are one key",
			a:    []any{"7", int64(3)},
			b:    []any{"7", int64(3)},
			same: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := queryKey(table, sql, tt.a) == queryKey(table, sql, tt.b)
			if got != tt.same {
				t.Errorf("keys are equal: got = %v, want %v\n a: %s\n b: %s",
					got, tt.same, queryKey(table, sql, tt.a), queryKey(table, sql, tt.b))
			}
		})
	}

	// The digest is not truncated. At 64 bits a collision is expected
	// around 2^32 entries, and a collision here answers one tenant's
	// query with another tenant's rows.
	key := queryKey(table, sql, []any{int64(7)})
	digest := strings.TrimPrefix(key, `drops:`+table+`:q:`)
	if digest == key {
		t.Fatalf("key does not carry the documented prefix: got = %v, want %v...", key, `drops:`+table+`:q:`)
	}
	if got, want := len(digest), 64; got != want {
		t.Errorf("digest length in hex digits: got = %v, want %v", got, want)
	}
}

// TestQueryKeyHandlesNilArguments pins the encoding of a nil argument:
// a NULL parameter is ordinary in a WHERE clause and must not panic
// the key builder that every cached read goes through.
func TestQueryKeyHandlesNilArguments(t *testing.T) {
	var nilPtr *int64
	sql := `SELECT * FROM "users" WHERE ("users"."name" = $1)`
	keys := map[string]string{
		"untyped nil": queryKey("users", sql, []any{nil}),
		"nil pointer": queryKey("users", sql, []any{nilPtr}),
		"zero value":  queryKey("users", sql, []any{int64(0)}),
	}
	seen := map[string]string{}
	for name, k := range keys {
		if other, dup := seen[k]; dup {
			t.Errorf("%s and %s share a key: got = %v, want distinct keys", name, other, k)
		}
		seen[k] = name
	}
}
