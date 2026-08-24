package sqlite

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// Forget With("posts") and the parent's Posts field stays nil, which
// reads exactly like "this row has no posts". Go has no lazy loading
// to fall back on — that is a feature — but the loop only closes if
// the omission is visible, so drops closes it by refusing the query
// rather than by letting the read lie. See drops/pg's strict.go for
// the full reasoning; this is the same check against drops/sqlite's
// single-level Find:
//
//	db := sqlite.New(drv)
//	if devMode {
//	    db = db.StrictLoading()
//	}
//
// Because Find here loads one level, the check looks at one level: a
// relation declared on the root table and carried as a field on the
// destination struct. Descending further would refuse queries drops
// has no way to satisfy.

// ErrRelationNotLoaded is the sentinel behind every strict-loading
// refusal, so a caller can errors.Is a failed query into "somebody
// forgot to load a relation".
var ErrRelationNotLoaded = errors.New("drops/sqlite: relation not loaded")

// StrictLoading returns a shallow copy of db whose Find builders refuse
// a query that would leave a declared relation field unloaded. Meant
// for development and test builds, where a refused query is a failing
// test rather than a failing request.
func (db *DB) StrictLoading() *DB {
	cp := *db
	cp.strictLoading = true
	return &cp
}

// IsStrictLoading reports whether db refuses under-specified relation
// queries.
func (db *DB) IsStrictLoading() bool { return db.strictLoading }

// Strict turns the strict-loading check on for this query alone.
func (f *FindBuilder) Strict() *FindBuilder { f.strict = true; return f }

// NoLoad declares that this query deliberately does not load rels — the
// waiver the strict-loading check accepts. It does not mean "do not
// load", which is already the default, but "I know it is not loaded and
// I will not read it". A nil relation is ignored, and outside strict
// mode NoLoad does nothing.
func (f *FindBuilder) NoLoad(rels ...*Relation) *FindBuilder {
	for _, r := range rels {
		if r == nil {
			continue
		}
		f.waived = append(f.waived, r.Name)
	}
	return f
}

// Without is [FindBuilder.NoLoad] taking relation names rather than
// handles.
func (f *FindBuilder) Without(names ...string) *FindBuilder {
	f.waived = append(f.waived, names...)
	return f
}

// checkStrictLoading refuses the query when the destination struct
// carries a relation field nothing in this query fills. Pure and cheap:
// one walk of a struct type, and no round trip.
func (f *FindBuilder) checkStrictLoading(dest any) error {
	if !f.strict || f.table == nil {
		return nil
	}
	structT := destStructType(dest)
	if structT == nil {
		return nil
	}
	names := make([]string, 0, len(f.table.relations))
	for name := range f.table.relations {
		names = append(names, name)
	}
	// Deterministic: a query missing two relations always reports the
	// same one first.
	sort.Strings(names)

	for _, name := range names {
		field, ok := relationTargetField(structT, name)
		if !ok {
			// The destination does not carry this relation — a
			// projection struct. Nothing can read a field that is not
			// there.
			continue
		}
		if !relationShaped(structT.FieldByIndex(field).Type) {
			// A column field that happens to share a name with a
			// relation; relationTargetField matches by name as a
			// fallback.
			continue
		}
		if containsName(f.waived, name) || containsName(f.withs, name) {
			continue
		}
		return fmt.Errorf(
			"%w: %q on struct %s — this query never loaded it, and an unloaded relation field is indistinguishable from an empty one. "+
				"Load it with .With(%q), or say the query does not need it with .Without(%q)",
			ErrRelationNotLoaded, name, structName(structT), name, name)
	}
	return nil
}

// structName renders a struct type the way a caller would recognise
// it: package-qualified, which is how it reads at the declaration.
func structName(t reflect.Type) string { return t.String() }

var timeType = reflect.TypeOf(time.Time{})

// relationShaped reports whether ft could hold a loaded relation: a
// struct, a pointer or slice of one, or an interface.
func relationShaped(ft reflect.Type) bool {
	if ft.Kind() == reflect.Interface {
		return true
	}
	for ft.Kind() == reflect.Slice || ft.Kind() == reflect.Ptr {
		ft = ft.Elem()
	}
	return ft.Kind() == reflect.Struct && ft != timeType
}

func containsName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// destStructType peels a *[]T / *[]*T / *T destination down to the
// struct type the rows land in, or nil for a shape the scanner is going
// to reject anyway.
func destStructType(dest any) reflect.Type {
	t := reflect.TypeOf(dest)
	if t == nil || t.Kind() != reflect.Ptr {
		return nil
	}
	t = t.Elem()
	if t.Kind() == reflect.Slice {
		t = t.Elem()
	}
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	return t
}
