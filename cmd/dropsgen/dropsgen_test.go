package main

import (
	"os"
	"path/filepath"
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
		_ = os.WriteFile(filepath.Join(t.TempDir(), "got.go"), got, 0644)
		t.Errorf("generated source does not match golden\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestParseRejectsMissingTable(t *testing.T) {
	tmp := t.TempDir()
	src := `package x

//drops:entity
type Bad struct {
	ID int64 ` + "`db:\"id\"`" + `
}
`
	path := filepath.Join(tmp, "bad.go")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
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
	ID       int64  ` + "`db:\"id\"`" + `
	Internal string
	Skipped  string ` + "`db:\"-\"`" + `
	Name     string ` + "`db:\"name\"`" + `
}
`
	path := filepath.Join(tmp, "foo.go")
	if err := os.WriteFile(path, []byte(src), 0644); err != nil {
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
	if err := os.WriteFile(in, []byte(src), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(in, ""); err == nil {
		t.Error("expected error when no entities found")
	}
}
