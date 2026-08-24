package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

type strictUser struct {
	ID    int64        `drop:"id"`
	Name  string       `drop:"name"`
	Posts []strictPost `dropRel:"posts"`
}

type strictPost struct {
	ID       int64           `drop:"id"`
	UserID   int64           `drop:"userId"`
	Title    string          `drop:"title"`
	Comments []strictComment `dropRel:"comments"`
}

type strictComment struct {
	ID     int64  `drop:"id"`
	PostID int64  `drop:"postId"`
	Body   string `drop:"body"`
}

// strictUserNoRel is the projection shape: no relation field at all,
// so nothing can read one and nothing needs guarding.
type strictUserNoRel struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func mkStrictSchema() (users, posts, comments *pg.Table) {
	users = pg.NewTable("users")
	uID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	posts = pg.NewTable("posts")
	pID := pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pUID := pg.Add(posts, pg.BigInt("userId").NotNull())
	pg.Add(posts, pg.Text("title").NotNull())

	comments = pg.NewTable("comments")
	pg.Add(comments, pg.BigSerial("id").PrimaryKey())
	cPID := pg.Add(comments, pg.BigInt("postId").NotNull())
	pg.Add(comments, pg.Text("body").NotNull())

	pg.NewRelations(users).HasMany("posts", posts, uID, pUID)
	pg.NewRelations(posts).HasMany("comments", comments, pID, cPID)
	return users, posts, comments
}

func strictDriver() *fakeDriver {
	return &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		if strings.Contains(sql, `FROM "users"`) {
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}},
			}, nil
		}
		if strings.Contains(sql, `FROM "posts"`) {
			return &fakeRows{
				cols: []string{"id", "userId", "title"},
				data: [][]any{{int64(10), int64(1), "Hello"}},
			}, nil
		}
		return &fakeRows{cols: []string{"id", "postId", "body"}}, nil
	}}
}

// The headline case. Without strict loading the query happily returns
// users whose Posts is nil, which reads as "no posts" and is not.
func TestStrictLoadingRefusesUnloadedRelation(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got []strictUser
	err := db.Find(users).All(context.Background(), &got)
	if !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("want ErrRelationNotLoaded, got %v", err)
	}
	// The failure has to name the relation and the call that fixes it,
	// or it is just another opaque error.
	for _, want := range []string{`"posts"`, "strictUser", `.Load(users.Rel("posts"))`, `.NoLoad(`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %s: %v", want, err)
		}
	}
}

// It must refuse before the SELECT runs: a query that cannot produce a
// trustworthy answer should not cost a round trip either.
func TestStrictLoadingRefusesBeforeQuerying(t *testing.T) {
	users, _, _ := mkStrictSchema()
	fd := strictDriver()
	db := pg.New(fd).StrictLoading()

	var got []strictUser
	_ = db.Find(users).All(context.Background(), &got)
	if len(fd.queries) != 0 {
		t.Errorf("strict loading must refuse before executing: %v", fd.queries)
	}
}

func TestStrictLoadingAcceptsALoadedRelation(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got []strictUser
	if err := db.Find(users).Load(users.Rel("posts")).
		LoadRel(users.Rel("posts"), func(p *pg.RelConfig) {
			p.Load(mustRel(t, "comments"))
		}).All(context.Background(), &got); err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(got) != 1 || len(got[0].Posts) != 1 {
		t.Fatalf("expected one user with one post, got %+v", got)
	}
}

// mustRel reaches the comments relation off the posts table the
// schema builder returned, keeping the test's intent legible.
func mustRel(t *testing.T, name string) *pg.Relation {
	t.Helper()
	_, posts, _ := mkStrictSchema()
	return posts.Rel(name)
}

// A relation loaded at the root but not at the next level down is the
// same silent nil one level deeper, so the check has to descend.
func TestStrictLoadingDescendsIntoLoadedRelations(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got []strictUser
	err := db.Find(users).With("posts").All(context.Background(), &got)
	if !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("want ErrRelationNotLoaded for posts.comments, got %v", err)
	}
	if !strings.Contains(err.Error(), `"posts.comments"`) {
		t.Errorf("error must name the nested path: %v", err)
	}
}

// The waiver: a query that says out loud it will not read the relation
// is allowed through, by handle and by name alike.
func TestNoLoadWaivesTheCheck(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got []strictUser
	if err := db.Find(users).NoLoad(users.Rel("posts")).All(context.Background(), &got); err != nil {
		t.Fatalf("NoLoad must satisfy the check: %v", err)
	}
	got = nil
	if err := db.Find(users).Without("posts").All(context.Background(), &got); err != nil {
		t.Fatalf("Without must satisfy the check: %v", err)
	}
}

