package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/examples/schemagen"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// The generated eager-load shape, against a live server.
//
// examples/schemagen checks what the struct *is* — a slice for the
// HasMany, a pointer for the BelongsTo, the dropRel tag the loader
// looks up. What no unit test can check is that the loader fills it,
// and the case a hand-written struct usually gets wrong is not the
// happy one: it is the parent with no children, and the row whose
// foreign key matches nothing. Both need rows in a real table.
//
// The tables are declared here rather than reused from the example
// because the suite shares one database and "users" is not a name to
// claim in it. mustMirror asserts this declaration is the example's
// column for column, so the fixture cannot drift away from the
// structs under test.

type relsgenFixture struct {
	Users      *pg.Table
	UserID     *pg.Col[int64]
	UserEmail  *pg.Col[string]
	UserName   *pg.Col[string]
	UserAge    *pg.Col[int32]
	Posts      *pg.Table
	PostID     *pg.Col[int64]
	PostUserID *pg.Col[int64]
	PostTitle  *pg.Col[string]
}

func relsgenTables(t *testing.T, db *pg.DB) relsgenFixture {
	t.Helper()
	f := relsgenFixture{
		Users: pg.NewTable(integration.UniqueName(t, "relsgen_users")),
		Posts: pg.NewTable(integration.UniqueName(t, "relsgen_posts")),
	}
	f.UserID = pg.Add(f.Users, pg.BigSerial("id").PrimaryKey())
	f.UserEmail = pg.Add(f.Users, pg.Text("email").NotNull().Unique())
	f.UserName = pg.Add(f.Users, pg.Text("name").NotNull())
	f.UserAge = pg.Add(f.Users, pg.Integer("age").Nullable())
	pg.Add(f.Users, pg.Timestamp("createdAt", true).NotNull().Default("now()"))

	f.PostID = pg.Add(f.Posts, pg.BigSerial("id").PrimaryKey())
	f.PostUserID = pg.Add(f.Posts, pg.BigInt("user_id").NotNull())
	f.PostTitle = pg.Add(f.Posts, pg.Text("title").NotNull())

	mustMirror(t, f.Users, schemagen.Users)
	mustMirror(t, f.Posts, schemagen.Posts)

	// The relation names are the load-bearing part: they are what the
	// generated dropRel tags spell, and the whole point of generating
	// the struct is that the two cannot disagree.
	pg.NewRelations(f.Users).HasMany("posts", f.Posts, f.UserID, f.PostUserID)
	pg.NewRelations(f.Posts).BelongsTo("author", f.Users, f.PostUserID, f.UserID)

	dropPG(t, db, f.Posts)
	dropPG(t, db, f.Users)
	execPG(t, db, pg.CreateTable(f.Users))
	execPG(t, db, pg.CreateTable(f.Posts))
	return f
}

// insertUser inserts one user and returns the key the server assigned.
func insertUser(t *testing.T, db *pg.DB, f relsgenFixture, email, name string) int64 {
	t.Helper()
	var back struct {
		ID int64 `drop:"id"`
	}
	err := db.Insert(f.Users).
		Row(f.UserEmail.Val(email), f.UserName.Val(name)).
		Returning(f.UserID).
		One(context.Background(), &back)
	if err != nil {
		t.Fatalf("insert user %s: %v", email, err)
	}
	return back.ID
}

func insertPost(t *testing.T, db *pg.DB, f relsgenFixture, userID int64, title string) {
	t.Helper()
	if _, err := db.Insert(f.Posts).
		Row(f.PostUserID.Val(userID), f.PostTitle.Val(title)).
		Exec(context.Background()); err != nil {
		t.Fatalf("insert post %s: %v", title, err)
	}
}

