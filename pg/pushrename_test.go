package pg_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/pg"
)

// The schema as the carrier of a rename answer.
//
// Push has no migration directory, so meta/_renames.json is not
// available to it and the answer has to come from the schema itself.
// These tests cover the reading of that declaration and its effect on
// the generator; that a live push honours it is a server behaviour and
// lives in the integration suite.

func TestDeclaredRenamesReadsColumnAndTable(t *testing.T) {
	people := pg.NewTable("people").RenamedFrom("users")
	pg.Add(people, pg.BigSerial("id").PrimaryKey())
	pg.Add(people, pg.Text("emailAddress").NotNull().RenamedFrom("email"))
	pg.Add(people, pg.Text("nickname"))

	got := pg.DeclaredRenames(pg.NewSchema(people))
	want := []pg.RenameDecision{
		{Rename: pg.Rename{Kind: pg.RenameColumn, Table: "people", From: "email", To: "emailAddress"}, IsRename: true},
		{Rename: pg.Rename{Kind: pg.RenameTable, From: "users", To: "people"}, IsRename: true},
	}
	if len(got) != len(want) {
		t.Fatalf("DeclaredRenames = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DeclaredRenames[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	// The column names its table by the name the table has now, because
	// the table rename in front of it has already run by then.
	if got[0].Table != "people" {
		t.Errorf("the column rename names table %q, want the new name", got[0].Table)
	}
	if pg.DeclaredRenames(nil) != nil {
		t.Error("DeclaredRenames(nil) invented a decision")
	}
}

// A column that never moved says nothing, so a schema full of ordinary
// columns does not answer questions nobody asked.
func TestDeclaredRenamesIgnoresUnmovedColumns(t *testing.T) {
	if got := pg.DeclaredRenames(usersWithEmail()); len(got) != 0 {
		t.Fatalf("DeclaredRenames of an unannotated schema = %+v", got)
	}
}

// The generator reads the declaration too. Otherwise a schema that
// states a rename would answer a push and not a generate, and the same
// schema would mean two different things to the two commands.
func TestGenerateTakesTheRenameTheSchemaDeclares(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("emailAddress").NotNull().RenamedFrom("email"))

	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(users),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, `RENAME COLUMN "email" TO "emailAddress"`) {
		t.Fatalf("migration:\n%s", res.SQL)
	}
	// The declaration is a fact the schema carries, not an answer
	// somebody typed, so nothing is written into the log for it.
	if _, ok := capt.written[pg.RenameLogFile]; ok {
		t.Errorf("a declared rename was written into the answer log:\n%s", capt.written[pg.RenameLogFile])
	}
}

// An answer given on a run whose diff came to nothing is still an
// answer. It used to be dropped on the floor: GenerateMigration
// returned its NoOp before the log was written, so a decision that
// settled a question without moving the schema had to be given again
// — by a run that may have nobody to give it.
func TestGenerateRecordsAnAnswerThatProducedNoStatements(t *testing.T) {
	// The schema and the last snapshot agree: whatever this answer is
	// about, this run has nothing to do about it.
	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmail(),
		Dir:    "drizzle",
		Name:   "settled",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
		Renames: []pg.RenameDecision{{
			Rename: pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			// A refusal, recorded against the pair it was asked about.
			IsRename: false,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("a schema that matches the snapshot generated %q", res.SQL)
	}
	body, ok := capt.written[pg.RenameLogFile]
	if !ok {
		t.Fatalf("the answer was not recorded; files written: %v", keysOf(capt.written))
	}
	if !strings.Contains(string(body), `"emailAddress"`) {
		t.Fatalf("%s:\n%s", pg.RenameLogFile, body)
	}
	if len(res.RenameLog) == 0 {
		t.Error("the result does not report the log it wrote")
	}

	// And that is what it is for: the question this answers is asked by
	// the next run, and the recorded answer is what keeps it from
	// stopping a run with nobody at it.
	next := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	next.read["drizzle/meta/_renames.json"] = &fstest.MapFile{Data: body}
	if _, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmailAddress(),
		Dir:    "drizzle",
		Name:   "drop",
		FS:     next.read,
		Write:  next.write,
		Now:    func() int64 { return 1700000002000 },
	}); err != nil {
		t.Fatalf("the recorded answer did not settle the question: %v", err)
	}
}

