package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/internal/drift"
)

// Every generated column states its nullability, so nothing dropsgen
// emits can be the unstated shape pg.NewEntity now rejects. The
// statement comes from the field's type, not from the absence of a
// tag.
func TestSchemaStatesNullabilityOnEveryColumn(t *testing.T) {
	src := `package models

import (
	"database/sql"
	"encoding/json"
	"time"
)

//drops:schema table=Rows name=rows
type Row struct {
	ID      int64            ` + "`drop:\"id,primaryKey,autoIncrement\"`" + `
	Name    string           ` + "`drop:\"name\"`" + `
	Bio     *string          ` + "`drop:\"bio\"`" + `
	Meta    sql.Null[string] ` + "`drop:\"meta\"`" + `
	Legacy  sql.NullInt64    ` + "`drop:\"legacy\"`" + `
	Blob    []byte           ` + "`drop:\"blob\"`" + `
	Raw     json.RawMessage  ` + "`drop:\"raw\"`" + `
	Deleted *time.Time       ` + "`drop:\"deleted\"`" + `
}
`
	got := collapse(generateSchema(t, src))
	for _, want := range []string{
		// PrimaryKey already said NOT NULL; saying it twice is noise.
		`pg.BigSerial("id").PrimaryKey())`,
		`pg.Text("name").NotNull()`,
		`pg.Text("bio").Nullable()`,
		// The wrapper says the column is optional and carries the
		// type that says what it holds — the constructor takes the
		// payload, so the operators stay typed on the value.
		`pg.Text("meta").Nullable()`,
		`pg.BigInt("legacy").Nullable()`,
		// A []byte can receive a NULL, but writing one is not the
		// author saying the column is optional. Expecting Nullable
		// here would encode exactly the conflation this design avoids.
		`pg.Bytea("blob").NotNull()`,
		`pg.JSONB("raw").NotNull()`,
		`pg.Timestamp("deleted", true).Nullable()`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated file missing:\n  %s\ngot:\n%s", want, got)
		}
	}
}

// `null` is the escape hatch for a field whose type cannot state
// optionality — a named type with its own Scan method, a domain type
// behind a type=. dropsgen reads source and cannot resolve interface
// satisfaction, so without the tag it declares NOT NULL.
func TestSchemaNullTagOverridesTheFieldType(t *testing.T) {
	src := `package models

//drops:schema table=T name=t
type Row struct {
	ID   int64  ` + "`drop:\"id,primaryKey\"`" + `
	Note string ` + "`drop:\"note,null,type=citext\"`" + `
}
`
	got := generateSchema(t, src)
	if !strings.Contains(got, `pg.Custom[string]("note", "citext").Nullable()`) {
		t.Errorf("`null` should have stated the column nullable:\n%s", got)
	}
}

func TestSchemaRejectsNotNullAndNullTogether(t *testing.T) {
	_, err := columnExpr("string", "note", []colOption{{Key: "notNull"}, {Key: "null"}})
	if err == nil {
		t.Fatal("expected an error for contradictory tags")
	}
	if !strings.Contains(err.Error(), "drop one or the other") {
		t.Errorf("error should say how to resolve it: %v", err)
	}
}

// The two generators are two implementations of one rule — dropsgen's
// over the AST, pg.AutoTable's over reflect.Type — and
// constructorFor's comment already demands they agree, since a
// project using both would otherwise get two schemas from one struct.
// This is where a divergence has to fail.
func TestInferNullableAgreesWithTheReflectiveTwin(t *testing.T) {
	cases := []struct {
		goType string
		value  any
	}{
		{"string", ""},
		{"*string", (*string)(nil)},
		{"int64", int64(0)},
		{"*int64", (*int64)(nil)},
		{"time.Time", time.Time{}},
		{"*time.Time", (*time.Time)(nil)},
		{"[]byte", []byte(nil)},
		{"json.RawMessage", json.RawMessage(nil)},
		{"sql.Null[string]", sql.Null[string]{}},
		{"sql.NullInt64", sql.NullInt64{}},
		{"sql.NullTime", sql.NullTime{}},
		{"sql.NullBool", sql.NullBool{}},
	}
	for _, c := range cases {
		textual := inferNullable(c.goType)
		reflective := drift.InferNullable(reflect.TypeOf(c.value))
		if textual != reflective {
			t.Errorf("%s: dropsgen says nullable=%v, pg.AutoTable says %v",
				c.goType, textual, reflective)
		}
	}
}

