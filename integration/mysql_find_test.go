package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// The eager loader against a real server.
//
// The unit tests in drops/mysql prove the loader emits one batched
// query per edge and threads the rows into the right parents, against a
// fake driver that returns whatever they hand it. What they cannot
// prove is that MySQL accepts any of it: the IN list, the junction
// read, the identifier quoting. This is the suite where the server
// parses the statement.
//
// The package had no relations at all until now — a Relation type and a
// map had once been declared against a type no file defined, so it had
// never compiled and there was no loader to feed. These are the first
// tests of the real thing.

type myAuthor struct {
	ID    int64    `drop:"id"`
	Name  string   `drop:"name"`
	Books []myBook `dropRel:"books"`
}

type myBook struct {
	ID       int64  `drop:"id"`
	AuthorID int64  `drop:"authorId"`
	Title    string `drop:"title"`
}

type myBookWithAuthor struct {
	ID       int64     `drop:"id"`
	AuthorID int64     `drop:"authorId"`
	Title    string    `drop:"title"`
	Author   *myAuthor `dropRel:"author"`
}

// mysqlLibrary creates the two tables, declares the edges both ways and
// seeds two authors with three books between them.
func mysqlLibrary(t *testing.T, db *mysql.DB) (authors, books *mysql.Table) {
	t.Helper()
	ctx := context.Background()

	authors = mysql.NewTable(integration.UniqueName(t, "authors"))
	aID := mysql.Add(authors, mysql.BigInt("id").PrimaryKey())
	aName := mysql.Add(authors, mysql.Varchar("name", 255).NotNull())

	books = mysql.NewTable(integration.UniqueName(t, "books"))
	bID := mysql.Add(books, mysql.BigInt("id").PrimaryKey())
	bAuthor := mysql.Add(books, mysql.BigInt("authorId").NotNull())
	bTitle := mysql.Add(books, mysql.Varchar("title", 255).NotNull())

	dropMySQL(t, db, authors)
	dropMySQL(t, db, books)
	execMySQL(t, db, mysql.CreateTable(authors))
	execMySQL(t, db, mysql.CreateTable(books))

	mysql.NewRelations(authors).HasMany("books", books, aID, bAuthor)
	mysql.NewRelations(books).BelongsTo("author", authors, bAuthor, aID)

	if _, err := db.Insert(authors).
		Row(aID.Val(1), aName.Val("Le Guin")).
		Row(aID.Val(2), aName.Val("Borges")).
		Exec(ctx); err != nil {
		t.Fatalf("seed authors: %v", err)
	}
	if _, err := db.Insert(books).
		Row(bID.Val(10), bAuthor.Val(1), bTitle.Val("A Wizard of Earthsea")).
		Row(bID.Val(11), bAuthor.Val(1), bTitle.Val("The Dispossessed")).
		Row(bID.Val(12), bAuthor.Val(2), bTitle.Val("Ficciones")).
		Exec(ctx); err != nil {
		t.Fatalf("seed books: %v", err)
	}
	return authors, books
}

func TestMySQLFindEagerLoadsHasMany(t *testing.T) {
	db := openMySQL(t)
	authors, _ := mysqlLibrary(t, db)

	var got []myAuthor
	if err := db.Find(authors).OrderBy(authors.Col("id")).
		All(context.Background(), &got); err != nil {
		t.Fatalf("Find without With: %v", err)
	}
	// Nothing requested, nothing loaded — the loader does no work it
	// was not asked for.
	for _, a := range got {
		if a.Books != nil {
			t.Errorf("author %d loaded books nobody asked for", a.ID)
		}
	}

	got = nil
	if err := db.Find(authors).With("books").OrderBy(authors.Col("id")).
		All(context.Background(), &got); err != nil {
		t.Fatalf("Find with books: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d authors, want 2", len(got))
	}
	if len(got[0].Books) != 2 {
		t.Errorf("Le Guin got %d books, want 2: %+v", len(got[0].Books), got[0].Books)
	}
	if len(got[1].Books) != 1 || got[1].Books[0].Title != "Ficciones" {
		t.Errorf("Borges got %+v, want just Ficciones", got[1].Books)
	}
}

