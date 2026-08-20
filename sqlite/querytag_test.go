package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// The rendered form of taggedCtx's tags: key-sorted, percent-encoded,
// at the end of the statement.
const wantQueryTag = `/*action='show',controller='users'*/`

func taggedCtx() context.Context {
	return drops.WithQueryTags(context.Background(),
		drops.Tag{Key: "controller", Value: "users"},
		drops.Tag{Key: "action", Value: "show"},
	)
}

// Tagging has to cover every statement, not only the raw Exec/Query
// escape hatch — a builder-issued statement is the common case and the
// one whose origin is hardest to recognise in a slow-query log.
func TestQueryTagsReachTheDriver(t *testing.T) {
	fd := &fakeDriver{}
	db := sqlite.New(fd)
	ctx := taggedCtx()

	if _, err := db.Exec(ctx, "DELETE FROM widgets"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, "SELECT id FROM widgets"); err != nil {
		t.Fatal(err)
	}
	tbl := sqlite.NewTable("widgets")
	sqlite.Add(tbl, sqlite.BigSerial("id").PrimaryKey())
	name := sqlite.Add(tbl, sqlite.Text("name").NotNull())
	if _, err := db.Insert(tbl).Values(name.Val("Alice")).Exec(ctx); err != nil {
		t.Fatal(err)
	}

	if len(fd.queries) != 3 {
		t.Fatalf("driver saw %d statements, want 3: %q", len(fd.queries), fd.queries)
	}
	for _, q := range fd.queries {
		if !strings.HasSuffix(q, wantQueryTag) {
			t.Errorf("statement reached the driver untagged: %q", q)
		}
	}
}

func TestUntaggedContextLeavesTheStatementAlone(t *testing.T) {
	fd := &fakeDriver{}
	const sql = "SELECT id FROM widgets WHERE id = ?"
	if _, err := sqlite.New(fd).Query(context.Background(), sql, 1); err != nil {
		t.Fatal(err)
	}
	if fd.queries[0] != sql {
		t.Fatalf("statement was rewritten with no tags on ctx:\n got %q\nwant %q", fd.queries[0], sql)
	}
}

// The hook is where a query log comes from, so what it reports and
// what the server actually ran have to be the same string — otherwise
// grepping the log for a statement seen in the slow-query log fails.
func TestHookSeesTheStatementTheServerSaw(t *testing.T) {
	fd := &fakeDriver{}
	var logged []string
	db := sqlite.New(fd).WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.SQL != "" {
			logged = append(logged, e.SQL)
		}
	})
	if _, err := db.Exec(taggedCtx(), "DELETE FROM widgets"); err != nil {
		t.Fatal(err)
	}
	if len(logged) != 1 || logged[0] != fd.queries[0] {
		t.Fatalf("hook logged %q, driver ran %q", logged, fd.queries)
	}
}

// Tagging sits on the hot path of every statement. With no tags on
// ctx it must cost a context lookup and nothing else.
func BenchmarkExecUntagged(b *testing.B) {
	fd := &fakeDriver{}
	db := sqlite.New(fd)
	ctx := context.Background()
	const sql = "SELECT id, email FROM users WHERE id = ?"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		fd.queries = fd.queries[:0]
		if _, err := db.Exec(ctx, sql, 1); err != nil {
			b.Fatal(err)
		}
	}
}
