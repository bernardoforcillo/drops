package integration_test

import (
	"context"
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

// And() and Or() with nothing left to join render the identity of the
// connective — TRUE and FALSE — and a join whose condition is nil
// renders ON TRUE. Whether those are terms the engine actually accepts
// is not something a string comparison can establish: SQLite only
// learned the TRUE and FALSE keywords in 3.23, and "ON TRUE" is a join
// with no equality for the planner to key on. So each engine is asked
// directly, and asked to agree on the answer: TRUE selects everything,
// FALSE selects nothing.

// rowCount renders a built statement and runs it, returning the single
// count it selected. It goes through ToSQL and the dialect DB's Query
// rather than a builder method so the three legs are the same code.
func rowCount(t *testing.T, db interface {
	Query(context.Context, string, ...any) (drops.Rows, error)
}, q interface{ ToSQL() (string, []any) }) int64 {
	t.Helper()
	text, args := q.ToSQL()
	rows, err := db.Query(context.Background(), text, args...)
	if err != nil {
		t.Fatalf("the engine rejected the statement: %v\n%s", err, text)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("no row came back from %s: %v", text, rows.Err())
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return n
}

func TestPGEmptyPredicatesAreAcceptedByTheServer(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "nilpred"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	execPG(t, db, pg.CreateTable(tbl))
	for i := int64(1); i <= 2; i++ {
		if _, err := db.Insert(tbl).Row(id.Val(i)).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n := pg.As(pg.CountAll(), "n")
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(pg.And())); got != 2 {
		t.Errorf("WHERE TRUE returned %d rows, want 2", got)
	}
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(pg.Or())); got != 0 {
		t.Errorf("WHERE FALSE returned %d rows, want 0", got)
	}
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(nil, nil)); got != 2 {
		t.Errorf("an all-nil Where returned %d rows, want 2", got)
	}
	// ON TRUE: a two-row table joined to itself unconditionally.
	other := tbl.As("other")
	if got := rowCount(t, db, db.Select(n).From(tbl).LeftJoin(other, nil)); got != 4 {
		t.Errorf("ON TRUE returned %d rows, want 4", got)
	}
}

func TestMySQLEmptyPredicatesAreAcceptedByTheServer(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	tbl := mysql.NewTable(integration.UniqueName(t, "nilpred"))
	dropMySQL(t, db, tbl)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	execMySQL(t, db, mysql.CreateTable(tbl))
	for i := int64(1); i <= 2; i++ {
		if _, err := db.Insert(tbl).Row(id.Val(i)).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n := mysql.As(mysql.CountAll(), "n")
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(mysql.And())); got != 2 {
		t.Errorf("WHERE TRUE returned %d rows, want 2", got)
	}
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(mysql.Or())); got != 0 {
		t.Errorf("WHERE FALSE returned %d rows, want 0", got)
	}
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(nil, nil)); got != 2 {
		t.Errorf("an all-nil Where returned %d rows, want 2", got)
	}
	other := tbl.As("other")
	if got := rowCount(t, db, db.Select(n).From(tbl).LeftJoin(other, nil)); got != 4 {
		t.Errorf("ON TRUE returned %d rows, want 4", got)
	}
}

func TestSQLiteEmptyPredicatesAreAcceptedByTheEngine(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("nilpred")
	id := sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	exec(t, db, sqlite.CreateTable(tbl))
	for i := int64(1); i <= 2; i++ {
		if _, err := db.Insert(tbl).Values(id.Val(i)).Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	n := sqlite.As(sqlite.CountAll(), "n")
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(sqlite.And())); got != 2 {
		t.Errorf("WHERE TRUE returned %d rows, want 2", got)
	}
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(sqlite.Or())); got != 0 {
		t.Errorf("WHERE FALSE returned %d rows, want 0", got)
	}
	if got := rowCount(t, db, db.Select(n).From(tbl).Where(nil, nil)); got != 2 {
		t.Errorf("an all-nil Where returned %d rows, want 2", got)
	}
	other := tbl.As("other")
	if got := rowCount(t, db, db.Select(n).From(tbl).LeftJoin(other, nil)); got != 4 {
		t.Errorf("ON TRUE returned %d rows, want 4", got)
	}
}

// Dropping the nils moved a decision: a Where whose predicates are all
// nil used to be a crash or unparseable SQL, and is now a Where that
// says nothing. What it must not have become is a Where that says
// nothing *and takes the table's guards with it*. The guards are the
// only thing standing between an "all filters optional" search endpoint
// and another tenant's rows, so the question is asked of the server
// with the other tenant's rows and a soft-deleted row sitting in the
// table, where a lost predicate shows up as a row and not as a string
// difference.

type nilGuardAuthor struct {
	ID    int64          `drop:"id"`
	Name  string         `drop:"name"`
	Books []nilGuardBook `dropRel:"books"`
}

type nilGuardBook struct {
	ID        int64      `drop:"id"`
	AuthorID  int64      `drop:"authorId"`
	Title     string     `drop:"title"`
	DeletedAt *time.Time `drop:"deletedAt"`
}

