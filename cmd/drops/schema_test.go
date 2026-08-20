package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The convention is the whole interface between a user's Go and this
// binary, so the message that reports a package missing it has to say
// what to write. "no Schema function" alone leaves the reader to guess
// the signature.
func TestCheckSchemaFuncExplainsTheConvention(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "schema.go"), "package schema\n\nvar Users = 1\n")

	err := checkSchemaFunc(dir, "example.com/db/schema")
	if err == nil {
		t.Fatal("a package with no Schema function was accepted")
	}
	for _, want := range []string{"example.com/db/schema", "func Schema() *pg.Schema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q:\n%v", want, err)
		}
	}
}

func TestCheckSchemaFuncAcceptsTheConvention(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "schema.go"),
		"package schema\n\nimport \"github.com/bernardoforcillo/drops/pg\"\n\nfunc Schema() *pg.Schema { return nil }\n")
	if err := checkSchemaFunc(dir, "example.com/db/schema"); err != nil {
		t.Fatalf("a package following the convention was rejected: %v", err)
	}
}

// A Schema that takes arguments cannot be the one the generated
// program calls, and saying so beats a compiler error inside a file
// the user never wrote.
func TestCheckSchemaFuncRejectsArguments(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "schema.go"),
		"package schema\n\nfunc Schema(env string) *int { return nil }\n")
	err := checkSchemaFunc(dir, "example.com/db/schema")
	if err == nil || !strings.Contains(err.Error(), "takes arguments") {
		t.Fatalf("error = %v", err)
	}
}

// A test file is not part of the package the generated program
// imports, so a Schema declared in one does not satisfy the
// convention.
func TestCheckSchemaFuncIgnoresTestFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "schema.go"), "package schema\n")
	write(t, filepath.Join(dir, "schema_test.go"), "package schema\n\nfunc Schema() *int { return nil }\n")
	if err := checkSchemaFunc(dir, "example.com/db/schema"); err == nil {
		t.Fatal("a Schema function in a _test.go file was accepted")
	}
}

// The generated program is the only part of drops a user never sees
// and cannot debug, so it has to be valid Go before it is written.
func TestRenderBridgeIsValidGo(t *testing.T) {
	src := renderBridge("example.com/db/schema")
	if strings.Contains(src, "$IMPORT$") {
		t.Fatal("the import path placeholder survived rendering")
	}
	if !strings.Contains(src, `userschema "example.com/db/schema"`) {
		t.Errorf("the generated program does not import the schema package:\n%s", src)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	write(t, path, src)
	// Parsing is the check that matters here: compiling would need the
	// schema package to exist, which the integration suite arranges.
	if err := parseGo(path); err != nil {
		t.Fatalf("the generated program does not parse: %v\n%s", err, src)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// parseGo reports whether a file is syntactically valid Go.
func parseGo(path string) error {
	fset := token.NewFileSet()
	_, err := parser.ParseFile(fset, path, nil, parser.AllErrors)
	return err
}
