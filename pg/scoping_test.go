package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// The proof that P0-1 and P0-2 are closed.
//
// Every assertion in this file is on rendered SQL, and that is the
// point rather than a stylistic preference: the bug being fixed is a
// missing WHERE clause, and a round-trip test against a fixture that
// only ever holds one tenant's rows passes with the clause absent. It
// passed for months. What follows asserts on the statements the driver
// was handed, for every relation kind, every per-edge option and every
// executor — because the failure mode is not "the feature does not
// work" but "the feature stops applying to whichever path was added
// last".

const (
	scopeTenant  = int64(7)
	scopeSubject = int64(42)
	// tenantPred and softDeletePred are the fragments a scoped
	// statement has to carry. Both are spelled without the table
	// qualification so one assertion covers every table in the fixture.
	tenantPred     = `"tenantId" = $`
	softDeletePred = `"deletedAt" IS NULL`
)

func scopeCtx() context.Context {
	return pg.WithSubject(pg.WithTenant(context.Background(), scopeTenant), scopeSubject)
}

// ----------------------------------------------------------------------
// Fixture
// ----------------------------------------------------------------------

// scopeSchema is a graph reaching all six relation kinds, where every
// table carries both kinds of automatic predicate: a DefaultFilter (the
// soft-delete guard, resolved at render time) and a ContextFilter (the
// tenant axis, resolved by the executor). Loading any edge of it must
// carry both onto the child query.
type scopeSchema struct {
	users    *pg.Table
	posts    *pg.Table
	profiles *pg.Table
	tags     *pg.Table
	userTags *pg.Table
	comments *pg.Table
}

func newScopeSchema() *scopeSchema {
	scoped := func(name string, cols func(*pg.Table)) *pg.Table {
		t := pg.NewTable(name)
		pg.Add(t, pg.BigSerial("id").PrimaryKey())
		tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
		cols(t)
		pg.ApplyMixins(t, &pg.SoftDeleteMixin{})
		t.ContextFilter(pg.TenantFilter(tenant))
		return t
	}

	s := &scopeSchema{}
	s.users = scoped("users", func(t *pg.Table) {
		pg.Add(t, pg.Text("name").NotNull())
	})
	s.posts = scoped("posts", func(t *pg.Table) {
		pg.Add(t, pg.BigInt("userId").NotNull())
		pg.Add(t, pg.Text("title").NotNull())
	})
	s.profiles = scoped("profiles", func(t *pg.Table) {
		pg.Add(t, pg.BigInt("userId").NotNull())
		pg.Add(t, pg.Text("bio").NotNull())
	})
	s.tags = scoped("tags", func(t *pg.Table) {
		pg.Add(t, pg.Text("label").NotNull())
	})
	s.comments = scoped("comments", func(t *pg.Table) {
		pg.Add(t, pg.Text("commentableType").NotNull())
		pg.Add(t, pg.BigInt("commentableId").NotNull())
		pg.Add(t, pg.Text("body").NotNull())
	})
	// The junction is scoped too: it is the table the many-to-many
	// traversal reads first, so a tenant that reached the targets but
	// not the junction would still be told which rows exist.
	s.userTags = pg.NewTable("user_tags")
	utUser := pg.Add(s.userTags, pg.BigInt("userId").NotNull())
	utTag := pg.Add(s.userTags, pg.BigInt("tagId").NotNull())
	utTenant := pg.Add(s.userTags, pg.BigInt("tenantId").NotNull())
	pg.ApplyMixins(s.userTags, &pg.SoftDeleteMixin{})
	s.userTags.ContextFilter(pg.TenantFilter(utTenant))

	morphs := pg.NewMorphMap()
	pg.RegisterMorph[scopeUser](morphs, "users", s.users)

	pg.NewRelations(s.users).
		HasMany("posts", s.posts, s.users.Col("id"), s.posts.Col("userId")).
		HasOne("profile", s.profiles, s.users.Col("id"), s.profiles.Col("userId")).
		ManyToMany("tags", s.tags, s.userTags, utUser, utTag, s.users.Col("id"), s.tags.Col("id")).
		MorphMany("comments", s.comments,
			s.comments.Col("commentableType"), s.comments.Col("commentableId"),
			s.users.Col("id"), "users")
	pg.NewRelations(s.posts).
		BelongsTo("author", s.users, s.posts.Col("userId"), s.users.Col("id"))
	pg.NewRelations(s.comments).
		MorphTo("commentable", s.comments.Col("commentableType"), s.comments.Col("commentableId"), morphs)
	return s
}

type scopeUser struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Name     string `drop:"name"`

	Posts    []scopePost    `dropRel:"posts"`
	Profile  *scopeProfile  `dropRel:"profile"`
	Tags     []scopeTag     `dropRel:"tags"`
	Comments []scopeComment `dropRel:"comments"`
}

