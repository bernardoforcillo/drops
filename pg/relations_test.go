package pg_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// fakeRows is a canned implementation of drops.Rows.
type fakeRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		rv := reflect.ValueOf(d).Elem()
		val := reflect.ValueOf(row[i])
		if val.IsValid() {
			rv.Set(val)
		}
	}
	return nil
}

func (r *fakeRows) Columns() ([]string, error) { return r.cols, nil }
func (r *fakeRows) Close() error               { return nil }
func (r *fakeRows) Err() error                 { return nil }

// fakeDriver routes queries through user-supplied handlers.
type fakeDriver struct {
	queries []string
	args    [][]any
	handler func(sql string, args []any) (drops.Rows, error)
	exec    func(sql string, args []any) (drops.Result, error)
}

type fakeResult struct{ affected int64 }

func (r fakeResult) RowsAffected() (int64, error) { return r.affected, nil }

func (f *fakeDriver) record(sql string, args []any) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
}

func (f *fakeDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	f.record(sql, args)
	if f.exec != nil {
		return f.exec(sql, args)
	}
	return fakeResult{}, nil
}

func (f *fakeDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	f.record(sql, args)
	if f.handler == nil {
		return &fakeRows{}, nil
	}
	return f.handler(sql, args)
}

func (f *fakeDriver) Begin(_ context.Context) (drops.Tx, error) {
	return &fakeTx{f}, nil
}

type fakeTx struct{ *fakeDriver }

func (t *fakeTx) Commit(_ context.Context) error   { return nil }
func (t *fakeTx) Rollback(_ context.Context) error { return nil }

// --- Relations tests --------------------------------------------------

type relUser struct {
	ID    int64
	Name  string
	Posts []relPost `db_rel:"posts"`
}

type relPost struct {
	ID     int64
	UserID int64 `db:"user_id"`
	Title  string
}

type relPostWithAuthor struct {
	ID     int64
	UserID int64 `db:"user_id"`
	Title  string
	Author *relUser `db_rel:"author"`
}

func mkRelSchema() (*pg.Table, *pg.Table, *pg.Col[int64], *pg.Col[int64]) {
	usersT := pg.NewTable("users")
	userIDc := pg.Add(usersT, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersT, pg.Text("name").NotNull())

	postsT := pg.NewTable("posts")
	pg.Add(postsT, pg.BigSerial("id").PrimaryKey())
	postUIDc := pg.Add(postsT, pg.BigInt("user_id").NotNull())
	pg.Add(postsT, pg.Text("title").NotNull())

	pg.NewRelations(usersT).
		HasMany("posts", postsT, userIDc, postUIDc)
	pg.NewRelations(postsT).
		BelongsTo("author", usersT, postUIDc, userIDc)

	return usersT, postsT, userIDc, postUIDc
}