// A nested waiver: load the posts, decline their comments.
func TestNestedWaiverByPathAndByHandle(t *testing.T) {
	users, posts, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got []strictUser
	if err := db.Find(users).With("posts").Without("posts.comments").
		All(context.Background(), &got); err != nil {
		t.Fatalf("Without(path) must satisfy the check: %v", err)
	}
	got = nil
	err := db.Find(users).LoadRel(users.Rel("posts"), func(p *pg.RelConfig) {
		p.NoLoad(posts.Rel("comments"))
	}).All(context.Background(), &got)
	if err != nil {
		t.Fatalf("RelConfig.NoLoad must satisfy the check: %v", err)
	}
}

// A destination that has no relation field cannot hold a misleading
// nil, so it must not be refused.
func TestStrictLoadingIgnoresProjectionStructs(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got []strictUserNoRel
	if err := db.Find(users).All(context.Background(), &got); err != nil {
		t.Fatalf("a struct with no relation field must pass: %v", err)
	}
}

// Off by default: the check is a development-mode tool and must not
// change what an existing program does.
func TestWithoutStrictModeNothingIsRefused(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver())

	var got []strictUser
	if err := db.Find(users).All(context.Background(), &got); err != nil {
		t.Fatalf("strict loading must be opt-in: %v", err)
	}
	if got[0].Posts != nil {
		t.Errorf("unloaded relation should still be nil: %+v", got[0])
	}
}

// One query can opt in without the whole DB doing so.
func TestPerQueryStrict(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver())

	var got []strictUser
	err := db.Find(users).Strict().All(context.Background(), &got)
	if !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("want ErrRelationNotLoaded, got %v", err)
	}
}

// Find.One routes through All, so it inherits the check.
func TestStrictLoadingCoversFindOne(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got strictUser
	if err := db.Find(users).One(context.Background(), &got); !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("want ErrRelationNotLoaded, got %v", err)
	}
}

// The entity path has its own fast scanner that never reaches
// FindBuilder.All — the check has to be stated there too or it applies
// to some queries and not others.
func TestStrictLoadingCoversEntityQuery(t *testing.T) {
	users, _, _ := mkStrictSchema()
	ent := pg.NewEntity[strictUser](users)
	db := pg.New(strictDriver()).StrictLoading()

	if _, err := ent.Query(db).All(context.Background()); !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("EntityQuery.All: want ErrRelationNotLoaded, got %v", err)
	}
	if _, err := ent.Query(db).One(context.Background()); !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("EntityQuery.One: want ErrRelationNotLoaded, got %v", err)
	}
	if _, err := ent.Query(db).NoLoad(users.Rel("posts")).All(context.Background()); err != nil {
		t.Fatalf("NoLoad must satisfy the check: %v", err)
	}
}

// Entity.Get takes a primary key and cannot load a relation at all, so
// the check would be an unconditional refusal rather than a signal
// about this query. It is exempt, and stays exempt on both the
// reflection and the fast-scan paths.
func TestStrictLoadingLeavesEntityGetAlone(t *testing.T) {
	users, _, _ := mkStrictSchema()
	ent := pg.NewEntity[strictUser](users)
	db := pg.New(strictDriver()).StrictLoading()

	if _, err := ent.Get(db, context.Background(), int64(1)); err != nil {
		t.Fatalf("Get must not be refused: %v", err)
	}
}

// A waiver is a statement about what this query will not read, not a
// load instruction: waiving a nested relation must not quietly fetch
// the level above it, and the level above is still owed.
func TestNestedWaiverLoadsNothingByItself(t *testing.T) {
	users, _, _ := mkStrictSchema()
	fd := strictDriver()
	db := pg.New(fd).StrictLoading()

	var got []strictUser
	err := db.Find(users).Without("posts.comments").All(context.Background(), &got)
	if !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("want ErrRelationNotLoaded for posts, got %v", err)
	}
	if !strings.Contains(err.Error(), `"posts"`) || strings.Contains(err.Error(), "posts.comments") {
		t.Errorf("the unloaded level is posts, not posts.comments: %v", err)
	}
	if len(fd.queries) != 0 {
		t.Errorf("a waiver must not issue a query: %v", fd.queries)
	}
}

// And it matches wherever it appears in the chain, since it is
// resolved when the check runs rather than when it is written.
func TestWaiverIsOrderIndependent(t *testing.T) {
	users, _, _ := mkStrictSchema()
	db := pg.New(strictDriver()).StrictLoading()

	var got []strictUser
	if err := db.Find(users).Without("posts.comments").With("posts").
		All(context.Background(), &got); err != nil {
		t.Fatalf("waiver written before the load must still count: %v", err)
	}
}
