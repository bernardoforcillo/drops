package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// relsFixtureSchema declares one table of every relation kind the
// loader knows, because the shape a kind produces is the whole
// decision this mode makes.
const relsFixtureSchema = `package fixture

import "github.com/bernardoforcillo/drops/pg"

var (
	Users         = pg.NewTable("users")
	UserID        = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserName      = pg.Add(Users, pg.Text("name").NotNull())
	UserManagerID = pg.Add(Users, pg.BigInt("managerId").Nullable())

	Posts      = pg.NewTable("posts")
	PostID     = pg.Add(Posts, pg.BigSerial("id").PrimaryKey())
	PostUserID = pg.Add(Posts, pg.BigInt("userId").NotNull())
	PostTitle  = pg.Add(Posts, pg.Text("title").NotNull())

	Comments        = pg.NewTable("comments")
	CommentID       = pg.Add(Comments, pg.BigSerial("id").PrimaryKey())
	CommentPostID   = pg.Add(Comments, pg.BigInt("postId").NotNull())
	CommentBody     = pg.Add(Comments, pg.Text("body").NotNull())
	CommentOwnerTyp = pg.Add(Comments, pg.Text("commentableType").NotNull())
	CommentOwnerID  = pg.Add(Comments, pg.BigInt("commentableId").NotNull())

	Profiles      = pg.NewTable("profiles")
	ProfileID     = pg.Add(Profiles, pg.BigSerial("id").PrimaryKey())
	ProfileUserID = pg.Add(Profiles, pg.BigInt("userId").NotNull())
	ProfileBio    = pg.Add(Profiles, pg.Text("bio").NotNull())

	Tags     = pg.NewTable("tags")
	TagID    = pg.Add(Tags, pg.BigSerial("id").PrimaryKey())
	TagLabel = pg.Add(Tags, pg.Text("label").NotNull())

	PostTags       = pg.NewTable("postTags")
	PostTagPostID  = pg.Add(PostTags, pg.BigInt("postId").NotNull())
	PostTagTagID   = pg.Add(PostTags, pg.BigInt("tagId").NotNull())
)

// morphUser is what the MorphTo side scans a user into. It is
// hand-written rather than the generated UsersRow because this
// declaration has to compile before anything is generated.
type morphUser struct {
	ID int64 ` + "`drop:\"id\"`" + `
}

var morphs = pg.RegisterMorph[morphUser](pg.NewMorphMap(), "users", Users)

var _ = pg.NewRelations(Users).
	HasMany("posts", Posts, UserID, PostUserID).
	HasOne("profile", Profiles, UserID, ProfileUserID).
	BelongsTo("manager", Users, UserManagerID, UserID).
	MorphMany("remarks", Comments, CommentOwnerTyp, CommentOwnerID, UserID, "users")

var _ = pg.NewRelations(Posts).
	BelongsTo("author", Users, PostUserID, UserID).
	HasMany("comments", Comments, PostID, CommentPostID).
	ManyToMany("tags", Tags, PostTags, PostTagPostID, PostTagTagID, PostID, TagID)

var _ = pg.NewRelations(Comments).
	MorphTo("commentable", CommentOwnerTyp, CommentOwnerID, morphs)

func Schema() *pg.Schema {
	return pg.NewSchema(Users, Posts, Comments, Profiles, Tags, PostTags)
}
`

