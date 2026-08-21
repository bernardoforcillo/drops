package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/examples/schemagen"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// The generated eager-load shapes, against a live server.
//
// examples/schemagen checks what the structs *are* — a slice for the
// many-kinds, a pointer for the one-kinds, an interface for the
// MorphTo, and the dropRel tag the loader looks up. What no unit test
// can check is that the loader fills them, and the case a
// hand-written struct usually gets wrong is not the happy one: it is
// the parent with no children, the HasOne with no match, the
// ManyToMany whose junction is empty. Every one of those needs rows
// in a real table — or, more precisely, the absence of them.
//
// Every relation kind the generator emits a field type for is loaded
// here, from the generated struct rather than from a mirror. That is
// the whole point of the file: a hand-written mirror and a generator
// can be wrong in the same way, because the same person wrote both
// expectations, and a live load from the generated type is the only
// arrangement where neither half can cover for the other.
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

	Profiles      *pg.Table
	ProfileID     *pg.Col[int64]
	ProfileUserID *pg.Col[int64]
	ProfileBio    *pg.Col[string]

	Tags     *pg.Table
	TagID    *pg.Col[int64]
	TagLabel *pg.Col[string]

	PostTags      *pg.Table
	PostTagPostID *pg.Col[int64]
	PostTagTagID  *pg.Col[int64]

	Notes         *pg.Table
	NoteID        *pg.Col[int64]
	NoteBody      *pg.Col[string]
	NoteOwnerType *pg.Col[string]
	NoteOwnerID   *pg.Col[int64]
}

func relsgenTables(t *testing.T, db *pg.DB) relsgenFixture {
	t.Helper()
	f := relsgenFixture{
		Users:    pg.NewTable(relsgenName(t, "users")),
		Posts:    pg.NewTable(relsgenName(t, "posts")),
		Profiles: pg.NewTable(relsgenName(t, "profiles")),
		Tags:     pg.NewTable(relsgenName(t, "tags")),
		PostTags: pg.NewTable(relsgenName(t, "postTags")),
		Notes:    pg.NewTable(relsgenName(t, "notes")),
	}
	f.UserID = pg.Add(f.Users, pg.BigSerial("id").PrimaryKey())
	f.UserEmail = pg.Add(f.Users, pg.Text("email").NotNull().Unique())
	f.UserName = pg.Add(f.Users, pg.Text("name").NotNull())
	f.UserAge = pg.Add(f.Users, pg.Integer("age").Nullable())
	pg.Add(f.Users, pg.Timestamp("createdAt", true).NotNull().Default("now()"))

	f.PostID = pg.Add(f.Posts, pg.BigSerial("id").PrimaryKey())
	f.PostUserID = pg.Add(f.Posts, pg.BigInt("user_id").NotNull())
	f.PostTitle = pg.Add(f.Posts, pg.Text("title").NotNull())

	f.ProfileID = pg.Add(f.Profiles, pg.BigSerial("id").PrimaryKey())
	f.ProfileUserID = pg.Add(f.Profiles, pg.BigInt("user_id").NotNull())
	f.ProfileBio = pg.Add(f.Profiles, pg.Text("bio").NotNull())

	f.TagID = pg.Add(f.Tags, pg.BigSerial("id").PrimaryKey())
	f.TagLabel = pg.Add(f.Tags, pg.Text("label").NotNull())

	f.PostTagPostID = pg.Add(f.PostTags, pg.BigInt("post_id").NotNull())
	f.PostTagTagID = pg.Add(f.PostTags, pg.BigInt("tag_id").NotNull())

	f.NoteID = pg.Add(f.Notes, pg.BigSerial("id").PrimaryKey())
	f.NoteBody = pg.Add(f.Notes, pg.Text("body").NotNull())
	f.NoteOwnerType = pg.Add(f.Notes, pg.Text("owner_type").NotNull())
	f.NoteOwnerID = pg.Add(f.Notes, pg.BigInt("owner_id").NotNull())

	mustMirror(t, f.Users, schemagen.Users)
	mustMirror(t, f.Posts, schemagen.Posts)
	mustMirror(t, f.Profiles, schemagen.Profiles)
	mustMirror(t, f.Tags, schemagen.Tags)
	mustMirror(t, f.PostTags, schemagen.PostTags)
	mustMirror(t, f.Notes, schemagen.Notes)

	// The relation names are the load-bearing part: they are what the
	// generated dropRel tags spell, and the whole point of generating
	// the struct is that the two cannot disagree. The kinds are the
	// other half — the field type the generator chose is wrong unless
	// the loader fills exactly that kind of field.
	pg.NewRelations(f.Users).
		HasMany("posts", f.Posts, f.UserID, f.PostUserID).
		HasOne("profile", f.Profiles, f.UserID, f.ProfileUserID).
		MorphMany("notes", f.Notes, f.NoteOwnerType, f.NoteOwnerID, f.UserID, relsgenMorphUsers)
	pg.NewRelations(f.Posts).
		BelongsTo("author", f.Users, f.PostUserID, f.UserID).
		ManyToMany("tags", f.Tags, f.PostTags, f.PostTagPostID, f.PostTagTagID, f.PostID, f.TagID)
	// The row type behind the discriminator is the one thing the
	// generated shape does not fix: NotesWithOwner declares `any`, and
	// which struct arrives in it is the MorphMap's decision. The
	// example registers its hand-written User there only because
	// `dropsgen -rows` compiles that package with its own output moved
	// aside — nothing stops a caller registering the generated row
	// struct, which is what this does.
	morphs := pg.NewMorphMap()
	pg.RegisterMorph[schemagen.UsersRow](morphs, relsgenMorphUsers, f.Users)
	pg.NewRelations(f.Notes).MorphTo("owner", f.NoteOwnerType, f.NoteOwnerID, morphs)

	tables := []*pg.Table{f.Users, f.Posts, f.Profiles, f.Tags, f.PostTags, f.Notes}
	for _, tbl := range tables {
		dropPG(t, db, tbl)
		execPG(t, db, pg.CreateTable(tbl))
	}
	return f
}

