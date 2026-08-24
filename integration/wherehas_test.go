package integration_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// WhereHas filters a parent by a property of its relation, and the
// property that has to hold on a real server is that the related
// table's own filters apply inside the subquery. A soft-deleted book
// must not make its author match — the same class of bug the per-parent
// limit path had, where a hand-rendered subquery quietly lost the
// guards Select().From() would have supplied.

type whAuthor struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

type whPost struct {
	ID    int64  `drop:"id"`
	Title string `drop:"title"`
}

type whEmployee struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func whNames(rows []whAuthor) string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// whLibrary builds authors + books on the live server. books carries a
// soft-delete filter, which is the guard every assertion below leans on.
func whLibrary(t *testing.T, db *pg.DB) (authors, books *pg.Table, published *pg.Col[bool]) {
	t.Helper()
	ctx := context.Background()

	authors = pg.NewTable(integration.UniqueName(t, "wh_a"))
	books = pg.NewTable(integration.UniqueName(t, "wh_b"))
	dropPG(t, db, authors)
	dropPG(t, db, books)

	authorID := pg.Add(authors, pg.BigSerial("id").PrimaryKey())
	authorName := pg.Add(authors, pg.Text("name").NotNull())

	pg.Add(books, pg.BigSerial("id").PrimaryKey())
	bookAuthorID := pg.Add(books, pg.BigInt("authorId").NotNull())
	bookTitle := pg.Add(books, pg.Text("title").NotNull())
	published = pg.Add(books, pg.Boolean("published").NotNull())
	bookDeletedAt := pg.Add(books, pg.Timestamp("deletedAt", true))
	books.AddFilter(pg.FilterSoftDelete, pg.IsNull(bookDeletedAt))

	pg.NewRelations(authors).HasMany("books", books, authorID, bookAuthorID)
	pg.NewRelations(books).BelongsTo("author", authors, bookAuthorID, authorID)

	execPG(t, db, pg.CreateTable(authors))
	execPG(t, db, pg.CreateTable(books))

	// alice   one live published book
	// bob     one published book, soft-deleted
	// carol   one live book, unpublished
	// dave    no books at all
	// erin    three live published books
	if _, err := db.Insert(authors).
		Row(authorName.Val("alice")).
		Row(authorName.Val("bob")).
		Row(authorName.Val("carol")).
		Row(authorName.Val("dave")).
		Row(authorName.Val("erin")).
		Exec(ctx); err != nil {
		t.Fatalf("seed authors: %v", err)
	}
	var all []whAuthor
	if err := db.Select().From(authors).OrderBy(authorID.Asc()).All(ctx, &all); err != nil {
		t.Fatalf("read authors: %v", err)
	}
	id := map[string]int64{}
	for _, a := range all {
		id[a.Name] = a.ID
	}

	ins := db.Insert(books).
		Row(bookAuthorID.Val(id["alice"]), bookTitle.Val("alice-live"), published.Val(true)).
		Row(bookAuthorID.Val(id["bob"]), bookTitle.Val("bob-doomed"), published.Val(true)).
		Row(bookAuthorID.Val(id["carol"]), bookTitle.Val("carol-draft"), published.Val(false))
	for _, title := range []string{"erin-1", "erin-2", "erin-3"} {
		ins = ins.Row(bookAuthorID.Val(id["erin"]), bookTitle.Val(title), published.Val(true))
	}
	if _, err := ins.Exec(ctx); err != nil {
		t.Fatalf("seed books: %v", err)
	}
	// Hide bob's only book without removing it: the row is still there
	// for a query that forgets the guard to find.
	if _, err := db.Update(books).
		Set(bookDeletedAt.Val(time.Now())).
		Where(bookTitle.Eq("bob-doomed")).
		Exec(ctx); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	return authors, books, published
}