type scopePost struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	UserID   int64  `drop:"userId"`
	Title    string `drop:"title"`
}

// scopePostWithAuthor is the inverse direction. It is a second type
// rather than a field on scopePost because a struct that reaches itself
// through a relation field has no terminating field walk.
type scopePostWithAuthor struct {
	ID       int64      `drop:"id"`
	TenantID int64      `drop:"tenantId"`
	UserID   int64      `drop:"userId"`
	Title    string     `drop:"title"`
	Author   *scopeUser `dropRel:"author"`
}

type scopeProfile struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	UserID   int64  `drop:"userId"`
	Bio      string `drop:"bio"`
}

type scopeTag struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Label    string `drop:"label"`
}

type scopeComment struct {
	ID              int64  `drop:"id"`
	TenantID        int64  `drop:"tenantId"`
	CommentableType string `drop:"commentableType"`
	CommentableID   int64  `drop:"commentableId"`
	Body            string `drop:"body"`

	Commentable any `dropRel:"commentable"`
}

// Canned result sets, one per query an edge issues.
var (
	scopeUserCols    = []string{"id", "tenantId", "name"}
	scopeUserRow     = []any{int64(1), scopeTenant, "Ada"}
	scopePostCols    = []string{"id", "tenantId", "userId", "title"}
	scopePostRow     = []any{int64(10), scopeTenant, int64(1), "post"}
	scopeProfileCols = []string{"id", "tenantId", "userId", "bio"}
	scopeProfileRow  = []any{int64(20), scopeTenant, int64(1), "bio"}
	scopeTagCols     = []string{"id", "tenantId", "label"}
	scopeTagRow      = []any{int64(30), scopeTenant, "go"}
	scopeJoinCols    = []string{"userId", "tagId"}
	scopeJoinRow     = []any{int64(1), int64(30)}
	scopeCommentCols = []string{"id", "tenantId", "commentableType", "commentableId", "body"}
	scopeCommentRow  = []any{int64(40), scopeTenant, "users", int64(1), "body"}
)

// ----------------------------------------------------------------------
// The matrix: relation kind × per-edge option
// ----------------------------------------------------------------------