func TestPGNilPredicatesDoNotDisarmTheGlobalFilters(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()

	authors := pg.NewTable(integration.UniqueName(t, "nilguard_a"))
	books := pg.NewTable(integration.UniqueName(t, "nilguard_b"))
	dropPG(t, db, authors)
	dropPG(t, db, books)

	authorID := pg.Add(authors, pg.BigSerial("id").PrimaryKey())
	authorTenant := pg.Add(authors, pg.BigInt("tenantId").NotNull())
	authorName := pg.Add(authors, pg.Text("name").NotNull())
	authors.AddFilter(pg.FilterTenant, authorTenant.Eq(int64(7)))

	bookID := pg.Add(books, pg.BigSerial("id").PrimaryKey())
	bookTenant := pg.Add(books, pg.BigInt("tenantId").NotNull())
	bookAuthorID := pg.Add(books, pg.BigInt("authorId").NotNull())
	bookTitle := pg.Add(books, pg.Text("title").NotNull())
	// The mixin supplies deletedAt, the FilterSoftDelete guard and the
	// DELETE-becomes-UPDATE hook the last subtest leans on.
	pg.ApplyMixins(books, &pg.SoftDeleteMixin{})
	books.AddFilter(pg.FilterTenant, bookTenant.Eq(int64(7)))

	pg.NewRelations(authors).HasMany("books", books, authorID, bookAuthorID)

	execPG(t, db, pg.CreateTable(authors))
	execPG(t, db, pg.CreateTable(books))

	if _, err := db.Insert(authors).
		Row(authorTenant.Val(int64(7)), authorName.Val("alice")).
		Row(authorTenant.Val(int64(7)), authorName.Val("bob")).
		Row(authorTenant.Val(int64(8)), authorName.Val("intruder")).
		Exec(ctx); err != nil {
		t.Fatalf("seed authors: %v", err)
	}
	var seeded []nilGuardAuthor
	if err := db.Select(authorID, authorName).From(authors).
		IgnoreFilters(pg.FilterTenant).OrderBy(authorID.Asc()).All(ctx, &seeded); err != nil {
		t.Fatalf("read authors: %v", err)
	}
	aid := map[string]int64{}
	for _, a := range seeded {
		aid[a.Name] = a.ID
	}

	// All three books hang off alice, so only the guards can tell them
	// apart: one live, one soft-deleted, one belonging to tenant 8.
	if _, err := db.Insert(books).
		Row(bookTenant.Val(int64(7)), bookAuthorID.Val(aid["alice"]), bookTitle.Val("alice-live")).
		Row(bookTenant.Val(int64(7)), bookAuthorID.Val(aid["alice"]), bookTitle.Val("alice-hidden")).
		Row(bookTenant.Val(int64(8)), bookAuthorID.Val(aid["alice"]), bookTitle.Val("theirs")).
		Exec(ctx); err != nil {
		t.Fatalf("seed books: %v", err)
	}
	if _, err := db.Delete(books).Where(bookTitle.Eq("alice-hidden")).Exec(ctx); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	names := func(rows []nilGuardAuthor) string {
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			out = append(out, r.Name)
		}
		sort.Strings(out)
		return strings.Join(out, ",")
	}

	t.Run("an all-nil Where keeps the tenancy filter", func(t *testing.T) {
		for _, tc := range []struct {
			what string
			q    *pg.SelectBuilder
		}{
			{"Where(nil, nil)", db.Select(authorID, authorName).From(authors).Where(nil, nil)},
			{"Where()", db.Select(authorID, authorName).From(authors).Where()},
			{"Where(pg.And())", db.Select(authorID, authorName).From(authors).Where(pg.And())},
		} {
			var rows []nilGuardAuthor
			if err := tc.q.All(ctx, &rows); err != nil {
				text, _ := tc.q.ToSQL()
				t.Fatalf("%s: %v\n%s", tc.what, err, text)
			}
			if got := names(rows); got != "alice,bob" {
				t.Errorf("%s returned %q; tenant 8 must not be in it", tc.what, got)
			}
		}
	})

	t.Run("a nil relation filter keeps the child table's guards", func(t *testing.T) {
		var rows []nilGuardAuthor
		f := db.Find(authors).Where(nil).
			WithRel("books", func(c *pg.RelConfig) { c.Where(nil) }).
			OrderBy(authorID.Asc())
		if err := f.All(ctx, &rows); err != nil {
			text, _ := f.Select().ToSQL()
			t.Fatalf("Find: %v\n%s", err, text)
		}
		if got := names(rows); got != "alice,bob" {
			t.Fatalf("parents = %q, want alice,bob", got)
		}
		for _, a := range rows {
			var titles []string
			for _, b := range a.Books {
				titles = append(titles, b.Title)
			}
			sort.Strings(titles)
			want := ""
			if a.Name == "alice" {
				want = "alice-live"
			}
			if got := strings.Join(titles, ","); got != want {
				t.Errorf("%s loaded %q, want %q — the soft-deleted and the other tenant's book must stay out",
					a.Name, got, want)
			}
		}
	})

	// The path that changed most: this statement used to be a nil
	// dereference, so nobody could reach the server with it. Now it
	// renders, and what it must render is the guards.
	t.Run("an all-nil Delete is still fenced by the filters", func(t *testing.T) {
		// The Where is all nils on purpose; what fences the statement
		// is the global filters, which is the thing under test.
		//drops:lint ignore unfilteredwrite — the filters are the fence
		if _, err := db.Delete(books).Where(nil, nil).Exec(ctx); err != nil {
			t.Fatalf("delete: %v", err)
		}
		var left []nilGuardBook
		if err := db.Select(bookID, bookAuthorID, bookTitle, books.Col("deletedAt")).
			From(books).Unscoped().OrderBy(bookTitle.Asc()).All(ctx, &left); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if len(left) != 3 {
			t.Fatalf("%d rows survive, want all 3 still present", len(left))
		}
		for _, b := range left {
			switch b.Title {
			case "theirs":
				if b.DeletedAt != nil {
					t.Error("the other tenant's row was soft-deleted: the tenancy filter did not survive the nil Where")
				}
			case "alice-live", "alice-hidden":
				if b.DeletedAt == nil {
					t.Errorf("%s should have been swept by the unfiltered delete", b.Title)
				}
			}
		}
	})
}
