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

// --- Nested (deep) relations -----------------------------------------

type nestUser struct {
	ID    int64
	Name  string
	Posts []nestPost `db_rel:"posts"`
}

type nestPost struct {
	ID       int64
	UserID   int64 `db:"user_id"`
	Title    string
	Comments []nestComment `db_rel:"comments"`
}

type nestComment struct {
	ID     int64
	PostID int64 `db:"post_id"`
	Body   string
}

type nestPostWithAuthor struct {
	ID     int64
	UserID int64 `db:"user_id"`
	Title  string
	Author *nestUser `db_rel:"author"`
}

func mkNestSchema() (users, posts, comments *pg.Table) {
	usersT := pg.NewTable("users")
	uID := pg.Add(usersT, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersT, pg.Text("name").NotNull())

	postsT := pg.NewTable("posts")
	pID := pg.Add(postsT, pg.BigSerial("id").PrimaryKey())
	pUID := pg.Add(postsT, pg.BigInt("user_id").NotNull())
	pg.Add(postsT, pg.Text("title").NotNull())

	commentsT := pg.NewTable("comments")
	pg.Add(commentsT, pg.BigSerial("id").PrimaryKey())
	cPID := pg.Add(commentsT, pg.BigInt("post_id").NotNull())
	pg.Add(commentsT, pg.Text("body").NotNull())

	pg.NewRelations(usersT).
		HasMany("posts", postsT, uID, pUID)
	pg.NewRelations(postsT).
		HasMany("comments", commentsT, pID, cPID).
		BelongsTo("author", usersT, pUID, uID)

	return usersT, postsT, commentsT
}

func TestFindNestedHasManyHasMany(t *testing.T) {
	usersT, _, _ := mkNestSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}, {int64(2), "Bob"}},
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
		case strings.Contains(sql, `FROM "comments"`):
			return &fakeRows{
				cols: []string{"id", "post_id", "body"},
				data: [][]any{
					{int64(100), int64(10), "c-a"},
					{int64(101), int64(10), "c-b"},
					{int64(102), int64(11), "c-c"},
					{int64(103), int64(12), "c-d"},
				},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var users []nestUser
	if err := db.Find(usersT).With("posts.comments").All(context.Background(), &users); err != nil {
		t.Fatalf("Find: %v", err)
	}

	if len(users) != 2 {
		t.Fatalf("got %d users, want 2", len(users))
	}
	if len(users[0].Posts) != 2 {
		t.Fatalf("Alice posts: got %d, want 2", len(users[0].Posts))
	}
	// Deep stitch: comments must land on the posts stored inside the user.
	if got := len(users[0].Posts[0].Comments); got != 2 {
		t.Errorf("post 10 comments: got %d, want 2", got)
	}
	if got := len(users[0].Posts[1].Comments); got != 1 {
		t.Errorf("post 11 comments: got %d, want 1", got)
	}
	if users[0].Posts[0].Comments[0].Body != "c-a" {
		t.Errorf("post 10 first comment: got %q, want c-a", users[0].Posts[0].Comments[0].Body)
	}
	if len(users[1].Posts) != 1 || len(users[1].Posts[0].Comments) != 1 {
		t.Errorf("Bob: got %d posts, post comments %v", len(users[1].Posts), users[1].Posts)
	}

	// Exactly three queries: users, posts, comments — one per edge.
	if len(fd.queries) != 3 {
		t.Errorf("expected 3 queries, got %d: %v", len(fd.queries), fd.queries)
	}
}

