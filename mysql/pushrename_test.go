package mysql_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/mysql"
)

// The schema as the carrier of a rename answer.
//
// Push has no migration directory, so meta/_renames.json is not
// available to it and the answer has to come from the schema itself.
// These tests cover the reading of that declaration and its effect on
// the generator; that a live push honours it — and, on this dialect,
// that the DROP COLUMN it prevents commits itself and cannot be rolled
// back — is a server behaviour and lives in the integration suite.

func TestDeclaredRenamesReadsColumnAndTable(t *testing.T) {
	people := mysql.NewTable("people").RenamedFrom("users")
	mysql.Add(people, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(people, mysql.Varchar("emailAddress", 190).NotNull().RenamedFrom("email"))
	mysql.Add(people, mysql.Varchar("nickname", 190))

	got := mysql.DeclaredRenames(mysql.NewSchema(people))
	want := []mysql.RenameDecision{
		{Rename: mysql.Rename{Kind: mysql.RenameColumn, Table: "people", From: "email", To: "emailAddress"}, IsRename: true},
		{Rename: mysql.Rename{Kind: mysql.RenameTable, From: "users", To: "people"}, IsRename: true},
	}
	if len(got) != len(want) {
		t.Fatalf("DeclaredRenames = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DeclaredRenames[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got := mysql.DeclaredRenames(renameUsersBefore()); len(got) != 0 {
		t.Errorf("an unannotated schema answered %+v", got)
	}
}

// The generator reads the declaration too. Otherwise a schema that
// states a rename would answer a push and not a generate, and the same
// schema would mean two different things to the two commands.
func TestGenerateTakesTheRenameTheSchemaDeclares(t *testing.T) {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(users, mysql.Varchar("emailAddress", 190).NotNull().RenamedFrom("email"))

	files := map[string][]byte{}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: mysql.NewSchema(users),
		Dir:    "migrations",
		Name:   "rename",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, "CHANGE COLUMN `email` `emailAddress`") {
		t.Fatalf("migration:\n%s", res.SQL)
	}
	// The declaration is a fact the schema carries, not an answer
	// somebody typed, so nothing is written into the log for it.
	if body, ok := files[mysql.RenameLogFile]; ok {
		t.Errorf("a declared rename was written into the answer log:\n%s", body)
	}
}

// An answer given on a run whose diff came to nothing is still an
// answer. It used to be dropped on the floor: GenerateMigration
// returned its NoOp before the log was written, so a decision that
// settled a question without moving the schema had to be given again
// — by a run that may have nobody to give it.
func TestGenerateRecordsAnAnswerThatProducedNoStatements(t *testing.T) {
	files := map[string][]byte{}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: renameUsersBefore(),
		Dir:    "migrations",
		Name:   "settled",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
		Renames: []mysql.RenameDecision{{
			Rename:   mysql.Rename{Kind: mysql.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			IsRename: false,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("a schema that matches the snapshot generated %q", res.SQL)
	}
	body, ok := files[mysql.RenameLogFile]
	if !ok {
		t.Fatalf("the answer was not recorded; files written: %v", files)
	}
	if !strings.Contains(string(body), `"emailAddress"`) {
		t.Fatalf("%s:\n%s", mysql.RenameLogFile, body)
	}

	// And that is what it is for: the question this answers is asked by
	// the next run, and the recorded answer is what keeps it from
	// stopping a run with nobody at it.
	fsys := renameFixtureFS(t, renameUsersBefore())
	fsys["migrations/meta/_renames.json"] = &fstest.MapFile{Data: body}
	if _, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: renameUsersAfter(),
		Dir:    "migrations",
		Name:   "drop",
		FS:     fsys,
		Write:  func(string, []byte) error { return nil },
		Now:    func() int64 { return 3 },
	}); err != nil {
		t.Fatalf("the recorded answer did not settle the question: %v", err)
	}
}

// A settled run that was told nothing writes nothing.
func TestGenerateNoOpWithoutAnAnswerWritesNothing(t *testing.T) {
	fsys := renameFixtureFS(t, renameUsersBefore())
	fsys["migrations/meta/_renames.json"] = &fstest.MapFile{Data: []byte(
		`{"version":"1","dialect":"mysql","decisions":[` +
			`{"kind":"column","table":"users","from":"email","to":"emailAddress","rename":false}]}`)}
	files := map[string][]byte{}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: renameUsersBefore(),
		Dir:    "migrations",
		Name:   "settled",
		FS:     fsys,
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("NoOp = false, SQL:\n%s", res.SQL)
	}
	if len(files) != 0 {
		t.Errorf("a no-op run with no answer to record wrote %v", files)
	}
}

// The refusal Push raises has to name the ways out that Push has. It
// has neither the flags `drops generate` takes nor the log they are
// written to, and a message that names the wrong ones is worse than
// one that names none.
func TestPushRenameAdviceReplacesTheGeneratorsWording(t *testing.T) {
	candidates := []mysql.RenameCandidate{{
		Rename:   mysql.Rename{Kind: mysql.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
		FromType: "varchar(190)", ToType: "varchar(190)",
	}}
	generated := (&mysql.RenameAmbiguityError{Candidates: candidates}).Error()
	if !strings.Contains(generated, "meta/_renames.json") ||
		!strings.Contains(generated, "--rename-column users.email=emailAddress") {
		t.Fatalf("the generator's wording changed:\n%s", generated)
	}

	pushed := (&mysql.RenameAmbiguityError{Candidates: candidates, Advice: "answer it in the schema"}).Error()
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
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(users, mysql.Varchar("emailAddress", 190).NotNull().RenamedFrom("email"))

	files := map[string][]byte{}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: mysql.NewSchema(users),
		Dir:    "migrations",
		Name:   "no_really_drop_it",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
		Renames: []mysql.RenameDecision{{
			// No To: this names only the column that is going.
			Rename: mysql.Rename{Kind: mysql.RenameColumn, Table: "users", From: "email"},
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if strings.Contains(res.SQL, "RENAME COLUMN") {
		t.Fatalf("the declaration outvoted the answer given for this run:\n%s", res.SQL)
	}
	if !strings.Contains(res.SQL, "DROP COLUMN `email`") {
		t.Fatalf("the column the run said was going is not being dropped:\n%s", res.SQL)
	}
}
