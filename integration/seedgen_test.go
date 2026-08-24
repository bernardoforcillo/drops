package integration_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
)

// Seeding is only useful if the rows land, the foreign keys point at
// something real, and the same seed produces the same run. All three
// are claims about a server, so they are settled by one.

type sgAuthor struct {
	ID    int64   `drop:"id"`
	Name  string  `drop:"name"`
	Email string  `drop:"email"`
	Bio   *string `drop:"bio"`
}

type sgBook struct {
	ID        int64     `drop:"id"`
	AuthorID  int64     `drop:"authorId"`
	Title     string    `drop:"title"`
	Published bool      `drop:"published"`
	WrittenAt time.Time `drop:"writtenAt"`
}

type sgSchema struct {
	authors, books   *pg.Table
	authorID, bookID *pg.Col[int64]
	authorEnt        *pg.Entity[sgAuthor]
	bookEnt          *pg.Entity[sgBook]
}

func sgBuild(t *testing.T, db *pg.DB) sgSchema {
	t.Helper()
	authors := pg.NewTable(integration.UniqueName(t, "sg_a"))
	books := pg.NewTable(integration.UniqueName(t, "sg_b"))
	// Registered parent-first so the LIFO cleanup drops the child
	// first: books holds a foreign key into authors, and dropping
	// authors while it stands fails — silently, since dropPG ignores
	// the error, leaving both tables behind to poison the next run.
	dropPG(t, db, authors)
	dropPG(t, db, books)

	authorID := pg.Add(authors, pg.BigSerial("id").PrimaryKey())
	pg.Add(authors, pg.Text("name").NotNull())
	pg.Add(authors, pg.Text("email").NotNull().Unique())
	// Nullable, so the struct field is a pointer — which is exactly
	// what pg.Maybe hands back.
	pg.Add(authors, pg.Text("bio"))

	bookID := pg.Add(books, pg.BigSerial("id").PrimaryKey())
	bookAuthorID := pg.Add(books, pg.BigInt("authorId").NotNull().References(authorID))
	pg.Add(books, pg.Text("title").NotNull())
	pg.Add(books, pg.Boolean("published").NotNull())
	pg.Add(books, pg.Timestamp("writtenAt", true).NotNull())

	pg.NewRelations(authors).HasMany("books", books, authorID, bookAuthorID)
	pg.NewRelations(books).BelongsTo("author", authors, bookAuthorID, authorID)

	execPG(t, db, pg.CreateTable(authors))
	execPG(t, db, pg.CreateTable(books))

	return sgSchema{
		authors: authors, books: books,
		authorID: authorID, bookID: bookID,
		authorEnt: pg.NewEntity[sgAuthor](authors),
		bookEnt:   pg.NewEntity[sgBook](books),
	}
}

// sgPlan is the plan under test, built fresh each time so a rerun goes
// through exactly the same declarations.
func sgPlan(db *pg.DB, s sgSchema, seed uint64) (*pg.Seeder, *pg.SeedSet[sgAuthor], *pg.SeedSet[sgBook]) {
	seeder := pg.NewSeeder(db).WithSeed(seed)
	lo := time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

	authors := pg.SeedMany(seeder, s.authorEnt, 10, func(g *pg.Gen, a *sgAuthor) {
		a.Name = g.FullName()
		a.Email = g.Email()
		a.Bio = pg.Maybe(g, 0.5, g.Sentence())
	})
	// Nothing here restates how a book points at an author.
	books := pg.SeedRelated(authors, "books", s.bookEnt, 2, 5,
		func(g *pg.Gen, b *sgBook, a *sgAuthor) {
			b.Title = g.Sentence()
			b.Published = pg.Weighted(g,
				pg.Choice[bool]{Value: true, Weight: 7},
				pg.Choice[bool]{Value: false, Weight: 3})
			b.WrittenAt = g.TimeBetween(lo, hi)
		})
	return seeder, authors, books
}