// TestScopingReachesEveryRelationEdge is the end-of-phase proof for
// P0-1 and P0-2. For every relation kind, under every option a
// RelConfig exposes, every statement the load issues has to carry the
// target table's tenant axis and its soft-delete guard — including the
// junction query of a many-to-many, and including the ROW_NUMBER()
// rewrite a Limit switches on, which used to hand-write its own WHERE
// clause and thereby resurrect soft-deleted rows.
func TestScopingReachesEveryRelationEdge(t *testing.T) {
	kinds := []struct {
		name string
		// root is the table the parent query runs against, and dest
		// the slice its rows scan into.
		root  func(*scopeSchema) *pg.Table
		dest  func() any
		child func(*scopeSchema) *pg.Table
		rel   string
		// sets are the canned answers, in the order the edge asks.
		sets func(*dropstest.Driver)
		// windowed says whether Limit switches on the ROW_NUMBER()
		// rewrite for this kind. It is false for a to-one edge, which
		// already caps at one row per parent, and false for
		// ManyToMany, whose loader reads the junction first and does
		// not implement a per-parent cap at all.
		windowed bool
	}{
		{
			name:     "HasMany",
			root:     func(s *scopeSchema) *pg.Table { return s.users },
			dest:     func() any { return &[]scopeUser{} },
			child:    func(s *scopeSchema) *pg.Table { return s.posts },
			rel:      "posts",
			windowed: true,
			sets: func(d *dropstest.Driver) {
				d.Rows(scopeUserCols, scopeUserRow).Rows(scopePostCols, scopePostRow)
			},
		},
		{
			name:  "HasOne",
			root:  func(s *scopeSchema) *pg.Table { return s.users },
			dest:  func() any { return &[]scopeUser{} },
			child: func(s *scopeSchema) *pg.Table { return s.profiles },
			rel:   "profile",
			sets: func(d *dropstest.Driver) {
				d.Rows(scopeUserCols, scopeUserRow).Rows(scopeProfileCols, scopeProfileRow)
			},
		},
		{
			name:  "BelongsTo",
			root:  func(s *scopeSchema) *pg.Table { return s.posts },
			dest:  func() any { return &[]scopePostWithAuthor{} },
			child: func(s *scopeSchema) *pg.Table { return s.users },
			rel:   "author",
			sets: func(d *dropstest.Driver) {
				d.Rows(scopePostCols, scopePostRow).Rows(scopeUserCols, scopeUserRow)
			},
		},
		{
			name:  "ManyToMany",
			root:  func(s *scopeSchema) *pg.Table { return s.users },
			dest:  func() any { return &[]scopeUser{} },
			child: func(s *scopeSchema) *pg.Table { return s.tags },
			rel:   "tags",
			sets: func(d *dropstest.Driver) {
				d.Rows(scopeUserCols, scopeUserRow).
					Rows(scopeJoinCols, scopeJoinRow).
					Rows(scopeTagCols, scopeTagRow)
			},
		},
		{
			name:     "MorphMany",
			root:     func(s *scopeSchema) *pg.Table { return s.users },
			dest:     func() any { return &[]scopeUser{} },
			child:    func(s *scopeSchema) *pg.Table { return s.comments },
			rel:      "comments",
			windowed: true,
			sets: func(d *dropstest.Driver) {
				d.Rows(scopeUserCols, scopeUserRow).Rows(scopeCommentCols, scopeCommentRow)
			},
		},
		{
			name:  "MorphTo",
			root:  func(s *scopeSchema) *pg.Table { return s.comments },
			dest:  func() any { return &[]scopeComment{} },
			child: func(s *scopeSchema) *pg.Table { return s.users },
			rel:   "commentable",
			sets: func(d *dropstest.Driver) {
				d.Rows(scopeCommentCols, scopeCommentRow).Rows(scopeUserCols, scopeUserRow)
			},
		},
	}

	options := []struct {
		name string
		// apply configures the edge; child is the relation's target
		// table so a predicate can name a real column.
		apply func(c *pg.RelConfig, child *pg.Table)
		// widened marks the option that deliberately drops the target
		// table's scoping for this edge.
		widened bool
	}{
		{name: "plain", apply: func(c *pg.RelConfig, child *pg.Table) {}},
		{name: "Where", apply: func(c *pg.RelConfig, child *pg.Table) {
			c.Where(pg.Gt(child.Col("id"), 0))
		}},
		{name: "OrderBy", apply: func(c *pg.RelConfig, child *pg.Table) {
			c.OrderBy(child.Col("id").Desc())
		}},
		{name: "Limit", apply: func(c *pg.RelConfig, child *pg.Table) {
			c.Limit(5)
		}},
		{name: "LimitOffset", apply: func(c *pg.RelConfig, child *pg.Table) {
			c.Limit(5).Offset(2)
		}},
		{name: "WhereOrderByLimit", apply: func(c *pg.RelConfig, child *pg.Table) {
			c.Where(pg.Gt(child.Col("id"), 0)).OrderBy(child.Col("id").Desc()).Limit(5)
		}},
		{name: "Unscoped", widened: true, apply: func(c *pg.RelConfig, child *pg.Table) {
			c.Unscoped()
		}},
		{name: "UnscopedLimit", widened: true, apply: func(c *pg.RelConfig, child *pg.Table) {
			c.Unscoped().Limit(5)
		}},
	}

	executors := []struct {
		name string
		run  func(f *pg.FindBuilder, ctx context.Context, dest any) error
	}{
		{"All", func(f *pg.FindBuilder, ctx context.Context, dest any) error {
			return f.All(ctx, dest)
		}},
		{"One", func(f *pg.FindBuilder, ctx context.Context, dest any) error {
			// One takes a pointer to a struct, not to a slice; the
			// caller's dest names the element type.
			return f.One(ctx, oneDest(dest))
		}},
	}

	for _, k := range kinds {
		for _, o := range options {
			for _, x := range executors {
				t.Run(k.name+"/"+o.name+"/"+x.name, func(t *testing.T) {
					s := newScopeSchema()
					drv := dropstest.New()
					k.sets(drv)
					db := pg.New(drv)

					dest := k.dest()
					f := db.Find(k.root(s)).WithRel(k.rel, func(c *pg.RelConfig) {
						o.apply(c, k.child(s))
					})
					if err := x.run(f, scopeCtx(), dest); err != nil {
						t.Fatalf("load: %v", err)
					}

					sqls := drv.SQL()
					if len(sqls) < 2 {
						t.Fatalf("expected a parent query and at least one child query, got %d: %v", len(sqls), sqls)
					}
					// The root is always scoped: widening one edge
					// must not touch the query that found the parents.
					assertScoped(t, "root", sqls[0], true)
					for _, sql := range sqls[1:] {
						assertScoped(t, "child", sql, !o.widened)
					}
					// A Limit on a to-many edge has to be the window
					// rewrite, or the option under test never ran.
					if k.windowed && strings.HasPrefix(o.name, "Limit") {
						if !strings.Contains(sqls[len(sqls)-1], "ROW_NUMBER() OVER") {
							t.Errorf("Limit did not produce the window rewrite: %s", sqls[len(sqls)-1])
						}
					}
				})
			}
		}
	}
}

