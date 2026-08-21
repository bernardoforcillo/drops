package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// A library schema with the shapes WhereHas has to cover: a one-to-many
// with a soft-delete filter on the child, the belongs-to that inverts
// it, a nested edge below the child, and a many-to-many through a
// junction that carries a filter of its own.
type hasSchema struct {
	authors, books, reviews *pg.Table
	posts, tags, postTags   *pg.Table
	published               *pg.Col[bool]
	rating                  *pg.Col[int64]
}

func mkHasSchema() hasSchema {
	authors := pg.NewTable("authors")
	authorID := pg.Add(authors, pg.BigSerial("id").PrimaryKey())
	pg.Add(authors, pg.Text("name").NotNull())

	books := pg.NewTable("books")
	bookID := pg.Add(books, pg.BigSerial("id").PrimaryKey())
	bookAuthorID := pg.Add(books, pg.BigInt("authorId").NotNull())
	published := pg.Add(books, pg.Boolean("published").NotNull())
	bookDeletedAt := pg.Add(books, pg.Timestamp("deletedAt", true))
	books.AddFilter(pg.FilterSoftDelete, pg.IsNull(bookDeletedAt))

	reviews := pg.NewTable("reviews")
	pg.Add(reviews, pg.BigSerial("id").PrimaryKey())
	reviewBookID := pg.Add(reviews, pg.BigInt("bookId").NotNull())
	rating := pg.Add(reviews, pg.BigInt("rating").NotNull())

	pg.NewRelations(authors).HasMany("books", books, authorID, bookAuthorID)
	pg.NewRelations(books).
		BelongsTo("author", authors, bookAuthorID, authorID).
		HasMany("reviews", reviews, bookID, reviewBookID)

	posts := pg.NewTable("posts")
	postID := pg.Add(posts, pg.BigSerial("id").PrimaryKey())

	tags := pg.NewTable("tags")
	tagID := pg.Add(tags, pg.BigSerial("id").PrimaryKey())
	tagDeletedAt := pg.Add(tags, pg.Timestamp("deletedAt", true))
	tags.AddFilter(pg.FilterSoftDelete, pg.IsNull(tagDeletedAt))

	postTags := pg.NewTable("postTags")
	ptPostID := pg.Add(postTags, pg.BigInt("postId").NotNull())
	ptTagID := pg.Add(postTags, pg.BigInt("tagId").NotNull())
	ptApproved := pg.Add(postTags, pg.Boolean("approved").NotNull())
	postTags.AddFilter("approved", ptApproved.Eq(true))

	pg.NewRelations(posts).
		ManyToMany("tags", tags, postTags, ptPostID, ptTagID, postID, tagID)

	return hasSchema{
		authors: authors, books: books, reviews: reviews,
		posts: posts, tags: tags, postTags: postTags,
		published: published, rating: rating,
	}
}

// mkEmployees is the self-referencing shape: one table, a manager FK
// back into itself, and relations in both directions.
func mkEmployees() (employees *pg.Table, active *pg.Col[bool]) {
	employees = pg.NewTable("employees")
	id := pg.Add(employees, pg.BigSerial("id").PrimaryKey())
	managerID := pg.Add(employees, pg.BigInt("managerId"))
	active = pg.Add(employees, pg.Boolean("active").NotNull())
	pg.NewRelations(employees).
		HasMany("reports", employees, id, managerID).
		BelongsTo("manager", employees, managerID, id)
	return employees, active
}

func hasSQL(t *testing.T, f *pg.FindBuilder) string {
	t.Helper()
	sql, _ := f.Select().ToSQL()
	return sql
}

func wantSQL(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("SQL mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestWhereHasHasMany(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(s.authors).WhereHas("books", func(q *pg.RelQuery) {
		q.Where(s.published.Eq(true))
	}))
	wantSQL(t, got, `SELECT * FROM "authors" WHERE EXISTS (SELECT 1 FROM "books" `+
		`WHERE ("books"."authorId" = "authors"."id") AND ("books"."deletedAt" IS NULL) `+
		`AND ("books"."published" = $1))`)
}

// The point of the whole exercise: the related table's own filters live
// inside the EXISTS, so a soft-deleted book cannot make its author
// match. Asserted on the rendered text here and against a real server
// in the integration suite.
func TestWhereHasCarriesTheRelatedTablesFilters(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(s.authors).WhereHas("books", nil))
	if !strings.Contains(got, `("books"."deletedAt" IS NULL)`) {
		t.Errorf("the books table's soft-delete filter is missing from the subquery: %s", got)
	}

	// And it is bypassable by name, one filter at a time, the way every
	// other read is.
	got = hasSQL(t, db.Find(s.authors).WhereHas("books", func(q *pg.RelQuery) {
		q.IgnoreFilters(pg.FilterSoftDelete)
	}))
	if strings.Contains(got, "deletedAt") {
		t.Errorf("IgnoreFilters(FilterSoftDelete) left the guard standing: %s", got)
	}

	got = hasSQL(t, db.Find(s.authors).WhereHas("books", func(q *pg.RelQuery) {
		q.Unscoped()
	}))
	if strings.Contains(got, "deletedAt") {
		t.Errorf("Unscoped left the guard standing: %s", got)
	}
}

