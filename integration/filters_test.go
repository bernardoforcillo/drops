package integration_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
)

// filterRow is the scan target for the named-filter tests. deletedAt
// is nullable on the table, so the field has to be able to hold a
// NULL — NewEntity refuses the pairing otherwise.
type filterRow struct {
	ID        int64      `drop:"id"`
	TenantID  int64      `drop:"tenantId"`
	Name      string     `drop:"name"`
	DeletedAt *time.Time `drop:"deletedAt"`
}

func filterNames(rows []filterRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// Two filters on one table, two tenants in it, and one soft-deleted
// row: the shape where an all-or-nothing Unscoped leaks. Reading the
// deleted row must not cost the tenancy predicate — the server has the
// other tenant's rows sitting right there to prove it either way.
func TestPGIgnoreFilterKeepsTheOtherGuard(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "filters"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenantID := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	pg.ApplyMixins(tbl, &pg.SoftDeleteMixin{})
	tbl.AddFilter(pg.FilterTenant, tenantID.Eq(int64(7)))
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).
		Row(tenantID.Val(7), name.Val("mine-live")).
		Row(tenantID.Val(7), name.Val("mine-deleted")).
		Row(tenantID.Val(8), name.Val("theirs-live")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// A DELETE against a soft-deleted table is rewritten into an
	// UPDATE, so this hides the row rather than removing it.
	if _, err := db.Delete(tbl).Where(name.Eq("mine-deleted")).Exec(ctx); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	read := func(q *pg.SelectBuilder) []filterRow {
		t.Helper()
		var rows []filterRow
		if err := q.All(ctx, &rows); err != nil {
			t.Fatalf("select: %v", err)
		}
		return rows
	}
	// A SelectBuilder is stateful, so each branch below gets its own.
	cols := func() *pg.SelectBuilder {
		return db.Select(id, tenantID, name, tbl.Col("deletedAt")).From(tbl)
	}

	// Default: this tenant's live rows only.
	got := filterNames(read(cols()))
	if strings.Join(got, ",") != "mine-live" {
		t.Errorf("default scoping returned %v, want just mine-live", got)
	}

	// Naming one filter drops that one. The deleted row comes back and
	// the other tenant's does not.
	got = filterNames(read(cols().IgnoreFilters(pg.FilterSoftDelete)))
	if strings.Join(got, ",") != "mine-deleted,mine-live" {
		t.Errorf("IgnoreFilters(FilterSoftDelete) returned %v, want both of tenant 7's rows and nothing else", got)
	}

	// The blunt instrument, on the same data, doing exactly what its
	// doc comment says: everything goes, tenancy included. This is the
	// leak IgnoreFilters exists to avoid, pinned so it stays a
	// deliberate choice rather than the only option.
	got = filterNames(read(cols().Unscoped()))
	if len(got) != 3 {
		t.Errorf("Unscoped returned %v, want every row in the table", got)
	}
}

// The same guarantee where tenancy comes from the context rather than
// the table: a soft-delete bypass must still scope to the caller's
// tenant, and it has to prove it by returning that tenant's hidden row
// while the other tenant's rows sit in the same table.
func TestPGEntityIgnoreSoftDeleteStaysInsideTheTenant(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "tenant_filters"))
	dropPG(t, db, tbl)

	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenantID := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	pg.ApplyMixins(tbl, &pg.SoftDeleteMixin{})
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).
		Row(tenantID.Val(7), name.Val("mine-live")).
		Row(tenantID.Val(7), name.Val("mine-deleted")).
		Row(tenantID.Val(8), name.Val("theirs-live")).
		Row(tenantID.Val(8), name.Val("theirs-deleted")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for _, n := range []string{"mine-deleted", "theirs-deleted"} {
		if _, err := db.Delete(tbl).Where(name.Eq(n)).Exec(ctx); err != nil {
			t.Fatalf("soft delete %s: %v", n, err)
		}
	}

	ent := pg.NewEntity[filterRow](tbl).ScopeByTenant(tenantID)
	mine := pg.WithTenant(ctx, int64(7))

	live, err := ent.Query(db).All(mine)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := filterNames(live); strings.Join(got, ",") != "mine-live" {
		t.Errorf("default query returned %v, want just mine-live", got)
	}

	all, err := ent.Query(db).IgnoreFilters(pg.FilterSoftDelete).All(mine)
	if err != nil {
		t.Fatalf("IgnoreFilters(FilterSoftDelete): %v", err)
	}
	got := filterNames(all)
	if strings.Join(got, ",") != "mine-deleted,mine-live" {
		t.Fatalf("bypassing soft delete returned %v, want tenant 7's two rows", got)
	}
	// Said plainly, because it is the whole point: the hidden row came
	// back, and not one row of tenant 8's came with it.
	for _, r := range all {
		if r.TenantID != 7 {
			t.Errorf("row from tenant %d leaked through a soft-delete bypass: %+v", r.TenantID, r)
		}
	}

	// Unscoped drops the table's filters and still does not reach the
	// ctx tenant guard.
	unscoped, err := ent.Query(db).Unscoped().All(mine)
	if err != nil {
		t.Fatalf("Unscoped: %v", err)
	}
	for _, r := range unscoped {
		if r.TenantID != 7 {
			t.Errorf("Unscoped reached the ctx tenant guard: %+v", r)
		}
	}

	// Crossing tenants stays possible, but only by saying so.
	cross, err := ent.Query(db).IgnoreFilters(pg.FilterTenant).All(mine)
	if err != nil {
		t.Fatalf("IgnoreFilters(FilterTenant): %v", err)
	}
	if got := filterNames(cross); strings.Join(got, ",") != "mine-live,theirs-live" {
		t.Errorf("cross-tenant read returned %v, want both tenants' live rows", got)
	}
}

