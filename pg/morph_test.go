package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// Polymorphic schema:
//
//	users:    id, name
//	posts:    id, title
//	comments: id, body, commentable_type, commentable_id
//
// A comment points to either a user or a post via the
// (commentable_type, commentable_id) pair.

type morphUser struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

type morphPost struct {
	ID    int64  `drop:"id"`
	Title string `drop:"title"`
}

type morphComment struct {
	ID              int64  `drop:"id"`
	Body            string `drop:"body"`
	CommentableType string `drop:"commentable_type"`
	CommentableID   int64  `drop:"commentable_id"`
	Commentable     any    `dropRel:"commentable"`
}

type morphUserWithComments struct {
	ID       int64          `drop:"id"`
	Name     string         `drop:"name"`
	Comments []morphComment `dropRel:"comments"`
}

func morphSchema() (users, posts, comments *pg.Table, morphs *pg.MorphMap) {
	users = pg.NewTable("users")
	userID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	posts = pg.NewTable("posts")
	postID := pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pg.Add(posts, pg.Text("title").NotNull())

	comments = pg.NewTable("comments")
	pg.Add(comments, pg.BigSerial("id").PrimaryKey())
	pg.Add(comments, pg.Text("body").NotNull())
	cType := pg.Add(comments, pg.Text("commentable_type").NotNull())
	cID := pg.Add(comments, pg.BigInt("commentable_id").NotNull())

	morphs = pg.NewMorphMap()
	pg.RegisterMorph[morphUser](morphs, "users", users)
	pg.RegisterMorph[morphPost](morphs, "posts", posts)

	pg.NewRelations(comments).MorphTo("commentable", cType, cID, morphs)
	pg.NewRelations(users).MorphMany("comments", comments, cType, cID, userID, "users")
	pg.NewRelations(posts).MorphMany("comments", comments, cType, cID, postID, "posts")

	return
}

// TestMorphToLoadsPolymorphicParent verifies that a child whose
// commentable_type/commentable_id pair points at a User receives a
// *morphUser, and one pointing at a Post receives a *morphPost.
func TestMorphToLoadsPolymorphicParent(t *testing.T) {
	_, _, comments, _ := morphSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "comments"`):
			return &fakeRows{
				cols: []string{"id", "body", "commentable_type", "commentable_id"},
				data: [][]any{
					{int64(1), "great post", "posts", int64(10)},
					{int64(2), "hi alice", "users", int64(7)},
					{int64(3), "ok", "posts", int64(11)},
				},
			}, nil
		case strings.Contains(sql, `FROM "users"`):
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(7), "Alice"}},
			}, nil
		case strings.Contains(sql, `FROM "posts"`):
			return &fakeRows{
				cols: []string{"id", "title"},
				data: [][]any{
					{int64(10), "Hello"},
					{int64(11), "World"},
				},
			}, nil
		}
		return &fakeRows{}, nil
	}}
	db := pg.New(fd)

	var cs []morphComment
	if err := db.Find(comments).With("commentable").All(context.Background(), &cs); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(cs) != 3 {
		t.Fatalf("expected 3 comments, got %d", len(cs))
	}
	post, ok := cs[0].Commentable.(*morphPost)
	if !ok || post.Title != "Hello" {
		t.Errorf("expected first comment to point at *morphPost{Hello}, got %T", cs[0].Commentable)
	}
	user, ok := cs[1].Commentable.(*morphUser)
	if !ok || user.Name != "Alice" {
		t.Errorf("expected second comment to point at *morphUser{Alice}, got %T", cs[1].Commentable)
	}
	post2, ok := cs[2].Commentable.(*morphPost)
	if !ok || post2.Title != "World" {
		t.Errorf("expected third comment to point at *morphPost{World}, got %T", cs[2].Commentable)
	}
}

// TestMorphToBatchesByType verifies that one query is issued per
// distinct morph type (plus the root query for the children).
func TestMorphToBatchesByType(t *testing.T) {
	_, _, comments, _ := morphSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		switch {
		case strings.Contains(sql, `FROM "comments"`):
			return &fakeRows{
				cols: []string{"id", "body", "commentable_type", "commentable_id"},
				data: [][]any{
					{int64(1), "x", "users", int64(1)},
					{int64(2), "y", "users", int64(2)},
					{int64(3), "z", "posts", int64(10)},
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
		case strings.Contains(sql, `FROM "posts"`):
			return &fakeRows{
				cols: []string{"id", "title"},
				data: [][]any{{int64(10), "Hello"}},
			}, nil
		}
		return &fakeRows{}, nil
	}}
	db := pg.New(fd)

	var cs []morphComment
	if err := db.Find(comments).With("commentable").All(context.Background(), &cs); err != nil {
		t.Fatalf("Find: %v", err)
	}
	// 1 query for comments + 1 for users + 1 for posts = 3.
	if len(fd.queries) != 3 {
		t.Errorf("expected 3 queries (children, users, posts), got %d: %v",
			len(fd.queries), fd.queries)
	}
}

// TestMorphToUnknownTypeReturnsError verifies that an unregistered
// morph type is reported.
func TestMorphToUnknownTypeReturnsError(t *testing.T) {
	_, _, comments, _ := morphSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "body", "commentable_type", "commentable_id"},
			data: [][]any{
				{int64(1), "x", "rocks", int64(99)}, // unregistered
			},
		}, nil
	}}
	db := pg.New(fd)

	var cs []morphComment
	err := db.Find(comments).With("commentable").All(context.Background(), &cs)
	if err == nil || !strings.Contains(err.Error(), "unknown morph type") {
		t.Errorf("expected unknown-morph-type error, got: %v", err)
	}
}

// TestMorphToRejectsNestedWith verifies the validation guard.
func TestMorphToRejectsNestedWith(t *testing.T) {
	_, _, comments, _ := morphSchema()
	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		return &fakeRows{
			cols: []string{"id", "body", "commentable_type", "commentable_id"},
			data: [][]any{{int64(1), "x", "users", int64(1)}},
		}, nil
	}}
	db := pg.New(fd)
	var cs []morphComment
	err := db.Find(comments).With("commentable.something").All(context.Background(), &cs)
	if err == nil || !strings.Contains(err.Error(), "nested With() is not supported") {
		t.Errorf("expected nested-With error, got: %v", err)
	}
}

// TestMorphManyLoadsTypedSlice verifies the inverse direction —
// a User's "comments" collects only comments whose commentable_type
// is "users" and commentable_id is the user's id.
func TestMorphManyLoadsTypedSlice(t *testing.T) {
	users, _, _, _ := morphSchema()

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
		case strings.Contains(sql, `FROM "comments"`):
			// Verify the morph_type guard is on the WHERE.
			if !strings.Contains(sql, "commentable_type") {
				t.Errorf("MorphMany query must include the commentable_type guard: %s", sql)
			}
			return &fakeRows{
				cols: []string{"id", "body", "commentable_type", "commentable_id"},
				data: [][]any{
					{int64(11), "hi", "users", int64(1)},
					{int64(12), "bye", "users", int64(1)},
					{int64(13), "yo", "users", int64(2)},
				},
			}, nil
		}
		return &fakeRows{}, nil
	}}
	db := pg.New(fd)

	var us []morphUserWithComments
	if err := db.Find(users).With("comments").All(context.Background(), &us); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(us) != 2 || len(us[0].Comments) != 2 || us[0].Comments[0].Body != "hi" {
		t.Errorf("MorphMany failed to populate comments: %+v", us)
	}
}