// A HasMany fills the generated slice — and a parent with no children
// gets an empty one rather than a nil. That distinction is the whole
// reason the struct is worth generating: a nil slice means nobody
// loaded the relation, and the loader never leaves one behind.
func TestPGGeneratedShapeLoadsAHasManyIncludingTheEmptyOne(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	loud := insertUser(t, db, f, "loud@example.com", "Loud")
	insertUser(t, db, f, "quiet@example.com", "Quiet")
	insertPost(t, db, f, loud, "first")
	insertPost(t, db, f, loud, "second")

	var rows []schemagen.UsersWithPosts
	if err := db.Find(f.Users).
		With("posts").
		OrderBy(f.UserEmail.Asc()).
		All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("loaded %d users, want 2", len(rows))
	}

	// The columns arrive through the embedded row struct: the
	// relation field is tagged drop:"-" and takes part in no scan.
	if rows[0].Email != "loud@example.com" || rows[0].Name != "Loud" || rows[0].ID == 0 {
		t.Errorf("the embedded columns did not scan: %+v", rows[0].UsersRow)
	}

	if len(rows[0].Posts) != 2 {
		t.Fatalf("loud has %d posts, want 2", len(rows[0].Posts))
	}
	for _, p := range rows[0].Posts {
		if p.UserID != loud {
			t.Errorf("post %q belongs to user %d, want %d", p.Title, p.UserID, loud)
		}
		if p.ID == 0 || p.Title == "" {
			t.Errorf("post scanned empty: %+v", p)
		}
	}

	// The case a hand-written struct gets wrong. Not nil, and not
	// absent: an empty slice, which is what makes len() the answer to
	// "how many posts" and a nil the answer to "did anyone load it".
	quiet := rows[1]
	if quiet.Posts == nil {
		t.Error("a user with no posts came back with a nil slice, which is what an unloaded relation looks like")
	}
	if len(quiet.Posts) != 0 {
		t.Errorf("quiet has %d posts, want 0", len(quiet.Posts))
	}

	// The other half of the distinction, which is what makes the
	// empty slice worth asserting: a query that never loads the
	// relation leaves a nil behind, so nil and empty are two answers
	// and not one.
	var unloaded []schemagen.UsersWithPosts
	if err := db.Find(f.Users).OrderBy(f.UserEmail.Asc()).All(ctx, &unloaded); err != nil {
		t.Fatalf("find without the relation: %v", err)
	}
	for _, r := range unloaded {
		if r.Posts != nil {
			t.Errorf("%s came back with a non-nil slice from a query that loaded nothing: %+v", r.Email, r.Posts)
		}
	}
}

// A BelongsTo fills the generated pointer, and a row whose key
// matches nothing keeps its nil. That is the reason the field is a
// pointer at all: the loader leaves it untouched, so a value struct
// would come back zeroed and a zeroed struct is a real row of zeros.
func TestPGGeneratedShapeLoadsABelongsToIncludingTheAbsentOne(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	author := insertUser(t, db, f, "author@example.com", "Author")
	insertPost(t, db, f, author, "attributed")
	// No foreign key constraint on the fixture, so this post points
	// at a user that does not exist — the orphan a nil states and a
	// zero struct cannot.
	insertPost(t, db, f, author+10_000, "orphaned")

	var rows []schemagen.PostsWithAuthor
	if err := db.Find(f.Posts).
		With("author").
		OrderBy(f.PostTitle.Asc()).
		All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("loaded %d posts, want 2", len(rows))
	}

	attributed, orphaned := rows[0], rows[1]
	if attributed.Title != "attributed" || orphaned.Title != "orphaned" {
		t.Fatalf("ordering is not what the test assumes: %q, %q", attributed.Title, orphaned.Title)
	}
	if attributed.Author == nil {
		t.Fatal("the author of an attributed post did not load")
	}
	if attributed.Author.ID != author || attributed.Author.Email != "author@example.com" {
		t.Errorf("loaded author %+v, want user %d", *attributed.Author, author)
	}
	if orphaned.Author != nil {
		t.Errorf("an orphaned post loaded an author: %+v", orphaned.Author)
	}

	// And this is why the generator does not emit a value there. The
	// loader leaves the field alone when nothing matches, so the same
	// two rows read into a value-typed struct come back with an
	// orphan that is a zero User — which is what a real user with an
	// empty email and a zero id would also look like. The hand-written
	// struct below is the mistake; the generated one above cannot make
	// it.
	type postWithValueAuthor struct {
		schemagen.PostsRow
		Author schemagen.UsersRow `drop:"-" dropRel:"author"`
	}
	var byValue []postWithValueAuthor
	if err := db.Find(f.Posts).
		With("author").
		OrderBy(f.PostTitle.Asc()).
		All(ctx, &byValue); err != nil {
		t.Fatalf("find into a value-typed shape: %v", err)
	}
	if len(byValue) != 2 {
		t.Fatalf("loaded %d posts, want 2", len(byValue))
	}
	if byValue[1].Author != (schemagen.UsersRow{}) {
		t.Errorf("the value-typed shape did not leave the orphan zeroed: %+v", byValue[1].Author)
	}
	if byValue[1].Author.ID != 0 || byValue[1].Author.Email != "" {
		t.Error("a zeroed author is supposed to be indistinguishable from a real row of zeros")
	}
}