// relsgenMorphUsers is the discriminator a note carries when it hangs
// off a user, and it is one constant because the MorphMany and the
// MorphTo have to agree on it.
const relsgenMorphUsers = "users"

// relsgenName names one of the fixture's six tables.
//
// integration.UniqueName trims a long identifier from the front,
// which is where it puts the prefix — so six tables that differ only
// in their prefix become one name the moment the test's own name is
// long enough, and CREATE TABLE reports the second one as a table
// that already exists. Putting the distinguishing part first and
// trimming the shared tail is the same trick the other way round.
func relsgenName(t *testing.T, part string) string {
	t.Helper()
	base := integration.UniqueName(t, "rg")
	// PostgreSQL truncates an identifier at 63 bytes, and a name that
	// lost its tail to the server is one two tables can share again.
	if limit := 62 - len(part); len(base) > limit {
		base = base[len(base)-limit:]
	}
	return part + "_" + base
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

func insertProfile(t *testing.T, db *pg.DB, f relsgenFixture, userID int64, bio string) {
	t.Helper()
	if _, err := db.Insert(f.Profiles).
		Row(f.ProfileUserID.Val(userID), f.ProfileBio.Val(bio)).
		Exec(context.Background()); err != nil {
		t.Fatalf("insert profile for %d: %v", userID, err)
	}
}

// insertTag inserts one tag and returns the key the server assigned.
func insertTag(t *testing.T, db *pg.DB, f relsgenFixture, label string) int64 {
	t.Helper()
	var back struct {
		ID int64 `drop:"id"`
	}
	if err := db.Insert(f.Tags).
		Row(f.TagLabel.Val(label)).
		Returning(f.TagID).
		One(context.Background(), &back); err != nil {
		t.Fatalf("insert tag %s: %v", label, err)
	}
	return back.ID
}

func insertPostTag(t *testing.T, db *pg.DB, f relsgenFixture, postID, tagID int64) {
	t.Helper()
	if _, err := db.Insert(f.PostTags).
		Row(f.PostTagPostID.Val(postID), f.PostTagTagID.Val(tagID)).
		Exec(context.Background()); err != nil {
		t.Fatalf("insert junction row (%d, %d): %v", postID, tagID, err)
	}
}

func insertNote(t *testing.T, db *pg.DB, f relsgenFixture, ownerType string, ownerID int64, body string) {
	t.Helper()
	if _, err := db.Insert(f.Notes).
		Row(f.NoteBody.Val(body), f.NoteOwnerType.Val(ownerType), f.NoteOwnerID.Val(ownerID)).
		Exec(context.Background()); err != nil {
		t.Fatalf("insert note %s: %v", body, err)
	}
}

// insertPostReturning is insertPost when the caller needs the key —
// a junction row cannot be written without one.
func insertPostReturning(t *testing.T, db *pg.DB, f relsgenFixture, userID int64, title string) int64 {
	t.Helper()
	var back struct {
		ID int64 `drop:"id"`
	}
	if err := db.Insert(f.Posts).
		Row(f.PostUserID.Val(userID), f.PostTitle.Val(title)).
		Returning(f.PostID).
		One(context.Background(), &back); err != nil {
		t.Fatalf("insert post %s: %v", title, err)
	}
	return back.ID
}

// A HasOne fills the generated pointer, and a user with no profile
// row keeps its nil.
//
// This is the cardinality a hand-written struct is most likely to
// declare as a value, because "a user has a profile" reads like a
// field rather than like a query — and a value cannot say that the
// row is missing. Note what the pointer buys and what it does not:
// nil here means "no matching row" *or* "nobody loaded it", and the
// two are one answer. That is why the many-kinds' empty-versus-nil
// distinction is worth the sentence the generator writes about it,
// and why this kind gets no such sentence.
func TestPGGeneratedShapeLoadsAHasOneIncludingTheAbsentOne(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	described := insertUser(t, db, f, "described@example.com", "Described")
	insertUser(t, db, f, "unwritten@example.com", "Unwritten")
	insertProfile(t, db, f, described, "the one profile")
	// A second profile row for a user nobody asked about, so a load
	// that ignored the key would be caught rather than pass.
	insertProfile(t, db, f, described+10_000, "somebody else's")

	var rows []schemagen.UsersWithProfile
	if err := db.Find(f.Users).
		With("profile").
		OrderBy(f.UserEmail.Asc()).
		All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("loaded %d users, want 2", len(rows))
	}
	with, without := rows[0], rows[1]
	if with.Email != "described@example.com" || without.Email != "unwritten@example.com" {
		t.Fatalf("ordering is not what the test assumes: %q, %q", with.Email, without.Email)
	}
	if with.Profile == nil {
		t.Fatal("the profile of a user who has one did not load")
	}
	if with.Profile.Bio != "the one profile" || with.Profile.UserID != described {
		t.Errorf("loaded profile %+v, want the one belonging to user %d", *with.Profile, described)
	}
	if without.Profile != nil {
		t.Errorf("a user with no profile row loaded one: %+v", without.Profile)
	}
}

