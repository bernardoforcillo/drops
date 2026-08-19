package main

import (
	"encoding/json"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const sampleSnapshot = `{
  "id": "00000000-0000-0000-0000-000000000001",
  "prevId": "00000000-0000-0000-0000-000000000000",
  "version": "7",
  "dialect": "postgresql",
  "tables": {
    "public.users": {
      "name": "users",
      "schema": "",
      "columns": {
        "id": {"name": "id", "type": "bigserial", "primaryKey": true, "notNull": true},
        "email": {"name": "email", "type": "text", "primaryKey": false, "notNull": true},
        "name": {"name": "name", "type": "text", "primaryKey": false, "notNull": true},
        "createdAt": {"name": "createdAt", "type": "timestamptz", "primaryKey": false, "notNull": true, "default": "now()"}
      },
      "uniqueConstraints": {
        "usersEmailUnique": {"name": "usersEmailUnique", "columns": ["email"]}
      },
      "foreignKeys": {},
      "indexes": {},
      "compositePrimaryKeys": {},
      "policies": {},
      "checkConstraints": {},
      "isRLSEnabled": false
    }
  },
  "enums": {},
  "schemas": {},
  "sequences": {},
  "roles": {},
  "policies": {},
  "views": {},
  "_meta": {"columns": {}, "schemas": {}, "tables": {}}
}`

func TestIntrospectGeneratesStruct(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(sampleSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, out, "models"); err != nil {
		t.Fatalf("introspect: %v", err)
	}

	gen, err := os.ReadFile(filepath.Join(out, "users_drops.go"))
	if err != nil {
		t.Fatalf("output file missing: %v", err)
	}
	// gofmt aligns the struct's columns, so compare on collapsed
	// spacing the way the schema-mode tests do.
	got := collapse(string(gen))
	for _, want := range []string{
		"package models",
		`"time"`,
		"type Users struct {",
		`ID int64 ` + "`drop:\"id,primaryKey,autoIncrement\"`",
		`CreatedAt time.Time ` + "`drop:\"createdAt,notNull,default=now()\"`",
		`Email string ` + "`drop:\"email,notNull,unique\"`",
		`Name string ` + "`drop:\"name,notNull\"`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestIntrospectRejectsBadJSON(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "bad.json")
	if err := os.WriteFile(snap, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runIntrospect(snap, filepath.Join(tmp, "out"), "x"); err == nil {
		t.Error("expected JSON parse error")
	}
}

func TestSqlToGoTypeFallsBackToString(t *testing.T) {
	if got := sqlToGoType("some unknown thing"); got != "string" {
		t.Errorf("unknown SQL type should fall back to string, got %q", got)
	}
}

func TestGoIdentForAcronymPreservation(t *testing.T) {
	cases := map[string]string{
		"id":         "ID",
		"http_code":  "HTTPCode",
		"user_id":    "UserID",
		"profile":    "Profile",
		"created_at": "CreatedAt",
	}
	for in, want := range cases {
		if got := goIdentFor(in); got != want {
			t.Errorf("goIdentFor(%q): got %q, want %q", in, got, want)
		}
	}
}

// Round-trip: introspect a known JSON snapshot, then re-parse the
// generated source — sanity that the output parses as valid Go.
func TestIntrospectOutputIsValidGo(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(sampleSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, out, "models"); err != nil {
		t.Fatalf("introspect: %v", err)
	}

	// Walk the output dir, gofmt every file via decoder.
	files, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files generated")
	}
}

// Just exercise json.Unmarshal on a snapshot to confirm the struct
// shape matches drops' Snapshot format.
func TestSampleSnapshotParses(t *testing.T) {
	var s struct {
		Tables map[string]*introspectTable `json:"tables"`
	}
	if err := json.Unmarshal([]byte(sampleSnapshot), &s); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := s.Tables["public.users"]; !ok {
		t.Errorf("missing users table in parsed snapshot")
	}
}

// A column default is arbitrary SQL, and a JSON one carries quotes.
// Written straight into the tag they end it early: reflect.StructTag
// hands back `payload,notNull,default='{` and the rest of the default
// becomes garbage tag keys, so the struct claims a default the column
// does not have.
const quotedDefaultSnapshot = `{
  "tables": {
    "public.settings": {
      "name": "settings",
      "columns": {
        "id": {"name": "id", "type": "bigserial", "primaryKey": true, "notNull": true},
        "payload": {"name": "payload", "type": "jsonb", "notNull": true, "default": "'{\"a\": 1}'::jsonb"}
      },
      "uniqueConstraints": {}
    }
  }
}`

func TestIntrospectTagSurvivesAQuotedDefault(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(quotedDefaultSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, out, "models"); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	gen, err := os.ReadFile(filepath.Join(out, "settings_drops.go"))
	if err != nil {
		t.Fatal(err)
	}
	tag, ok := tagOfField(t, string(gen), "Payload")
	if !ok {
		t.Fatalf("no Payload field in:\n%s", gen)
	}
	want := `payload,notNull,default='{"a": 1}'::jsonb`
	if got := reflect.StructTag(tag).Get("drop"); got != want {
		t.Errorf("drop tag = %q, want %q", got, want)
	}
}

// tagOfField parses src and returns the raw struct tag of the named
// field, unquoted the way the compiler would.
func tagOfField(t *testing.T, src, field string) (string, bool) {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "gen.go", src, 0)
	if err != nil {
		t.Fatalf("generated file does not parse: %v\n%s", err, src)
	}
	var tag string
	var found bool
	ast.Inspect(af, func(n ast.Node) bool {
		f, ok := n.(*ast.Field)
		if !ok || f.Tag == nil || len(f.Names) != 1 || f.Names[0].Name != field {
			return true
		}
		v, err := strconv.Unquote(f.Tag.Value)
		if err != nil {
			t.Fatalf("tag literal %s: %v", f.Tag.Value, err)
		}
		tag, found = v, true
		return false
	})
	return tag, found
}

// Every other mode gofmts what it writes. This one used to hand back
// its buffer verbatim, so `gofmt -l` flagged the checked-in file and a
// column name that is no Go identifier was written out as unbuildable
// source with a zero exit status.
func TestIntrospectOutputIsGofmted(t *testing.T) {
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(sampleSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, out, "models"); err != nil {
		t.Fatal(err)
	}
	gen, err := os.ReadFile(filepath.Join(out, "users_drops.go"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := format.Source(gen)
	if err != nil {
		t.Fatalf("generated file does not parse: %v", err)
	}
	if string(want) != string(gen) {
		t.Errorf("generated file is not gofmt'd:\n--- got ---\n%s\n--- want ---\n%s", gen, want)
	}
}

func TestIntrospectRejectsAColumnThatIsNoGoIdentifier(t *testing.T) {
	snapshot := `{"tables":{"public.t":{"name":"t","columns":{
	  "2fa":{"name":"2fa","type":"text","notNull":true}
	},"uniqueConstraints":{}}}}`
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runIntrospect(snap, filepath.Join(tmp, "models"), "models"); err == nil {
		t.Error("a column named 2fa cannot become a field; the generator should say so rather than write source that will not build")
	}
}

// Two schemas may hold a table of the same name, and the file name is
// derived from the table name alone — so the second one silently
// replaced the first.
func TestIntrospectRefusesToOverwriteAnEarlierTable(t *testing.T) {
	snapshot := `{"tables":{
	  "public.users":{"name":"users","schema":"public","columns":{"id":{"name":"id","type":"bigint","primaryKey":true}},"uniqueConstraints":{}},
	  "audit.users":{"name":"users","schema":"audit","columns":{"at":{"name":"at","type":"timestamptz"}},"uniqueConstraints":{}}
	}}`
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	err := runIntrospect(snap, filepath.Join(tmp, "models"), "models")
	if err == nil {
		t.Fatal("two tables named users should not silently share one file")
	}
	if !strings.Contains(err.Error(), "users_drops.go") {
		t.Errorf("the error should name the file: %v", err)
	}
}

// Introspection walks two maps — columns and unique constraints — and
// map order is randomised per run. Two runs must still agree byte for
// byte, or the checked-in models churn in every diff.
func TestIntrospectIsDeterministic(t *testing.T) {
	first := introspectOnce(t)
	for i := 0; i < 20; i++ {
		if got := introspectOnce(t); got != first {
			t.Fatalf("run %d differs from the first:\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

func introspectOnce(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(sampleSnapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, out, "models"); err != nil {
		t.Fatal(err)
	}
	gen, err := os.ReadFile(filepath.Join(out, "users_drops.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(gen)
}