func sgReadAll(t *testing.T, db *pg.DB, s sgSchema) (authors []sgAuthor, books []sgBook) {
	t.Helper()
	ctx := context.Background()
	if err := db.Select().From(s.authors).OrderBy(s.authors.Col("id").Asc()).All(ctx, &authors); err != nil {
		t.Fatalf("read authors: %v", err)
	}
	if err := db.Select().From(s.books).OrderBy(s.books.Col("id").Asc()).All(ctx, &books); err != nil {
		t.Fatalf("read books: %v", err)
	}
	return authors, books
}

// sgFingerprint renders the seeded data as text, so two runs can be
// compared as a whole rather than field by field. Primary keys are in
// it deliberately: the truncate below restarts the sequences, so a run
// that reproduces the values but not their identity has not reproduced
// the data an FK points at.
func sgFingerprint(authors []sgAuthor, books []sgBook) string {
	var b strings.Builder
	for _, a := range authors {
		bio := "<null>"
		if a.Bio != nil {
			bio = *a.Bio
		}
		fmt.Fprintf(&b, "author %d %s %s %s\n", a.ID, a.Name, a.Email, bio)
	}
	for _, bk := range books {
		fmt.Fprintf(&b, "book %d author=%d published=%v at=%s %s\n",
			bk.ID, bk.AuthorID, bk.Published, bk.WrittenAt.UTC().Format(time.RFC3339Nano), bk.Title)
	}
	return b.String()
}

func TestPGSeedRelatedWiresTheForeignKeys(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	s := sgBuild(t, db)

	seeder, authorSet, bookSet := sgPlan(db, s, 42)
	if err := seeder.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	authors, books := sgReadAll(t, db, s)
	if len(authors) != 10 {
		t.Fatalf("got %d authors, want 10", len(authors))
	}
	if len(books) < 20 || len(books) > 50 {
		t.Fatalf("got %d books, want between 2 and 5 per author", len(books))
	}

	// The handles report what was created, keys included.
	if len(authorSet.Rows()) != 10 || len(bookSet.Rows()) != len(books) {
		t.Errorf("SeedSet.Rows: %d authors / %d books, database has 10 / %d",
			len(authorSet.Rows()), len(bookSet.Rows()), len(books))
	}
	for _, a := range authorSet.Rows() {
		if a.ID == 0 {
			t.Fatal("a seeded author came back without its generated key")
		}
	}

	// Every book belongs to a real author, and every author got between
	// two and five. The FK constraint on the table already refuses a
	// dangling key, so reaching this point at all is half the proof;
	// the counts are the other half.
	byAuthor := map[int64]int{}
	for _, b := range books {
		byAuthor[b.AuthorID]++
		if b.AuthorID == 0 {
			t.Fatalf("book %d has a zero author id — the relation was not wired", b.ID)
		}
		if b.Title == "" || b.WrittenAt.IsZero() {
			t.Fatalf("book %d was not filled in: %+v", b.ID, b)
		}
	}
	if len(byAuthor) != 10 {
		t.Errorf("books are spread over %d authors, want 10", len(byAuthor))
	}
	for _, a := range authors {
		if n := byAuthor[a.ID]; n < 2 || n > 5 {
			t.Errorf("author %d got %d books, want 2..5", a.ID, n)
		}
	}
	// The counts vary per parent rather than being drawn once and
	// reused, which is the difference between "two to five books each"
	// and "the same number each".
	seen := map[int]bool{}
	for _, n := range byAuthor {
		seen[n] = true
	}
	if len(seen) < 2 {
		t.Errorf("every author got the same number of books (%v) — the count is not drawn per parent", seen)
	}

	// A nullable column filled with Maybe is sometimes NULL and
	// sometimes not.
	nulls := 0
	for _, a := range authors {
		if a.Bio == nil {
			nulls++
		}
	}
	if nulls == 0 || nulls == len(authors) {
		t.Errorf("Maybe(0.5) produced %d NULL bios out of %d", nulls, len(authors))
	}
}