func TestFindNestedBelongsToHasMany(t *testing.T) {
	_, postsT, _ := mkNestSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "comments"`):
			return &fakeRows{cols: []string{"id", "post_id", "body"}}, nil
		case strings.Contains(sql, `FROM "posts"`):
			return &fakeRows{
				cols: []string{"id", "user_id", "title"},
				data: [][]any{
					{int64(10), int64(1), "Hello"},
					{int64(20), int64(1), "Again"},
					{int64(11), int64(2), "Hi"},
				},
			}, nil
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}, {int64(2), "Bob"}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	// Start from posts, load each post's author, then that author's posts.
	var posts []nestPostWithAuthor
	if err := db.Find(postsT).With("author.posts").
		All(context.Background(), &posts); err != nil {
		t.Fatalf("Find: %v", err)
	}

	// Restrict assertions to a representative post authored by user 1.
	var alicePost *nestPostWithAuthor
	for i := range posts {
		if posts[i].UserID == 1 {
			alicePost = &posts[i]
			break
		}
	}
	if alicePost == nil || alicePost.Author == nil {
		t.Fatalf("expected an Alice post with a loaded author, got %+v", posts)
	}
	if alicePost.Author.Name != "Alice" {
		t.Errorf("author name: got %q, want Alice", alicePost.Author.Name)
	}
	// Alice authored posts 10 and 20 → her nested Posts slice has both.
	if got := len(alicePost.Author.Posts); got != 2 {
		t.Errorf("Alice nested posts: got %d, want 2", got)
	}
}

func TestFindNestedSharedPrefixMergedIntoOneQuery(t *testing.T) {
	usersT, _, _ := mkNestSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "Alice"}}}, nil
		case strings.Contains(sql, `FROM "posts"`):
			return &fakeRows{
				cols: []string{"id", "user_id", "title"},
				data: [][]any{{int64(10), int64(1), "Hello"}},
			}, nil
		case strings.Contains(sql, `FROM "comments"`):
			return &fakeRows{cols: []string{"id", "post_id", "body"}}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var users []nestUser
	// Two paths sharing the "posts" prefix must fetch posts exactly once.
	if err := db.Find(usersT).With("posts.comments", "posts").
		All(context.Background(), &users); err != nil {
		t.Fatalf("Find: %v", err)
	}

	postQueries := 0
	for _, q := range fd.queries {
		if strings.Contains(q, `FROM "posts"`) {
			postQueries++
		}
	}
	if postQueries != 1 {
		t.Errorf("expected posts fetched once, got %d queries", postQueries)
	}
}

func TestFindNestedUnknownRelationFailsFast(t *testing.T) {
	usersT, _, _ := mkNestSchema()
	// Handler would error if any query ran; validation must short-circuit.
	db := pg.New(&fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		return nil, fmt.Errorf("no query should run: %s", sql)
	}})

	var users []nestUser
	err := db.Find(usersT).With("posts.nope").All(context.Background(), &users)
	if err == nil || !strings.Contains(err.Error(), "unknown relation") {
		t.Fatalf("expected unknown-relation error before any query, got %v", err)
	}
}

func TestFindInvalidRelationPath(t *testing.T) {
	usersT, _, _ := mkNestSchema()
	db := pg.New(&fakeDriver{})
	var users []nestUser
	err := db.Find(usersT).With("posts..comments").All(context.Background(), &users)
	if err == nil || !strings.Contains(err.Error(), "invalid relation path") {
		t.Fatalf("expected invalid-path error, got %v", err)
	}
}

// --- Per-relation constraints (WithRel) -------------------------------

type filterUser struct {
	ID    int64
	Name  string
	Posts []filterPost `db_rel:"posts"`
}

type filterPost struct {
	ID        int64
	UserID    int64 `db:"user_id"`
	Title     string
	Published bool
}

func TestFindWithRelWhereFilters(t *testing.T) {
	usersT := pg.NewTable("users")
	uID := pg.Add(usersT, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersT, pg.Text("name").NotNull())

	postsT := pg.NewTable("posts")
	pg.Add(postsT, pg.BigSerial("id").PrimaryKey())
	pUID := pg.Add(postsT, pg.BigInt("user_id").NotNull())
	pg.Add(postsT, pg.Text("title").NotNull())
	published := pg.Add(postsT, pg.Boolean("published").NotNull())

	pg.NewRelations(usersT).HasMany("posts", postsT, uID, pUID)

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "Alice"}}}, nil
		case strings.Contains(sql, `FROM "posts"`):
			// Pretend the DB honoured the filter and returned published only.
			return &fakeRows{
				cols: []string{"id", "user_id", "title", "published"},
				data: [][]any{{int64(10), int64(1), "Live", true}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var users []filterUser
	err := db.Find(usersT).WithRel("posts", func(p *pg.RelConfig) {
		p.Where(published.Eq(true))
	}).All(context.Background(), &users)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	// The child query must carry the user-key IN predicate AND the filter.
	var postSQL string
	var postArgs []any
	for i, q := range fd.queries {
		if strings.Contains(q, `FROM "posts"`) {
			postSQL, postArgs = q, fd.args[i]
		}
	}
	if !strings.Contains(postSQL, `"posts"."published"`) {
		t.Errorf("child query missing filter predicate: %s", postSQL)
	}
	hasTrue := false
	for _, a := range postArgs {
		if b, ok := a.(bool); ok && b {
			hasTrue = true
		}
	}
	if !hasTrue {
		t.Errorf("child query args missing filter value true: %v", postArgs)
	}
	if len(users[0].Posts) != 1 || !users[0].Posts[0].Published {
		t.Errorf("expected one published post, got %+v", users[0].Posts)
	}
}

func TestFindWithRelOrderByEmitsClauseAndPreservesOrder(t *testing.T) {
	usersT := pg.NewTable("users")
	uID := pg.Add(usersT, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersT, pg.Text("name").NotNull())

	postsT := pg.NewTable("posts")
	pg.Add(postsT, pg.BigSerial("id").PrimaryKey())
	pUID := pg.Add(postsT, pg.BigInt("user_id").NotNull())
	title := pg.Add(postsT, pg.Text("title").NotNull())
	pg.Add(postsT, pg.Boolean("published").NotNull())

	pg.NewRelations(usersT).HasMany("posts", postsT, uID, pUID)

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "Alice"}}}, nil
		case strings.Contains(sql, `FROM "posts"`):
			// Returned already sorted by title (as the DB would).
			return &fakeRows{
				cols: []string{"id", "user_id", "title", "published"},
				data: [][]any{
					{int64(11), int64(1), "Apple", true},
					{int64(10), int64(1), "Banana", true},
				},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var users []filterUser
	err := db.Find(usersT).WithRel("posts", func(p *pg.RelConfig) {
		p.OrderBy(title.Asc())
	}).All(context.Background(), &users)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	var postSQL string
	for _, q := range fd.queries {
		if strings.Contains(q, `FROM "posts"`) {
			postSQL = q
		}
	}
	if !strings.Contains(postSQL, `ORDER BY "posts"."title" ASC`) {
		t.Errorf("child query missing ORDER BY: %s", postSQL)
	}
	if len(users[0].Posts) != 2 ||
		users[0].Posts[0].Title != "Apple" || users[0].Posts[1].Title != "Banana" {
		t.Errorf("per-parent order not preserved: %+v", users[0].Posts)
	}
}

func TestFindWithRelNestedFilterAndDepth(t *testing.T) {
	usersT, postsT, _ := mkNestSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "Alice"}}}, nil
		case strings.Contains(sql, `FROM "posts"`):
			return &fakeRows{
				cols: []string{"id", "user_id", "title"},
				data: [][]any{{int64(10), int64(1), "Hello"}},
			}, nil
		case strings.Contains(sql, `FROM "comments"`):
			return &fakeRows{
				cols: []string{"id", "post_id", "body"},
				data: [][]any{{int64(100), int64(10), "c-a"}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	// Filter posts by title, and still load their comments (one level deeper).
	title := postsT.Col("title")
	var users []nestUser
	err := db.Find(usersT).WithRel("posts", func(p *pg.RelConfig) {
		p.Where(pg.Eq(title, "Hello")).With("comments")
	}).All(context.Background(), &users)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	var postSQL string
	for _, q := range fd.queries {
		if strings.Contains(q, `FROM "posts"`) {
			postSQL = q
		}
	}
	if !strings.Contains(postSQL, `"posts"."title"`) {
		t.Errorf("posts query missing title filter: %s", postSQL)
	}
	if len(users[0].Posts) != 1 || len(users[0].Posts[0].Comments) != 1 {
		t.Errorf("nested comments not loaded under filtered posts: %+v", users[0].Posts)
	}
	// users, posts, comments — exactly three queries.
	if len(fd.queries) != 3 {
		t.Errorf("expected 3 queries, got %d: %v", len(fd.queries), fd.queries)
	}
}

func TestFindManyToManyOrderByUsesTargetOrder(t *testing.T) {
	usersT := pg.NewTable("users")
	userIDc := pg.Add(usersT, pg.BigSerial("id").PrimaryKey())
	pg.Add(usersT, pg.Text("name").NotNull())

	groupsT := pg.NewTable("groups")
	groupIDc := pg.Add(groupsT, pg.BigSerial("id").PrimaryKey())
	groupName := pg.Add(groupsT, pg.Text("name").NotNull())

	userGroupsT := pg.NewTable("user_groups")
	ugUserIDc := pg.Add(userGroupsT, pg.BigInt("user_id").NotNull())
	ugGroupIDc := pg.Add(userGroupsT, pg.BigInt("group_id").NotNull())

	pg.NewRelations(usersT).
		ManyToMany("groups", groupsT, userGroupsT, ugUserIDc, ugGroupIDc, userIDc, groupIDc)

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "Alice"}}}, nil
		case strings.Contains(sql, `FROM "user_groups"`):
			// Junction order: Editors(11) before Admins(10).
			return &fakeRows{
				cols: []string{"user_id", "group_id"},
				data: [][]any{{int64(1), int64(11)}, {int64(1), int64(10)}},
			}, nil
		case strings.Contains(sql, `FROM "groups"`):
			// Target query order (name ASC): Admins(10) before Editors(11).
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(10), "Admins"}, {int64(11), "Editors"}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := pg.New(fd)

	var users []m2mUser
	err := db.Find(usersT).WithRel("groups", func(g *pg.RelConfig) {
		g.OrderBy(groupName.Asc())
	}).All(context.Background(), &users)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	// Junction order would yield [Editors, Admins]; OrderBy must win.
	groups := users[0].Groups
	if len(groups) != 2 || groups[0].Name != "Admins" || groups[1].Name != "Editors" {
		t.Errorf("M2M OrderBy did not reindex to target order: %+v", groups)
	}
}

func TestFindWithRelInvalidPathFailsFast(t *testing.T) {
	usersT, _, _ := mkNestSchema()
	db := pg.New(&fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		return nil, fmt.Errorf("no query should run: %s", sql)
	}})
	var users []nestUser
	err := db.Find(usersT).WithRel("posts.", nil).All(context.Background(), &users)
	if err == nil || !strings.Contains(err.Error(), "invalid relation path") {
		t.Fatalf("expected invalid-path error, got %v", err)
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
