package mysql_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

// --- fake driver (named frXxx to avoid clashing with mysql_test.go) --

type frRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *frRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *frRows) Scan(dest ...any) error {
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

func (r *frRows) Columns() ([]string, error) { return r.cols, nil }
func (r *frRows) Close() error               { return nil }
func (r *frRows) Err() error                 { return nil }

type frResult struct{}

func (frResult) RowsAffected() (int64, error) { return 1, nil }

type frDriver struct {
	queries []string
	args    [][]any
	handler func(sql string, args []any) (drops.Rows, error)
}

func (f *frDriver) record(sql string, args []any) {
	f.queries = append(f.queries, sql)
	f.args = append(f.args, args)
}

func (f *frDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	f.record(sql, args)
	return frResult{}, nil
}

func (f *frDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	f.record(sql, args)
	if f.handler == nil {
		return &frRows{}, nil
	}
	return f.handler(sql, args)
}

func (f *frDriver) Begin(_ context.Context) (drops.Tx, error) { return &frTx{f}, nil }

type frTx struct{ *frDriver }

func (*frTx) Commit(_ context.Context) error   { return nil }
func (*frTx) Rollback(_ context.Context) error { return nil }

// --- fixtures ---------------------------------------------------------

type frUser struct {
	ID    int64    `drop:"id"`
	Name  string   `drop:"name"`
	Posts []frPost `dropRel:"posts"`
}

type frPost struct {
	ID     int64  `drop:"id"`
	UserID int64  `drop:"userId"`
	Title  string `drop:"title"`
}

type frPostWithAuthor struct {
	ID     int64   `drop:"id"`
	UserID int64   `drop:"userId"`
	Title  string  `drop:"title"`
	Author *frUser `dropRel:"author"`
}

func mkFindSchema() (users, posts *mysql.Table) {
	users = mysql.NewTable("users")
	uID := mysql.Add(users, mysql.BigInt("id").PrimaryKey())
	mysql.Add(users, mysql.Text("name").NotNull())

	posts = mysql.NewTable("posts")
	mysql.Add(posts, mysql.BigInt("id").PrimaryKey())
	pUID := mysql.Add(posts, mysql.BigInt("userId").NotNull())
	mysql.Add(posts, mysql.Text("title").NotNull())

	mysql.NewRelations(users).HasMany("posts", posts, uID, pUID)
	mysql.NewRelations(posts).BelongsTo("author", users, pUID, uID)

	return users, posts
}

// --- HasMany ----------------------------------------------------------

func TestFindHasManyEagerLoads(t *testing.T) {
	usersT, _ := mkFindSchema()

	fd := &frDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, "FROM `users`"):
			return &frRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}, {int64(2), "Bob"}},
			}, nil
		case strings.Contains(sql, "FROM `posts`"):
			return &frRows{
				cols: []string{"id", "userId", "title"},
				data: [][]any{
					{int64(10), int64(1), "Hello"},
					{int64(11), int64(1), "World"},
					{int64(12), int64(2), "Hi"},
				},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := mysql.New(fd)

	var users []frUser
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
	if users[0].Posts[0].Title != "Hello" || users[0].Posts[1].Title != "World" {
		t.Errorf("Alice post titles wrong: %+v", users[0].Posts)
	}
	if users[1].Posts[0].Title != "Hi" {
		t.Errorf("Bob post title wrong: %+v", users[1].Posts)
	}

	// Exactly two queries: parent + one batched child query (no N+1).
	if len(fd.queries) != 2 {
		t.Errorf("expected exactly 2 queries (parent + child), got %d: %v", len(fd.queries), fd.queries)
	}
	// The batched child query must use a single IN over both parent keys.
	var postSQL string
	var postArgs []any
	for i, q := range fd.queries {
		if strings.Contains(q, "FROM `posts`") {
			postSQL, postArgs = q, fd.args[i]
		}
	}
	if !strings.Contains(postSQL, "`posts`.`userId` IN (") {
		t.Errorf("child query missing IN predicate over userId: %s", postSQL)
	}
	if len(postArgs) != 2 {
		t.Errorf("child query should bind 2 deduped parent keys, got %d: %v", len(postArgs), postArgs)
	}
}

// --- BelongsTo --------------------------------------------------------