func TestPGWhereHasAppliesTheRelatedTablesFilters(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	authors, _, published := whLibrary(t, db)

	read := func(f *pg.FindBuilder) []whAuthor {
		t.Helper()
		var rows []whAuthor
		if err := f.All(ctx, &rows); err != nil {
			sql, args := f.Select().ToSQL()
			t.Fatalf("PostgreSQL rejected the query: %v\n%s\nargs: %v", err, sql, args)
		}
		return rows
	}

	// bob's book is published — and soft-deleted. If the guard is not
	// inside the EXISTS he shows up here, which is the whole bug.
	got := whNames(read(db.Find(authors).WhereHas("books", func(q *pg.RelQuery) {
		q.Where(published.Eq(true))
	})))
	if got != "alice,erin" {
		t.Errorf("WhereHas(published) returned %v, want alice,erin — bob's only published book is soft-deleted", got)
	}

	// Any book at all: carol's draft counts, bob's deleted one does not.
	got = whNames(read(db.Find(authors).WhereHas("books", nil)))
	if got != "alice,carol,erin" {
		t.Errorf("WhereHas(books) returned %v, want alice,carol,erin", got)
	}

	// The negation is the same subquery, so bob counts as having none.
	got = whNames(read(db.Find(authors).WhereDoesntHave("books", nil)))
	if got != "bob,dave" {
		t.Errorf("WhereDoesntHave(books) returned %v, want bob,dave", got)
	}

	// Asking for the deleted row back is a thing a caller can do, by
	// name, without losing anything else.
	got = whNames(read(db.Find(authors).WhereHas("books", func(q *pg.RelQuery) {
		q.Where(published.Eq(true)).IgnoreFilters(pg.FilterSoftDelete)
	})))
	if got != "alice,bob,erin" {
		t.Errorf("IgnoreFilters(FilterSoftDelete) returned %v, want alice,bob,erin", got)
	}
}

func TestPGWhereHasBelongsToAndNesting(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	authors, books, published := whLibrary(t, db)

	// The inverse edge: books whose author is named erin.
	var rows []struct {
		Title string `drop:"title"`
	}
	if err := db.Find(books).WhereHas("author", func(q *pg.RelQuery) {
		q.Where(pg.Eq(authors.Col("name"), "erin"))
	}).OrderBy(books.Col("title").Asc()).All(ctx, &rows); err != nil {
		t.Fatalf("WhereHas(author): %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d of erin's books, want 3", len(rows))
	}

	// Nested: authors who have a book whose author is named erin. A
	// contrived path, but it exercises an EXISTS inside an EXISTS with
	// the intermediate table correlated correctly, which is the part
	// that breaks.
	var got []whAuthor
	if err := db.Find(authors).WhereHas("books", func(q *pg.RelQuery) {
		q.Where(published.Eq(true)).WhereHas("author", func(a *pg.RelQuery) {
			a.Where(pg.Eq(authors.Col("name"), "erin"))
		})
	}).All(ctx, &got); err != nil {
		t.Fatalf("nested WhereHas: %v", err)
	}
	if whNames(got) != "erin" {
		t.Errorf("nested WhereHas returned %v, want erin", whNames(got))
	}
}

func TestPGWhereHasCountBound(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	authors, _, _ := whLibrary(t, db)

	read := func(f *pg.FindBuilder) string {
		t.Helper()
		var rows []whAuthor
		if err := f.All(ctx, &rows); err != nil {
			sql, args := f.Select().ToSQL()
			t.Fatalf("PostgreSQL rejected the query: %v\n%s\nargs: %v", err, sql, args)
		}
		return whNames(rows)
	}

	if got := read(db.Find(authors).WhereHas("books", func(q *pg.RelQuery) {
		q.CountGte(3)
	})); got != "erin" {
		t.Errorf("CountGte(3) returned %v, want erin", got)
	}
	if got := read(db.Find(authors).WhereHas("books", func(q *pg.RelQuery) {
		q.CountGt(1)
	})); got != "erin" {
		t.Errorf("CountGt(1) returned %v, want erin", got)
	}
	// A count runs through the same filters: bob has one book in the
	// table and none the query can see.
	if got := read(db.Find(authors).WhereHas("books", func(q *pg.RelQuery) {
		q.CountEq(0)
	})); got != "bob,dave" {
		t.Errorf("CountEq(0) returned %v, want bob,dave", got)
	}
	// And the bound that a parent with no books satisfies has to
	// include those parents rather than quietly drop them.
	if got := read(db.Find(authors).WhereHas("books", func(q *pg.RelQuery) {
		q.CountLt(2)
	})); got != "alice,bob,carol,dave" {
		t.Errorf("CountLt(2) returned %v, want alice,bob,carol,dave", got)
	}
	// Negation folds into the operator: "does not have 3 or more".
	if got := read(db.Find(authors).WhereDoesntHave("books", func(q *pg.RelQuery) {
		q.CountGte(3)
	})); got != "alice,bob,carol,dave" {
		t.Errorf("WhereDoesntHave + CountGte(3) returned %v, want alice,bob,carol,dave", got)
	}
}

