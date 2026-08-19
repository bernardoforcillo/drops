package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strconv"
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

// `type=` is the only option that puts a Go type in the output, as
// pg.Custom's type argument. A qualified one needs its package
// imported or the generated file does not compile — and format.Source
// only parses, so nothing upstream notices.
func TestSchemaExplicitTypeCarriesItsImport(t *testing.T) {
	src := `package models

import "time"

//drops:schema table=Events name=events
type Event struct {
	ID int64     ` + "`drop:\"id,primaryKey\"`" + `
	At time.Time ` + "`drop:\"at,type=timestamptz\"`" + `
}
`
	got := generateSchema(t, src)
	if !strings.Contains(got, `pg.Custom[time.Time]("at", "timestamptz")`) {
		t.Fatalf("explicit type= should win:\n%s", got)
	}
	if !strings.Contains(got, `"time"`) {
		t.Errorf("generated file references time.Time but imports no time:\n%s", got)
	}
	assertParsesAndResolves(t, got)
}

// An import whose package name is not its last path element still has
// to resolve, which means the generated file spells the alias out.
func TestSchemaExplicitTypeAliasesAnUnusualImport(t *testing.T) {
	src := `package models

import (
	uuid "github.com/example/go-uuid"
)

//drops:schema table=Rows name=rows
type Row struct {
	ID uuid.UUID ` + "`drop:\"id,primaryKey,type=uuid\"`" + `
}
`
	got := generateSchema(t, src)
	if !strings.Contains(got, `uuid "github.com/example/go-uuid"`) {
		t.Errorf("an aliased import must be reproduced with its alias:\n%s", got)
	}
}

// assertParsesAndResolves checks that every qualifier the generated
// file uses is one it imports — the compile error the golden compare
// cannot see.
func assertParsesAndResolves(t *testing.T, src string) {
	t.Helper()
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "gen.go", src, parser.ParseComments)
	if err != nil {
		t.Fatalf("generated file does not parse: %v", err)
	}
	imported := map[string]bool{}
	for _, spec := range af.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(p)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		imported[name] = true
	}
	ast.Inspect(af, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if !imported[id.Name] {
			t.Errorf("generated file uses %s.%s but imports no %q", id.Name, sel.Sel.Name, id.Name)
		}
		return true
	})
}

// The runtime path over the same tag — pg.AutoTable — refuses an
// option it does not know. The generator must too: a `notnull` that
// generates a nullable column is a schema bug with nothing downstream
// left to catch it.
func TestSchemaRejectsUnknownTagOption(t *testing.T) {
	for _, tag := range []string{
		`drop:"email,notnull"`,
		`drop:"email,uniqe"`,
		`drop:"email,defualt=x"`,
		`drop:"email,default"`,
		`drop:"email,type"`,
	} {
		src := "package models\n\n//drops:schema table=T name=t\ntype Row struct {\n\tID int64 `drop:\"id,primaryKey\"`\n\tEmail string `" + tag + "`\n}\n"
		dir := t.TempDir()
		in := filepath.Join(dir, "models.go")
		if err := os.WriteFile(in, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := runSchema(in, ""); err == nil {
			t.Errorf("%s: expected an error, got a silently weaker column", tag)
		}
	}
}

// Schema mode is blind to embedded structs for the same reason
// bind/scan mode is: the AST names the type and stops. Their columns
// would simply be missing from the generated table, which is exactly
// the drift this mode exists to remove.
func TestSchemaRejectsEmbeddedFields(t *testing.T) {
	src := `package models

//drops:schema table=Users name=users
type User struct {
	ID int64 ` + "`drop:\"id,primaryKey\"`" + `
	Timestamps
}

type Timestamps struct {
	CreatedAt string ` + "`drop:\"createdAt\"`" + `
}
`
	dir := t.TempDir()
	in := filepath.Join(dir, "models.go")
	if err := os.WriteFile(in, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runSchema(in, "")
	if err == nil {
		t.Fatal("an embedded field should be refused, not silently dropped from the schema")
	}
	if !strings.Contains(err.Error(), "Timestamps") {
		t.Errorf("the error should name the embedded type: %v", err)
	}
}