func TestWhereHasBelongsTo(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(s.books).WhereHas("author", nil))
	wantSQL(t, got, `SELECT * FROM "books" WHERE ("books"."deletedAt" IS NULL) `+
		`AND EXISTS (SELECT 1 FROM "authors" WHERE ("authors"."id" = "books"."authorId"))`)
}

func TestWhereDoesntHave(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(s.authors).WhereDoesntHave("books", nil))
	wantSQL(t, got, `SELECT * FROM "authors" WHERE NOT EXISTS (SELECT 1 FROM "books" `+
		`WHERE ("books"."authorId" = "authors"."id") AND ("books"."deletedAt" IS NULL))`)
}

// ManyToMany is two joins, and getting it wrong is the usual way this
// feature ships broken: the junction is the subquery's FROM, the target
// is joined onto it, and both tables' filters have to survive.
func TestWhereHasManyToMany(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(s.posts).WhereHas("tags", func(q *pg.RelQuery) {
		q.Where(pg.Eq(s.tags.Col("id"), int64(7)))
	}))
	wantSQL(t, got, `SELECT * FROM "posts" WHERE EXISTS (SELECT 1 FROM "postTags" `+
		`INNER JOIN "tags" ON ("tags"."id" = "postTags"."tagId") `+
		`WHERE ("postTags"."postId" = "posts"."id") AND ("postTags"."approved" = $1) `+
		`AND ("tags"."deletedAt" IS NULL) AND ("tags"."id" = $2))`)
}

func TestWhereHasNested(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(s.authors).WhereHas("books", func(q *pg.RelQuery) {
		q.WhereHas("reviews", func(r *pg.RelQuery) {
			r.Where(s.rating.Gte(4))
		})
	}))
	wantSQL(t, got, `SELECT * FROM "authors" WHERE EXISTS (SELECT 1 FROM "books" `+
		`WHERE ("books"."authorId" = "authors"."id") AND ("books"."deletedAt" IS NULL) `+
		`AND EXISTS (SELECT 1 FROM "reviews" WHERE ("reviews"."bookId" = "books"."id") `+
		`AND ("reviews"."rating" >= $1)))`)
}

func TestWhereHasRelTakesAHandle(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})
	authorBooks := s.authors.Rel("books")

	byHandle := hasSQL(t, db.Find(s.authors).WhereHasRel(authorBooks, nil))
	byName := hasSQL(t, db.Find(s.authors).WhereHas("books", nil))
	if byHandle != byName {
		t.Errorf("handle and name forms disagree\nhandle: %s\n  name: %s", byHandle, byName)
	}

	// A nil handle is ignored, so an optional relation needs no guard.
	plain := hasSQL(t, db.Find(s.authors))
	if got := hasSQL(t, db.Find(s.authors).WhereHasRel(nil, nil)); got != plain {
		t.Errorf("nil handle changed the query: %s", got)
	}
	if got := hasSQL(t, db.Find(s.authors).WhereDoesntHaveRel(nil, nil)); got != plain {
		t.Errorf("nil handle changed the query: %s", got)
	}
}

// A count bound abandons EXISTS — there is nothing to short-circuit
// once a threshold is involved — and compares a correlated count.
func TestWhereHasCountBound(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	sql, args := db.Find(s.authors).WhereHas("books", func(q *pg.RelQuery) {
		q.CountGte(3)
	}).Select().ToSQL()
	wantSQL(t, sql, `SELECT * FROM "authors" WHERE ((SELECT count(*) FROM "books" `+
		`WHERE ("books"."authorId" = "authors"."id") AND ("books"."deletedAt" IS NULL)) >= $1)`)
	if len(args) != 1 || args[0] != 3 {
		t.Errorf("args: got %v, want [3]", args)
	}

	// A junction may link the same pair twice; the caller asked how
	// many tags the post has, not how many links.
	got := hasSQL(t, db.Find(s.posts).WhereHas("tags", func(q *pg.RelQuery) {
		q.CountGt(2)
	}))
	if !strings.Contains(got, `count(DISTINCT "tags"."id")`) {
		t.Errorf("many-to-many count is not distinct on the target key: %s", got)
	}
}

