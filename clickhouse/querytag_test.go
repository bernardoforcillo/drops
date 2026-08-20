package clickhouse_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
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
// one whose origin is hardest to recognise in system.query_log.
func TestQueryTagsReachTheDriver(t *testing.T) {
	fd := &fakeDriver{}
	db := clickhouse.New(fd)
	ctx := taggedCtx()

	if _, err := db.Exec(ctx, "TRUNCATE TABLE widgets"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Query(ctx, "SELECT id FROM widgets"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Insert(events).Row(eventUser.Val(42)).Exec(ctx); err != nil {
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
	if _, err := clickhouse.New(fd).Query(context.Background(), sql, 1); err != nil {
		t.Fatal(err)
	}
	if fd.queries[0] != sql {
		t.Fatalf("statement was rewritten with no tags on ctx:\n got %q\nwant %q", fd.queries[0], sql)
	}
}

// The hook is where a query log comes from, so what it reports and
// what the server actually ran have to be the same string.
func TestHookSeesTheStatementTheServerSaw(t *testing.T) {
	fd := &fakeDriver{}
	var logged []string
	db := clickhouse.New(fd).WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.SQL != "" {
			logged = append(logged, e.SQL)
		}
	})
	if _, err := db.Exec(taggedCtx(), "TRUNCATE TABLE widgets"); err != nil {
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
	db := clickhouse.New(fd)
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