// oneDest turns the *[]T a case declares into the *T that Find.One
// wants, without every case having to spell both.
func oneDest(sliceDest any) any {
	switch sliceDest.(type) {
	case *[]scopeUser:
		return &scopeUser{}
	case *[]scopePostWithAuthor:
		return &scopePostWithAuthor{}
	case *[]scopeComment:
		return &scopeComment{}
	default:
		panic("scoping_test: unhandled dest type")
	}
}

// assertScoped checks one statement for both automatic predicates.
func assertScoped(t *testing.T, role, sql string, want bool) {
	t.Helper()
	for _, frag := range []string{tenantPred, softDeletePred} {
		if got := strings.Contains(sql, frag); got != want {
			t.Errorf("%s statement carries %q = %v, want %v:\n%s", role, frag, got, want, sql)
		}
	}
}

// ----------------------------------------------------------------------
// The matrix: executor
// ----------------------------------------------------------------------

type scopeNote struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Body     string `drop:"body"`
}

var (
	scopeNoteCols = []string{"id", "tenantId", "body"}
	scopeNoteRow  = []any{int64(1), scopeTenant, "note"}
)

func newNoteEntity() (*pg.Table, *pg.Col[string], *pg.Entity[scopeNote]) {
	t := pg.NewTable("notes")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	body := pg.Add(t, pg.Text("body").NotNull())
	return t, body, pg.NewEntity[scopeNote](t).ScopeByTenant(tenant)
}

// TestScopingReachesEveryExecutor pins the other half of the design:
// the predicate is resolved by whichever method has the ctx, so every
// method that has one applies it. The raw-builder cases matter as much
// as the entity ones — ScopeByTenant registers the axis on the table,
// so a query built straight from db.Select() is scoped too, and that is
// exactly why an eager-loaded relation is.
func TestScopingReachesEveryExecutor(t *testing.T) {
	cases := []struct {
		name string
		// rows overrides the canned answer for executors that scan
		// something other than a note row.
		rows func(*dropstest.Driver)
		run  func(db *pg.DB, ctx context.Context, tbl *pg.Table, body *pg.Col[string], ent *pg.Entity[scopeNote]) error
	}{
		{name: "Select.Rows", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			rows, err := db.Select().From(tbl).Rows(ctx)
			if err == nil {
				rows.Close()
			}
			return err
		}},
		{name: "Select.All", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out []scopeNote
			return db.Select().From(tbl).All(ctx, &out)
		}},
		{name: "Select.One", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out scopeNote
			return db.Select().From(tbl).One(ctx, &out)
		}},
		{
			name: "Select.Count",
			rows: func(d *dropstest.Driver) { d.AlwaysRows([]string{"count"}, []any{int64(1)}) },
			run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
				_, err := db.Select().From(tbl).Count(ctx)
				return err
			},
		},
		{name: "Find.All", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out []scopeNote
			return db.Find(tbl).All(ctx, &out)
		}},
		{name: "Find.One", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out scopeNote
			return db.Find(tbl).One(ctx, &out)
		}},
		{name: "Entity.Get", run: func(db *pg.DB, ctx context.Context, _ *pg.Table, _ *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			_, err := ent.Get(db, ctx, int64(1))
			return err
		}},
		{name: "EntityQuery.All", run: func(db *pg.DB, ctx context.Context, _ *pg.Table, _ *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			_, err := ent.Query(db).All(ctx)
			return err
		}},
		{name: "EntityQuery.One", run: func(db *pg.DB, ctx context.Context, _ *pg.Table, _ *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			_, err := ent.Query(db).One(ctx)
			return err
		}},
		{name: "EntityQuery.Stream", run: func(db *pg.DB, ctx context.Context, _ *pg.Table, _ *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			return ent.Query(db).Stream(ctx, func(*scopeNote) error { return nil })
		}},
		{name: "Entity.Page", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			_, err := ent.Page(db).OrderBy(pg.Asc(tbl.Col("id"))).Limit(10).All(ctx)
			return err
		}},
		{name: "Update.Exec", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, body *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			_, err := db.Update(tbl).Set(body.Val("x")).Exec(ctx)
			return err
		}},
		{name: "Update.All", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, body *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out []scopeNote
			return db.Update(tbl).Set(body.Val("x")).Returning(tbl.Col("id")).All(ctx, &out)
		}},
		{name: "Update.One", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, body *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out scopeNote
			return db.Update(tbl).Set(body.Val("x")).Returning(tbl.Col("id")).One(ctx, &out)
		}},
		{name: "Delete.Exec", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			_, err := db.Delete(tbl).Exec(ctx)
			return err
		}},
		{name: "Delete.All", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out []scopeNote
			return db.Delete(tbl).Returning(tbl.Col("id")).All(ctx, &out)
		}},
		{name: "Delete.One", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, _ *pg.Col[string], _ *pg.Entity[scopeNote]) error {
			var out scopeNote
			return db.Delete(tbl).Returning(tbl.Col("id")).One(ctx, &out)
		}},
		{name: "Entity.Update", run: func(db *pg.DB, ctx context.Context, _ *pg.Table, _ *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			n := scopeNote{ID: 1, TenantID: scopeTenant, Body: "x"}
			return ent.Update(db, ctx, &n)
		}},
		{name: "Entity.Delete", run: func(db *pg.DB, ctx context.Context, _ *pg.Table, _ *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			_, err := ent.Delete(db, ctx, int64(1))
			return err
		}},
		{name: "Entity.Patch", run: func(db *pg.DB, ctx context.Context, tbl *pg.Table, body *pg.Col[string], ent *pg.Entity[scopeNote]) error {
			_, err := ent.Patch(db, ctx, int64(1), body.Val("x"))
			return err
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tbl, body, ent := newNoteEntity()
			drv := dropstest.New()
			if c.rows != nil {
				c.rows(drv)
			} else {
				drv.AlwaysRows(scopeNoteCols, scopeNoteRow)
			}
			db := pg.New(drv)

			if err := c.run(db, scopeCtx(), tbl, body, ent); err != nil {
				t.Fatalf("run: %v", err)
			}
			sqls := drv.SQL()
			if len(sqls) == 0 {
				t.Fatal("no statement was recorded")
			}
			for _, sql := range sqls {
				if !strings.Contains(sql, tenantPred) {
					t.Errorf("statement is not tenant-scoped:\n%s", sql)
				}
			}
		})
	}
}