func TestFindHasMany(t *testing.T) {
	usersT, _, _, _ := mkRelSchema()

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
		case strings.Contains(sql, `FROM "posts"`):
			return &fakeRows{
				cols: []string{"id", "user_id", "title"},
				data: [][]any{
					{int64(10), int64(1), "Hello"},
					{int64(11), int64(1), "World"},
					{int64(12), int64(2), "Hi"},
				},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var users []relUser
	if err := db.Find(usersT).With("posts").All(context.Background(), &users); err != nil {
		t.Fatalf("Find: %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
	if got := len(users[0].Posts); got != 2 {
		t.Errorf("Alice posts: got %d, want 2", got)
	}
	if got := len(users[1].Posts); got != 1 {
		t.Errorf("Bob posts: got %d, want 1", got)
	}
	if users[0].Posts[0].Title != "Hello" {
		t.Errorf("first post title: got %q, want Hello", users[0].Posts[0].Title)
	}

	if len(fd.queries) != 2 {
		t.Errorf("expected exactly 2 queries (parent + child), got %d", len(fd.queries))
	}
}

func TestFindBelongsTo(t *testing.T) {
	_, postsT, _, _ := mkRelSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "posts"`):
			return &fakeRows{
				cols: []string{"id", "user_id", "title"},
				data: [][]any{
					{int64(10), int64(1), "Hello"},
					{int64(11), int64(2), "Hi"},
				},
			}, nil
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{
					{int64(1), "Alice"},
					{int64(2), "Bob"},
				},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var posts []relPostWithAuthor
	if err := db.Find(postsT).With("author").All(context.Background(), &posts); err != nil {
		t.Fatalf("Find: %v", err)
	}

	if len(posts) != 2 {
		t.Fatalf("got %d posts, want 2", len(posts))
	}
	if posts[0].Author == nil || posts[0].Author.Name != "Alice" {
		t.Errorf("post 0 author: %+v, want Alice", posts[0].Author)
	}
	if posts[1].Author == nil || posts[1].Author.Name != "Bob" {
		t.Errorf("post 1 author: %+v, want Bob", posts[1].Author)
	}
}

type m2mGroup struct {
	ID   int64
	Name string
}

type m2mUser struct {
	ID     int64
	Name   string
	Groups []m2mGroup `db_rel:"groups"`
}

func TestFindManyToMany(t *testing.T) {
	usersT := pg.NewTable("users")
	userIDc := pg.Add(usersT, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersT, pg.Text("name").NotNull())

	groupsT := pg.NewTable("groups")
	groupIDc := pg.Add(groupsT, pg.BigSerial("id").PrimaryKey())
	pg.Add(groupsT, pg.Text("name").NotNull())

	userGroupsT := pg.NewTable("user_groups")
	ugUserIDc := pg.Add(userGroupsT, pg.BigInt("user_id").NotNull())
	ugGroupIDc := pg.Add(userGroupsT, pg.BigInt("group_id").NotNull())

	pg.NewRelations(usersT).
		ManyToMany("groups", groupsT, userGroupsT, ugUserIDc, ugGroupIDc, userIDc, groupIDc)

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
		case strings.Contains(sql, `FROM "user_groups"`):
			return &fakeRows{
				cols: []string{"user_id", "group_id"},
				data: [][]any{
					{int64(1), int64(10)},
					{int64(1), int64(11)},
					{int64(2), int64(11)},
				},
			}, nil
		case strings.Contains(sql, `FROM "groups"`):
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{
					{int64(10), "Admins"},
					{int64(11), "Editors"},
				},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var users []m2mUser
	if err := db.Find(usersT).With("groups").All(context.Background(), &users); err != nil {
		t.Fatalf("Find: %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}

	alice := users[0]
	if len(alice.Groups) != 2 {
		t.Errorf("Alice should have 2 groups, got %d", len(alice.Groups))
	} else if alice.Groups[0].Name != "Admins" || alice.Groups[1].Name != "Editors" {
		t.Errorf("Alice groups in wrong order/values: %+v", alice.Groups)
	}

	bob := users[1]
	if len(bob.Groups) != 1 {
		t.Errorf("Bob should have 1 group, got %d", len(bob.Groups))
	} else if bob.Groups[0].Name != "Editors" {
		t.Errorf("Bob group should be Editors, got %s", bob.Groups[0].Name)
	}

	// Verify it took exactly 3 queries (users + junction + groups).
	if len(fd.queries) != 3 {
		t.Errorf("expected 3 queries, got %d: %v", len(fd.queries), fd.queries)
	}
}

func TestFindUnknownRelation(t *testing.T) {
	usersT, _, _, _ := mkRelSchema()
	db := pg.New(&fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name"}}, nil
	}})

	var users []relUser
	err := db.Find(usersT).With("nonexistent").All(context.Background(), &users)
	if err == nil || !strings.Contains(err.Error(), "unknown relation") {
		t.Errorf("expected unknown-relation error, got %v", err)
	}
}

// --- Migrations tests -------------------------------------------------

func TestParseMigrationName(t *testing.T) {
	cases := []struct {
		in                       string
		wantVer, wantName, wantK string
		wantOK                   bool
	}{
		{"0001_init.up.sql", "0001", "init", "up", true},
		{"0001_init.down.sql", "0001", "init", "down", true},
		{"0002_add_users.up.sql", "0002", "add_users", "up", true},
		{"README.md", "", "", "", false},
		{"missing_kind.sql", "", "", "", false},
		{"_no_version.up.sql", "", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			v, n, k, ok := pg.ParseMigrationName(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if v != tc.wantVer || n != tc.wantName || k != tc.wantK {
				t.Errorf("got (%q,%q,%q), want (%q,%q,%q)",
					v, n, k, tc.wantVer, tc.wantName, tc.wantK)
			}
		})
	}
}

func TestMigrateUpAndDown(t *testing.T) {
	applied := map[string]bool{}
	fd := &fakeDriver{
		exec: func(sql string, args []any) (drops.Result, error) {
			switch {
			case strings.Contains(sql, "INSERT INTO"):
				applied[args[0].(string)] = true
			case strings.Contains(sql, "DELETE FROM"):
				delete(applied, args[0].(string))
			}
			return fakeResult{affected: 1}, nil
		},
		handler: func(sql string, _ []any) (drops.Rows, error) {
			rows := [][]any{}
			for v := range applied {
				rows = append(rows, []any{v, time.Time{}})
			}
			return &fakeRows{cols: []string{"version", "applied_at"}, data: rows}, nil
		},
	}
	db := pg.New(fd)

	fsys := fstest.MapFS{
		"migrations/0001_init.up.sql":   {Data: []byte("CREATE TABLE foo();")},
		"migrations/0001_init.down.sql": {Data: []byte("DROP TABLE foo;")},
		"migrations/0002_more.up.sql":   {Data: []byte("ALTER TABLE foo ADD c text;")},
		"migrations/0002_more.down.sql": {Data: []byte("ALTER TABLE foo DROP COLUMN c;")},
	}
	m := pg.NewMigrator(db)
	if err := m.AddFS(fsys, "migrations"); err != nil {
		t.Fatalf("AddFS: %v", err)
	}

	ctx := context.Background()
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !applied["0001"] || !applied["0002"] {
		t.Errorf("after Up, applied = %v", applied)
	}

	if err := m.Down(ctx); err != nil {
		t.Fatalf("Down: %v", err)
	}
	if applied["0002"] {
		t.Errorf("after Down, 0002 should be reverted")
	}
	if !applied["0001"] {
		t.Errorf("after one Down, 0001 should still be applied")
	}
}
