package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleSQL = `-- queries/users.sql
-- name: GetUserByEmail(email string) :one User
SELECT id, name, email FROM users WHERE email = $1;

-- name: ListByOrg(orgID int64, limit int64) :many User
SELECT id, name, email FROM users WHERE orgId = $1 LIMIT $2;

-- name: DeactivateUser(id int64) :exec
UPDATE users SET active = false WHERE id = $1;
`

func TestParseSQLFileExtractsAllQueries(t *testing.T) {
	queries, err := parseSQLFile(sampleSQL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(queries) != 3 {
		t.Fatalf("expected 3 queries, got %d", len(queries))
	}
	wantNames := map[string]string{
		"GetUserByEmail": "one",
		"ListByOrg":      "many",
		"DeactivateUser": "exec",
	}
	for _, q := range queries {
		if wantKind, ok := wantNames[q.Name]; !ok || wantKind != q.Kind {
			t.Errorf("unexpected query %s :%s", q.Name, q.Kind)
		}
	}
}

func TestParseSQLArgsTyped(t *testing.T) {
	queries, err := parseSQLFile(sampleSQL)
	if err != nil {
		t.Fatal(err)
	}
	list := queries[0]
	for _, q := range queries {
		if q.Name == "ListByOrg" {
			list = q
		}
	}
	if list.Name != "ListByOrg" || len(list.Args) != 2 {
		t.Fatalf("ListByOrg args: %+v", list.Args)
	}
	if list.Args[0].Name != "orgID" || list.Args[0].Type != "int64" {
		t.Errorf("arg 0: %+v", list.Args[0])
	}
	if list.Args[1].Name != "limit" || list.Args[1].Type != "int64" {
		t.Errorf("arg 1: %+v", list.Args[1])
	}
}

func TestEmitSQLProducesCompilableGo(t *testing.T) {
	queries, err := parseSQLFile(sampleSQL)
	if err != nil {
		t.Fatal(err)
	}
	src, err := emitSQL("queries", queries)
	if err != nil {
		t.Fatalf("emit: %v\n%s", err, src)
	}
	got := string(src)
	for _, want := range []string{
		"package queries",
		`func GetUserByEmail(db *pg.DB, ctx context.Context, email string) (User, error)`,
		`func ListByOrg(db *pg.DB, ctx context.Context, orgID int64, limit int64) ([]User, error)`,
		`func DeactivateUser(db *pg.DB, ctx context.Context, id int64) (drops.Result, error)`,
		"pg.ScanOne",
		"pg.ScanAll",
		"db.Exec(ctx",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRunSQLEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	in := filepath.Join(tmp, "queries")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "users.sql"), []byte(sampleSQL), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "outpkg")
	if err := runSQL(in, out, "outpkg"); err != nil {
		t.Fatalf("runSQL: %v", err)
	}
	src, err := os.ReadFile(filepath.Join(out, "queries_gen.go"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(src), "package outpkg") {
		t.Errorf("package missing or wrong: %s", src)
	}
}

func TestParseRejectsMissingResultType(t *testing.T) {
	bad := `-- name: Bad() :one
SELECT 1;
`
	if _, err := parseSQLFile(bad); err == nil {
		t.Error(":one without ResultType should error")
	}
}

func TestParseRejectsBadArgs(t *testing.T) {
	bad := `-- name: Bad(noType) :exec
SELECT 1;
`
	if _, err := parseSQLFile(bad); err == nil {
		t.Error("untyped argument should error")
	}
}

func TestRunSQLNoFilesErrors(t *testing.T) {
	tmp := t.TempDir()
	in := filepath.Join(tmp, "empty")
	_ = os.MkdirAll(in, 0o755)
	if err := runSQL(in, filepath.Join(tmp, "out"), "x"); err == nil {
		t.Error("empty queries dir should error")
	}
}

func TestParseSeveralFilesMergedDeterministically(t *testing.T) {
	tmp := t.TempDir()
	in := filepath.Join(tmp, "q")
	_ = os.MkdirAll(in, 0o755)
	_ = os.WriteFile(filepath.Join(in, "b.sql"), []byte(`-- name: B(x int64) :exec
SELECT $1;
`), 0o644)
	_ = os.WriteFile(filepath.Join(in, "a.sql"), []byte(`-- name: A(x int64) :exec
SELECT $1;
`), 0o644)
	out := filepath.Join(tmp, "out")
	if err := runSQL(in, out, "out"); err != nil {
		t.Fatalf("runSQL: %v", err)
	}
	src, _ := os.ReadFile(filepath.Join(out, "queries_gen.go"))
	got := string(src)
	posA := strings.Index(got, "func A(")
	posB := strings.Index(got, "func B(")
	if posA < 0 || posB < 0 || posA > posB {
		t.Errorf("functions should be emitted in alphabetical order; got A=%d B=%d", posA, posB)
	}
}

// The generated function already binds db and ctx as parameters and
// out, rows and err as locals. An argument reusing one of those names
// still parses, so gofmt passes it and dropsgen exits 0 on a file the
// compiler then rejects — "db redeclared in this block".
func TestParseRejectsArgumentsThatCollideWithGeneratedNames(t *testing.T) {
	for _, name := range []string{"db", "ctx", "out", "rows", "err"} {
		sql := "-- name: Get(" + name + " int64) :exec\nSELECT $1;\n"
		if _, err := parseSQLFile(sql); err == nil {
			t.Errorf("argument %q should be refused: the generated function already uses it", name)
		}
	}
}

func TestParseRejectsArgumentsThatAreNotIdentifiers(t *testing.T) {
	for _, name := range []string{"type", "func", "1st", "a-b"} {
		sql := "-- name: Get(" + name + " int64) :exec\nSELECT $1;\n"
		if _, err := parseSQLFile(sql); err == nil {
			t.Errorf("argument %q is not a Go identifier and should be refused", name)
		}
	}
}

func TestParseRejectsDuplicateArguments(t *testing.T) {
	sql := `-- name: Get(id int64, id int64) :exec
SELECT $1;
`
	if _, err := parseSQLFile(sql); err == nil {
		t.Error("a repeated argument name should be refused")
	}
}

// Two queries of the same name become two Go functions of the same
// name. Reporting it here names the query; leaving it produces a
// compile error in someone else's package.
func TestRunSQLRejectsDuplicateQueryNames(t *testing.T) {
	tmp := t.TempDir()
	in := filepath.Join(tmp, "q")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.sql", "b.sql"} {
		if err := os.WriteFile(filepath.Join(in, f), []byte("-- name: Get(id int64) :exec\nSELECT $1;\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := runSQL(in, filepath.Join(tmp, "out"), "out")
	if err == nil {
		t.Fatal("two queries named Get should be refused")
	}
	if !strings.Contains(err.Error(), "Get") {
		t.Errorf("the error should name the query: %v", err)
	}
}

// Same contract as the other modes: regenerating an unchanged
// directory must produce an identical file.
func TestSQLGenIsDeterministic(t *testing.T) {
	first := sqlGenOnce(t)
	for i := 0; i < 20; i++ {
		if got := sqlGenOnce(t); got != first {
			t.Fatalf("run %d differs from the first:\n--- first ---\n%s\n--- got ---\n%s", i, first, got)
		}
	}
}

func sqlGenOnce(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	in := filepath.Join(tmp, "queries")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(in, "users.sql"), []byte(sampleSQL), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(tmp, "outpkg")
	if err := runSQL(in, out, "outpkg"); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(out, "queries_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}
