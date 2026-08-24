package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureSchema is a table declaration that exercises every decision
// the generator has to make: a serial key, a DEFAULT, two nullable
// columns of different shapes, a generated column, a column drops
// writes, a type the schema package itself declares, and two columns
// whose Go types need an import.
const fixtureSchema = `package fixture

import (
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// Kind is declared by this package. The generated file lives here
// too, so the field must be spelled without a qualifier.
type Kind string

var (
	Users         = pg.NewTable("users")
	UserID        = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail     = pg.Add(Users, pg.Text("email").NotNull().Unique())
	UserKind      = pg.Add(Users, pg.Custom[Kind]("kind", "text").NotNull())
	UserBio       = pg.Add(Users, pg.Text("bio").Nullable())
	UserMeta      = pg.Add(Users, pg.JSONB("meta").NotNull())
	UserAvatar    = pg.Add(Users, pg.Bytea("avatar").Nullable())
	UserCreatedAt = pg.Add(Users, pg.Timestamp("createdAt", true).NotNull().Default("now()"))
	UserFullName  = pg.Add(Users, pg.Custom[string]("fullName", "text GENERATED ALWAYS AS (email) STORED").NotNull())
	UserDeletedAt = pg.Add(Users, pg.Timestamp("deletedAt", true).Nullable().Managed())
)

var _ = time.Now

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`

// fixtureRoot is where a throwaway schema package goes. It has to be
// inside the module — the generator evaluates a table declaration by
// compiling a program that imports the package, and an import only
// resolves for a package the module contains, so t.TempDir is no use
// — and it has to be somewhere `go build ./...` will not look, or a
// fixture a panicking test left behind breaks the build for everyone
// until someone deletes it. testdata is both: the go command skips it
// when matching a pattern and still resolves an explicit import of a
// package inside it.
const fixtureRoot = "testdata"

// fixtureImportBase is fixtureRoot as an import path, for a fixture
// that has to name a package of its own.
const fixtureImportBase = "github.com/bernardoforcillo/drops/cmd/dropsgen/" + fixtureRoot