func TestFindBelongsToEagerLoads(t *testing.T) {
	_, postsT := mkFindSchema()

	fd := &frDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, "FROM `posts`"):
			return &frRows{
				cols: []string{"id", "userId", "title"},
				data: [][]any{
					{int64(10), int64(1), "Hello"},
					{int64(11), int64(2), "Hi"},
				},
			}, nil
		case strings.Contains(sql, "FROM `users`"):
			return &frRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}, {int64(2), "Bob"}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := mysql.New(fd)

	var posts []frPostWithAuthor
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

	// Exactly two queries: posts + one batched author query.
	if len(fd.queries) != 2 {
		t.Errorf("expected exactly 2 queries (parent + child), got %d: %v", len(fd.queries), fd.queries)
	}
	var userSQL string
	for _, q := range fd.queries {
		if strings.Contains(q, "FROM `users`") {
			userSQL = q
		}
	}
	if !strings.Contains(userSQL, "`users`.`id` IN (") {
		t.Errorf("author query missing IN predicate over users.id: %s", userSQL)
	}
}

// --- error + empty paths ----------------------------------------------

func TestFindUnknownRelationErrors(t *testing.T) {
	usersT, _ := mkFindSchema()
	db := mysql.New(&frDriver{handler: func(string, []any) (drops.Rows, error) {
		return &frRows{cols: []string{"id", "name"}}, nil
	}})

	var users []frUser
	err := db.Find(usersT).With("nope").All(context.Background(), &users)
	if err == nil || !strings.Contains(err.Error(), "unknown relation") {
		t.Errorf("expected unknown-relation error, got %v", err)
	}
}

func TestFindOneReturnsErrNoRows(t *testing.T) {
	usersT, _ := mkFindSchema()
	db := mysql.New(&frDriver{handler: func(string, []any) (drops.Rows, error) {
		return &frRows{cols: []string{"id", "name"}}, nil
	}})

	var user frUser
	err := db.Find(usersT).One(context.Background(), &user)
	// drops.ErrNoRows, not this package's ErrNoRows — see
	// TestFindOneReportsNoRowsAndNotTheInsertSentinel below. The
	// assertion this test was ported with named the identifier that
	// means "INSERT has no rows" here.
	if !errors.Is(err, drops.ErrNoRows) {
		t.Errorf("expected drops.ErrNoRows, got %v", err)
	}
}

// --- HasOne and ManyToMany --------------------------------------------

// Neither of these shapes is covered by the tests this file was ported
// from, and ManyToMany is the intricate one: two joins, a junction
// table nobody scans into a struct, and a key on each side. Shipping
// the loader into a dialect that had none of it without exercising the
// junction path would have been shipping the least-checked code in the
// package.

type frProfile struct {
	ID     int64  `drop:"id"`
	UserID int64  `drop:"userId"`
	Bio    string `drop:"bio"`
}

type frUserWithProfile struct {
	ID      int64      `drop:"id"`
	Name    string     `drop:"name"`
	Profile *frProfile `dropRel:"profile"`
}

func TestFindHasOneEagerLoads(t *testing.T) {
	users := mysql.NewTable("users")
	uID := mysql.Add(users, mysql.BigInt("id").PrimaryKey())
	mysql.Add(users, mysql.Text("name").NotNull())

	profiles := mysql.NewTable("profiles")
	mysql.Add(profiles, mysql.BigInt("id").PrimaryKey())
	pUID := mysql.Add(profiles, mysql.BigInt("userId").NotNull())
	mysql.Add(profiles, mysql.Text("bio").NotNull())

	mysql.NewRelations(users).HasOne("profile", profiles, uID, pUID)

	fd := &frDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, "FROM `users`"):
			return &frRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}, {int64(2), "Bob"}},
			}, nil
		case strings.Contains(sql, "FROM `profiles`"):
			return &frRows{
				cols: []string{"id", "userId", "bio"},
				// Only Alice has one, so Bob's field must stay nil
				// rather than borrow hers.
				data: [][]any{{int64(10), int64(1), "hi"}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}

	var got []frUserWithProfile
	if err := mysql.New(fd).Find(users).With("profile").All(context.Background(), &got); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d users, want 2", len(got))
	}
	if got[0].Profile == nil || got[0].Profile.Bio != "hi" {
		t.Errorf("Alice's profile did not load: %+v", got[0].Profile)
	}
	if got[1].Profile != nil {
		t.Errorf("Bob has no profile row and got one anyway: %+v", got[1].Profile)
	}
	// One query for the parents and exactly one for the edge.
	if len(fd.queries) != 2 {
		t.Errorf("ran %d queries, want 2 — eager loading must not go N+1: %v", len(fd.queries), fd.queries)
	}
}