// nullWrappers is hardcoded because an AST generator cannot ask
// reflection what a type expression means. This asserts the table is
// right, so a member added to the family — or a payload field
// renamed — fails here rather than in someone's generated schema.
func TestNullWrappersAgreeWithReflection(t *testing.T) {
	values := map[string]any{
		"sql.NullBool":    sql.NullBool{},
		"sql.NullByte":    sql.NullByte{},
		"sql.NullFloat64": sql.NullFloat64{},
		"sql.NullInt16":   sql.NullInt16{},
		"sql.NullInt32":   sql.NullInt32{},
		"sql.NullInt64":   sql.NullInt64{},
		"sql.NullString":  sql.NullString{},
		"sql.NullTime":    sql.NullTime{},
	}
	if len(values) != len(nullWrappers) {
		t.Fatalf("nullWrappers has %d entries, the test knows %d", len(nullWrappers), len(values))
	}
	for name, v := range values {
		payload, ok := drift.NullPayload(reflect.TypeOf(v))
		if !ok {
			t.Errorf("%s: reflection does not see a null wrapper", name)
			continue
		}
		want := payload.String()
		// reflect spells time.Time's package the same way source does,
		// and byte as uint8 — which is the same type either way.
		if want == "uint8" {
			want = "byte"
		}
		if got := nullWrappers[name]; got != want {
			t.Errorf("%s: table says %q, reflection says %q", name, got, want)
		}
	}
}

// The two modes have to compose: introspect a live schema, feed the
// struct it writes to schema mode, and the declarations must describe
// the schema you started from. A nullable column therefore has to
// come back as a field type schema mode reads as nullable — and one
// that can actually receive the NULL production will send.
func TestIntrospectRoundTripsNullability(t *testing.T) {
	const snapshot = `{
  "id": "00000000-0000-0000-0000-000000000001",
  "prevId": "00000000-0000-0000-0000-000000000000",
  "version": "7",
  "dialect": "postgresql",
  "tables": {
    "public.people": {
      "name": "people",
      "schema": "",
      "columns": {
        "id": {"name": "id", "type": "bigserial", "primaryKey": true, "notNull": true},
        "name": {"name": "name", "type": "text", "primaryKey": false, "notNull": true},
        "bio": {"name": "bio", "type": "text", "primaryKey": false, "notNull": false}
      },
      "uniqueConstraints": {},
      "foreignKeys": {},
      "indexes": {},
      "compositePrimaryKeys": {},
      "policies": {},
      "checkConstraints": {},
      "isRLSEnabled": false
    }
  },
  "enums": {}, "schemas": {}, "sequences": {}, "roles": {},
  "policies": {}, "views": {},
  "_meta": {"columns": {}, "schemas": {}, "tables": {}}
}`
	tmp := t.TempDir()
	snap := filepath.Join(tmp, "snap.json")
	if err := os.WriteFile(snap, []byte(snapshot), 0o644); err != nil {
		t.Fatal(err)
	}
	outDir := filepath.Join(tmp, "models")
	if err := runIntrospect(snap, outDir, "models"); err != nil {
		t.Fatalf("introspect: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(outDir, "people_drops.go"))
	if err != nil {
		t.Fatal(err)
	}
	got := collapse(string(src))
	if !strings.Contains(got, "Bio *string") {
		t.Errorf("a nullable column must yield a field that can receive NULL:\n%s", src)
	}
	if !strings.Contains(got, "Name string ") {
		t.Errorf("a NOT NULL column must stay a plain field:\n%s", src)
	}
	// And the second leg: what schema mode makes of that field.
	if !inferNullable("*string") || inferNullable("string") {
		t.Error("schema mode must read the introspected field types back the same way")
	}
}
