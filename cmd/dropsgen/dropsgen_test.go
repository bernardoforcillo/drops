package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratorMatchesGolden(t *testing.T) {
	entities, pkg, err := parseFile("testdata/input/users.go")
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if pkg != "models" {
		t.Errorf("package: got %q, want models", pkg)
	}
	if len(entities) != 2 {
		t.Fatalf("expected 2 entities (User, Post), got %d", len(entities))
	}
	if entities[0].StructName != "User" || entities[0].TableVar != "Users" {
		t.Errorf("User entity: %+v", entities[0])
	}
	if entities[1].StructName != "Post" || entities[1].TableVar != "Posts" {
		t.Errorf("Post entity: %+v", entities[1])
	}

	got, err := emit(pkg, entities)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	want, err := os.ReadFile("testdata/golden/users_drops_gen.go")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if string(got) != string(want) {
		// Drop the actual generated output next to the test for
		// easier diffing on failure.
		_ = os.WriteFile(filepath.Join(t.TempDir(), "got.go"), got, 0o644)
		t.Errorf("generated source does not match golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestParseRejectsMissingTable(t *testing.T) {
	tmp := t.TempDir()
	src := `package x

//drops:entity
type Bad struct {
	ID int64 ` + "`drop:\"id\"`" + `
}
`
	path := filepath.Join(tmp, "bad.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseFile(path); err == nil {
		t.Error("expected error for missing `table=` key")
	}
}

func TestParseSkipsUntaggedFields(t *testing.T) {
	tmp := t.TempDir()
	src := `package x

//drops:entity table=Foo
type Foo struct {
	ID       int64  ` + "`drop:\"id\"`" + `
	Internal string
	Skipped  string ` + "`drop:\"-\"`" + `
	Name     string ` + "`drop:\"name\"`" + `
}
`
	path := filepath.Join(tmp, "foo.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	entities, _, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(entities) != 1 || len(entities[0].Fields) != 2 {
		t.Fatalf("expected 1 entity with 2 fields, got %+v", entities)
	}
	if entities[0].Fields[0].Column != "id" || entities[0].Fields[1].Column != "name" {
		t.Errorf("unexpected fields: %+v", entities[0].Fields)
	}
}

func TestRunWritesFile(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "out.go")
	if err := run("testdata/input/users.go", out); err != nil {
		t.Fatalf("run: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("output not written: %v", err)
	}
	if info.Size() == 0 {
		t.Error("output is empty")
	}
}

func TestRunFailsOnNoEntities(t *testing.T) {
	tmp := t.TempDir()
	src := `package x

type Plain struct { ID int }
`
	in := filepath.Join(tmp, "plain.go")
	if err := os.WriteFile(in, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(in, ""); err == nil {
		t.Error("expected error when no entities found")
	}
}

// An embedded struct contributes columns to pg's reflection scanner
// but is invisible to a generator reading the AST: Cols / Bind / Scan
// come out one column short, and the first Entity.Get then fails with
// "expected 3 destination arguments in Scan, not 2" — a message that
// points at nothing.
func TestParseRejectsEmbeddedFields(t *testing.T) {
	tmp := t.TempDir()
	src := `package x

//drops:entity table=Users
type User struct {
	ID int64 ` + "`drop:\"id\"`" + `
	Timestamps
}

type Timestamps struct {
	CreatedAt string ` + "`drop:\"createdAt\"`" + `
}
`
	path := filepath.Join(tmp, "users.go")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := parseFile(path)
	if err == nil {
		t.Fatal("an embedded field should be refused, not silently dropped from the column list")
	}
	if !strings.Contains(err.Error(), "Timestamps") {
		t.Errorf("the error should name the embedded type: %v", err)
	}
}

// Two runs over the same input must be byte-identical, or the
// checked-in generated file churns in every diff and people stop
// rerunning the generator.
func TestBindScanIsDeterministic(t *testing.T) {
	first := generateOnce(t)
	for i := 0; i < 20; i++ {
		if got := generateOnce(t); got != first {
			t.Fatalf("run %d differs from the first:\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

func generateOnce(t *testing.T) string {
	t.Helper()
	entities, pkg, err := parseFile("testdata/input/users.go")
	if err != nil {
		t.Fatal(err)
	}
	src, err := emit(pkg, entities)
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}
