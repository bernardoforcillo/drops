package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// relPostFixture is the relation target used by per-parent
// limit/offset tests. Distinct from relPost to keep tests
// independent of other relation-test fixtures.
type relPostFixture struct {
	ID     int64  `drop:"id"`
	UserID int64  `drop:"userId"`
	Title  string `drop:"title"`
}

type relUserFixture struct {
	ID    int64            `drop:"id"`
	Name  string           `drop:"name"`
	Posts []relPostFixture `dropRel:"posts"`
}

func mkRelLimitedSchema() (*pg.Table, *pg.Table) {
	users := pg.NewTable("users")
	uID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pUID := pg.Add(posts, pg.BigInt("userId").NotNull())
	pg.Add(posts, pg.Text("title").NotNull())

	pg.NewRelations(users).HasMany("posts", posts, uID, pUID)
	return users, posts
}

func TestPerParentLimitEmitsWindowFunction(t *testing.T) {
	users, _ := mkRelLimitedSchema()

	var sawWindow bool
	var capturedSQL string
	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{
					{int64(1), "Alice"},
					{int64(2), "Bob"},
				},
			}, nil
		case strings.Contains(sql, "ROW_NUMBER"):
			sawWindow = true
			capturedSQL = sql
			return &fakeRows{
				cols: []string{"id", "userId", "title", "_rn"},
				data: [][]any{
					{int64(10), int64(1), "A1", int64(1)},
					{int64(11), int64(1), "A2", int64(2)},
					{int64(12), int64(2), "B1", int64(1)},
					{int64(13), int64(2), "B2", int64(2)},
				},
			}, nil
		}
		return &fakeRows{}, nil
	}}
	db := pg.New(fd)

	var got []relUserFixture
	err := db.Find(users).
		WithRel("posts", func(c *pg.RelConfig) {
			c.Limit(2)
		}).
		All(context.Background(), &got)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if !sawWindow {
		t.Fatalf("expected ROW_NUMBER window in child query, queries: %v", fd.queries)
	}
	for _, frag := range []string{
		"ROW_NUMBER() OVER",
		"PARTITION BY",
		`"posts"."userId"`,
		"_rn >",
		"_rn <=",
	} {
		if !strings.Contains(capturedSQL, frag) {
			t.Errorf("expected fragment %q in window query:\n%s", frag, capturedSQL)
		}
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 users, got %d", len(got))
	}
	if len(got[0].Posts) != 2 || len(got[1].Posts) != 2 {
		t.Errorf("each parent should receive 2 posts after window cap; got %+v", got)
	}
}

func TestPerParentLimitWithOffset(t *testing.T) {
	users, _ := mkRelLimitedSchema()
	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		if strings.Contains(sql, `FROM "users"`) {
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}},
			}, nil
		}
		return &fakeRows{
			cols: []string{"id", "userId", "title", "_rn"},
			data: [][]any{
				{int64(11), int64(1), "second", int64(2)},
				{int64(12), int64(1), "third", int64(3)},
			},
		}, nil
	}}
	db := pg.New(fd)
	var got []relUserFixture
	if err := db.Find(users).
		WithRel("posts", func(c *pg.RelConfig) {
			c.Limit(2).Offset(1)
		}).
		All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	// Two queries expected: parent + window.
	if len(fd.queries) != 2 {
		t.Errorf("expected 2 queries (parent + windowed child), got %d", len(fd.queries))
	}
	// The window query carries (offset, offset+limit) as args.
	childArgs := fd.args[1]
	if len(childArgs) < 3 {
		t.Fatalf("window query args: %v", childArgs)
	}
	gotOffset := childArgs[len(childArgs)-2]
	gotMax := childArgs[len(childArgs)-1]
	if gotOffset != int(1) {
		t.Errorf("offset arg: %v", gotOffset)
	}
	if gotMax != int(3) {
		t.Errorf("offset+limit arg: %v", gotMax)
	}
}

func TestPerParentLimitWithOrderBy(t *testing.T) {
	users, posts := mkRelLimitedSchema()
	postID := posts.Col("id")
	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		if strings.Contains(sql, `FROM "users"`) {
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}},
			}, nil
		}
		return &fakeRows{
			cols: []string{"id", "userId", "title", "_rn"},
		}, nil
	}}
	db := pg.New(fd)
	var got []relUserFixture
	if err := db.Find(users).
		WithRel("posts", func(c *pg.RelConfig) {
			c.OrderBy(postID.Desc()).Limit(3)
		}).
		All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	if !strings.Contains(fd.queries[1], "ORDER BY") || !strings.Contains(fd.queries[1], "DESC") {
		t.Errorf("OrderBy should land inside ROW_NUMBER OVER (... ORDER BY ...): %s", fd.queries[1])
	}
}

func TestPerParentLimitZeroFallsBackToOriginalPath(t *testing.T) {
	users, _ := mkRelLimitedSchema()
	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		if strings.Contains(sql, `FROM "users"`) {
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "X"}},
			}, nil
		}
		return &fakeRows{
			cols: []string{"id", "userId", "title"},
			data: [][]any{{int64(10), int64(1), "post"}},
		}, nil
	}}
	db := pg.New(fd)
	var got []relUserFixture
	if err := db.Find(users).
		WithRel("posts", func(c *pg.RelConfig) {
			// no Limit / no Offset
		}).
		All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	for _, q := range fd.queries {
		if strings.Contains(q, "ROW_NUMBER") {
			t.Errorf("no limit → no window rewrite, but saw: %s", q)
		}
	}
}
