package sqlite_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// --- fake driver (named frXxx to avoid clashing with sqlite_test.go) --

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

func mkFindSchema() (users, posts *sqlite.Table) {
	users = sqlite.NewTable("users")
	uID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(users, sqlite.Text("name").NotNull())

	posts = sqlite.NewTable("posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	pUID := sqlite.Add(posts, sqlite.BigInt("userId").NotNull())
	sqlite.Add(posts, sqlite.Text("title").NotNull())

	sqlite.NewRelations(users).HasMany("posts", posts, uID, pUID)
	sqlite.NewRelations(posts).BelongsTo("author", users, pUID, uID)

	return users, posts
}

// --- HasMany ----------------------------------------------------------

func TestFindHasManyEagerLoads(t *testing.T) {
	usersT, _ := mkFindSchema()

	fd := &frDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "users"`):
			return &frRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}, {int64(2), "Bob"}},
			}, nil
		case strings.Contains(sql, `FROM "posts"`):
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
	db := sqlite.New(fd)

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
		if strings.Contains(q, `FROM "posts"`) {
			postSQL, postArgs = q, fd.args[i]
		}
	}
	if !strings.Contains(postSQL, `"posts"."userId" IN (`) {
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
		case strings.Contains(sql, `FROM "posts"`):
			return &frRows{
				cols: []string{"id", "userId", "title"},
				data: [][]any{
					{int64(10), int64(1), "Hello"},
					{int64(11), int64(2), "Hi"},
				},
			}, nil
		case strings.Contains(sql, `FROM "users"`):
			return &frRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}, {int64(2), "Bob"}},
			}, nil
		}
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}}
	db := sqlite.New(fd)

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
		if strings.Contains(q, `FROM "users"`) {
			userSQL = q
		}
	}
	if !strings.Contains(userSQL, `"users"."id" IN (`) {
		t.Errorf("author query missing IN predicate over users.id: %s", userSQL)
	}
}

// --- error + empty paths ----------------------------------------------

func TestFindUnknownRelationErrors(t *testing.T) {
	usersT, _ := mkFindSchema()
	db := sqlite.New(&frDriver{handler: func(string, []any) (drops.Rows, error) {
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
	db := sqlite.New(&frDriver{handler: func(string, []any) (drops.Rows, error) {
		return &frRows{cols: []string{"id", "name"}}, nil
	}})

	var user frUser
	err := db.Find(usersT).One(context.Background(), &user)
	if err != sqlite.ErrNoRows {
		t.Errorf("expected ErrNoRows, got %v", err)
	}
}