type frTag struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

type frPostWithTags struct {
	ID    int64   `drop:"id"`
	Title string  `drop:"title"`
	Tags  []frTag `dropRel:"tags"`
}

func TestFindManyToManyEagerLoadsThroughTheJunction(t *testing.T) {
	posts := mysql.NewTable("posts")
	postID := mysql.Add(posts, mysql.BigInt("id").PrimaryKey())
	mysql.Add(posts, mysql.Text("title").NotNull())

	tags := mysql.NewTable("tags")
	tagID := mysql.Add(tags, mysql.BigInt("id").PrimaryKey())
	mysql.Add(tags, mysql.Text("name").NotNull())

	postTags := mysql.NewTable("post_tags")
	ptPost := mysql.Add(postTags, mysql.BigInt("postId").NotNull())
	ptTag := mysql.Add(postTags, mysql.BigInt("tagId").NotNull())

	mysql.NewRelations(posts).ManyToMany("tags", tags, postTags, ptPost, ptTag, postID, tagID)

	var junctionSQL string
	fd := &frDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, "FROM `posts`"):
			return &frRows{
				cols: []string{"id", "title"},
				data: [][]any{{int64(1), "Hello"}, {int64(2), "World"}},
			}, nil
		case strings.Contains(sql, "FROM `post_tags`"):
			junctionSQL = sql
			// Post 1 has both tags, post 2 has the second one only.
			return &frRows{
				cols: []string{"postId", "tagId"},
				data: [][]any{
					{int64(1), int64(10)},
					{int64(1), int64(11)},
					{int64(2), int64(11)},
				},
			}, nil
		case strings.Contains(sql, "FROM `tags`"):
			return &frRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(10), "go"}, {int64(11), "sql"}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}

	var got []frPostWithTags
	if err := mysql.New(fd).Find(posts).With("tags").All(context.Background(), &got); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d posts, want 2", len(got))
	}
	if len(got[0].Tags) != 2 {
		t.Errorf("post 1 got %d tags, want 2: %+v", len(got[0].Tags), got[0].Tags)
	}
	if len(got[1].Tags) != 1 || got[1].Tags[0].Name != "sql" {
		t.Errorf("post 2 got %+v, want just the sql tag", got[1].Tags)
	}

	// The junction is read on its own rather than joined into the
	// target query, and the reason is in loadManyToMany: rendering a
	// join would put the junction table's own context filters nowhere.
	// A membership row is what says which tenant an association
	// belongs to, so a junction read that skipped them would link this
	// tenant's parents to every tenant's children — and the target
	// query cannot notice, because it is handed the keys as values.
	if !strings.Contains(junctionSQL, "`post_tags`.`postId` IN (") {
		t.Errorf("the junction query is not batched over the parent keys: %s", junctionSQL)
	}
	if strings.Contains(junctionSQL, "JOIN") {
		t.Errorf("the junction query joined instead of standing alone: %s", junctionSQL)
	}
	// Three statements: the parents, the junction, the targets. Never
	// one per parent.
	if len(fd.queries) != 3 {
		t.Errorf("ran %d queries, want 3: %v", len(fd.queries), fd.queries)
	}
}

// Find().One() reports "no rows in the result set", not "INSERT has no
// rows".
//
// This package has an exported ErrNoRows whose message is "drops/mysql:
// INSERT has no rows" — a different fact that happens to share the name
// drops/pg and drops/sqlite give to the query-result sentinel. The port
// of find.go returned that one, so a Find that matched nothing failed
// with a complaint about an INSERT nobody ran.
func TestFindOneReportsNoRowsAndNotTheInsertSentinel(t *testing.T) {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigInt("id").PrimaryKey())
	mysql.Add(users, mysql.Text("name").NotNull())

	fd := &frDriver{handler: func(string, []any) (drops.Rows, error) {
		return &frRows{cols: []string{"id", "name"}}, nil
	}}

	var got frUser
	err := mysql.New(fd).Find(users).One(context.Background(), &got)
	if !errors.Is(err, drops.ErrNoRows) {
		t.Fatalf("Find.One with no matching row: %v, want drops.ErrNoRows", err)
	}
	if errors.Is(err, mysql.ErrNoRows) {
		t.Errorf("Find.One reported the INSERT sentinel: %v", err)
	}
}