// ----------------------------------------------------------------------
// Failing closed, and the mechanics that keep it honest
// ----------------------------------------------------------------------

// TestTenantFilterFailsClosed is the assertion the whole design rests
// on: with no tenant on ctx the executor refuses, and — the part worth
// asserting separately — nothing reaches the driver. A filter that
// errored after sending would be a leak with a stack trace attached.
func TestTenantFilterFailsClosed(t *testing.T) {
	tbl, body, ent := newNoteEntity()
	cases := []struct {
		name string
		run  func(db *pg.DB, ctx context.Context) error
	}{
		{"Select.All", func(db *pg.DB, ctx context.Context) error {
			var out []scopeNote
			return db.Select().From(tbl).All(ctx, &out)
		}},
		{"Select.Count", func(db *pg.DB, ctx context.Context) error {
			_, err := db.Select().From(tbl).Count(ctx)
			return err
		}},
		{"Find.All", func(db *pg.DB, ctx context.Context) error {
			var out []scopeNote
			return db.Find(tbl).All(ctx, &out)
		}},
		{"Update.Exec", func(db *pg.DB, ctx context.Context) error {
			_, err := db.Update(tbl).Set(body.Val("x")).Exec(ctx)
			return err
		}},
		{"Delete.Exec", func(db *pg.DB, ctx context.Context) error {
			_, err := db.Delete(tbl).Exec(ctx)
			return err
		}},
		{"Entity.Get", func(db *pg.DB, ctx context.Context) error {
			_, err := ent.Get(db, ctx, int64(1))
			return err
		}},
		{"EntityQuery.All", func(db *pg.DB, ctx context.Context) error {
			_, err := ent.Query(db).All(ctx)
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drv := dropstest.New().AlwaysRows(scopeNoteCols, scopeNoteRow)
			db := pg.New(drv)
			err := c.run(db, context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Errorf("got = %v, want %v", err, pg.ErrTenantMissing)
			}
			if got := drv.SQL(); len(got) != 0 {
				t.Errorf("a refused query still reached the driver: %v", got)
			}
		})
	}
}