// "Does not have three or more" is "has fewer than three": the negation
// folds into the operator rather than wrapping the comparison in NOT.
func TestWhereDoesntHaveWithCountFoldsIntoTheOperator(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(s.authors).WhereDoesntHave("books", func(q *pg.RelQuery) {
		q.CountGte(3)
	}))
	if strings.Contains(got, "NOT") {
		t.Errorf("expected the negation folded into the comparison, got: %s", got)
	}
	if !strings.Contains(got, `) < $1)`) {
		t.Errorf("expected a < comparison, got: %s", got)
	}
}

// A self-referencing relation cannot be tested with a correlated
// EXISTS: the subquery's own FROM shadows the table name the
// correlation would use. It becomes a semi-join on the key instead, so
// every reference inside the subquery still means the inner row and
// nothing has to be rewritten against an alias.
func TestWhereHasSelfReferencing(t *testing.T) {
	employees, active := mkEmployees()
	db := pg.New(&fakeDriver{})

	got := hasSQL(t, db.Find(employees).WhereHas("reports", func(q *pg.RelQuery) {
		q.Where(active.Eq(true))
	}))
	wantSQL(t, got, `SELECT * FROM "employees" WHERE ("employees"."id" IN `+
		`(SELECT "employees"."managerId" FROM "employees" `+
		`WHERE ("employees"."managerId" IS NOT NULL) AND ("employees"."active" = $1)))`)

	// The negated form has to keep the rows whose key is NULL — an
	// employee with no manager does not have an active one — which a
	// bare NOT IN would drop.
	got = hasSQL(t, db.Find(employees).WhereDoesntHave("manager", func(q *pg.RelQuery) {
		q.Where(active.Eq(true))
	}))
	wantSQL(t, got, `SELECT * FROM "employees" WHERE (("employees"."managerId" IS NULL) `+
		`OR "employees"."managerId" NOT IN (SELECT "employees"."id" FROM "employees" `+
		`WHERE ("employees"."id" IS NOT NULL) AND ("employees"."active" = $1)))`)
}

func TestWhereHasSelfReferencingCount(t *testing.T) {
	employees, _ := mkEmployees()
	db := pg.New(&fakeDriver{})

	// A bound no parent with zero related rows can satisfy is a
	// grouped semi-join.
	got := hasSQL(t, db.Find(employees).WhereHas("reports", func(q *pg.RelQuery) {
		q.CountGte(3)
	}))
	wantSQL(t, got, `SELECT * FROM "employees" WHERE ("employees"."id" IN `+
		`(SELECT "employees"."managerId" FROM "employees" `+
		`WHERE ("employees"."managerId" IS NOT NULL) `+
		`GROUP BY "employees"."managerId" HAVING count(*) >= $1))`)

	// One that a parent with none does satisfy has to be asked the
	// other way round, because a semi-join can only ever return
	// parents that have at least one.
	got = hasSQL(t, db.Find(employees).WhereHas("reports", func(q *pg.RelQuery) {
		q.CountLt(3)
	}))
	wantSQL(t, got, `SELECT * FROM "employees" WHERE (("employees"."id" IS NULL) `+
		`OR "employees"."id" NOT IN (SELECT "employees"."managerId" FROM "employees" `+
		`WHERE ("employees"."managerId" IS NOT NULL) `+
		`GROUP BY "employees"."managerId" HAVING count(*) >= $1))`)
}

func TestWhereHasUnknownRelation(t *testing.T) {
	s := mkHasSchema()
	db := pg.New(&fakeDriver{})

	var rows []struct{ ID int64 }
	err := db.Find(s.authors).WhereHas("nope", nil).All(context.Background(), &rows)
	if err == nil || !strings.Contains(err.Error(), `unknown relation "nope"`) {
		t.Fatalf("expected an unknown-relation error, got %v", err)
	}

	// And from a nested one, whose relation lives on the related table.
	err = db.Find(s.authors).WhereHas("books", func(q *pg.RelQuery) {
		q.WhereHas("nope", nil)
	}).All(context.Background(), &rows)
	if err == nil || !strings.Contains(err.Error(), `unknown relation "nope"`) {
		t.Fatalf("expected a nested unknown-relation error, got %v", err)
	}
}

// MorphTo is refused for the reason nested With refuses it: the table
// to look in varies row by row, so there is no single subquery.
func TestWhereHasMorphToIsRefused(t *testing.T) {
	comments := pg.NewTable("comments")
	pg.Add(comments, pg.BigSerial("id").PrimaryKey())
	ctype := pg.Add(comments, pg.Text("commentableType").NotNull())
	cid := pg.Add(comments, pg.BigInt("commentableId").NotNull())
	morphs := pg.NewMorphMap()
	pg.NewRelations(comments).MorphTo("commentable", ctype, cid, morphs)

	db := pg.New(&fakeDriver{})
	var rows []struct{ ID int64 }
	err := db.Find(comments).WhereHas("commentable", nil).All(context.Background(), &rows)
	if err == nil || !strings.Contains(err.Error(), "MorphTo") {
		t.Fatalf("expected a MorphTo refusal, got %v", err)
	}
}

