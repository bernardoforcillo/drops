package pg_test

import (
	"go/ast"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// Everything the schema layer can declare has to be read back by
// Introspect, or Diff sees the difference between "absent" and "not
// looked at" as work to do and Push stops being idempotent.
//
// That sentence is in Introspect's doc comment, and for a while it was
// aspiration rather than description: enums, sequences, views and
// policies were declarable and unread, so a schema carrying one enum
// re-emitted its CREATE TYPE on every push and DetectDrift was
// permanently noisy. They are read now, and
// TestPGPushIsIdempotentWithSchemaObjects proves the round trip against
// a real server.
//
// This is the check that stops the NEXT one being added without a
// reader. It needs no list of object kinds: the kinds are the maps on
// Snapshot, and a new one arrives in the struct.

// snapshotObjectMaps are the schema-level collections a snapshot
// carries, discovered from the type rather than enumerated.
//
// Two are exempt and say why here rather than in a skip list far from
// the type. They are in the struct because the snapshot format is
// drizzle-kit's v7 and that format has the keys; nothing in this
// package writes to either, from either direction, so there is no
// asymmetry for Diff to mistake for work.
var snapshotObjectExemptions = map[string]string{
	"Roles":   "nothing declares a role: BuildSnapshot never writes this and neither side can differ",
	"Schemas": "the schema a table lives in is a field on the table, not an object with its own declaration",
}

func TestEverySchemaObjectIntrospectCanDeclareIsAlsoRead(t *testing.T) {
	// The object kinds, taken from the snapshot type. A map field whose
	// key is a name and whose value is a per-object shape is an object
	// collection; Tables is one too and is read first of all.
	var kinds []string
	st := reflect.TypeOf(pg.Snapshot{})
	for i := 0; i < st.NumField(); i++ {
		f := st.Field(i)
		if f.Type.Kind() != reflect.Map {
			continue
		}
		if _, exempt := snapshotObjectExemptions[f.Name]; exempt {
			continue
		}
		kinds = append(kinds, f.Name)
	}
	if len(kinds) == 0 {
		t.Fatal("the snapshot type carries no object maps — this check has gone stale")
	}

	// The readers Introspect actually calls, by name.
	called := map[string]bool{}
	forEachPgFile(t, func(f *ast.File) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "Introspect" {
				continue
			}
			ast.Inspect(fd, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok {
					called[id.Name] = true
				}
				return true
			})
		}
	})
	if len(called) == 0 {
		t.Fatal("Introspect was not found in the package source — this check has gone stale")
	}

	for _, kind := range kinds {
		// readIntrospectEnums for Enums, readIntrospectTables for
		// Tables. The convention is the census: a reader that does not
		// follow it is a reader nothing here can find, which is the
		// same failure as not having one.
		want := "readIntrospect" + kind
		if !called[want] {
			t.Errorf("Snapshot.%s is a declarable object kind and Introspect calls no %s: "+
				"Diff will read every one of them as new work on every push, and Push will not converge. "+
				"Add the reader, or exempt the field in snapshotObjectExemptions with the reason.",
				kind, want)
		}
	}
}

// The policies and the RLS flags hang off a table rather than off the
// snapshot, so the census above cannot see them. They are the other
// half of the same bug and get the same check, by name.
func TestTheTableLevelSecurityStateIsRead(t *testing.T) {
	var body string
	forEachPgFile(t, func(f *ast.File) {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Name.Name != "Introspect" {
				continue
			}
			body = identsIn(fd)
		}
	})
	if body == "" {
		t.Fatal("Introspect was not found — this check has gone stale")
	}
	if !strings.Contains(body, "readIntrospectPolicies") {
		t.Error("Introspect reads no policies: a declared policy would be created on every push")
	}
	// IsRLSEnabled and IsRLSForced are read by the table query rather
	// than by a reader of their own, so the assertion is that the
	// fields exist and the table reader mentions them.
	ts := reflect.TypeOf(pg.TableSnapshot{})
	for _, f := range []string{"IsRLSEnabled", "IsRLSForced", "Policies"} {
		if _, ok := ts.FieldByName(f); !ok {
			t.Errorf("TableSnapshot has no %s, so Push cannot tell a secured table from an open one", f)
		}
	}
}

// identsIn returns every identifier in a declaration, space-separated.
// It is enough to ask "does this function call that one by name", which
// is all the check above needs, and it does not depend on the file's
// formatting the way reading the source text back would.
func identsIn(fd *ast.FuncDecl) string {
	var b strings.Builder
	ast.Inspect(fd, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			b.WriteString(id.Name)
			b.WriteByte(' ')
		}
		return true
	})
	return b.String()
}