// TestContextFilterDoesNotAccumulate executes one builder three times.
// Resolving into the builder's own WHERE list instead of into a copy
// would bind the tenant once more on each run — with the same value, so
// the rows come back correct and nothing fails until an argument limit
// does.
func TestContextFilterDoesNotAccumulate(t *testing.T) {
	tbl, body, _ := newNoteEntity()
	ctx := scopeCtx()

	run := func(t *testing.T, drv *dropstest.Driver, exec func() error) {
		t.Helper()
		for i := 0; i < 3; i++ {
			if err := exec(); err != nil {
				t.Fatalf("run %d: %v", i, err)
			}
		}
		first := drv.SQL()[0]
		for i, sql := range drv.SQL() {
			if sql != first {
				t.Errorf("run %d rendered a different statement:\n got = %v\nwant = %v", i, sql, first)
			}
			if n := strings.Count(sql, tenantPred); n != 1 {
				t.Errorf("run %d carries the tenant predicate %d times, want 1:\n%s", i, n, sql)
			}
		}
	}

	t.Run("Select", func(t *testing.T) {
		drv := dropstest.New().AlwaysRows(scopeNoteCols, scopeNoteRow)
		sel := pg.New(drv).Select().From(tbl).Where(pg.Gt(tbl.Col("id"), 0))
		run(t, drv, func() error {
			var out []scopeNote
			return sel.All(ctx, &out)
		})
	})
	t.Run("Update", func(t *testing.T) {
		drv := dropstest.New().AlwaysRows(scopeNoteCols, scopeNoteRow)
		upd := pg.New(drv).Update(tbl).Set(body.Val("x"))
		run(t, drv, func() error {
			_, err := upd.Exec(ctx)
			return err
		})
	})
	t.Run("Delete", func(t *testing.T) {
		drv := dropstest.New().AlwaysRows(scopeNoteCols, scopeNoteRow)
		del := pg.New(drv).Delete(tbl).Where(pg.Gt(tbl.Col("id"), 0))
		run(t, drv, func() error {
			_, err := del.Exec(ctx)
			return err
		})
	})
}

// TestToSQLCtxRendersWhatToSQLCannot documents the cost of resolving at
// execution time, and pins it: ToSQL is no longer the whole statement,
// ToSQLCtx is, and the difference is exactly the context filters.
func TestToSQLCtxRendersWhatToSQLCannot(t *testing.T) {
	tbl, body, _ := newNoteEntity()
	db := pg.New(dropstest.New())
	ctx := scopeCtx()

	t.Run("Select", func(t *testing.T) {
		sel := db.Select().From(tbl)
		if bare, _ := sel.ToSQL(); strings.Contains(bare, tenantPred) {
			t.Errorf("ToSQL cannot resolve a context filter, yet rendered one: %s", bare)
		}
		full, args, err := sel.ToSQLCtx(ctx)
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if !strings.Contains(full, tenantPred) {
			t.Errorf("ToSQLCtx did not resolve the tenant axis: %s", full)
		}
		if len(args) != 1 || args[0] != scopeTenant {
			t.Errorf("got = %v, want [%v]", args, scopeTenant)
		}
	})
	t.Run("Update", func(t *testing.T) {
		upd := db.Update(tbl).Set(body.Val("x"))
		if bare, _ := upd.ToSQL(); strings.Contains(bare, tenantPred) {
			t.Errorf("ToSQL cannot resolve a context filter, yet rendered one: %s", bare)
		}
		full, _, err := upd.ToSQLCtx(ctx)
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if !strings.Contains(full, tenantPred) {
			t.Errorf("ToSQLCtx did not resolve the tenant axis: %s", full)
		}
	})
	t.Run("Delete", func(t *testing.T) {
		del := db.Delete(tbl)
		if bare, _ := del.ToSQL(); strings.Contains(bare, tenantPred) {
			t.Errorf("ToSQL cannot resolve a context filter, yet rendered one: %s", bare)
		}
		full, _, err := del.ToSQLCtx(ctx)
		if err != nil {
			t.Fatalf("ToSQLCtx: %v", err)
		}
		if !strings.Contains(full, tenantPred) {
			t.Errorf("ToSQLCtx did not resolve the tenant axis: %s", full)
		}
	})
	t.Run("ToSQLCtxPropagatesRefusal", func(t *testing.T) {
		if _, _, err := db.Select().From(tbl).ToSQLCtx(context.Background()); !errors.Is(err, pg.ErrTenantMissing) {
			t.Errorf("got = %v, want %v", err, pg.ErrTenantMissing)
		}
	})
}

