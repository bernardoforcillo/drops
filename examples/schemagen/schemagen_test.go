package schemagen_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/examples/schemagen"
	"github.com/bernardoforcillo/drops/pg"
)

// The whole point: because every column came from a field, the entity
// builds without a single AllowUnmappedColumns exemption. A generator
// that produced a schema the struct could not satisfy would fail
// right here.
func TestGeneratedSchemaSatisfiesTheDriftCheck(t *testing.T) {
	ent := pg.NewEntity[schemagen.User](schemagen.Users)
	if ent.PK() == nil || ent.PK().Name() != "id" {
		t.Fatalf("PK = %v, want id", ent.PK())
	}
}

// And the handles are typed, which is what a runtime AutoTable cannot
// give you: this line would not compile if Age were declared as text.
func TestGeneratedColumnsAreTyped(t *testing.T) {
	pred := schemagen.UserAge.Gte(18)
	sql, args := drops.String(pred)
	if !strings.Contains(sql, `"age"`) || len(args) != 1 {
		t.Errorf("predicate = %s %v", sql, args)
	}
}

// The checked-in generated file must match what the generator
// produces today, or the example documents a stale workflow.
func TestGeneratedFileIsCurrent(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	src, err := os.ReadFile("models.go")
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(dir, "models.go")
	if err := os.WriteFile(in, src, 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.Background(),
		"go", "run", "github.com/bernardoforcillo/drops/cmd/dropsgen", "-schema", in)
	cmd.Dir = ".."
	// Not a skip. The toolchain's absence is the only reason this
	// test cannot run, and that was settled above; a generator that
	// refuses to run is the regression, and a skip would report it
	// as a passing suite.
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dropsgen -schema failed: %v\n%s", err, out)
	}

	fresh, err := os.ReadFile(filepath.Join(dir, "models_drops_schema.go"))
	if err != nil {
		t.Fatal(err)
	}
	checkedIn, err := os.ReadFile("models_drops_schema.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) != string(checkedIn) {
		t.Errorf("models_drops_schema.go is stale — rerun go generate\n--- fresh ---\n%s\n--- checked in ---\n%s", fresh, checkedIn)
	}
}

// The generated row struct is one pg.NewEntity accepts with no
// exemptions at all. Every column has a field because every field
// came from a column, and "age" is a *int32 because the column admits
// NULL — the pairing NewEntity refuses is the one this generator
// cannot emit.
func TestGeneratedRowSatisfiesTheDriftCheck(t *testing.T) {
	ent := pg.NewEntity[schemagen.UsersRow](schemagen.Users)
	if ent.PK() == nil || ent.PK().Name() != "id" {
		t.Fatalf("PK = %v, want id", ent.PK())
	}
}

// Round trip: struct -> table -> struct. UsersRow was derived from
// the table that was derived from User, so the two must agree field
// for field. A disagreement is drift with two generators between it
// and the reader, which is the worst place for one to hide.
func TestRowStructRoundTripsTheHandWrittenOne(t *testing.T) {
	row := reflect.TypeOf(schemagen.UsersRow{})
	hand := reflect.TypeOf(schemagen.User{})
	if row.NumField() != hand.NumField() {
		t.Fatalf("UsersRow has %d fields, User has %d", row.NumField(), hand.NumField())
	}
	for i := 0; i < hand.NumField(); i++ {
		h, r := hand.Field(i), row.Field(i)
		if h.Name != r.Name || h.Type != r.Type {
			t.Errorf("field %d: generated %s %s, hand-written %s %s", i, r.Name, r.Type, h.Name, h.Type)
		}
		// The hand-written tag carries the schema too — primaryKey,
		// notNull, default. The generated one carries only the
		// column name, because the column already exists and saying
		// it twice is the drift this file exists to remove.
		if name, _, _ := strings.Cut(h.Tag.Get("drop"), ","); name != r.Tag.Get("drop") {
			t.Errorf("field %s binds to %q, generated binds to %q", h.Name, name, r.Tag.Get("drop"))
		}
	}
}

// The insert struct is the row minus what the database fills: the
// bigserial key and the column with a DEFAULT. What is left is
// exactly what a caller has to decide, and a nil Age is a NULL
// written to a column that admits one — the column is in the
// statement, which is why a column the database fills is not in the
// struct at all.
func TestGeneratedInsertOmitsWhatTheDatabaseFills(t *testing.T) {
	ins := reflect.TypeOf(schemagen.UsersInsert{})
	var got []string
	for i := 0; i < ins.NumField(); i++ {
		got = append(got, ins.Field(i).Tag.Get("drop"))
	}
	if want := []string{"email", "name", "age"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("UsersInsert binds %v, want %v", got, want)
	}

	// And it is usable: the typed column handles take the fields
	// straight, with ValPtr for the optional one.
	row := schemagen.UsersInsert{Email: "a@b.c", Name: "A"}
	// New(nil) renders SQL without a connection: nothing here runs a
	// statement, it only checks what one would say.
	sql, args := pg.New(nil).Insert(schemagen.Users).Row(
		schemagen.UserEmail.Val(row.Email),
		schemagen.UserName.Val(row.Name),
		schemagen.UserAge.ValPtr(row.Age),
	).ToSQL()
	if !strings.Contains(sql, `"email"`) || !strings.Contains(sql, `"age"`) {
		t.Fatalf("insert = %s", sql)
	}
	if len(args) != 3 || args[2] != nil {
		t.Fatalf("args = %v, want a nil for the unset age", args)
	}
}

// The checked-in generated file must match what the generator
// produces today, or the example documents a stale workflow.
//
// The package is copied rather than generated in place, and the copy
// has to live inside this module: the generator evaluates the table
// declaration by compiling a program that imports the package, and an
// import only resolves for a package the module contains. testdata is
// the one place that is true of and `go build ./...` still ignores,
// so a copy a failing run leaves behind breaks nothing.
func TestGeneratedRowsFileIsCurrent(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	if err := os.MkdirAll("testdata", 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("testdata", "schemagenrows")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// Absolute, because a bare relative path is an import path to
	// `go list` and only a rooted or ./-prefixed one is a directory.
	if dir, err = filepath.Abs(dir); err != nil {
		t.Fatal(err)
	}

	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		body, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, src), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.CommandContext(context.Background(),
		"go", "run", "github.com/bernardoforcillo/drops/cmd/dropsgen", "-rows", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dropsgen -rows failed: %v\n%s", err, out)
	}

	fresh, err := os.ReadFile(filepath.Join(dir, "schemagen_drops_rows.go"))
	if err != nil {
		t.Fatal(err)
	}
	checkedIn, err := os.ReadFile("schemagen_drops_rows.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) != string(checkedIn) {
		t.Errorf("schemagen_drops_rows.go is stale — rerun go generate\n--- fresh ---\n%s\n--- checked in ---\n%s", fresh, checkedIn)
	}
}