// The shape and the query are written from one spelling, and strict
// loading is what enforces it: the struct declares the relation, so a
// query that does not load it is refused before it runs. A generated
// tag that did not match the declaration would fail here too — the
// check looks the field up exactly as the loader does.
func TestPGGeneratedShapeAgreesWithStrictLoading(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)
	insertUser(t, db, f, "strict@example.com", "Strict")

	strict := db.StrictLoading()
	var rows []schemagen.UsersWithPosts
	err := strict.Find(f.Users).All(ctx, &rows)
	if !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("a query that skipped the relation returned %v, want ErrRelationNotLoaded", err)
	}

	// And the paths the generated doc comment names are the ones that
	// satisfy it.
	rows = nil
	if err := strict.Find(f.Users).With("posts").All(ctx, &rows); err != nil {
		t.Fatalf("the query the shape was generated for was refused: %v", err)
	}
	if len(rows) != 1 || rows[0].Posts == nil {
		t.Fatalf("loaded %d rows: %+v", len(rows), rows)
	}
}

// A nested path nests a generated struct inside a generated struct,
// and the loader walks it the same way: the second edge is loaded
// against the rows the first one just attached. The nested type here
// is UsersWithPosts, the same struct the users shape generates —
// which is what makes the two shapes agree rather than diverge.
func TestPGGeneratedShapeLoadsANestedPath(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	prolific := insertUser(t, db, f, "prolific@example.com", "Prolific")
	insertPost(t, db, f, prolific, "one")
	insertPost(t, db, f, prolific, "two")
	// A post whose author exists but has written nothing else cannot
	// happen here, so the empty case at depth two is the orphan: an
	// author that does not load at all, and therefore no posts under
	// it either.
	insertPost(t, db, f, prolific+10_000, "orphaned")

	var rows []schemagen.PostsWithAuthorPosts
	if err := db.Find(f.Posts).
		With("author.posts").
		OrderBy(f.PostTitle.Asc()).
		All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("loaded %d posts, want 3", len(rows))
	}

	byTitle := map[string]schemagen.PostsWithAuthorPosts{}
	for _, r := range rows {
		byTitle[r.Title] = r
	}

	// "one" and "two" both carry the same author, and the author
	// carries both of them back — the second edge loaded against the
	// rows the first one attached.
	for _, title := range []string{"one", "two"} {
		r, ok := byTitle[title]
		if !ok {
			t.Fatalf("post %q did not come back", title)
		}
		if r.Author == nil {
			t.Fatalf("post %q loaded no author", title)
		}
		if r.Author.ID != prolific {
			t.Errorf("post %q loaded author %d, want %d", title, r.Author.ID, prolific)
		}
		if len(r.Author.Posts) != 2 {
			t.Errorf("post %q: author carries %d posts, want 2", title, len(r.Author.Posts))
		}
	}
	if orphan := byTitle["orphaned"]; orphan.Author != nil {
		t.Errorf("the orphan loaded an author: %+v", orphan.Author)
	}
}