// A ManyToMany fills the generated slice with the *target's* rows,
// and a post whose junction is empty gets an empty slice rather than
// a nil.
//
// Two things here are only observable against a server. The junction
// never appears in the shape at all — the field is []TagsRow, not
// []PostTagsRow — so a generator that nested the wrong table would
// still compile and would still be wrong. And the empty case takes
// two queries to get right: the junction query returns nothing, and
// the loader has to assign an empty slice anyway rather than skip the
// parent.
func TestPGGeneratedShapeLoadsAManyToManyIncludingTheEmptyJunction(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	author := insertUser(t, db, f, "author@example.com", "Author")
	tagged := insertPostReturning(t, db, f, author, "tagged")
	insertPostReturning(t, db, f, author, "untagged")

	goTag := insertTag(t, db, f, "go")
	sqlTag := insertTag(t, db, f, "sql")
	insertTag(t, db, f, "orphan") // a tag no junction row mentions
	insertPostTag(t, db, f, tagged, goTag)
	insertPostTag(t, db, f, tagged, sqlTag)
	// A junction row pointing at a tag that does not exist. The
	// second query finds nothing for it, and the slice must be the
	// two real tags rather than three entries with a zero in it.
	insertPostTag(t, db, f, tagged, sqlTag+10_000)

	var rows []schemagen.PostsWithTags
	if err := db.Find(f.Posts).
		With("tags").
		OrderBy(f.PostTitle.Asc()).
		All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("loaded %d posts, want 2", len(rows))
	}
	hasTags, hasNone := rows[0], rows[1]
	if hasTags.Title != "tagged" || hasNone.Title != "untagged" {
		t.Fatalf("ordering is not what the test assumes: %q, %q", hasTags.Title, hasNone.Title)
	}

	labels := map[string]bool{}
	for _, tag := range hasTags.Tags {
		if tag.ID == 0 {
			t.Errorf("a tag scanned empty: %+v", tag)
		}
		labels[tag.Label] = true
	}
	if len(hasTags.Tags) != 2 || !labels["go"] || !labels["sql"] {
		t.Errorf("tagged post carries %+v, want the go and sql tags and nothing else", hasTags.Tags)
	}

	// The case a hand-written struct gets wrong, and the one the
	// junction makes easy to get wrong twice.
	if hasNone.Tags == nil {
		t.Error("a post with an empty junction came back with a nil slice, which is what an unloaded relation looks like")
	}
	if len(hasNone.Tags) != 0 {
		t.Errorf("untagged post carries %+v, want nothing", hasNone.Tags)
	}
}