// The property to pin: the same seed produces the same rows. Proved by
// running the plan twice against the same emptied tables and comparing
// everything, not by trusting the PRNG.
func TestPGSeedIsDeterministic(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	s := sgBuild(t, db)

	run := func(seed uint64) string {
		t.Helper()
		// RESTART IDENTITY so the generated keys repeat too: a run that
		// reproduces the values under different ids has not reproduced
		// the data.
		if _, err := db.Exec(ctx, fmt.Sprintf(`TRUNCATE %q, %q RESTART IDENTITY CASCADE`,
			s.books.Name(), s.authors.Name())); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		seeder, _, _ := sgPlan(db, s, seed)
		if err := seeder.Apply(ctx); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		return sgFingerprint(sgReadAll(t, db, s))
	}

	first := run(42)
	second := run(42)
	if first != second {
		t.Errorf("seed 42 produced different data on the second run\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if other := run(43); other == first {
		t.Error("seeds 42 and 43 produced identical data — the seed is not reaching the generators")
	}
}

// Applying one Seeder twice is the same claim from the other side: the
// generator rewinds, so the second Apply inserts what the first did
// rather than continuing the stream.
func TestPGSeedApplyTwiceRepeatsItself(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	s := sgBuild(t, db)

	seeder, authorSet, _ := sgPlan(db, s, 7)
	if err := seeder.Apply(ctx); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	first := sgFingerprint(sgReadAll(t, db, s))
	firstRows := append([]sgAuthor(nil), authorSet.Rows()...)

	if _, err := db.Exec(ctx, fmt.Sprintf(`TRUNCATE %q, %q RESTART IDENTITY CASCADE`,
		s.books.Name(), s.authors.Name())); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := seeder.Apply(ctx); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if second := sgFingerprint(sgReadAll(t, db, s)); second != first {
		t.Errorf("the second Apply produced different data\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	// And Rows reports that run, not both runs concatenated.
	if got := len(authorSet.Rows()); got != len(firstRows) {
		t.Errorf("after a second Apply, Rows has %d authors, want %d", got, len(firstRows))
	}
}

// The transactional harness still holds: a plan that fails partway
// leaves nothing behind.
func TestPGSeedRollsBackOnFailure(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	s := sgBuild(t, db)

	seeder := pg.NewSeeder(db).WithSeed(3)
	authors := pg.SeedMany(seeder, s.authorEnt, 5, func(g *pg.Gen, a *sgAuthor) {
		a.Name = g.FullName()
		// The same address every time, against a UNIQUE column: the
		// second row is refused by the server.
		a.Email = "clash@example.com"
	})
	pg.SeedRelated(authors, "books", s.bookEnt, 1, 1, func(g *pg.Gen, b *sgBook, a *sgAuthor) {
		b.Title = g.Sentence()
		b.WrittenAt = time.Unix(0, 0).UTC()
	})
	if err := seeder.Apply(ctx); err == nil {
		t.Fatal("expected the duplicate email to fail the seed")
	}
	got, books := sgReadAll(t, db, s)
	if len(got) != 0 || len(books) != 0 {
		t.Errorf("a failed seed left %d authors and %d books behind", len(got), len(books))
	}
}

// A relation whose child does not hold the key cannot be seeded beneath
// its parent, and saying so is better than writing zeroes into a column.
func TestPGSeedRelatedRefusesTheWrongSide(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	s := sgBuild(t, db)

	seeder := pg.NewSeeder(db)
	books := pg.SeedMany(seeder, s.bookEnt, 1, func(g *pg.Gen, b *sgBook) {
		b.Title = g.Sentence()
		b.WrittenAt = time.Unix(0, 0).UTC()
	})
	pg.SeedRelated(books, "author", s.authorEnt, 1, 1, nil)
	err := seeder.Apply(ctx)
	if err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("expected a refusal naming the foreign key, got %v", err)
	}

	seeder = pg.NewSeeder(db)
	more := pg.SeedMany(seeder, s.authorEnt, 1, func(g *pg.Gen, a *sgAuthor) {
		a.Name = g.FullName()
		a.Email = g.Email()
	})
	pg.SeedRelated(more, "nope", s.bookEnt, 1, 1, nil)
	if err := seeder.Apply(ctx); err == nil || !strings.Contains(err.Error(), `unknown relation "nope"`) {
		t.Fatalf("expected an unknown-relation error, got %v", err)
	}

	// Nothing was written on either attempt.
	authors, bs := sgReadAll(t, db, s)
	if len(authors) != 0 || len(bs) != 0 {
		t.Errorf("a refused plan left %d authors and %d books behind", len(authors), len(bs))
	}
}

// Depth: a plan is a tree, and a grandchild's keys come from the child
// that was just inserted rather than from whichever row was last seen.
func TestPGSeedNestsThreeDeep(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	s := sgBuild(t, db)

	reviews := pg.NewTable(integration.UniqueName(t, "sg_r"))
	dropPG(t, db, reviews)
	pg.Add(reviews, pg.BigSerial("id").PrimaryKey())
	reviewBookID := pg.Add(reviews, pg.BigInt("bookId").NotNull().References(s.bookID))
	pg.Add(reviews, pg.BigInt("stars").NotNull())
	pg.NewRelations(s.books).HasMany("reviews", reviews, s.bookID, reviewBookID)
	execPG(t, db, pg.CreateTable(reviews))
	type sgReview struct {
		ID     int64 `drop:"id"`
		BookID int64 `drop:"bookId"`
		Stars  int64 `drop:"stars"`
	}
	reviewEnt := pg.NewEntity[sgReview](reviews)

	seeder := pg.NewSeeder(db).WithSeed(99)
	authors := pg.SeedMany(seeder, s.authorEnt, 3, func(g *pg.Gen, a *sgAuthor) {
		a.Name = g.FullName()
		a.Email = g.Email()
	})
	books := pg.SeedRelated(authors, "books", s.bookEnt, 2, 2, func(g *pg.Gen, b *sgBook, a *sgAuthor) {
		b.Title = g.Sentence()
		b.WrittenAt = time.Unix(0, 0).UTC()
	})
	pg.SeedRelated(books, "reviews", reviewEnt, 1, 3, func(g *pg.Gen, r *sgReview, b *sgBook) {
		r.Stars = int64(g.IntRange(1, 5))
	})
	if err := seeder.Apply(ctx); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var rs []sgReview
	if err := db.Select().From(reviews).All(ctx, &rs); err != nil {
		t.Fatalf("read reviews: %v", err)
	}
	if len(rs) < 6 || len(rs) > 18 {
		t.Fatalf("got %d reviews, want 6..18", len(rs))
	}
	_, books6 := sgReadAll(t, db, s)
	if len(books6) != 6 {
		t.Fatalf("got %d books, want 6", len(books6))
	}
	bookIDs := map[int64]bool{}
	for _, b := range books6 {
		bookIDs[b.ID] = true
	}
	perBook := map[int64]int{}
	for _, r := range rs {
		if !bookIDs[r.BookID] {
			t.Fatalf("review %d points at book %d, which is not one of the seeded books", r.ID, r.BookID)
		}
		perBook[r.BookID]++
		if r.Stars < 1 || r.Stars > 5 {
			t.Errorf("review %d has %d stars", r.ID, r.Stars)
		}
	}
	if len(perBook) != 6 {
		var got []int64
		for id := range perBook {
			got = append(got, id)
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		t.Errorf("reviews landed on %d of the 6 books (%v) — a grandchild took the wrong parent", len(perBook), got)
	}
}