func TestMySQLFindEagerLoadsBelongsTo(t *testing.T) {
	db := openMySQL(t)
	_, books := mysqlLibrary(t, db)

	var got []myBookWithAuthor
	if err := db.Find(books).With("author").OrderBy(books.Col("id")).
		All(context.Background(), &got); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d books, want 3", len(got))
	}
	// Two books share an author: the parent is threaded onto both,
	// from one row read once.
	for _, b := range got {
		if b.Author == nil {
			t.Fatalf("book %d has no author loaded", b.ID)
		}
	}
	if got[0].Author.Name != "Le Guin" || got[2].Author.Name != "Borges" {
		t.Errorf("authors landed on the wrong books: %q / %q", got[0].Author.Name, got[2].Author.Name)
	}
}

type myTag struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

type myBookWithTags struct {
	ID    int64   `drop:"id"`
	Title string  `drop:"title"`
	Tags  []myTag `dropRel:"tags"`
}

// The junction path, which is the one with two queries behind it and
// the one no test in any dialect covered before now.
func TestMySQLFindEagerLoadsManyToMany(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	books := mysql.NewTable(integration.UniqueName(t, "mmbooks"))
	bID := mysql.Add(books, mysql.BigInt("id").PrimaryKey())
	bTitle := mysql.Add(books, mysql.Varchar("title", 255).NotNull())

	tags := mysql.NewTable(integration.UniqueName(t, "mmtags"))
	tID := mysql.Add(tags, mysql.BigInt("id").PrimaryKey())
	tName := mysql.Add(tags, mysql.Varchar("name", 255).NotNull())

	bookTags := mysql.NewTable(integration.UniqueName(t, "mmbook_tags"))
	btBook := mysql.Add(bookTags, mysql.BigInt("bookId").NotNull().PrimaryKey())
	btTag := mysql.Add(bookTags, mysql.BigInt("tagId").NotNull().PrimaryKey())

	for _, tbl := range []*mysql.Table{books, tags, bookTags} {
		dropMySQL(t, db, tbl)
	}
	execMySQL(t, db, mysql.CreateTable(books))
	execMySQL(t, db, mysql.CreateTable(tags))
	execMySQL(t, db, mysql.CreateTable(bookTags))

	mysql.NewRelations(books).ManyToMany("tags", tags, bookTags, btBook, btTag, bID, tID)

	if _, err := db.Insert(books).
		Row(bID.Val(1), bTitle.Val("Earthsea")).
		Row(bID.Val(2), bTitle.Val("Ficciones")).
		Exec(ctx); err != nil {
		t.Fatalf("seed books: %v", err)
	}
	if _, err := db.Insert(tags).
		Row(tID.Val(10), tName.Val("fantasy")).
		Row(tID.Val(11), tName.Val("classic")).
		Exec(ctx); err != nil {
		t.Fatalf("seed tags: %v", err)
	}
	if _, err := db.Insert(bookTags).
		Row(btBook.Val(1), btTag.Val(10)).
		Row(btBook.Val(1), btTag.Val(11)).
		Row(btBook.Val(2), btTag.Val(11)).
		Exec(ctx); err != nil {
		t.Fatalf("seed junction: %v", err)
	}

	var got []myBookWithTags
	if err := db.Find(books).With("tags").OrderBy(books.Col("id")).
		All(ctx, &got); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d books, want 2", len(got))
	}
	if len(got[0].Tags) != 2 {
		t.Errorf("Earthsea got %d tags, want 2: %+v", len(got[0].Tags), got[0].Tags)
	}
	if len(got[1].Tags) != 1 || got[1].Tags[0].Name != "classic" {
		t.Errorf("Ficciones got %+v, want just classic", got[1].Tags)
	}
}

// Strict loading, against a server rather than a fake: the refusal has
// to happen before the query, so a caller who forgot With never sees a
// nil slice that reads as "no rows".
func TestMySQLStrictLoadingRefusesAnUnloadedRelation(t *testing.T) {
	db := openMySQL(t)
	authors, _ := mysqlLibrary(t, db)

	var got []myAuthor
	err := db.StrictLoading().Find(authors).All(context.Background(), &got)
	if !errors.Is(err, mysql.ErrRelationNotLoaded) {
		t.Fatalf("got %v, want ErrRelationNotLoaded", err)
	}
	if len(got) != 0 {
		t.Errorf("the refused query still scanned %d rows", len(got))
	}

	// Asked for, it loads; waived, it is allowed to stay empty.
	if err := db.StrictLoading().Find(authors).With("books").
		All(context.Background(), &got); err != nil {
		t.Fatalf("with the relation loaded: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d authors, want 2", len(got))
	}
}
