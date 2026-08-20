package pg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestRowScopeFilterKeyIsTheOnlyKeyBuilder is a source-level assertion,
// and it is one deliberately: the defect it pins is a declaration that
// nothing calls.
//
// Two spellings of the row-scope filter key existed at once — the
// entity's own (*Entity[T]).rowScopeKey, which named the row type with
// reflect.Type.String(), and rowScopeFilterKey, written to replace it
// because String() prints the SHORT package name and two User types
// from two packages both ending in "app" would share one key, losing
// one of the two row-visibility filters. Only the second had callers.
//
// Go does not report an unused method, and neither does vet, so the
// dead one sat there behind an authoritative doc comment describing the
// keying policy — exactly what the next person adding a scope kind
// reaches for. Nothing about its behaviour can fail a test, because
// nothing runs it; what can fail is its existence, so that is what this
// checks.
func TestRowScopeFilterKeyIsTheOnlyKeyBuilder(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing package pg: %v", err)
	}
	var got []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if ok && strings.Contains(fn.Name.Name, "rowScope") && strings.HasSuffix(fn.Name.Name, "Key") {
					got = append(got, fn.Name.Name)
				}
			}
		}
	}
	sort.Strings(got)
	want := []string{"rowScopeFilterKey"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("row-scope filter key builders declared in package pg: got = %v, want %v", got, want)
	}
}

// TestRowScopeFilterKeySeparatesSameNamedTypes pins the property the
// surviving spelling exists for: the key carries the row type's import
// path, so two types with the same name under two packages sharing a
// last path element keep two filters instead of one silently replacing
// the other.
func TestRowScopeFilterKeySeparatesSameNamedTypes(t *testing.T) {
	tests := []struct {
		name string
		a, b reflect.Type
		same bool
	}{
		{
			name: "two row types from one package are two keys",
			a:    reflect.TypeOf(struct{ A int }{}),
			b:    reflect.TypeOf(scopeKeyRow{}),
		},
		{
			name: "one row type is one key",
			a:    reflect.TypeOf(scopeKeyRow{}),
			b:    reflect.TypeOf(scopeKeyRow{}),
			same: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rowScopeFilterKey(tt.a, "tenant") == rowScopeFilterKey(tt.b, "tenant")
			if got != tt.same {
				t.Errorf("keys are equal: got = %v, want %v\n a: %s\n b: %s", got, tt.same,
					rowScopeFilterKey(tt.a, "tenant"), rowScopeFilterKey(tt.b, "tenant"))
			}
		})
	}
	// The named type is spelled with its import path, which is what a
	// short package name cannot be trusted to disambiguate.
	if got, want := rowScopeFilterKey(reflect.TypeOf(scopeKeyRow{}), "tenant"), "tenant:github.com/bernardoforcillo/drops/pg.scopeKeyRow"; got != want {
		t.Errorf("key for a named row type: got = %v, want %v", got, want)
	}
	// A kind and a type are never confusable across kinds.
	if got, want := rowScopeFilterKey(reflect.TypeOf(scopeKeyRow{}), "guard") == rowScopeFilterKey(reflect.TypeOf(scopeKeyRow{}), "tenant"), false; got != want {
		t.Errorf("tenant key equals guard key: got = %v, want %v", got, want)
	}
}

// scopeKeyRow is a named row type for the key tests above.
type scopeKeyRow struct{ ID int64 }
