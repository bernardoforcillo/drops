package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func generateSchema(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	in := filepath.Join(dir, "models.go")
	if err := os.WriteFile(in, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSchema(in, ""); err != nil {
		t.Fatalf("runSchema: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(dir, "models_drops_schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

const basicSrc = `package models

import "time"

//drops:schema table=Users name=users
type User struct {
	ID        int64     ` + "`drop:\"id,primaryKey,autoIncrement\"`" + `
	Email     string    ` + "`drop:\"email,notNull,unique\"`" + `
	Age       *int32    ` + "`drop:\"age\"`" + `
	CreatedAt time.Time ` + "`drop:\"createdAt,notNull,default=now()\"`" + `
	Ignored   string    ` + "`drop:\"-\"`" + `
	Untagged  string
}
`

func TestSchemaGeneratesTypedDeclarations(t *testing.T) {
	got := generateSchema(t, basicSrc)
	// gofmt aligns the var block, so compare on collapsed spacing.
	flat := collapse(got)
	for _, want := range []string{
		`Users = pg.NewTable("users")`,
		`UserID = pg.Add(Users, pg.BigSerial("id").PrimaryKey())`,
		`UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())`,
		`UserAge = pg.Add(Users, pg.Integer("age"))`,
		`pg.Timestamp("createdAt", true).NotNull().Default("now()")`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("generated file missing:\n  %s\ngot:\n%s", want, got)
		}
	}
	// A skipped or untagged field declares no column.
	if strings.Contains(got, "Ignored") || strings.Contains(got, "Untagged") {
		t.Errorf("untagged / skipped fields must not become columns:\n%s", got)
	}
}

// The header has to tell a reader where to make the change, or the
// generator creates the very drift it removes.
// collapse squeezes runs of spaces and tabs so assertions do not
// depend on gofmt's column alignment.
func collapse(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " \n ")), " ")
}

func TestSchemaHeaderPointsAtTheStruct(t *testing.T) {
	got := generateSchema(t, basicSrc)
	if !strings.Contains(got, "DO NOT EDIT") || !strings.Contains(got, "Edit the struct") {
		t.Errorf("header should say where to edit:\n%s", got)
	}
}

// Regenerating an unchanged struct must produce a byte-identical
// file, or the generator shows up in every diff and stops being run.
func TestSchemaIsDeterministic(t *testing.T) {
	first := generateSchema(t, basicSrc)
	second := generateSchema(t, basicSrc)
	if first != second {
		t.Errorf("two runs over the same source differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

func TestSchemaTypeMapping(t *testing.T) {
	src := `package models

import (
	"encoding/json"
	"time"
)

//drops:schema table=Kinds name=kinds
type Kind struct {
	A int16           ` + "`drop:\"a\"`" + `
	B int32           ` + "`drop:\"b\"`" + `
	C int64           ` + "`drop:\"c,primaryKey\"`" + `
	D float32         ` + "`drop:\"d\"`" + `
	E float64         ` + "`drop:\"e\"`" + `
	F bool            ` + "`drop:\"f\"`" + `
	G []byte          ` + "`drop:\"g\"`" + `
	H time.Time       ` + "`drop:\"h\"`" + `
	I json.RawMessage ` + "`drop:\"i\"`" + `
	J int16           ` + "`drop:\"j,autoIncrement\"`" + `
	K int32           ` + "`drop:\"k,autoIncrement\"`" + `
}
`
	got := generateSchema(t, src)
	want := map[string]string{
		"a": "pg.SmallInt", "b": "pg.Integer", "c": "pg.BigInt",
		"d": "pg.Real", "e": "pg.DoublePrecision", "f": "pg.Boolean",
		"g": "pg.Bytea", "h": "pg.Timestamp", "i": "pg.JSONB",
		"j": "pg.SmallSerial", "k": "pg.Serial",
	}
	for col, ctor := range want {
		if !strings.Contains(got, ctor+`("`+col+`"`) {
			t.Errorf("column %q should use %s:\n%s", col, ctor, got)
		}
	}
}

// A type the mapping does not cover needs an explicit escape hatch,
// not a guess.
func TestSchemaUnknownTypeNeedsExplicitType(t *testing.T) {
	src := `package models

//drops:schema table=T name=t
type Row struct {
	ID  int64 ` + "`drop:\"id,primaryKey\"`" + `
	Loc Point ` + "`drop:\"loc\"`" + `
}

type Point struct{}
`
	dir := t.TempDir()
	in := filepath.Join(dir, "models.go")
	_ = os.WriteFile(in, []byte(src), 0o600)
	err := runSchema(in, "")
	if err == nil {
		t.Fatal("expected an error for an unmappable Go type")
	}
	if !strings.Contains(err.Error(), "type=") {
		t.Errorf("error should point at the escape hatch: %v", err)
	}
}

func TestSchemaExplicitTypeOverride(t *testing.T) {
	src := `package models

//drops:schema table=T name=t
type Row struct {
	ID  string ` + "`drop:\"id,primaryKey,type=uuid\"`" + `
}
`
	got := generateSchema(t, src)
	if !strings.Contains(got, `pg.Custom[string]("id", "uuid").PrimaryKey()`) {
		t.Errorf("explicit type= should win:\n%s", got)
	}
}

// A pointer field is nullable by nature, so notNull on one is a
// contradiction the generator should refuse rather than resolve.
func TestSchemaRejectsNotNullPointer(t *testing.T) {
	src := `package models

//drops:schema table=T name=t
type Row struct {
	ID  int64  ` + "`drop:\"id,primaryKey\"`" + `
	Age *int32 ` + "`drop:\"age,notNull\"`" + `
}
`
	dir := t.TempDir()
	in := filepath.Join(dir, "models.go")
	_ = os.WriteFile(in, []byte(src), 0o600)
	if err := runSchema(in, ""); err == nil {
		t.Fatal("expected an error for a pointer field tagged notNull")
	}
}

func TestSchemaQualifiedTable(t *testing.T) {
	src := `package models

//drops:schema table=Users name=users schema=auth
type User struct {
	ID int64 ` + "`drop:\"id,primaryKey\"`" + `
}
`
	got := generateSchema(t, src)
	if !strings.Contains(got, `pg.NewSchemaTable("auth", "users")`) {
		t.Errorf("schema= should qualify the table:\n%s", got)
	}
}

func TestSchemaRequiresDirectiveKeys(t *testing.T) {
	cases := map[string]string{
		"missing table=": "//drops:schema name=users",
		"missing name=":  "//drops:schema table=Users",
	}
	for label, directive := range cases {
		src := "package models\n\n" + directive + "\ntype User struct {\n\tID int64 `drop:\"id,primaryKey\"`\n}\n"
		dir := t.TempDir()
		in := filepath.Join(dir, "models.go")
		_ = os.WriteFile(in, []byte(src), 0o600)
		if err := runSchema(in, ""); err == nil {
			t.Errorf("%s: expected an error", label)
		}
	}
}

func TestSchemaNoDirectives(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "models.go")
	_ = os.WriteFile(in, []byte("package models\n\ntype User struct{}\n"), 0o600)
	if err := runSchema(in, ""); err == nil {
		t.Error("expected an error when nothing is marked for generation")
	}
}