// TestUnscopedClearsContextFilters pins the documented escape hatch on
// all three builders. It is the one place a cross-tenant statement is
// legal, and it has to be spelled at the call site.
func TestUnscopedClearsContextFilters(t *testing.T) {
	tbl, body, _ := newNoteEntity()
	drv := dropstest.New().AlwaysRows(scopeNoteCols, scopeNoteRow)
	db := pg.New(drv)
	// No tenant on ctx: an unscoped statement must not even ask for one.
	ctx := context.Background()

	var out []scopeNote
	if err := db.Select().From(tbl).Unscoped().All(ctx, &out); err != nil {
		t.Fatalf("Select: %v", err)
	}
	if _, err := db.Update(tbl).Set(body.Val("x")).Unscoped().Exec(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := db.Delete(tbl).Unscoped().Exec(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := db.Find(tbl).Unscoped().All(ctx, &out); err != nil {
		t.Fatalf("Find: %v", err)
	}
	for _, sql := range drv.SQL() {
		if strings.Contains(sql, tenantPred) {
			t.Errorf("Unscoped statement still carries the tenant axis:\n%s", sql)
		}
	}
}

// TestPerParentLimitKeepsDefaultFilters is P0-2 as its reporter would
// write it: the same relation, loaded twice, differing only by a Limit
// that is not supposed to change which rows are visible.
func TestPerParentLimitKeepsDefaultFilters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
	}{{"withoutLimit", 0}, {"withLimit", 5}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScopeSchema()
			drv := dropstest.New().
				Rows(scopeUserCols, scopeUserRow).
				Rows(scopePostCols, scopePostRow)
			db := pg.New(drv)

			var users []scopeUser
			err := db.Find(s.users).WithRel("posts", func(c *pg.RelConfig) {
				if tc.limit > 0 {
					c.Limit(tc.limit)
				}
			}).All(scopeCtx(), &users)
			if err != nil {
				t.Fatalf("All: %v", err)
			}
			child := drv.SQL()[1]
			if !strings.Contains(child, softDeletePred) {
				t.Errorf("a limited relation must not resurrect soft-deleted rows:\n%s", child)
			}
			if !strings.Contains(child, tenantPred) {
				t.Errorf("a limited relation must stay tenant-scoped:\n%s", child)
			}
		})
	}
}

// TestGuardReachesRelations is the authorisation half of P0-1. The
// guard is declared on the Entity and the relation loader has no Entity
// to ask, so before the guard became a filter on the table the child
// rows came back whoever asked.
func TestGuardReachesRelations(t *testing.T) {
	s := newScopeSchema()
	// The guard names a column on the child table, since it is the
	// child rows whose visibility is in question.
	ent := pg.NewEntity[scopePost](s.posts).
		AuthorizeWith(pg.OwnerGuard{Owner: s.posts.Col("userId")})
	_ = ent

	drv := dropstest.New().
		Rows(scopeUserCols, scopeUserRow).
		Rows(scopePostCols, scopePostRow)
	db := pg.New(drv)

	var users []scopeUser
	if err := db.Find(s.users).With("posts").All(scopeCtx(), &users); err != nil {
		t.Fatalf("All: %v", err)
	}
	child := drv.SQL()[1]
	if !strings.Contains(child, `"posts"."userId" = $`) {
		t.Errorf("the guard did not reach the eager-loaded relation:\n%s", child)
	}
}

// TestGuardWithoutSubjectRefusesRelation is the same story told by its
// error: a guarded table reached through a relation on a ctx with no
// subject must refuse, not answer.
func TestGuardWithoutSubjectRefusesRelation(t *testing.T) {
	s := newScopeSchema()
	pg.NewEntity[scopePost](s.posts).
		AuthorizeWith(pg.OwnerGuard{Owner: s.posts.Col("userId")})

	drv := dropstest.New().
		Rows(scopeUserCols, scopeUserRow).
		Rows(scopePostCols, scopePostRow)
	db := pg.New(drv)

	var users []scopeUser
	err := db.Find(s.users).With("posts").All(pg.WithTenant(context.Background(), scopeTenant), &users)
	if !errors.Is(err, pg.ErrSubjectMissing) {
		t.Errorf("got = %v, want %v", err, pg.ErrSubjectMissing)
	}
	if got := len(drv.SQL()); got != 1 {
		t.Errorf("the refused child query was still sent: %v", drv.SQL())
	}
}

// TestSoftDeleteRewriteKeepsTheTenant covers the one path where the
// resolved predicates have to be visible to something other than the
// renderer: a DeleteHook rewrites the statement into an UPDATE built
// from the builder's WHERE list, so a tenant resolved after the hook
// ran would be missing from the statement that actually mutates rows.
func TestSoftDeleteRewriteKeepsTheTenant(t *testing.T) {
	s := newScopeSchema()
	drv := dropstest.New()
	db := pg.New(drv)

	if _, err := db.Delete(s.posts).Where(pg.Eq(s.posts.Col("id"), 10)).Exec(scopeCtx()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	sql := drv.LastSQL()
	if !strings.HasPrefix(sql, "UPDATE ") {
		t.Fatalf("SoftDeleteMixin should have rewritten the DELETE: %s", sql)
	}
	if !strings.Contains(sql, tenantPred) {
		t.Errorf("the soft-delete rewrite dropped the tenant axis:\n%s", sql)
	}
}

// TestContextFilterRegistrationIsIdempotent guards the footgun the key
// exists for: an entity declared inside a request handler installs its
// filter on the shared table on every call, and stacking one predicate
// per construction would grow the WHERE clause without bound.
func TestContextFilterRegistrationIsIdempotent(t *testing.T) {
	tbl := pg.NewTable("notes")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("body").NotNull())
	for i := 0; i < 5; i++ {
		pg.NewEntity[scopeNote](tbl).ScopeByTenant(tenant)
	}

	drv := dropstest.New().AlwaysRows(scopeNoteCols, scopeNoteRow)
	db := pg.New(drv)
	var out []scopeNote
	if err := db.Select().From(tbl).All(scopeCtx(), &out); err != nil {
		t.Fatalf("All: %v", err)
	}
	if n := strings.Count(drv.LastSQL(), tenantPred); n != 1 {
		t.Errorf("five declarations left %d tenant predicates, want 1:\n%s", n, drv.LastSQL())
	}
}

