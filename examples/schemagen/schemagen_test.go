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

// The generated eager-load shape is the one the loader fills. Each
// of these four is something a hand-written struct gets wrong
// silently: a slice where the loader fills a slice, a pointer where
// at most one row arrives, the tag the loader looks the relation up
// by, and the row struct of the table on the far side.
func TestGeneratedShapeMatchesWhatTheLoaderFills(t *testing.T) {
	many, ok := reflect.TypeOf(schemagen.UsersWithPosts{}).FieldByName("Posts")
	if !ok {
		t.Fatal("UsersWithPosts has no Posts field")
	}
	if want := reflect.TypeOf([]schemagen.PostsRow(nil)); many.Type != want {
		t.Errorf("Posts is %s, want %s — HasMany fills a slice and the loader refuses anything else", many.Type, want)
	}
	if got := many.Tag.Get("dropRel"); got != "posts" {
		t.Errorf(`Posts is tagged dropRel:%q, want "posts"`, got)
	}
	// Not decoration: the relation field sits at depth 0 and would
	// outrank a real column of the same name promoted from the
	// embedded row struct, so it is opted out of column binding.
	if got := many.Tag.Get("drop"); got != "-" {
		t.Errorf(`Posts is tagged drop:%q, want "-"`, got)
	}

	// A nested path nests a generated struct, and it is the same
	// struct the other shape generates rather than a second copy of
	// it: a shape is named for its table and the paths beneath it, so
	// two shapes that reach one node share one declaration.
	nested, ok := reflect.TypeOf(schemagen.PostsWithAuthorPosts{}).FieldByName("Author")
	if !ok {
		t.Fatal("PostsWithAuthorPosts has no Author field")
	}
	if want := reflect.TypeOf((*schemagen.UsersWithPosts)(nil)); nested.Type != want {
		t.Errorf("the nested author is %s, want %s", nested.Type, want)
	}

	one, ok := reflect.TypeOf(schemagen.PostsWithAuthor{}).FieldByName("Author")
	if !ok {
		t.Fatal("PostsWithAuthor has no Author field")
	}
	if want := reflect.TypeOf((*schemagen.UsersRow)(nil)); one.Type != want {
		t.Errorf("Author is %s, want %s — BelongsTo leaves the field untouched when nothing matches, and only a pointer can say so", one.Type, want)
	}
}

// The other four kinds, which is the rest of the decision. A
// generated struct exists for every kind the generator can emit, so
// the live suite can load all six from generated types rather than
// from a mirror somebody typed twice.
func TestGeneratedShapeCoversEveryRelationKind(t *testing.T) {
	for _, tc := range []struct {
		shape reflect.Type
		field string
		want  reflect.Type
		rel   string
		why   string
	}{
		{reflect.TypeOf(schemagen.UsersWithProfile{}), "Profile", reflect.TypeOf((*schemagen.ProfilesRow)(nil)), "profile",
			"HasOne loads at most one row, and a parent with no match keeps the nil"},
		{reflect.TypeOf(schemagen.PostsWithTags{}), "Tags", reflect.TypeOf([]schemagen.TagsRow(nil)), "tags",
			"ManyToMany fills a slice of the target's rows — never the junction's"},
		{reflect.TypeOf(schemagen.UsersWithNotes{}), "Notes", reflect.TypeOf([]schemagen.NotesRow(nil)), "notes",
			"MorphMany fills a slice"},
		{reflect.TypeOf(schemagen.NotesWithOwner{}), "Owner", reflect.TypeOf((*any)(nil)).Elem(), "owner",
			"MorphTo's concrete type varies row by row, so the loader requires interface kind"},
	} {
		f, ok := tc.shape.FieldByName(tc.field)
		if !ok {
			t.Errorf("%s has no %s field", tc.shape, tc.field)
			continue
		}
		if f.Type != tc.want {
			t.Errorf("%s.%s is %s, want %s — %s", tc.shape, tc.field, f.Type, tc.want, tc.why)
		}
		if got := f.Tag.Get("dropRel"); got != tc.rel {
			t.Errorf("%s.%s is tagged dropRel:%q, want %q", tc.shape, tc.field, got, tc.rel)
		}
		if got := f.Tag.Get("drop"); got != "-" {
			t.Errorf("%s.%s is tagged drop:%q, want \"-\"", tc.shape, tc.field, got)
		}
	}
}

