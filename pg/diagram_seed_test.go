package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestMermaidDiagramEmptySchema(t *testing.T) {
	got := pg.MermaidDiagram(pg.NewSchema())
	if !strings.HasPrefix(got, "erDiagram") {
		t.Errorf("expected erDiagram header, got: %q", got)
	}
}

func TestMermaidDiagramRendersTablesAndRelations(t *testing.T) {
	users := pg.NewTable("users")
	userID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	postUID := pg.Add(posts, pg.BigInt("userId").NotNull().References(userID))
	pg.Add(posts, pg.Text("title").NotNull())

	pg.NewRelations(users).HasMany("posts", posts, userID, postUID)
	pg.NewRelations(posts).BelongsTo("author", users, postUID, userID)

	d := pg.MermaidDiagram(pg.NewSchema(users, posts))
	for _, want := range []string{
		"erDiagram",
		"USERS {",
		"bigserial id PK",
		"POSTS {",
		"bigint userId FK",
		"USERS ||--o{ POSTS : posts",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("diagram missing %q in:\n%s", want, d)
		}
	}
	// BelongsTo is the inverse of HasMany; should not appear.
	if strings.Contains(d, "POSTS ||--o| USERS : author") {
		t.Errorf("BelongsTo edge should be omitted (inverse of HasMany)")
	}
}

func TestSeederBatchesEntities(t *testing.T) {
	_, ent := entUsersSchema()
	fd := &fakeDriver{}
	db := pg.New(fd)

	seeder := pg.NewSeeder(db).WithoutTransaction()
	pg.SeedAdd(seeder, ent,
		entUser{Name: "Alice", Email: "a@x"},
		entUser{Name: "Bob", Email: "b@x"},
	)
	if err := seeder.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(fd.queries) != 1 {
		t.Errorf("SeedAdd should batch into one INSERT, got %d", len(fd.queries))
	}
	if !strings.HasPrefix(fd.queries[0], "INSERT INTO") {
		t.Errorf("expected INSERT, got: %s", fd.queries[0])
	}
}

func TestSeederDoFunctionRuns(t *testing.T) {
	fd := &fakeDriver{}
	db := pg.New(fd)
	called := false
	seeder := pg.NewSeeder(db).WithoutTransaction()
	pg.SeedDo(seeder, func(db *pg.DB, ctx context.Context) error {
		called = true
		_, err := db.Exec(ctx, "INSERT INTO seeds_audit ...")
		return err
	})
	if err := seeder.Apply(context.Background()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !called {
		t.Error("SeedDo function must run")
	}
	if len(fd.queries) != 1 {
		t.Errorf("expected 1 query, got %d", len(fd.queries))
	}
}