// A MorphMany carries a discriminator, and the test is only meaningful
// with it: without the type guard every commented-on row of any kind
// would make this one match.
func TestWhereHasMorphManyKeepsTheTypeGuard(t *testing.T) {
	users := pg.NewTable("users")
	userID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	comments := pg.NewTable("comments")
	pg.Add(comments, pg.BigSerial("id").PrimaryKey())
	ctype := pg.Add(comments, pg.Text("commentableType").NotNull())
	cid := pg.Add(comments, pg.BigInt("commentableId").NotNull())
	pg.NewRelations(users).MorphMany("comments", comments, ctype, cid, userID, "users")

	db := pg.New(&fakeDriver{})
	sql, args := db.Find(users).WhereHas("comments", nil).Select().ToSQL()
	wantSQL(t, sql, `SELECT * FROM "users" WHERE EXISTS (SELECT 1 FROM "comments" `+
		`WHERE ("comments"."commentableId" = "users"."id") `+
		`AND ("comments"."commentableType" = $1))`)
	if len(args) != 1 || args[0] != "users" {
		t.Errorf("args: got %v, want [users]", args)
	}
}

// Entity's fast-scan path renders the SelectBuilder itself and never
// reaches FindBuilder.All, so a predicate held until then would be one
// this path silently dropped — and so would the error for a name no
// relation answers to, which is worse: the query still runs, without
// the predicate, and hands back every row in the table.
//
// The fast scanner is installed deliberately. Without it All falls
// through to FindBuilder.All, which has always surfaced both, and the
// test would pass whether or not the typed query does its own check.
func TestWhereHasReachesTheEntityFastPath(t *testing.T) {
	s := mkHasSchema()
	type author struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	ent := pg.NewEntity[author](s.authors)
	scanned := 0
	ent.SetFastScan(func(rows pg.Scanner, a *author) error {
		scanned++
		return rows.Scan(&a.ID, &a.Name)
	})

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "alice"}}}, nil
	}}
	db := pg.New(fd)

	got, err := ent.Query(db).WhereHas("books", func(q *pg.RelQuery) {
		q.Where(s.published.Eq(true))
	}).All(context.Background())
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if scanned != 1 || len(got) != 1 {
		t.Fatalf("the fast scanner ran %d times for %d rows — this is not the fast path", scanned, len(got))
	}
	if len(fd.queries) != 1 {
		t.Fatalf("expected one query, got %d", len(fd.queries))
	}
	if !strings.Contains(fd.queries[0], "EXISTS (SELECT 1") {
		t.Errorf("the typed query lost its WhereHas: %s", fd.queries[0])
	}

	// And the name it could not resolve is an error rather than a
	// predicate quietly left off — no query at all, rather than an
	// unfiltered one.
	fd.queries = nil
	if _, err := ent.Query(db).WhereHas("nope", nil).All(context.Background()); err == nil {
		t.Error("expected an unknown-relation error from the typed query")
	}
	if len(fd.queries) != 0 {
		t.Errorf("an unresolvable relation still ran a query, widening the result: %s", fd.queries[0])
	}
}

// Stream reaches for the SelectBuilder directly, the way the fast-scan
// and cache paths do, so it owes the same account of a relation name it
// could not resolve. Losing that error is worse here than anywhere
// else: a Stream is what an export or a batch job runs, and the query
// it silently falls back to is the whole table.
func TestWhereHasStreamRefusesAnUnknownRelation(t *testing.T) {
	s := mkHasSchema()
	type author struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	ent := pg.NewEntity[author](s.authors)

	fd := &fakeDriver{handler: func(string, []any) (drops.Rows, error) {
		return &fakeRows{cols: []string{"id", "name"}, data: [][]any{{int64(1), "alice"}}}, nil
	}}
	db := pg.New(fd)

	seen := 0
	err := ent.Query(db).WhereHas("nope", nil).
		Stream(context.Background(), func(*author) error { seen++; return nil })
	if err == nil || !strings.Contains(err.Error(), `unknown relation "nope"`) {
		t.Fatalf("expected an unknown-relation error from Stream, got %v", err)
	}
	if seen != 0 || len(fd.queries) != 0 {
		t.Errorf("Stream handed back %d rows from %v — an unresolvable relation widened it to the whole table",
			seen, fd.queries)
	}

	// The resolvable case still streams, predicate and all.
	fd.queries = nil
	if err := ent.Query(db).WhereHas("books", nil).
		Stream(context.Background(), func(*author) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(fd.queries) != 1 || !strings.Contains(fd.queries[0], "EXISTS (SELECT 1") {
		t.Errorf("the streamed query lost its WhereHas: %v", fd.queries)
	}
}