// relsFixture writes the fixture package and runs rows mode over it,
// because an eager-load shape nests the row structs rows mode emits
// and the two modes are meant to be run together.
func relsFixture(t *testing.T, extra ...string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := rowsFixture(t, map[string]string{"schema.go": relsFixtureSchema})
	if err := runRows(dir, ""); err != nil {
		t.Fatalf("runRows: %v", err)
	}
	// Extra files land after rows mode has run, because a
	// hand-written shape names the row structs rows mode declares and
	// the bridge has to compile the package to write them.
	for i := 0; i+1 < len(extra); i += 2 {
		if err := os.WriteFile(filepath.Join(dir, extra[i]), []byte(extra[i+1]), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// generateRels runs the real thing and returns the generated source.
func generateRels(t *testing.T, dir string, shapes ...string) string {
	t.Helper()
	if err := runRels(dir, shapes, ""); err != nil {
		t.Fatalf("runRels %v: %v", shapes, err)
	}
	src, err := os.ReadFile(filepath.Join(dir, "fixture_drops_rels.go"))
	if err != nil {
		t.Fatal(err)
	}
	return string(src)
}

// hasField reports whether src declares the given field, ignoring the
// alignment gofmt adds between a name, its type and its tag.
func hasField(src, want string) bool {
	return strings.Contains(strings.Join(strings.Fields(src), " "), strings.Join(strings.Fields(want), " "))
}

// relsError runs the generator expecting a refusal.
func relsError(t *testing.T, dir string, shapes ...string) string {
	t.Helper()
	err := runRels(dir, shapes, "")
	if err == nil {
		t.Fatalf("runRels %v succeeded, want an error", shapes)
	}
	return err.Error()
}

// The cardinalities, which is the decision the whole mode exists to
// make. A slice where the loader fills a slice, a pointer where it
// fills at most one row, an interface where the type varies.
func TestRelsShapeFollowsTheLoader(t *testing.T) {
	dir := relsFixture(t)
	src := generateRels(t, dir,
		"users:posts,profile,manager,remarks",
		"posts:tags",
		"comments:commentable")
	for _, want := range []string{
		"Posts []PostsRow",
		"Profile *ProfilesRow",
		"Manager *UsersRow",
		"Remarks []CommentsRow",
		"Tags []TagsRow",
		"Commentable any",
	} {
		if !hasField(src, want) {
			t.Errorf("generated file has no %q\n%s", want, src)
		}
	}
	mustBuild(t, dir)
}

// The tag the loader actually looks for, plus the one that keeps the
// field out of the scanner's way. Without drop:"-" the relation field
// competes for a column name at depth 0 and outranks the real column
// promoted from the embedded row struct.
func TestRelsFieldCarriesBothTags(t *testing.T) {
	dir := relsFixture(t)
	src := generateRels(t, dir, "users:posts")
	if want := "Posts []PostsRow `drop:\"-\" dropRel:\"posts\"`"; !hasField(src, want) {
		t.Errorf("generated file has no %s\n%s", want, src)
	}
}

// A nested path produces a nested struct, and the leaf is the row
// struct rows mode already emits rather than a third copy of it.
func TestRelsNestsAsDeepAsThePathGoes(t *testing.T) {
	dir := relsFixture(t)
	src := generateRels(t, dir, "users:posts.comments")
	for _, want := range []string{
		"type UsersWithPostsComments struct {",
		"Posts []PostsWithComments",
		"type PostsWithComments struct {",
		"Comments []CommentsRow",
	} {
		if !hasField(src, want) {
			t.Errorf("generated file has no %q\n%s", want, src)
		}
	}
	mustBuild(t, dir)
}

// A self-referential relation nests without the two structs
// colliding: the name carries the whole path, not just the edge.
func TestRelsNamesASelfReferentialChainApart(t *testing.T) {
	dir := relsFixture(t)
	src := generateRels(t, dir, "users:manager.manager")
	for _, want := range []string{
		"type UsersWithManagerManager struct {",
		"Manager *UsersWithManager",
		"type UsersWithManager struct {",
		"Manager *UsersRow",
	} {
		if !hasField(src, want) {
			t.Errorf("generated file has no %q\n%s", want, src)
		}
	}
	mustBuild(t, dir)
}

// Two runs over one input produce one file, byte for byte.
func TestRelsGenerationIsByteStable(t *testing.T) {
	dir := relsFixture(t)
	shapes := []string{"users:posts.comments,profile", "posts:tags,author"}
	first := generateRels(t, dir, shapes...)
	second := generateRels(t, dir, shapes...)
	if first != second {
		t.Errorf("two runs disagree\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// And the order the shapes were asked for is not part of the input:
// the same set produces the same file however it was spelled.
func TestRelsOutputDoesNotDependOnArgumentOrder(t *testing.T) {
	dir := relsFixture(t)
	forward := generateRels(t, dir, "users:profile,posts.comments", "posts:tags")
	backward := generateRels(t, dir, "posts:tags", "users:posts.comments,profile")
	if forward != backward {
		t.Errorf("argument order changed the output\n--- forward ---\n%s\n--- backward ---\n%s", forward, backward)
	}
}

// A name the package already declares is left alone, and the header
// says which name and where. Two declarations of one name in one
// package do not compile.
func TestRelsSkipsANameThePackageAlreadyDeclares(t *testing.T) {
	hand := `package fixture

// UsersWithPosts is hand-written, and this file owns the name.
type UsersWithPosts struct {
	UsersRow
	Posts []PostsRow ` + "`drop:\"-\" dropRel:\"posts\"`" + `
}
`
	dir := relsFixture(t, "hand.go", hand)
	src := generateRels(t, dir, "users:posts")
	if strings.Contains(src, "type UsersWithPosts struct") {
		t.Errorf("generated a second declaration of a name the package has\n%s", src)
	}
	if !strings.Contains(src, "UsersWithPosts (hand.go)") {
		t.Errorf("header does not say what was skipped or where\n%s", src)
	}
	mustBuild(t, dir)
}

// The row structs are the other half of the shape, and a missing one
// is worth a sentence rather than a compile error naming a type the
// reader never asked for.
func TestRelsRequiresTheRowStructs(t *testing.T) {
	dir := rowsFixture(t, map[string]string{"schema.go": relsFixtureSchema})
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	msg := relsError(t, dir, "users:posts")
	if !strings.Contains(msg, "PostsRow") || !strings.Contains(msg, "dropsgen -rows") {
		t.Errorf("error does not name the missing struct or the mode that writes it: %s", msg)
	}
}

// A misspelled relation is the mistake this generator exists to
// prevent, so it cannot be one the generator makes silently.
func TestRelsRefusesAnUnknownRelation(t *testing.T) {
	dir := relsFixture(t)
	msg := relsError(t, dir, "users:postz")
	if !strings.Contains(msg, `no relation "postz"`) || !strings.Contains(msg, "posts") {
		t.Errorf("error does not name the typo or list the alternatives: %s", msg)
	}
}

// pg.validateRelTree refuses a nested With() through a MorphTo, so a
// struct for one could never be filled.
func TestRelsRefusesAPathThroughAMorphTo(t *testing.T) {
	dir := relsFixture(t)
	msg := relsError(t, dir, "comments:commentable.posts")
	if !strings.Contains(msg, "MorphTo") {
		t.Errorf("error does not say why: %s", msg)
	}
}

// Two shapes that derive one name with different fields cannot both
// be written, and the way out is to name one.
func TestRelsRefusesTwoShapesThatBecomeOneName(t *testing.T) {
	dir := relsFixture(t)
	msg := relsError(t, dir, "Same=users:posts", "Same=users:profile")
	if !strings.Contains(msg, "Same") || !strings.Contains(msg, "name one of them") {
		t.Errorf("error does not name the collision or the way out: %s", msg)
	}
	mustSuggestARunnableShape(t, dir, msg)
}

// Two shapes can collide without anybody naming one: the derived name
// erases where a path was split, so "comments.posts" and
// "comments,posts" arrive at one name carrying different fields. The
// generator has to refuse rather than pick, and say which name.
func TestRelsRefusesTwoDerivedNamesThatCollide(t *testing.T) {
	dir := relsFixture(t, "extra.go", `package fixture

import "github.com/bernardoforcillo/drops/pg"

// A second edge named "tags", so that "posts.tags" and "posts,tags"
// are two different trees whose derived names are the same string.
var _ = pg.NewRelations(Users).HasMany("tags", Tags, UserID, TagID)
`)
	msg := relsError(t, dir, "users:posts.tags", "users:posts,tags")
	if !strings.Contains(msg, "UsersWithPostsTags") {
		t.Errorf("error does not name the struct both shapes generate: %s", msg)
	}
	mustSuggestARunnableShape(t, dir, msg)
}

// The way out a collision message offers is a command line, so it has
// to be one: a -shape the generator would itself accept. Quoting the
// shape into it — which is what it takes to word the first half of
// the sentence — puts a quote inside the table name and hands the
// reader an argument that cannot parse.
func mustSuggestARunnableShape(t *testing.T, dir, msg string) {
	t.Helper()
	const marker = "-shape 'SomeName="
	i := strings.Index(msg, marker)
	if i < 0 {
		t.Fatalf("error offers no -shape to run: %s", msg)
	}
	rest := msg[i+len("-shape '"):]
	j := strings.IndexByte(rest, '\'')
	if j < 0 {
		t.Fatalf("the suggested -shape is not closed: %s", msg)
	}
	suggested := rest[:j]
	if _, err := parseShapeSpec(suggested); err != nil {
		t.Fatalf("the suggested -shape %q does not parse: %v", suggested, err)
	}
	// Parsing is not enough: a quote swallowed into the table name
	// parses fine and then names a table no schema registers.
	if err := runRels(dir, []string{suggested}, ""); err != nil {
		t.Fatalf("the suggested -shape %q was refused by the generator: %v", suggested, err)
	}
}

// The same node reached from two shapes is one struct, not a refusal:
// the body is a function of the table and the paths beneath it.
func TestRelsSharesANodeTwoShapesReach(t *testing.T) {
	dir := relsFixture(t)
	src := generateRels(t, dir, "users:posts.comments", "posts:comments")
	if n := strings.Count(src, "type PostsWithComments struct"); n != 1 {
		t.Errorf("PostsWithComments declared %d times, want 1\n%s", n, src)
	}
	mustBuild(t, dir)
}

// A shape can be named outright, for the case where the derived name
// is not the one a codebase wants.
func TestRelsTakesAnExplicitName(t *testing.T) {
	dir := relsFixture(t)
	src := generateRels(t, dir, "AuthorWithBooks=users:posts")
	if !strings.Contains(src, "type AuthorWithBooks struct") {
		t.Errorf("explicit name ignored\n%s", src)
	}
	if strings.Contains(src, "type UsersWithPosts struct") {
		t.Errorf("derived name emitted as well\n%s", src)
	}
	mustBuild(t, dir)
}

// The doc comment has to say the thing a hand-written struct gets
// wrong: nil is not empty, and the pointer is there because absence
// is a state a zero struct cannot express.
func TestRelsDocumentsWhatANilFieldMeans(t *testing.T) {
	dir := relsFixture(t)
	src := generateRels(t, dir, "users:posts,profile")
	if !strings.Contains(src, "A nil slice is not an empty relation") {
		t.Errorf("nothing says what a nil slice means\n%s", src)
	}
	if !strings.Contains(src, "row of zeros") {
		t.Errorf("nothing says why the single-row relation is a pointer\n%s", src)
	}
	if !strings.Contains(src, `db.Find(Users).With("posts", "profile")`) {
		t.Errorf("doc does not show the query that fills it\n%s", src)
	}
}

// With no shape to generate there is nothing to write, and the schema
// is the one place that knows what could have been asked for.
func TestRelsWithNoShapeListsWhatCouldBeAsked(t *testing.T) {
	dir := relsFixture(t)
	msg := relsError(t, dir)
	if !strings.Contains(msg, "-shape 'users:") || !strings.Contains(msg, "profile") {
		t.Errorf("error does not list the declared relations: %s", msg)
	}
}

// The generated file belongs in the schema package, for the same
// reason rows mode's does.
func TestRelsRefusesAnOutputPathOutsideThePackage(t *testing.T) {
	if err := runRels(".", []string{"users:posts"}, "sub/dir/out.go"); err == nil {
		t.Fatal("an -o with a directory in it was accepted")
	}
}

// -o names the file, and the name is part of a contract rather than a
// preference: rows mode moves *_drops_rels.go aside while it compiles
// the package, and a shape file called anything else is one it will
// not know to move. That leaves the package unable to regenerate its
// row structs at all — which is the very trap the stashing exists to
// close, reopened by an argument nobody warned about.
func TestRelsRefusesAnOutputNameRowsModeWillNotKnowToMove(t *testing.T) {
	dir := relsFixture(t)
	err := runRels(dir, []string{"users:posts"}, "shapes.go")
	if err == nil {
		t.Fatal("-o shapes.go was accepted; -rows can no longer run on this package")
	}
	if !strings.Contains(err.Error(), relsFileSuffix) {
		t.Errorf("error does not say what the name has to end in: %v", err)
	}
	// And the name the mode writes by default is of course accepted.
	if err := runRels(dir, []string{"users:posts"}, "extra"+relsFileSuffix); err != nil {
		t.Fatalf("a correctly suffixed -o was refused: %v", err)
	}
}

// A malformed -shape is reported as itself.
func TestRelsSpecParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"users", "names no relations"},
		{"users:", "names no relations"},
		{":posts", "names no table"},
		{"users:posts,", "empty relation path"},
		{"users:posts..comments", "is not a relation path"},
		{"type=users:posts", "not a Go type name"},
	} {
		if _, err := parseShapeSpec(tc.in); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseShapeSpec(%q) = %v, want an error mentioning %q", tc.in, err, tc.want)
		}
	}
	// Paths are sorted and deduplicated, which is what makes the
	// output independent of how the argument was spelled.
	spec, err := parseShapeSpec("Shape=app.users:profile,posts,profile")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "Shape" || spec.Table != "app.users" || strings.Join(spec.Paths, ",") != "posts,profile" {
		t.Errorf("parsed %+v", spec)
	}
}

// A package may hold more than one shape file, because -o names the
// output. The guard that keeps the generator from redeclaring a name
// the package already has must still see those names: every shape
// file is moved aside so the bridge can compile the package, and if
// they are still out of the way when the declarations are read, two
// shape files asking for one shape each write the same struct twice
// and the package stops compiling.
func TestRelsSeesTheNamesAnotherShapeFileDeclares(t *testing.T) {
	dir := relsFixture(t)
	if err := runRels(dir, []string{"users:posts"}, "a_drops_rels.go"); err != nil {
		t.Fatalf("first shape file: %v", err)
	}
	if err := runRels(dir, []string{"users:posts", "posts:comments"}, "b_drops_rels.go"); err != nil {
		t.Fatalf("second shape file: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "b_drops_rels.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(second), "type UsersWithPosts struct") {
		t.Errorf("the second file redeclares a name the first one owns\n%s", second)
	}
	if !strings.Contains(string(second), "UsersWithPosts (a_drops_rels.go)") {
		t.Errorf("header does not say which file already owns the name\n%s", second)
	}
	// The shape the first file does not have is still written.
	if !strings.Contains(string(second), "type PostsWithComments struct") {
		t.Errorf("the second file skipped a name nobody had\n%s", second)
	}
	mustBuild(t, dir)
}

// Rows mode has to run with a shape file already in the package, and
// that file names the very structs rows mode is about to rewrite. It
// only compiles while they exist, so it goes out of the way while the
// bridge runs — otherwise regenerating the row structs is impossible
// from the moment the first shape is generated.
func TestRowsRunsWithAShapeFileInThePackage(t *testing.T) {
	dir := relsFixture(t)
	generateRels(t, dir, "users:posts.comments")
	if err := runRows(dir, ""); err != nil {
		t.Fatalf("runRows with a shape file present: %v", err)
	}
	// And the shape file is still there afterwards: regenerating it
	// is the other mode's job, not this one's.
	if _, err := os.Stat(filepath.Join(dir, "fixture_drops_rels.go")); err != nil {
		t.Fatalf("rows mode did not put the shape file back: %v", err)
	}
	mustBuild(t, dir)
}

// The same collision, split across two runs — which is where the
// derived name stops being a name and becomes a hazard.
//
// Within one run the generator sees both trees and refuses. Across
// two runs it sees only the second, finds the name already declared,
// and stands aside as though a hand-written struct had claimed it.
// The caller asked for a shape, got no error, and the name now
// resolves to a struct with different fields — a query written
// against it loads the wrong relations, or refuses to run at all
// under StrictLoading.
//
// So the package's own declarations are the manifest: a name already
// declared has to carry the fields this run would have written, or it
// is a collision and gets the same refusal the in-run one does.
func TestRelsRefusesANameAnotherFileDeclaresWithADifferentTree(t *testing.T) {
	dir := relsFixture(t, "extra.go", `package fixture

import "github.com/bernardoforcillo/drops/pg"

// A second edge named "tags", so that "posts.tags" and "posts,tags"
// are two different trees whose derived names are the same string.
var _ = pg.NewRelations(Users).HasMany("tags", Tags, UserID, TagID)
`)
	if err := runRels(dir, []string{"users:posts.tags"}, "a"+relsFileSuffix); err != nil {
		t.Fatalf("first shape file: %v", err)
	}
	err := runRels(dir, []string{"users:posts,tags"}, "b"+relsFileSuffix)
	if err == nil {
		t.Fatal("the second file was written against a name the first one owns, with different fields")
	}
	msg := err.Error()
	if !strings.Contains(msg, "UsersWithPostsTags") {
		t.Errorf("error does not name the struct both runs derive: %s", msg)
	}
	if !strings.Contains(msg, "a"+relsFileSuffix) {
		t.Errorf("error does not say which file already owns the name: %s", msg)
	}
	mustSuggestARunnableShape(t, dir, msg)
	// And nothing was written: a run that cannot say what it means is
	// not one that leaves half a file behind.
	if _, err := os.Stat(filepath.Join(dir, "b"+relsFileSuffix)); !os.IsNotExist(err) {
		t.Errorf("the refused run left a file behind: %v", err)
	}
	mustBuild(t, dir)
}

// The other half, and the one that must not become an error: a name
// another shape file declares for the *same* tree is the struct this
// run would have written, so it is still skipped rather than
// redeclared. Two shape files that overlap are the ordinary case.
func TestRelsStillSkipsANameAnotherFileDeclaresForTheSameTree(t *testing.T) {
	dir := relsFixture(t)
	if err := runRels(dir, []string{"users:posts"}, "a"+relsFileSuffix); err != nil {
		t.Fatalf("first shape file: %v", err)
	}
	if err := runRels(dir, []string{"users:posts", "posts:comments"}, "b"+relsFileSuffix); err != nil {
		t.Fatalf("a second file asking for the same shape was refused: %v", err)
	}
	mustBuild(t, dir)
}

// The skip rule is not "somebody got there first", it is "the struct
// that is there is the struct this would have written". A
// hand-written one that is not — a slice of the wrong row, a value
// where the loader fills a pointer — is the drift the generator
// exists to remove, and standing aside for it hides exactly that.
func TestRelsRefusesAHandWrittenStructThatIsNotTheShape(t *testing.T) {
	hand := `package fixture

// UsersWithPosts is hand-written and wrong: the loader fills a slice
// for a HasMany and refuses anything else.
type UsersWithPosts struct {
	UsersRow
	Posts *PostsRow ` + "`drop:\"-\" dropRel:\"posts\"`" + `
}
`
	dir := relsFixture(t, "hand.go", hand)
	msg := relsError(t, dir, "users:posts")
	if !strings.Contains(msg, "UsersWithPosts") || !strings.Contains(msg, "hand.go") {
		t.Errorf("error does not name the struct or the file that declares it: %s", msg)
	}
	mustSuggestARunnableShape(t, dir, msg)
}

// The nested half of the same refusal, which is the branch the
// message was reworded for.
//
// A shape's own struct can be renamed on the command line; a nested
// one cannot, because it is named for its table and the paths beneath
// it and that is exactly what makes two shapes reaching one node
// share one struct. So the refusal has to say something else, and a
// message that suggested -shape 'SomeName=…' here would be advice
// that does not work.
func TestRelsRefusesANestedNameTheDeclarationDoesNotMatch(t *testing.T) {
	hand := `package fixture

// PostsWithComments is what "users:posts.comments" nests, and this is
// not it: the loader fills a slice for a HasMany.
type PostsWithComments struct {
	PostsRow
	Comments *CommentsRow ` + "`drop:\"-\" dropRel:\"comments\"`" + `
}
`
	dir := relsFixture(t, "hand.go", hand)
	msg := relsError(t, dir, "users:posts.comments")
	if !strings.Contains(msg, "PostsWithComments") || !strings.Contains(msg, "hand.go") {
		t.Errorf("error does not name the nested struct or the file that declares it: %s", msg)
	}
	if strings.Contains(msg, "-shape 'SomeName=") {
		t.Errorf("the refusal offers a rename a nested shape cannot take: %s", msg)
	}
}

// And the nested collision split across two runs, which is the cross-run
// hazard one level down from where it was first caught.
//
// The root names are kept apart on purpose — the second run names its
// own struct outright — so the only thing left to disagree about is
// the struct both runs derive for the node in the middle. That name
// is not the caller's to change, so the way out is not a rename.
func TestRelsRefusesANestedNameAnotherFileDeclaresWithADifferentTree(t *testing.T) {
	dir := relsFixture(t, "extra.go", `package fixture

import "github.com/bernardoforcillo/drops/pg"

// A second edge named "tags" on users, so that a users node reached
// with "posts.tags" and one reached with "posts,tags" are two trees
// under one derived name.
var _ = pg.NewRelations(Users).HasMany("tags", Tags, UserID, TagID)
`)
	if err := runRels(dir, []string{"posts:author.posts.tags"}, "a"+relsFileSuffix); err != nil {
		t.Fatalf("first shape file: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "a"+relsFileSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), "type UsersWithPostsTags struct") {
		t.Fatalf("the first run did not nest the struct this test is about\n%s", first)
	}
	// The root is named outright so the two runs cannot collide there:
	// what is left is the nested node, which no argument can rename.
	err = runRels(dir, []string{"Other=posts:author.posts,author.tags"}, "b"+relsFileSuffix)
	if err == nil {
		t.Fatal("the second file redeclared a nested name the first one owns, with different fields")
	}
	msg := err.Error()
	if !strings.Contains(msg, "UsersWithPostsTags") || !strings.Contains(msg, "a"+relsFileSuffix) {
		t.Errorf("error does not name the nested struct or the file that owns it: %s", msg)
	}
	if _, err := os.Stat(filepath.Join(dir, "b"+relsFileSuffix)); !os.IsNotExist(err) {
		t.Errorf("the refused run left a file behind: %v", err)
	}
	mustBuild(t, dir)
}

// A name in the way is not always a struct.
//
// declaredNames records every top-level declaration, so the thing
// holding the name may be a func, a var or an alias — and the refusal
// has to say that rather than send its author looking for fields the
// declaration does not have.
func TestRelsSaysSoWhenTheNameIsNotAStructAtAll(t *testing.T) {
	dir := relsFixture(t, "hand.go", `package fixture

// UsersWithPosts is a func here, and the shape wants the name.
func UsersWithPosts() {}
`)
	msg := relsError(t, dir, "users:posts")
	if !strings.Contains(msg, "not a struct type") {
		t.Errorf("the refusal does not say what is actually in the way: %s", msg)
	}
	if strings.Contains(msg, "with different fields") {
		t.Errorf("the refusal credits a func with fields: %s", msg)
	}
	mustSuggestARunnableShape(t, dir, msg)
}