// Many-to-many is two joins inside one EXISTS, and both tables it
// touches carry filters that have to survive the trip.
func TestPGWhereHasManyToMany(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	posts := pg.NewTable(integration.UniqueName(t, "wh_p"))
	tags := pg.NewTable(integration.UniqueName(t, "wh_t"))
	postTags := pg.NewTable(integration.UniqueName(t, "wh_pt"))
	dropPG(t, db, postTags)
	dropPG(t, db, posts)
	dropPG(t, db, tags)

	postID := pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	postTitle := pg.Add(posts, pg.Text("title").NotNull())

	tagID := pg.Add(tags, pg.BigSerial("id").PrimaryKey())
	tagName := pg.Add(tags, pg.Text("name").NotNull())
	tagDeletedAt := pg.Add(tags, pg.Timestamp("deletedAt", true))
	tags.AddFilter(pg.FilterSoftDelete, pg.IsNull(tagDeletedAt))

	ptPostID := pg.Add(postTags, pg.BigInt("postId").NotNull())
	ptTagID := pg.Add(postTags, pg.BigInt("tagId").NotNull())
	ptApproved := pg.Add(postTags, pg.Boolean("approved").NotNull())
	postTags.AddFilter("approved", ptApproved.Eq(true))

	pg.NewRelations(posts).
		ManyToMany("tags", tags, postTags, ptPostID, ptTagID, postID, tagID)

	execPG(t, db, pg.CreateTable(posts))
	execPG(t, db, pg.CreateTable(tags))
	execPG(t, db, pg.CreateTable(postTags))

	if _, err := db.Insert(posts).
		Row(postTitle.Val("live-tag")).
		Row(postTitle.Val("deleted-tag")).
		Row(postTitle.Val("unapproved-tag")).
		Row(postTitle.Val("untagged")).
		Row(postTitle.Val("two-tags")).
		Exec(ctx); err != nil {
		t.Fatalf("seed posts: %v", err)
	}
	if _, err := db.Insert(tags).
		Row(tagName.Val("go")).
		Row(tagName.Val("gone")).
		Row(tagName.Val("sql")).
		Exec(ctx); err != nil {
		t.Fatalf("seed tags: %v", err)
	}

	var ps []whPost
	if err := db.Select().From(posts).OrderBy(postID.Asc()).All(ctx, &ps); err != nil {
		t.Fatalf("read posts: %v", err)
	}
	pid := map[string]int64{}
	for _, p := range ps {
		pid[p.Title] = p.ID
	}
	var ts []struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	if err := db.Select().From(tags).OrderBy(tagID.Asc()).All(ctx, &ts); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	tid := map[string]int64{}
	for _, tg := range ts {
		tid[tg.Name] = tg.ID
	}

	if _, err := db.Insert(postTags).
		Row(ptPostID.Val(pid["live-tag"]), ptTagID.Val(tid["go"]), ptApproved.Val(true)).
		Row(ptPostID.Val(pid["deleted-tag"]), ptTagID.Val(tid["gone"]), ptApproved.Val(true)).
		Row(ptPostID.Val(pid["unapproved-tag"]), ptTagID.Val(tid["go"]), ptApproved.Val(false)).
		Row(ptPostID.Val(pid["two-tags"]), ptTagID.Val(tid["go"]), ptApproved.Val(true)).
		Row(ptPostID.Val(pid["two-tags"]), ptTagID.Val(tid["sql"]), ptApproved.Val(true)).
		// The same link twice, so a count that counts junction rows
		// rather than distinct tags gets the wrong answer.
		Row(ptPostID.Val(pid["two-tags"]), ptTagID.Val(tid["sql"]), ptApproved.Val(true)).
		Exec(ctx); err != nil {
		t.Fatalf("seed junction: %v", err)
	}
	// Hide the tag "gone" — the post linked to it must stop matching.
	if _, err := db.Update(tags).
		Set(tagDeletedAt.Val(time.Now())).
		Where(tagName.Eq("gone")).
		Exec(ctx); err != nil {
		t.Fatalf("soft delete tag: %v", err)
	}

	titles := func(f *pg.FindBuilder) string {
		t.Helper()
		var rows []whPost
		if err := f.All(ctx, &rows); err != nil {
			sql, args := f.Select().ToSQL()
			t.Fatalf("PostgreSQL rejected the query: %v\n%s\nargs: %v", err, sql, args)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Title)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	if got := titles(db.Find(posts).WhereHas("tags", nil)); got != "live-tag,two-tags" {
		t.Errorf("WhereHas(tags) returned %v, want live-tag,two-tags — the deleted tag and the unapproved link must not count", got)
	}
	if got := titles(db.Find(posts).WhereDoesntHave("tags", nil)); got != "deleted-tag,unapproved-tag,untagged" {
		t.Errorf("WhereDoesntHave(tags) returned %v, want deleted-tag,unapproved-tag,untagged", got)
	}
	if got := titles(db.Find(posts).WhereHas("tags", func(q *pg.RelQuery) {
		q.Where(tagName.Eq("sql"))
	})); got != "two-tags" {
		t.Errorf("WhereHas(tags where name=sql) returned %v, want two-tags", got)
	}
	// two-tags has three junction rows and two distinct tags.
	if got := titles(db.Find(posts).WhereHas("tags", func(q *pg.RelQuery) {
		q.CountGte(3)
	})); got != "" {
		t.Errorf("CountGte(3) returned %v — the duplicated junction row inflated the count", got)
	}
	if got := titles(db.Find(posts).WhereHas("tags", func(q *pg.RelQuery) {
		q.CountEq(2)
	})); got != "two-tags" {
		t.Errorf("CountEq(2) returned %v, want two-tags", got)
	}
}

// A self-referencing relation has no correlated EXISTS to write, so it
// takes the semi-join path. The server is the only place to prove that
// path means what the EXISTS would have.
func TestPGWhereHasSelfReferencing(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	emp := pg.NewTable(integration.UniqueName(t, "wh_e"))
	dropPG(t, db, emp)
	id := pg.Add(emp, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(emp, pg.Text("name").NotNull())
	managerID := pg.Add(emp, pg.BigInt("managerId"))
	active := pg.Add(emp, pg.Boolean("active").NotNull())
	pg.NewRelations(emp).
		HasMany("reports", emp, id, managerID).
		BelongsTo("manager", emp, managerID, id)
	execPG(t, db, pg.CreateTable(emp))

	if _, err := db.Insert(emp).
		Row(name.Val("root"), active.Val(true)).
		Exec(ctx); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	var rows []whEmployee
	if err := db.Select().From(emp).All(ctx, &rows); err != nil {
		t.Fatalf("read root: %v", err)
	}
	rootID := rows[0].ID

	// root manages ann (active), bob (inactive) and cat (active).
	// ann manages nobody. dana has no manager at all.
	if _, err := db.Insert(emp).
		Row(name.Val("ann"), managerID.Val(rootID), active.Val(true)).
		Row(name.Val("bob"), managerID.Val(rootID), active.Val(false)).
		Row(name.Val("cat"), managerID.Val(rootID), active.Val(true)).
		Row(name.Val("dana"), active.Val(true)).
		Exec(ctx); err != nil {
		t.Fatalf("seed staff: %v", err)
	}

	read := func(f *pg.FindBuilder) string {
		t.Helper()
		var got []whEmployee
		if err := f.All(ctx, &got); err != nil {
			sql, args := f.Select().ToSQL()
			t.Fatalf("PostgreSQL rejected the query: %v\n%s\nargs: %v", err, sql, args)
		}
		out := make([]string, 0, len(got))
		for _, r := range got {
			out = append(out, r.Name)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	if got := read(db.Find(emp).WhereHas("reports", func(q *pg.RelQuery) {
		q.Where(active.Eq(true))
	})); got != "root" {
		t.Errorf("WhereHas(reports active) returned %v, want root", got)
	}
	if got := read(db.Find(emp).WhereHas("reports", func(q *pg.RelQuery) {
		q.CountGte(3)
	})); got != "root" {
		t.Errorf("WhereHas(reports).CountGte(3) returned %v, want root", got)
	}
	if got := read(db.Find(emp).WhereHas("reports", func(q *pg.RelQuery) {
		q.CountGte(4)
	})); got != "" {
		t.Errorf("WhereHas(reports).CountGte(4) returned %v, want nothing", got)
	}
	// dana has no manager. NOT EXISTS keeps her, and a bare NOT IN
	// against a column holding NULLs would have thrown away every row.
	if got := read(db.Find(emp).WhereDoesntHave("manager", nil)); got != "dana,root" {
		t.Errorf("WhereDoesntHave(manager) returned %v, want dana,root", got)
	}
	if got := read(db.Find(emp).WhereHas("manager", nil)); got != "ann,bob,cat" {
		t.Errorf("WhereHas(manager) returned %v, want ann,bob,cat", got)
	}
}

// A many-to-many that points back at its own table — followers — is
// both awkward cases at once: two joins, and a target that shadows the
// table being filtered. It takes the semi-join path with the junction
// as the subquery's FROM.
func TestPGWhereHasSelfReferencingManyToMany(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	users := pg.NewTable(integration.UniqueName(t, "wh_u"))
	follows := pg.NewTable(integration.UniqueName(t, "wh_f"))
	dropPG(t, db, users)
	dropPG(t, db, follows)

	userID := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	userName := pg.Add(users, pg.Text("name").NotNull())
	userActive := pg.Add(users, pg.Boolean("active").NotNull())

	followerID := pg.Add(follows, pg.BigInt("followerId").NotNull().References(userID))
	followeeID := pg.Add(follows, pg.BigInt("followeeId").NotNull().References(userID))

	// "following": the users this row follows, through follows.
	pg.NewRelations(users).
		ManyToMany("following", users, follows, followerID, followeeID, userID, userID)

	execPG(t, db, pg.CreateTable(users))
	execPG(t, db, pg.CreateTable(follows))

	if _, err := db.Insert(users).
		Row(userName.Val("ann"), userActive.Val(true)).
		Row(userName.Val("bob"), userActive.Val(false)).
		Row(userName.Val("cat"), userActive.Val(true)).
		Row(userName.Val("dan"), userActive.Val(true)).
		Exec(ctx); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	var us []whAuthor
	if err := db.Select().From(users).OrderBy(userID.Asc()).All(ctx, &us); err != nil {
		t.Fatalf("read users: %v", err)
	}
	uid := map[string]int64{}
	for _, u := range us {
		uid[u.Name] = u.ID
	}

	// ann follows bob (inactive) and cat (active); dan follows bob only.
	if _, err := db.Insert(follows).
		Row(followerID.Val(uid["ann"]), followeeID.Val(uid["bob"])).
		Row(followerID.Val(uid["ann"]), followeeID.Val(uid["cat"])).
		Row(followerID.Val(uid["dan"]), followeeID.Val(uid["bob"])).
		Exec(ctx); err != nil {
		t.Fatalf("seed follows: %v", err)
	}

	read := func(f *pg.FindBuilder) string {
		t.Helper()
		var got []whAuthor
		if err := f.All(ctx, &got); err != nil {
			sql, args := f.Select().ToSQL()
			t.Fatalf("PostgreSQL rejected the query: %v\n%s\nargs: %v", err, sql, args)
		}
		return whNames(got)
	}

	if got := read(db.Find(users).WhereHas("following", nil)); got != "ann,dan" {
		t.Errorf("WhereHas(following) returned %v, want ann,dan", got)
	}
	// The predicate is about the followee, not the follower: only ann
	// follows somebody active, and cat is that somebody.
	if got := read(db.Find(users).WhereHas("following", func(q *pg.RelQuery) {
		q.Where(userActive.Eq(true))
	})); got != "ann" {
		t.Errorf("WhereHas(following active) returned %v, want ann — dan only follows the inactive bob", got)
	}
	if got := read(db.Find(users).WhereDoesntHave("following", nil)); got != "bob,cat" {
		t.Errorf("WhereDoesntHave(following) returned %v, want bob,cat", got)
	}
	if got := read(db.Find(users).WhereHas("following", func(q *pg.RelQuery) {
		q.CountGte(2)
	})); got != "ann" {
		t.Errorf("WhereHas(following).CountGte(2) returned %v, want ann", got)
	}
}

type whTenantedAuthor struct {
	ID    int64  `drop:"id"`
	OrgID int64  `drop:"orgId"`
	Name  string `drop:"name"`
}

type whTenantedBook struct {
	ID       int64  `drop:"id"`
	OrgID    int64  `drop:"orgId"`
	AuthorID int64  `drop:"authorId"`
	Title    string `drop:"title"`
}

// Two tenants share the tables, and the question is what a WhereHas
// does with the other one's rows. The answer is not symmetric, and the
// asymmetry is the thing worth pinning:
//
//   - The parent side is covered. Entity.ScopeByTenant narrows the
//     outer SELECT, so another tenant's authors never appear.
//   - The related side is not. The tenant axis is not a filter the
//     table carries — it is built per query from the ctx tenant, which
//     a *Table cannot see — so it is not among the filters WhereHas
//     threads into the subquery, and neither is it among the ones
//     RelQuery.IgnoreFilters can name.
//
// That is the same gap the eager loaders have: With("books") on a
// tenant-scoped author loads the other tenant's books too. It is
// recorded here rather than left to be discovered, because "the related
// table's global filters are inside the subquery" reads like a promise
// that covers tenancy, and it does not. Until a relation can be told
// about the axis, a cross-tenant edge has to be excluded by hand — the
// callback's Where is where it goes.
func TestPGWhereHasAndTheTenantAxis(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	authors := pg.NewTable(integration.UniqueName(t, "wt_a"))
	books := pg.NewTable(integration.UniqueName(t, "wt_b"))
	dropPG(t, db, authors)
	dropPG(t, db, books)

	authorID := pg.Add(authors, pg.BigSerial("id").PrimaryKey())
	authorOrg := pg.Add(authors, pg.BigInt("orgId").NotNull())
	authorName := pg.Add(authors, pg.Text("name").NotNull())

	pg.Add(books, pg.BigSerial("id").PrimaryKey())
	bookOrg := pg.Add(books, pg.BigInt("orgId").NotNull())
	bookAuthorID := pg.Add(books, pg.BigInt("authorId").NotNull())
	bookTitle := pg.Add(books, pg.Text("title").NotNull())
	bookDeletedAt := pg.Add(books, pg.Timestamp("deletedAt", true))
	books.AddFilter(pg.FilterSoftDelete, pg.IsNull(bookDeletedAt))

	pg.NewRelations(authors).HasMany("books", books, authorID, bookAuthorID)

	execPG(t, db, pg.CreateTable(authors))
	execPG(t, db, pg.CreateTable(books))

	authorEnt := pg.NewEntity[whTenantedAuthor](authors).ScopeByTenant(authorOrg)

	// alice  org 1, one org-1 book
	// dave   org 1, one book — belonging to org 2
	// gil    org 1, one org-1 book, soft-deleted
	// eve    org 2, one org-2 book
	if _, err := db.Insert(authors).
		Row(authorOrg.Val(int64(1)), authorName.Val("alice")).
		Row(authorOrg.Val(int64(1)), authorName.Val("dave")).
		Row(authorOrg.Val(int64(1)), authorName.Val("gil")).
		Row(authorOrg.Val(int64(2)), authorName.Val("eve")).
		Exec(ctx); err != nil {
		t.Fatalf("seed authors: %v", err)
	}
	var all []whTenantedAuthor
	if err := db.Select().From(authors).OrderBy(authorID.Asc()).All(ctx, &all); err != nil {
		t.Fatalf("read authors: %v", err)
	}
	id := map[string]int64{}
	for _, a := range all {
		id[a.Name] = a.ID
	}
	if _, err := db.Insert(books).
		Row(bookOrg.Val(int64(1)), bookAuthorID.Val(id["alice"]), bookTitle.Val("alice-1")).
		Row(bookOrg.Val(int64(2)), bookAuthorID.Val(id["dave"]), bookTitle.Val("dave-other-org")).
		Row(bookOrg.Val(int64(1)), bookAuthorID.Val(id["gil"]), bookTitle.Val("gil-doomed")).
		Row(bookOrg.Val(int64(2)), bookAuthorID.Val(id["eve"]), bookTitle.Val("eve-1")).
		Exec(ctx); err != nil {
		t.Fatalf("seed books: %v", err)
	}
	if _, err := db.Update(books).
		Set(bookDeletedAt.Val(time.Now())).
		Where(bookTitle.Eq("gil-doomed")).
		Exec(ctx); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	tctx := pg.WithTenant(ctx, int64(1))
	read := func(q *pg.EntityQuery[whTenantedAuthor]) string {
		t.Helper()
		rows, err := q.All(tctx)
		if err != nil {
			t.Fatalf("PostgreSQL rejected the query: %v", err)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Name)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	// The parent side: eve belongs to org 2 and is gone, and gil's only
	// book is soft-deleted so the subquery cannot see it.
	if got := read(authorEnt.Query(db).WhereHas("books", nil)); got != "alice,dave" {
		t.Errorf("WhereHas(books) under tenant 1 returned %v, want alice,dave", got)
	}

	// The related side, pinned as it is rather than as it reads: dave is
	// in that answer, and the only book he has belongs to org 2. Change
	// this expectation the day a relation learns about the tenant axis —
	// and change the doc comment on WhereHas with it.
	var daveBooks []whTenantedBook
	if err := db.Select().From(books).Where(bookAuthorID.Eq(id["dave"])).All(ctx, &daveBooks); err != nil {
		t.Fatalf("read dave's books: %v", err)
	}
	if len(daveBooks) != 1 || daveBooks[0].OrgID != 2 {
		t.Fatalf("the fixture no longer puts dave's only book in org 2: %+v", daveBooks)
	}

	// Which is to say: the exclusion has to be written out, and once it
	// is, the callback's Where reaches it.
	if got := read(authorEnt.Query(db).WhereHas("books", func(q *pg.RelQuery) {
		q.Where(bookOrg.Eq(int64(1)))
	})); got != "alice" {
		t.Errorf("WhereHas(books in org 1) returned %v, want alice", got)
	}

	// And naming the tenant filter does nothing, because it was never in
	// the subquery to bypass — an unknown name is kept rather than
	// rejected, so this is silent.
	if got := read(authorEnt.Query(db).WhereHas("books", func(q *pg.RelQuery) {
		q.IgnoreFilters(pg.FilterTenant)
	})); got != "alice,dave" {
		t.Errorf("IgnoreFilters(FilterTenant) inside a WhereHas changed the answer to %v", got)
	}
}