// --- strict loading ----------------------------------------------------

type strictAuthor struct {
	ID    int64        `drop:"id"`
	Name  string       `drop:"name"`
	Books []strictBook `dropRel:"books"`
}

type strictBook struct {
	ID       int64  `drop:"id"`
	AuthorID int64  `drop:"authorId"`
	Title    string `drop:"title"`
}

// Against a real server with real rows: the author has a book, and the
// query that forgets to load it is refused rather than answering "no
// books". The row that would have been silently missing is right there
// in the table, so the two branches are told apart by data and not by
// rendered SQL.
func TestPGStrictLoadingRefusesTheSilentlyEmptyRelation(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	authors := pg.NewTable(integration.UniqueName(t, "authors"))
	books := pg.NewTable(integration.UniqueName(t, "books"))
	dropPG(t, db, books)
	dropPG(t, db, authors)

	aID := pg.Add(authors, pg.BigSerial("id").PrimaryKey())
	aName := pg.Add(authors, pg.Text("name").NotNull())
	pg.Add(books, pg.BigSerial("id").PrimaryKey())
	bAuthor := pg.Add(books, pg.BigInt("authorId").NotNull())
	bTitle := pg.Add(books, pg.Text("title").NotNull())
	pg.NewRelations(authors).HasMany("books", books, aID, bAuthor)
	execPG(t, db, pg.CreateTable(authors))
	execPG(t, db, pg.CreateTable(books))

	var author struct {
		ID int64 `drop:"id"`
	}
	if err := db.Insert(authors).Row(aName.Val("Le Guin")).
		Returning(aID).One(ctx, &author); err != nil {
		t.Fatalf("seed author: %v", err)
	}
	if _, err := db.Insert(books).
		Row(bAuthor.Val(author.ID), bTitle.Val("A Wizard of Earthsea")).
		Exec(ctx); err != nil {
		t.Fatalf("seed book: %v", err)
	}

	// Lenient: the query succeeds and the author looks book-less.
	var lenient []strictAuthor
	if err := db.Find(authors).All(ctx, &lenient); err != nil {
		t.Fatalf("lenient All: %v", err)
	}
	if len(lenient) != 1 || lenient[0].Books != nil {
		t.Fatalf("expected one author with a nil Books, got %+v", lenient)
	}

	// Strict: the same query is refused, and the message says which
	// relation and which call.
	strict := db.StrictLoading()
	var refused []strictAuthor
	err := strict.Find(authors).All(ctx, &refused)
	if !errors.Is(err, pg.ErrRelationNotLoaded) {
		t.Fatalf("strict All: want ErrRelationNotLoaded, got %v", err)
	}
	if !strings.Contains(err.Error(), `"books"`) || !strings.Contains(err.Error(), ".Load(") {
		t.Errorf("the refusal must name the relation and the call: %v", err)
	}

	// Loading it satisfies the check and returns the row that was
	// silently missing a moment ago.
	var loaded []strictAuthor
	if err := strict.Find(authors).Load(authors.Rel("books")).All(ctx, &loaded); err != nil {
		t.Fatalf("strict All with Load: %v", err)
	}
	if len(loaded) != 1 || len(loaded[0].Books) != 1 || loaded[0].Books[0].Title != "A Wizard of Earthsea" {
		t.Fatalf("expected the author's one book, got %+v", loaded)
	}

	// So does declining it out loud — and then the nil means what it
	// says, because somebody wrote down that they would not read it.
	var waived []strictAuthor
	if err := strict.Find(authors).NoLoad(authors.Rel("books")).All(ctx, &waived); err != nil {
		t.Fatalf("strict All with NoLoad: %v", err)
	}
	if len(waived) != 1 || waived[0].Books != nil {
		t.Fatalf("expected one author with a nil Books, got %+v", waived)
	}
}