// rowsFixture writes a throwaway schema package inside the drops
// module and returns its directory.
func rowsFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(fixtureRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(fixtureRoot, "rowsfixture")
	if err != nil {
		t.Fatal(err)
	}
	// Absolute, because a bare relative path is an import path to
	// `go list` and only a rooted or ./-prefixed one is a directory.
	if dir, err = filepath.Abs(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		// A name may carry a directory, because some of what the
		// generator has to get right is about the *other* packages a
		// schema imports, and those need somewhere to live.
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		// $PKG$ is the fixture's own import path, which nothing can
		// know until the directory has a name.
		body = strings.ReplaceAll(body, "$PKG$", fixtureImportBase+"/"+filepath.Base(dir))
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// generateRows runs the real thing — go list, the bridge, the emit —
// and returns the generated source.
func generateRows(t *testing.T, dir string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	if err := runRows(dir, ""); err != nil {
		t.Fatalf("runRows: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "fixture_drops_rows.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// mustBuild compiles the fixture package. format.Source proves the
// generated file parses; only the compiler proves it builds, and the
// mistake that gets past a parser is an import nothing uses.
func mustBuild(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "go", "build", dir)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated package does not build: %v\n%s", err, out)
	}
}

// The row struct is the table, field for field: the Go type each
// column's handle carries, a pointer wherever the column admits NULL,
// and the tag that binds the two.
func TestRowsMirrorsTheTableDeclaration(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": fixtureSchema})
	got := generateRows(t, dir)
	mustBuild(t, dir)

	flat := collapse(got)
	for _, want := range []string{
		"type UsersRow struct {",
		`ID int64 ` + "`" + `drop:"id"` + "`",
		`Email string ` + "`" + `drop:"email"` + "`",
		// A type the schema package declares is written bare: the
		// generated file is in that package.
		`Kind Kind ` + "`" + `drop:"kind"` + "`",
		// Nullable, so a pointer — the only field type pg.NewEntity
		// will let a column that admits NULL bind to.
		`Bio *string ` + "`" + `drop:"bio"` + "`",
		`Meta json.RawMessage ` + "`" + `drop:"meta"` + "`",
		`Avatar *[]byte ` + "`" + `drop:"avatar"` + "`",
		`CreatedAt time.Time ` + "`" + `drop:"createdAt"` + "`",
		`DeletedAt *time.Time ` + "`" + `drop:"deletedAt"` + "`",
	} {
		if !strings.Contains(flat, collapse(want)) {
			t.Errorf("row struct missing:\n  %s\ngot:\n%s", want, got)
		}
	}
}

// The insert struct is the row minus what the caller must not supply.
// Which columns those are is the whole content of the type, so the
// doc comment says which went and why.
func TestRowsInsertOmitsWhatTheDatabaseFills(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": fixtureSchema})
	got := generateRows(t, dir)

	body := structBody(t, got, "UsersInsert")
	for _, gone := range []string{"ID ", "CreatedAt ", "FullName ", "DeletedAt "} {
		if strings.Contains(body, gone) {
			t.Errorf("UsersInsert still carries %s:\n%s", gone, body)
		}
	}
	for _, kept := range []string{"Email string", "Kind Kind", "Bio *string", "Avatar *[]byte"} {
		if !strings.Contains(collapse(body), kept) {
			t.Errorf("UsersInsert missing %s:\n%s", kept, body)
		}
	}
	for _, why := range []string{
		"id — bigserial",
		"createdAt — has a DEFAULT",
		"fullName — generated",
		"deletedAt — written by drops",
	} {
		if !strings.Contains(collapse(got), why) {
			t.Errorf("insert doc comment does not say %q:\n%s", why, got)
		}
	}
}

// Two runs over one declaration must produce the same bytes, or the
// checked-in file churns in every diff and nobody reruns the
// generator.
func TestRowsGenerationIsByteStable(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": fixtureSchema})
	first := generateRows(t, dir)
	// The second run reads a package that already contains the first
	// run's output, which is the case that would drift: a generator
	// that saw its own structs as names already taken would emit
	// nothing the second time.
	second := generateRows(t, dir)
	if first != second {
		t.Errorf("two runs differ\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// A hand-written struct wins. Overwriting one somebody maintains is
// not a generator's call, and emitting a second declaration of the
// name does not compile — so the only move left is to stand aside and
// say so where the reader will look.
func TestRowsSkipsANameThePackageAlreadyDeclares(t *testing.T) {
	hand := `package fixture

// UsersRow is hand-written and stays that way.
type UsersRow struct {
	ID int64 ` + "`" + `drop:"id"` + "`" + `
}
`
	dir := rowsFixture(t, map[string]string{"schema.go": fixtureSchema, "hand.go": hand})
	got := generateRows(t, dir)
	mustBuild(t, dir)

	if strings.Contains(got, "type UsersRow struct") {
		t.Errorf("generated a second UsersRow:\n%s", got)
	}
	if !strings.Contains(got, "type UsersInsert struct") {
		t.Errorf("the other struct should still be generated:\n%s", got)
	}
	if !strings.Contains(got, "UsersRow (hand.go)") {
		t.Errorf("header does not name the skipped struct and its file:\n%s", got)
	}
	// The skipped struct held the only time.Time fields — both of the
	// table's timestamps are ones the insert struct omits — and an
	// import kept for a struct that is not there is a compile error,
	// which is what mustBuild above is for.
	if strings.Contains(got, `"time"`) {
		t.Errorf("the time import outlived the struct that needed it:\n%s", got)
	}
}

// The header has to point at the declaration, or the generator
// creates the very drift it removes.
func TestRowsHeaderPointsAtTheDeclaration(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": fixtureSchema})
	got := generateRows(t, dir)
	if !strings.Contains(got, "DO NOT EDIT") || !strings.Contains(got, "Edit the table declaration") {
		t.Errorf("header does not say where to make the change:\n%s", got)
	}
}

// A package with no Schema function is the common mistake, and it is
// reported as itself rather than as a compiler error inside a file
// the user never wrote.
func TestRowsReportsAMissingSchemaFunction(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": `package fixture

var X = 1
`})
	err := runRows(dir, "")
	if err == nil || !strings.Contains(err.Error(), "declares no Schema function") {
		t.Fatalf("err = %v, want one naming the missing Schema function", err)
	}
}

// The generated file belongs in the schema package: a column typed
// with a type that package declares is written without a qualifier,
// which is only true there.
func TestRowsRefusesAnOutputPathOutsideThePackage(t *testing.T) {
	err := runRows("./...", "../elsewhere/rows.go")
	if err == nil || !strings.Contains(err.Error(), "file name") {
		t.Fatalf("err = %v, want a refusal naming the -o restriction", err)
	}
}

// structBody returns the field lines of one generated struct.
func structBody(t *testing.T, src, name string) string {
	t.Helper()
	head := "type " + name + " struct {"
	i := strings.Index(src, head)
	if i < 0 {
		t.Fatalf("no struct %s in:\n%s", name, src)
	}
	rest := src[i+len(head):]
	j := strings.Index(rest, "\n}")
	if j < 0 {
		t.Fatalf("struct %s is not closed:\n%s", name, src)
	}
	return rest[:j]
}

// The cases below drive emitRows directly. They are the ones a
// fixture cannot reach — a column with no typed handle is one
// AutoTable built, and two tables of the same name live in two
// schemas — and they are the ones where saying nothing would mean
// emitting a file that does not compile.

func TestRowsRefusesAColumnWithNoTypedHandle(t *testing.T) {
	tables := []rowsTable{{Name: "users", Columns: []rowsColumn{{Name: "id"}}}}
	_, err := emitRows(&rowsPackage{Name: "fixture"}, tables, nil)
	if err == nil || !strings.Contains(err.Error(), "no typed handle") {
		t.Fatalf("err = %v, want one naming the untyped column", err)
	}
}

func TestRowsRefusesTwoColumnsThatBecomeOneField(t *testing.T) {
	tables := []rowsTable{{Name: "users", Columns: []rowsColumn{
		{Name: "user_id", GoType: "int64"},
		{Name: "userID", GoType: "int64"},
	}}}
	_, err := emitRows(&rowsPackage{Name: "fixture"}, tables, nil)
	if err == nil || !strings.Contains(err.Error(), "both become the Go field") {
		t.Fatalf("err = %v, want one naming the colliding columns", err)
	}
}

func TestRowsRefusesTwoTablesThatBecomeOneStruct(t *testing.T) {
	col := []rowsColumn{{Name: "id", GoType: "int64"}}
	tables := []rowsTable{
		{Name: "users", Schema: "app", Columns: col},
		{Name: "users", Schema: "audit", Columns: col},
	}
	_, err := emitRows(&rowsPackage{Name: "fixture"}, tables, nil)
	if err == nil || !strings.Contains(err.Error(), "both generate UsersRow") {
		t.Fatalf("err = %v, want one naming the colliding tables", err)
	}
}

// omitReason is the whole policy of the insert struct, so it is
// stated once and tested here rather than inferred from output.
func TestRowsOmitReason(t *testing.T) {
	cases := []struct {
		col  rowsColumn
		want string
	}{
		// The sequence lives in the type, not in a DEFAULT clause,
		// so HasDefault never sees it.
		{rowsColumn{SQLType: "bigserial", PrimaryKey: true}, "bigserial"},
		{rowsColumn{SQLType: "serial"}, "serial"},
		{rowsColumn{SQLType: "text GENERATED ALWAYS AS (email) STORED"}, "generated"},
		{rowsColumn{SQLType: "timestamptz", HasDefault: true}, "has a DEFAULT"},
		{rowsColumn{SQLType: "timestamptz", Managed: true}, "written by drops"},
		// A plain key the caller does supply — a natural key, a uuid
		// the application mints — stays in the insert struct.
		{rowsColumn{SQLType: "text", PrimaryKey: true}, ""},
		{rowsColumn{SQLType: "text"}, ""},
	}
	for _, c := range cases {
		if got := omitReason(c.col); got != c.want {
			t.Errorf("omitReason(%+v) = %q, want %q", c.col, got, c.want)
		}
	}
}

// The pointer is the point: pg.NewEntity refuses a nullable column
// bound to a field that cannot receive NULL, and a field that already
// says it can must not be wrapped twice.
func TestRowsFieldTypeStatesNullability(t *testing.T) {
	cases := []struct {
		col  rowsColumn
		want string
	}{
		{rowsColumn{GoType: "string"}, "string"},
		{rowsColumn{GoType: "string", Nullable: true}, "*string"},
		{rowsColumn{GoType: "time.Time", Nullable: true}, "*time.Time"},
		// A []byte can receive a NULL, but the pointer is what states
		// the intent — and what a schema generator reads back as
		// "this column is optional", so the round trip closes.
		{rowsColumn{GoType: "[]byte", Nullable: true}, "*[]byte"},
		// Already optional: a second pointer would be a **string.
		{rowsColumn{GoType: "*string", Nullable: true}, "*string"},
		{rowsColumn{GoType: "sql.NullString", Nullable: true}, "sql.NullString"},
		{rowsColumn{GoType: "sql.Null[string]", Nullable: true}, "sql.Null[string]"},
	}
	for _, c := range cases {
		if got := rowFieldType(c.col); got != c.want {
			t.Errorf("rowFieldType(%+v) = %q, want %q", c.col, got, c.want)
		}
	}
}

// The previous output is moved aside before the bridge compiles the
// package, so a generated file that has gone stale — a dropped column
// leaving its import unused, which is a compile error in Go — cannot
// break the run that would have fixed it. When the bridge fails for
// any other reason the file has to come back.
func TestRowsRestoresTheOldFileWhenTheBridgeFails(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": fixtureSchema})
	want := generateRows(t, dir)

	broken := filepath.Join(dir, "broken.go")
	if err := os.WriteFile(broken, []byte("package fixture\n\nvar _ = noSuchThing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runRows(dir, ""); err == nil {
		t.Fatal("runRows succeeded against a package that does not compile")
	}
	got, err := os.ReadFile(filepath.Join(dir, "fixture_drops_rows.go"))
	if err != nil {
		t.Fatalf("the previous output was not put back: %v", err)
	}
	if string(got) != want {
		t.Errorf("the file that came back is not the one that went away:\n%s", got)
	}
}

// A Go package's name is not the last element of its import path, and
// the difference is not exotic: every module past v1 ends its path in
// a version element, so github.com/gofrs/uuid/v5 is package uuid. A
// generator that guessed the qualifier from the path writes v5.UUID
// and an import nothing uses, which is two compile errors in a file
// the user never wrote.
func TestRowsQualifiesAnImportByItsPackageNameNotItsPath(t *testing.T) {
	dir := rowsFixture(t, map[string]string{
		"sub/v2/lib.go": `// Package lib sits in a directory named for a module version,
// the way every Go module past v1 does.
package lib

type Kind string
`,
		"schema.go": `package fixture

import (
	lib "$PKG$/sub/v2"

	"github.com/bernardoforcillo/drops/pg"
)

var (
	Users    = pg.NewTable("users")
	UserKind = pg.Add(Users, pg.Custom[lib.Kind]("kind", "text").NotNull())
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`})
	got := generateRows(t, dir)
	mustBuild(t, dir)

	if !strings.Contains(collapse(got), "Kind lib.Kind") {
		t.Errorf("field is not qualified by the package's own name:\n%s", got)
	}
	// The import has to carry the name too, or the qualifier in the
	// field resolves to nothing.
	if !strings.Contains(got, `lib "`) {
		t.Errorf("import is not named, so lib. refers to nothing:\n%s", got)
	}
}

// An instantiated generic carries its type arguments inside the name
// reflect reports, and reflect reports them by package *name* — there
// is no path to import. sql.Null[string] needs nothing beyond
// database/sql and is fine; sql.Null[time.Time] needs "time" and the
// generator has no way to learn that, so it has to say so rather than
// emit a file that does not compile.
func TestRowsRefusesAGenericWhoseTypeArgumentNeedsAnImport(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": `package fixture

import (
	"database/sql"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

var (
	Events  = pg.NewTable("events")
	EventAt = pg.Add(Events, pg.Custom[sql.Null[time.Time]]("at", "timestamptz").Nullable())
)

var _ = time.Now

func Schema() *pg.Schema { return pg.NewSchema(Events) }
`})
	err := runRows(dir, "")
	if err == nil {
		t.Fatalf("runRows accepted a type whose argument it cannot import; it emitted:\n%s", readGenerated(t, dir))
	}
	if !strings.Contains(err.Error(), "sql.Null[time.Time]") || !strings.Contains(err.Error(), "hand-written") {
		t.Fatalf("err = %v, want one naming the type and saying the struct has to be hand-written", err)
	}
}

// The builtin type argument is the case that does work, and it has to
// keep working: nothing inside the brackets needs importing.
func TestRowsAcceptsAGenericOverABuiltin(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": `package fixture

import (
	"database/sql"

	"github.com/bernardoforcillo/drops/pg"
)

var (
	Events    = pg.NewTable("events")
	EventNote = pg.Add(Events, pg.Custom[sql.Null[string]]("note", "text").Nullable())
)

func Schema() *pg.Schema { return pg.NewSchema(Events) }
`})
	got := generateRows(t, dir)
	mustBuild(t, dir)
	if !strings.Contains(collapse(got), "Note sql.Null[string]") {
		t.Errorf("field is not the wrapper the column carries:\n%s", got)
	}
}

// Two packages of one name cannot both be imported unqualified, and
// the generator has no basis for choosing an alias the reader would
// recognise. Refusing names both paths; emitting would produce a
// redeclaration the user has to decipher.
func TestRowsRefusesTwoImportsThatShareAQualifier(t *testing.T) {
	pkg := func(dir string) string {
		return "// Package shared is one of two.\npackage shared\n\ntype " + dir + " string\n"
	}
	dir := rowsFixture(t, map[string]string{
		"a/shared/lib.go": pkg("A"),
		"b/shared/lib.go": pkg("B"),
		"schema.go": `package fixture

import (
	a "$PKG$/a/shared"
	b "$PKG$/b/shared"

	"github.com/bernardoforcillo/drops/pg"
)

var (
	Users  = pg.NewTable("users")
	UserA  = pg.Add(Users, pg.Custom[a.A]("a", "text").NotNull())
	UserB  = pg.Add(Users, pg.Custom[b.B]("b", "text").NotNull())
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`})
	err := runRows(dir, "")
	if err == nil {
		t.Fatalf("runRows accepted two packages named shared; it emitted:\n%s", readGenerated(t, dir))
	}
	if !strings.Contains(err.Error(), "shared") {
		t.Fatalf("err = %v, want one naming the two colliding packages", err)
	}
}

// readGenerated reports what a run that should have failed wrote, so
// the failure message shows the file rather than describing it.
func readGenerated(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "fixture_drops_rows.go"))
	if err != nil {
		return "(no file)"
	}
	return string(body)
}

// The insert struct's doc has to describe the statement a caller
// actually gets. A nil pointer bound with (*pg.Col[T]).ValPtr becomes
// SetNull, so the column is in the INSERT carrying a NULL — it is not
// left out of the column list. A doc comment promising omission sends
// a reader looking for a DEFAULT that never fires, which is the
// mistake the insert struct exists to prevent.
func TestRowsInsertDocSaysWhatANilPointerWrites(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": fixtureSchema})
	got := generateRows(t, dir)
	if strings.Contains(got, "leaves the column out") {
		t.Errorf("the doc promises an omission ValPtr does not perform:\n%s", got)
	}
	if !strings.Contains(got, "writes NULL") {
		t.Errorf("the doc does not say what a nil pointer writes:\n%s", got)
	}
}

// A PostgreSQL identifier is not a Go one. "2fa_secret" and
// "first name" are both legal columns and neither yields a Go field
// name, so they have to be reported as themselves — the parser error
// they used to produce points at a line number in a file the user
// never saw.
func TestRowsRefusesANameThatIsNotAGoIdentifier(t *testing.T) {
	for _, c := range []struct{ table, column string }{
		{"users", "2fa_secret"},
		{"users", "first name"},
		{"2fa", "id"},
	} {
		tables := []rowsTable{{Name: c.table, Columns: []rowsColumn{{Name: c.column, GoType: "string"}}}}
		_, err := emitRows(&rowsPackage{Name: "fixture"}, tables, nil)
		if err == nil {
			t.Errorf("table %q column %q was accepted", c.table, c.column)
			continue
		}
		if !strings.Contains(err.Error(), c.column) && !strings.Contains(err.Error(), c.table) {
			t.Errorf("table %q column %q: err = %v, want one naming it", c.table, c.column, err)
		}
	}
}
