package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

func TestMermaidDiagram(t *testing.T) {
	users := sqlite.NewTable("users")
	uid := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	posts := sqlite.NewTable("posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	pUser := sqlite.Add(posts, sqlite.BigInt("userId").NotNull())
	sqlite.NewRelations(users).HasMany("posts", posts, uid, pUser)

	out := sqlite.MermaidDiagram(sqlite.NewSchema(users, posts))
	for _, want := range []string{
		"erDiagram",
		"USERS {",
		"INTEGER id PK",
		"POSTS {",
		"USERS ||--o{ POSTS : posts",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diagram missing %q:\n%s", want, out)
		}
	}
}

func TestPushDryRun(t *testing.T) {
	users := sqlite.NewTable("users")
	sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	schema := sqlite.NewSchema(users)

	drv := &entDriver{}
	db := sqlite.New(drv)
	// Introspect sees an empty DB (fake driver returns no rows), so the
	// diff is a CREATE TABLE. DryRun must not execute it.
	res, err := sqlite.Push(context.Background(), db, schema, sqlite.PushOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied || len(res.Statements) == 0 {
		t.Fatalf("dry run: applied=%v stmts=%d", res.Applied, len(res.Statements))
	}
	if !strings.Contains(strings.Join(res.Statements, "\n"), `CREATE TABLE "users"`) {
		t.Errorf("expected CREATE TABLE, got: %v", res.Statements)
	}
	// No DDL should have been executed (only introspection reads).
	for _, q := range drv.queries {
		if strings.HasPrefix(q, "CREATE TABLE") {
			t.Errorf("dry run executed DDL: %s", q)
		}
	}
}

func TestPushApplies(t *testing.T) {
	users := sqlite.NewTable("users")
	sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	drv := &entDriver{}
	db := sqlite.New(drv)
	res, err := sqlite.Push(context.Background(), db, sqlite.NewSchema(users))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatal("expected applied")
	}
	if !strings.Contains(strings.Join(drv.queries, "\n"), `CREATE TABLE "users"`) {
		t.Errorf("CREATE not executed:\n%v", drv.queries)
	}
}

func TestPushNilSchema(t *testing.T) {
	db := sqlite.New(&entDriver{})
	if _, err := sqlite.Push(context.Background(), db, nil); err != sqlite.ErrSchemaRequired {
		t.Errorf("want ErrSchemaRequired, got %v", err)
	}
}
