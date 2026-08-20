package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

type rhUser struct {
	ID    int64
	Name  string
	Posts []rhPost `dropRel:"posts"`
}

type rhPost struct {
	ID     int64
	UserID int64 `drop:"userId"`
	Title  string
}

func rhSchema() (users, posts *pg.Table) {
	users = pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name"))

	posts = pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	puid := pg.Add(posts, pg.BigInt("userId"))
	pg.Add(posts, pg.Text("title"))

	pg.NewRelations(users).HasMany("posts", posts, uid, puid)
	return users, posts
}

func rhDriver() *fakeDriver {
	return &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name"}}, nil
	}}
}

// The handle is a Go identifier, so a rename refactors and a typo
// does not compile.
func TestRelHandleLoadsTheSameAsWith(t *testing.T) {
	users, _ := rhSchema()
	UserPosts := users.Rel("posts")

	byName := rhDriver()
	var a []rhUser
	if err := pg.New(byName).Find(users).With("posts").All(context.Background(), &a); err != nil {
		t.Fatalf("With: %v", err)
	}

	byHandle := rhDriver()
	var b []rhUser
	if err := pg.New(byHandle).Find(users).Load(UserPosts).All(context.Background(), &b); err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(byName.queries) != len(byHandle.queries) {
		t.Fatalf("query counts differ: %d vs %d", len(byName.queries), len(byHandle.queries))
	}
	for i := range byName.queries {
		if byName.queries[i] != byHandle.queries[i] {
			t.Errorf("query %d differs:\n With: %s\n Load: %s", i, byName.queries[i], byHandle.queries[i])
		}
	}
}

// A name that was never declared is a startup panic, not a failed
// query hours later.
func TestRelPanicsOnUnknownName(t *testing.T) {
	users, _ := rhSchema()
	msg := mustPanic(t, func() { users.Rel("psots") })
	if !strings.Contains(msg, "psots") {
		t.Errorf("message should name the bad relation: %s", msg)
	}
	// Listing what does exist turns the panic into a fix.
	if !strings.Contains(msg, "posts") {
		t.Errorf("message should list the declared relations: %s", msg)
	}
}

// An optional handle needs no nil guard at the call site.
func TestLoadIgnoresNilHandles(t *testing.T) {
	users, _ := rhSchema()
	fd := rhDriver()
	var out []rhUser
	err := pg.New(fd).Find(users).Load(nil).All(context.Background(), &out)
	if err != nil {
		t.Fatalf("Load(nil): %v", err)
	}
	if len(fd.queries) != 1 {
		t.Errorf("a nil handle should load nothing extra, got %d queries", len(fd.queries))
	}
}

func TestLoadRelAppliesConstraints(t *testing.T) {
	users, posts := rhSchema()
	UserPosts := users.Rel("posts")
	title := posts.Col("title")

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		if strings.Contains(sql, "posts") {
			return &fakeRows{cols: []string{"id", "userId", "title"}}, nil
		}
		return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "ada"}}}, nil
	}}
	var out []rhUser
	err := pg.New(fd).Find(users).
		LoadRel(UserPosts, func(p *pg.RelConfig) { p.Where(pg.Eq(title, "go")) }).
		All(context.Background(), &out)
	if err != nil {
		t.Fatalf("LoadRel: %v", err)
	}
	var sawFilter bool
	for _, q := range fd.queries {
		if strings.Contains(q, "posts") && strings.Contains(q, `"title"`) {
			sawFilter = true
		}
	}
	if !sawFilter {
		t.Errorf("the relation filter did not reach the child query: %v", fd.queries)
	}
}

func TestLoadRelIgnoresNilHandle(t *testing.T) {
	users, _ := rhSchema()
	fd := rhDriver()
	var out []rhUser
	if err := pg.New(fd).Find(users).LoadRel(nil, nil).All(context.Background(), &out); err != nil {
		t.Fatalf("LoadRel(nil): %v", err)
	}
}

// The entity query exposes the same pair.
func TestEntityQueryLoad(t *testing.T) {
	users, _ := rhSchema()
	UserPosts := users.Rel("posts")
	ent := pg.NewEntity[rhUser](users)

	fd := rhDriver()
	if _, err := ent.Query(pg.New(fd)).Load(UserPosts).All(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(fd.queries) == 0 {
		t.Fatal("no query issued")
	}
}