// TestContextFilterOnUnionOperands covers the set-operation shape: each
// operand carries its own FROM table, so resolving only the leading one
// would leave the other half of a UNION unscoped.
func TestContextFilterOnUnionOperands(t *testing.T) {
	tbl, _, _ := newNoteEntity()
	other, _, _ := newNoteEntity()
	drv := dropstest.New().AlwaysRows(scopeNoteCols, scopeNoteRow)
	db := pg.New(drv)

	var out []scopeNote
	q := db.Select().From(tbl).Union(db.Select().From(other))
	if err := q.All(scopeCtx(), &out); err != nil {
		t.Fatalf("All: %v", err)
	}
	if n := strings.Count(drv.LastSQL(), tenantPred); n != 2 {
		t.Errorf("both UNION operands must be scoped, found %d:\n%s", n, drv.LastSQL())
	}
}

// Compile-time proof that the roadmap's ContextFilterFunc signature is
// what shipped: an application filter is an ordinary function.
var _ pg.ContextFilterFunc = func(ctx context.Context) (drops.Expression, error) {
	return nil, nil
}

// EntityQuery.Unscoped is narrower than [SelectBuilder.Unscoped], on
// purpose and in all four dialects: it drops the DEFAULT filters and
// keeps the context filters.
//
// The two lists are not the same kind of thing. A default filter is a
// default scope; a context filter is a row-visibility boundary.
// Widening a default scope when the caller asked to widen it costs
// nothing. Dropping the boundary hands this request every tenant's
// rows, or every subject's, and it does so on the one method a caller
// reaches for while thinking about soft-deleted rows rather than about
// tenancy.
//
// So Unscoped has ONE meaning per level: statement-wide on the raw
// builder, where the caller is describing the whole statement's
// authority and a reviewer reads the whole of what was given up, and
// defaults-only on the entity query.
//
// This dialect used to make the other trade — the entity query
// delegated straight to the builder — and its doc sentence promised the
// narrow one regardless.
func TestEntityQueryUnscopedKeepsTheTenantAndDropsTheDefaultFilter(t *testing.T) {
	tbl := pg.NewTable("equ_posts")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	pg.Add(tbl, pg.Text("title").NotNull())
	deleted := pg.SoftDelete(tbl).DeletedAt
	tbl.DefaultFilter(deleted.IsNull())
	ent := pg.NewEntity[equPost](tbl, pg.AllowUnmappedColumns("deletedAt")).
		ScopeByTenant(tenant)

	drv := dropstest.New().AlwaysRows([]string{"id", "tenantId", "title"})
	db := pg.New(drv)

	if _, err := ent.Query(db).All(scopeCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := drv.LastSQL(); !strings.Contains(got, softDeletePred) {
		t.Fatalf("the scoped query lost its default filter:\n%s", got)
	}

	drv.Reset()
	if _, err := ent.Query(db).Unscoped().All(scopeCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	got := drv.LastSQL()
	if strings.Contains(got, softDeletePred) {
		t.Errorf("Unscoped did not drop the default filter:\n%s", got)
	}
	if !strings.Contains(got, `"equ_posts"."tenantId" = $`) {
		t.Errorf("Unscoped dropped the tenant axis:\n%s", got)
	}

	// And it still refuses on a ctx with no tenant, rather than widening
	// to every tenant because the caller asked for hidden rows.
	drv.Reset()
	if _, err := ent.Query(db).Unscoped().All(context.Background()); !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, pg.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// equPost is the row type the case above scans.
type equPost struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// The raw builder keeps the wide meaning: a caller who says Unscoped
// there is describing this statement's authority, and gets a statement
// with no automatic predicate of either kind and no refusal.
func TestSelectBuilderUnscopedStaysStatementWide(t *testing.T) {
	tbl := pg.NewTable("sbu_posts")
	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	tbl.DefaultFilter(pg.SoftDelete(tbl).DeletedAt.IsNull())
	tbl.ContextFilter(pg.TenantFilter(tenant))

	db := pg.New(dropstest.New())
	sql, args, err := db.Select(id).From(tbl).Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if sql != `SELECT "sbu_posts"."id" FROM "sbu_posts"` {
		t.Errorf("sql = %s", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}
