package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

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

type strictAuthorNoRel struct {
	ID   int64  `drop:"id"`
	Name string `drop:"name"`
}

func mkStrictSchema() (authors, books *sqlite.Table) {
	authors = sqlite.NewTable("authors")
	aID := sqlite.Add(authors, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(authors, sqlite.Text("name").NotNull())

	books = sqlite.NewTable("books")
	sqlite.Add(books, sqlite.BigInt("id").PrimaryKey())
	bAuthor := sqlite.Add(books, sqlite.BigInt("authorId").NotNull())
	sqlite.Add(books, sqlite.Text("title").NotNull())

	sqlite.NewRelations(authors).HasMany("books", books, aID, bAuthor)
	return authors, books
}

func strictRows() *fakeDriver {
	return &fakeDriver{rows: &fakeRows{
		cols: []string{"id", "name"},
		data: [][]any{{int64(1), "Le Guin"}},
	}}
}

// The headline case: without the check the query returns authors whose
// Books is nil, which reads as "no books" and is not.
func TestStrictLoadingRefusesUnloadedRelation(t *testing.T) {
	authors, _ := mkStrictSchema()
	db := sqlite.New(strictRows()).StrictLoading()

	var got []strictAuthor
	err := db.Find(authors).All(context.Background(), &got)
	if !errors.Is(err, sqlite.ErrRelationNotLoaded) {
		t.Fatalf("want ErrRelationNotLoaded, got %v", err)
	}
	for _, want := range []string{`"books"`, "strictAuthor", `.With("books")`, `.Without("books")`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %s: %v", want, err)
		}
	}
}

// It must refuse before the SELECT: a query that cannot produce a
// trustworthy answer should not cost a round trip either.
func TestStrictLoadingRefusesBeforeQuerying(t *testing.T) {
	authors, _ := mkStrictSchema()
	fd := strictRows()
	db := sqlite.New(fd).StrictLoading()

	var got []strictAuthor
	_ = db.Find(authors).All(context.Background(), &got)
	if len(fd.queries) != 0 {
		t.Errorf("strict loading must refuse before executing: %v", fd.queries)
	}
}

func TestStrictLoadingAcceptsALoadedOrWaivedRelation(t *testing.T) {
	authors, _ := mkStrictSchema()
	db := sqlite.New(strictRows()).StrictLoading()
	ctx := context.Background()

	var got []strictAuthor
	if err := db.Find(authors).With("books").All(ctx, &got); err != nil {
		t.Fatalf("With must satisfy the check: %v", err)
	}
	got = nil
	if err := db.Find(authors).NoLoad(authors.Relation("books")).All(ctx, &got); err != nil {
		t.Fatalf("NoLoad must satisfy the check: %v", err)
	}
	got = nil
	if err := db.Find(authors).Without("books").All(ctx, &got); err != nil {
		t.Fatalf("Without must satisfy the check: %v", err)
	}
}

// A destination with no relation field cannot hold a misleading nil.
func TestStrictLoadingIgnoresProjectionStructs(t *testing.T) {
	authors, _ := mkStrictSchema()
	db := sqlite.New(strictRows()).StrictLoading()

	var got []strictAuthorNoRel
	if err := db.Find(authors).All(context.Background(), &got); err != nil {
		t.Fatalf("a struct with no relation field must pass: %v", err)
	}
}

// Off by default: the check must not change what an existing program
// does.
func TestWithoutStrictModeNothingIsRefused(t *testing.T) {
	authors, _ := mkStrictSchema()
	db := sqlite.New(strictRows())

	var got []strictAuthor
	if err := db.Find(authors).All(context.Background(), &got); err != nil {
		t.Fatalf("strict loading must be opt-in: %v", err)
	}
	if len(got) != 1 || got[0].Books != nil {
		t.Errorf("unloaded relation should still be nil: %+v", got)
	}
}

// One query can opt in without the whole DB doing so, and Find.One
// routes through All so it inherits the check.
func TestPerQueryStrictAndFindOne(t *testing.T) {
	authors, _ := mkStrictSchema()
	db := sqlite.New(strictRows())

	var all []strictAuthor
	if err := db.Find(authors).Strict().All(context.Background(), &all); !errors.Is(err, sqlite.ErrRelationNotLoaded) {
		t.Fatalf("All: want ErrRelationNotLoaded, got %v", err)
	}
	var one strictAuthor
	if err := db.Find(authors).Strict().One(context.Background(), &one); !errors.Is(err, sqlite.ErrRelationNotLoaded) {
		t.Fatalf("One: want ErrRelationNotLoaded, got %v", err)
	}
}