// A MorphMany fills the generated slice, the discriminator decides
// what belongs in it, and a user with no notes gets an empty slice.
func TestPGGeneratedShapeLoadsAMorphManyIncludingTheEmptyOne(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	annotated := insertUser(t, db, f, "annotated@example.com", "Annotated")
	insertUser(t, db, f, "silent@example.com", "Silent")
	insertNote(t, db, f, relsgenMorphUsers, annotated, "first")
	insertNote(t, db, f, relsgenMorphUsers, annotated, "second")
	// The same id under a different discriminator. A MorphMany that
	// filtered on the key alone would hand this to the user, which is
	// the whole difference between a MorphMany and a HasMany and is
	// invisible to any test that only ever writes one owner type.
	insertNote(t, db, f, "posts", annotated, "belongs to a post")

	var rows []schemagen.UsersWithNotes
	if err := db.Find(f.Users).
		With("notes").
		OrderBy(f.UserEmail.Asc()).
		All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("loaded %d users, want 2", len(rows))
	}
	noisy, silent := rows[0], rows[1]
	if noisy.Email != "annotated@example.com" || silent.Email != "silent@example.com" {
		t.Fatalf("ordering is not what the test assumes: %q, %q", noisy.Email, silent.Email)
	}

	bodies := map[string]bool{}
	for _, note := range noisy.Notes {
		if note.OwnerType != relsgenMorphUsers || note.OwnerID != annotated {
			t.Errorf("note %q is attached to (%s, %d)", note.Body, note.OwnerType, note.OwnerID)
		}
		bodies[note.Body] = true
	}
	if len(noisy.Notes) != 2 || !bodies["first"] || !bodies["second"] {
		t.Errorf("loaded %+v, want the two notes whose owner type is %q", noisy.Notes, relsgenMorphUsers)
	}
	if silent.Notes == nil {
		t.Error("a user with no notes came back with a nil slice, which is what an unloaded relation looks like")
	}
	if len(silent.Notes) != 0 {
		t.Errorf("silent carries %+v, want nothing", silent.Notes)
	}
}

// A MorphTo fills the generated interface with a pointer to whatever
// the discriminator names, and a note pointing at nothing keeps its
// nil.
//
// The generated field is `any` because the concrete type varies row
// by row; which type arrives is the MorphMap's decision, and this
// fixture registers the generated row struct. So the assertion below
// is the round trip the shape cannot make on its own: a declaration
// the generator wrote, a registration the caller wrote, and one type
// assertion that only succeeds if the two agree.
func TestPGGeneratedShapeLoadsAMorphToIncludingTheAbsentOne(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	f := relsgenTables(t, db)

	owner := insertUser(t, db, f, "owner@example.com", "Owner")
	insertNote(t, db, f, relsgenMorphUsers, owner, "attached")
	// A note whose discriminator is registered but whose id matches
	// no row: the bucket is queried, finds nothing, and the field is
	// left alone.
	insertNote(t, db, f, relsgenMorphUsers, owner+10_000, "dangling")

	var rows []schemagen.NotesWithOwner
	if err := db.Find(f.Notes).
		With("owner").
		OrderBy(f.NoteBody.Asc()).
		All(ctx, &rows); err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("loaded %d notes, want 2", len(rows))
	}
	attached, dangling := rows[0], rows[1]
	if attached.Body != "attached" || dangling.Body != "dangling" {
		t.Fatalf("ordering is not what the test assumes: %q, %q", attached.Body, dangling.Body)
	}

	loaded, ok := attached.Owner.(*schemagen.UsersRow)
	if !ok {
		t.Fatalf("the loaded owner is %T, not the *UsersRow the MorphMap registered", attached.Owner)
	}
	if loaded.ID != owner || loaded.Email != "owner@example.com" {
		t.Errorf("loaded owner %+v, want user %d", *loaded, owner)
	}
	if dangling.Owner != nil {
		t.Errorf("a note pointing at no row loaded an owner: %+v", dangling.Owner)
	}
}