// A settled run that was told nothing writes nothing. Recording an
// answer that was not given would rewrite the log on every no-op run
// and report a file nobody changed.
func TestGenerateNoOpWithoutAnAnswerWritesNothing(t *testing.T) {
	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	capt.read["drizzle/meta/_renames.json"] = &fstest.MapFile{Data: []byte(
		`{"version":"1","dialect":"postgresql","decisions":[` +
			`{"kind":"column","table":"users","from":"email","to":"emailAddress","rename":false}]}`)}
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmail(),
		Dir:    "drizzle",
		Name:   "settled",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("NoOp = false, SQL:\n%s", res.SQL)
	}
	if len(capt.written) != 0 {
		t.Errorf("a no-op run with no answer to record wrote %v", keysOf(capt.written))
	}
}

// The refusal Push raises has to name the two ways out that Push has.
// It has neither the flags `drops generate` takes nor the log they are
// written to, and a message that names the wrong two is worse than one
// that names none.
func TestPushRenameAdviceReplacesTheGeneratorsWording(t *testing.T) {
	candidates := []pg.RenameCandidate{{
		Rename:   pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
		FromType: "text", ToType: "text",
	}}
	generated := (&pg.RenameAmbiguityError{Candidates: candidates}).Error()
	if !strings.Contains(generated, "meta/_renames.json") ||
		!strings.Contains(generated, "--rename-column users.email=emailAddress") {
		t.Fatalf("the generator's wording changed:\n%s", generated)
	}

	pushed := (&pg.RenameAmbiguityError{Candidates: candidates, Advice: "answer it in the schema"}).Error()
	if !strings.Contains(pushed, `column "email" on table "users" is gone`) {
		t.Errorf("the refusal stopped naming the columns:\n%s", pushed)
	}
	if !strings.Contains(pushed, "answer it in the schema") {
		t.Errorf("the refusal does not carry the advice it was given:\n%s", pushed)
	}
	for _, unwanted := range []string{"meta/_renames.json", "--rename-column", "--drop-column"} {
		if strings.Contains(pushed, unwanted) {
			t.Errorf("the refusal still points at %q, which this caller does not have:\n%s", unwanted, pushed)
		}
	}
}

// A declaration is the schema's standing answer; the answer given for
// this run beats it. That is what PushOptions.Renames and
// GenerateOptions.Renames promise, and it is what the refusal Push
// prints tells a reader to reach for — "one naming only the object that
// is going declines".
//
// The two answers are not the same shape. A rename names a pair; a
// refusal names only the object that is going, because what the diff
// guessed it might have become is drops's question and not the
// operator's. So the refusal cannot beat the declaration by carrying
// the same key, and it has to beat it by naming the same column.
func TestDeclineBeatsTheSchemasDeclaration(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("emailAddress").NotNull().RenamedFrom("email"))

	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(users),
		Dir:    "drizzle",
		Name:   "no_really_drop_it",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
		Renames: []pg.RenameDecision{{
			// No To: this names only the column that is going.
			Rename: pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email"},
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if strings.Contains(res.SQL, "RENAME COLUMN") {
		t.Fatalf("the declaration outvoted the answer given for this run:\n%s", res.SQL)
	}
	if !strings.Contains(res.SQL, `DROP COLUMN "email"`) {
		t.Fatalf("the column the run said was going is not being dropped:\n%s", res.SQL)
	}
}

// The same, for a declaration the detector never offered as a
// candidate — the case where the old name is still in the database and
// the new one is there too, so nothing pairs and the declaration is
// replayed on its own. Without the refusal reaching it, there was no
// way past it at all: the pair-shaped answer has a different key from
// the declaration only when it agrees with it, and validateRenames then
// rejects the replay with advice about writing a migration, which is
// not something a push has.
func TestDeclineBeatsAReplayedDeclaration(t *testing.T) {
	// The database kept the old column and gained the new one.
	both := pg.NewTable("users")
	pg.Add(both, pg.BigSerial("id").PrimaryKey())
	pg.Add(both, pg.Text("email").NotNull())
	pg.Add(both, pg.Text("emailAddress").NotNull())

	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("emailAddress").NotNull().RenamedFrom("email"))

	capt := newCaptureFS(withPrevSnapshot(t, pg.NewSchema(both)))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(users),
		Dir:    "drizzle",
		Name:   "settle_it",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
		Renames: []pg.RenameDecision{{
			Rename: pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email"},
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, `DROP COLUMN "email"`) {
		t.Fatalf("the column the run said was going is not being dropped:\n%s", res.SQL)
	}
}