// The shape carries the parent's columns by embedding, which is also
// what gives the loader the key column it joins on: it looks the
// join key up with the same field mapping the scanner uses, and that
// walks embedded structs.
func TestGeneratedShapeCarriesTheJoinKey(t *testing.T) {
	shape := reflect.TypeOf(schemagen.UsersWithPosts{})
	if _, ok := shape.FieldByName("UsersRow"); !ok {
		t.Fatal("UsersWithPosts does not embed UsersRow")
	}
	f, ok := shape.FieldByName("ID")
	if !ok || f.Tag.Get("drop") != "id" {
		t.Fatalf("the users primary key is not reachable through the shape: %+v", f)
	}
}

// The checked-in generated file must match what the generator
// produces today, or the example documents a stale workflow. The
// copy-into-testdata dance is the rows test's, and for the same
// reason: the generator evaluates the declarations by compiling a
// program that imports the package.
func TestGeneratedRelsFileIsCurrent(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := copyPackageToTestdata(t, "schemagenrels")

	args := append([]string{"run", "github.com/bernardoforcillo/drops/cmd/dropsgen", "-rels", dir}, shapeArgs(t)...)
	cmd := exec.CommandContext(context.Background(), "go", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("dropsgen -rels failed: %v\n%s", err, out)
	}

	fresh, err := os.ReadFile(filepath.Join(dir, "schemagen_drops_rels.go"))
	if err != nil {
		t.Fatal(err)
	}
	checkedIn, err := os.ReadFile("schemagen_drops_rels.go")
	if err != nil {
		t.Fatal(err)
	}
	if string(fresh) != string(checkedIn) {
		t.Errorf("schemagen_drops_rels.go is stale — rerun go generate\n--- fresh ---\n%s\n--- checked in ---\n%s", fresh, checkedIn)
	}
}

// And a second run over the same input produces the same bytes. A
// generator whose output churns turns every regeneration into a diff
// nobody can read, and the file is checked in, so the churn is
// permanent.
func TestGeneratedRelsFileIsByteStable(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := copyPackageToTestdata(t, "schemagenrelstwice")
	run := func() string {
		t.Helper()
		args := append([]string{"run", "github.com/bernardoforcillo/drops/cmd/dropsgen", "-rels", dir}, shapeArgs(t)...)
		cmd := exec.CommandContext(context.Background(), "go", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("dropsgen -rels failed: %v\n%s", err, out)
		}
		src, err := os.ReadFile(filepath.Join(dir, "schemagen_drops_rels.go"))
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	if first, second := run(), run(); first != second {
		t.Errorf("two runs disagree\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// copyPackageToTestdata copies this package's non-test sources into a
// fresh directory inside the module, and returns it. See
// TestGeneratedRowsFileIsCurrent for why testdata is the only place
// this works.
func copyPackageToTestdata(t *testing.T, prefix string) string {
	t.Helper()
	if err := os.MkdirAll("testdata", 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("testdata", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
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
	return dir
}

// shapeArgs is the -shape list relations.go's //go:generate directive
// carries, read out of the directive rather than repeated here.
//
// The file these tests check is the one `go generate` writes, so a
// list kept separately would drift the moment a shape is added — and
// it would drift silently, checking the generator against arguments
// nobody runs while the shape that was actually added goes untested.
func shapeArgs(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("relations.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(line, "//go:generate") || !strings.Contains(line, "-rels") {
			continue
		}
		fields := strings.Fields(line)
		var out []string
		for i, f := range fields {
			if f == "-shape" && i+1 < len(fields) {
				out = append(out, "-shape", fields[i+1])
			}
		}
		if len(out) == 0 {
			t.Fatalf("the -rels directive in relations.go names no shapes: %s", line)
		}
		return out
	}
	t.Fatal("relations.go has no //go:generate directive for -rels")
	return nil
}