// --- SQLite: a soft delete must not step outside the tenancy guard ----

type sqliteFilterRow struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// SoftDeleteByID and Restore have to reach a row the soft-delete guard
// is hiding, which used to mean Unscoped — and Unscoped drops every
// other filter on the table with it. Against a real engine that is a
// write to another tenant's row, so the fix is worth proving by what
// the table holds afterwards rather than by the SQL text.
func TestSQLiteSoftDeleteStaysInsideTheTenancyGuard(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("scoped_posts")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	tenantID := sqlite.Add(tbl, sqlite.BigInt("tenantId").NotNull())
	title := sqlite.Add(tbl, sqlite.Text("title").NotNull())
	sd := sqlite.SoftDelete(tbl)
	tbl.AddFilter(sqlite.FilterTenant, tenantID.Eq(int64(7)))
	exec(t, db, sqlite.CreateTable(tbl))

	if _, err := db.Insert(tbl).
		Values(id.Val(1), tenantID.Val(7), title.Val("mine")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Insert(tbl).
		Values(id.Val(2), tenantID.Val(8), title.Val("theirs")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ent := sqlite.NewEntity[sqliteFilterRow](tbl)

	// Hiding another tenant's row must do nothing: the tenancy guard
	// is not what SoftDeleteByID is stepping around.
	if _, err := ent.SoftDeleteByID(db, ctx, int64(2), sd); err != nil {
		t.Fatalf("SoftDeleteByID: %v", err)
	}
	var theirs []sqliteFilterRow
	if err := db.Select(id, tenantID, title).From(tbl).Unscoped().
		Where(id.Eq(int64(2)), sd.DeletedAt.IsNull()).All(ctx, &theirs); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(theirs) != 1 {
		t.Error("a soft delete reached across the tenancy guard and hid another tenant's row")
	}

	// This tenant's row hides and comes back, guard intact throughout.
	if _, err := ent.SoftDeleteByID(db, ctx, int64(1), sd); err != nil {
		t.Fatalf("SoftDeleteByID: %v", err)
	}
	live, err := ent.Query(db).All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("the row should be hidden, got %+v", live)
	}
	if _, err := ent.Restore(db, ctx, int64(1), sd); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	back, err := ent.Query(db).All(ctx)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if len(back) != 1 || back[0].Title != "mine" {
		t.Errorf("Restore did not bring the row back: %+v", back)
	}
}

// A tenant-scoped entity promises that every read is narrowed to the
// ctx tenant. Stream and Page used to build their statements without
// asking for the predicate, which made an export or a paginated
// listing the one way to read every tenant's rows at once. Against a
// server with two tenants in the table, the leak is visible in the
// rows themselves.
func TestPGStreamAndPageStayInsideTheTenant(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := pg.NewTable(integration.UniqueName(t, "tenant_reads"))
	dropPG(t, db, tbl)

	id := pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	tenantID := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	name := pg.Add(tbl, pg.Text("name").NotNull())
	pg.Add(tbl, pg.Timestamp("deletedAt", true).Nullable())
	execPG(t, db, pg.CreateTable(tbl))

	if _, err := db.Insert(tbl).
		Row(tenantID.Val(7), name.Val("mine-a")).
		Row(tenantID.Val(7), name.Val("mine-b")).
		Row(tenantID.Val(8), name.Val("theirs")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ent := pg.NewEntity[filterRow](tbl).ScopeByTenant(tenantID)
	mine := pg.WithTenant(ctx, int64(7))

	var streamed []filterRow
	if err := ent.Query(db).Stream(mine, func(r *filterRow) error {
		streamed = append(streamed, *r)
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := filterNames(streamed); strings.Join(got, ",") != "mine-a,mine-b" {
		t.Errorf("Stream returned %v, want only tenant 7's rows", got)
	}

	page, err := ent.Page(db).OrderBy(pg.Asc(id)).Limit(10).All(mine)
	if err != nil {
		t.Fatalf("Page: %v", err)
	}
	if got := filterNames(page.Items); strings.Join(got, ",") != "mine-a,mine-b" {
		t.Errorf("Page returned %v, want only tenant 7's rows", got)
	}

	// And both fail closed when the ctx carries no tenant at all,
	// rather than quietly reading the whole table.
	if err := ent.Query(db).Stream(ctx, func(*filterRow) error { return nil }); !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("Stream without a tenant: want ErrTenantMissing, got %v", err)
	}
	if _, err := ent.Page(db).OrderBy(pg.Asc(id)).Limit(10).All(ctx); !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("Page without a tenant: want ErrTenantMissing, got %v", err)
	}
}

// --- a per-parent Limit must not widen the relation's own scope ------

type limitedAuthor struct {
	ID    int64         `drop:"id"`
	Name  string        `drop:"name"`
	Books []limitedBook `dropRel:"books"`
}

type limitedBook struct {
	ID        int64      `drop:"id"`
	AuthorID  int64      `drop:"authorId"`
	Title     string     `drop:"title"`
	DeletedAt *time.Time `drop:"deletedAt"`
}

// An eager-loaded relation is filtered by its own table's guards — the
// child SELECT is built From(rel.To) like any other, so a soft-deleted
// or out-of-tenant child never reaches the parent's slice. Asking for
// a per-parent cap takes a different route: RelConfig.Limit rewrites
// the child query into a hand-written ROW_NUMBER() statement, and that
// writer has to carry the same guards the builder would have applied.
// It did not, so adding .Limit(n) to a load silently widened what came
// back — and on a tenant-scoped child table that is another customer's
// row, arriving through a call that only asked for fewer of its own.
//
// The two loads below differ by nothing but the cap, so the engine's
// answer to them has to agree.
func TestPGPerParentLimitKeepsTheRelationsFilters(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	authors := pg.NewTable(integration.UniqueName(t, "limit_authors"))
	books := pg.NewTable(integration.UniqueName(t, "limit_books"))
	dropPG(t, db, books)
	dropPG(t, db, authors)

	aID := pg.Add(authors, pg.BigSerial("id").PrimaryKey())
	aName := pg.Add(authors, pg.Text("name").NotNull())
	pg.Add(books, pg.BigSerial("id").PrimaryKey())
	bAuthor := pg.Add(books, pg.BigInt("authorId").NotNull())
	bTenant := pg.Add(books, pg.BigInt("tenantId").NotNull())
	bTitle := pg.Add(books, pg.Text("title").NotNull())
	pg.ApplyMixins(books, &pg.SoftDeleteMixin{})
	books.AddFilter(pg.FilterTenant, bTenant.Eq(int64(7)))
	pg.NewRelations(authors).HasMany("books", books, aID, bAuthor)
	execPG(t, db, pg.CreateTable(authors))
	execPG(t, db, pg.CreateTable(books))

	var author struct {
		ID int64 `drop:"id"`
	}
	if err := db.Insert(authors).Row(aName.Val("Le Guin")).
		Returning(aID).One(ctx, &author); err != nil {
		t.Fatalf("seed author: %v", err)
	}
	if _, err := db.Insert(books).
		Row(bAuthor.Val(author.ID), bTenant.Val(7), bTitle.Val("mine-live")).
		Row(bAuthor.Val(author.ID), bTenant.Val(7), bTitle.Val("mine-deleted")).
		Row(bAuthor.Val(author.ID), bTenant.Val(8), bTitle.Val("theirs")).
		Exec(ctx); err != nil {
		t.Fatalf("seed books: %v", err)
	}
	// A DELETE on a soft-deleted table is rewritten into an UPDATE,
	// and the tenancy filter keeps it on tenant 7's row.
	if _, err := db.Delete(books).Where(bTitle.Eq("mine-deleted")).Exec(ctx); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	titles := func(as []limitedAuthor) []string {
		t.Helper()
		if len(as) != 1 {
			t.Fatalf("expected one author, got %d", len(as))
		}
		out := make([]string, 0, len(as[0].Books))
		for _, b := range as[0].Books {
			out = append(out, b.Title)
		}
		sort.Strings(out)
		return out
	}

	var plain []limitedAuthor
	if err := db.Find(authors).Load(authors.Rel("books")).All(ctx, &plain); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := strings.Join(titles(plain), ","); got != "mine-live" {
		t.Fatalf("uncapped load returned %v, want just the live in-tenant book", got)
	}

	// Same load, capped high enough that the cap excludes nothing.
	// Only the table's own guards may narrow it, and they must narrow
	// it to exactly what the uncapped load returned.
	var capped []limitedAuthor
	if err := db.Find(authors).
		LoadRel(authors.Rel("books"), func(c *pg.RelConfig) { c.Limit(10) }).
		All(ctx, &capped); err != nil {
		t.Fatalf("capped load: %v", err)
	}
	if got := strings.Join(titles(capped), ","); got != "mine-live" {
		t.Errorf("a per-parent Limit widened the relation's scope: got %v, want just the live in-tenant book", got)
	}
}

// --- MariaDB: the same guarantee, on the engine that renders it ------

type mysqlFilterRow struct {
	ID        int64      `drop:"id"`
	TenantID  int64      `drop:"tenantId"`
	Name      string     `drop:"name"`
	DeletedAt *time.Time `drop:"deletedAt"`
}

// drops/mysql grew the same named filters as pg and sqlite, but its
// rendering is its own — backtick idents, `?` placeholders, and no
// RETURNING — so "the string looks right" is worth less here than
// anywhere. The engine gets a table with two guards on it and two
// tenants in it, and the assertion is what MariaDB hands back: from a
// SELECT, and from the UPDATE and DELETE that carry the same filters
// into a write.
func TestMySQLIgnoreFilterKeepsTheOtherGuard(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	tbl := mysql.NewTable(integration.UniqueName(t, "filters"))
	dropMySQL(t, db, tbl)

	id := mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	tenantID := mysql.Add(tbl, mysql.BigInt("tenantId").NotNull())
	name := mysql.Add(tbl, mysql.Text("name").NotNull())
	deletedAt := mysql.Add(tbl, mysql.Timestamp("deletedAt", false).Nullable())
	tbl.AddFilter(mysql.FilterSoftDelete, deletedAt.IsNull())
	tbl.AddFilter(mysql.FilterTenant, tenantID.Eq(int64(7)))
	execMySQL(t, db, mysql.CreateTable(tbl))

	if _, err := db.Insert(tbl).
		Row(tenantID.Val(7), name.Val("mine-live")).
		Row(tenantID.Val(7), name.Val("mine-deleted")).
		Row(tenantID.Val(8), name.Val("theirs-live")).
		Row(tenantID.Val(8), name.Val("theirs-deleted")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Hide one row per tenant, reaching past the soft-delete guard by
	// name so the tenancy guard still says which row may be touched.
	// If it did not, this single statement would hide both.
	if _, err := db.Update(tbl).IgnoreFilters(mysql.FilterSoftDelete).
		SetExpr(deletedAt.Column, drops.Raw("CURRENT_TIMESTAMP")).
		Where(mysql.Like(name, "%-deleted")).
		Exec(ctx); err != nil {
		t.Fatalf("hide: %v", err)
	}

	read := func(q *mysql.SelectBuilder) []string {
		t.Helper()
		var rows []mysqlFilterRow
		if err := q.All(ctx, &rows); err != nil {
			text, args := q.ToSQL()
			t.Fatalf("select: %v\n%s\nargs: %v", err, text, args)
		}
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Name)
		}
		sort.Strings(out)
		return out
	}
	cols := func() *mysql.SelectBuilder {
		return db.Select(id, tenantID, name, deletedAt).From(tbl)
	}

	// The UPDATE above stayed inside the tenancy guard, so tenant 8's
	// row is untouched and both tenants still have a live row.
	if got := read(cols()); strings.Join(got, ",") != "mine-live" {
		t.Fatalf("default scoping returned %v, want just mine-live", got)
	}
	if got := read(cols().Unscoped().Where(tenantID.Eq(int64(8)), deletedAt.IsNotNull())); len(got) != 0 {
		t.Errorf("the UPDATE reached tenant 8's row: %v", got)
	}

	// Naming one guard drops that one only.
	if got := read(cols().IgnoreFilters(mysql.FilterSoftDelete)); strings.Join(got, ",") != "mine-deleted,mine-live" {
		t.Errorf("IgnoreFilters(FilterSoftDelete) returned %v, want both of tenant 7's rows", got)
	}
	// The blunt instrument drops both, as documented.
	if got := read(cols().Unscoped()); len(got) != 4 {
		t.Errorf("Unscoped returned %v, want every row", got)
	}

	// A DELETE carries the filters too: with the soft-delete guard
	// named, this addresses tenant 7's hidden row and no other.
	res, err := db.Delete(tbl).IgnoreFilters(mysql.FilterSoftDelete).
		Where(mysql.Like(name, "%-deleted")).Exec(ctx)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Errorf("DELETE removed %d row(s), want only tenant 7's hidden one", n)
	}
	if got := read(cols().Unscoped()); strings.Join(got, ",") != "mine-live,theirs-deleted,theirs-live" {
		t.Errorf("after the delete the table holds %v", got)
	}
}
