package sqlite_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/sqlite"
)

// The two revisions of one table every test below moves between: the
// same table, the same column, a different name.
func renameUsersBefore() *sqlite.Schema {
	users := sqlite.NewTable("users")
	sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(users, sqlite.Text("email").NotNull())
	return sqlite.NewSchema(users)
}

func renameUsersAfter() *sqlite.Schema {
	users := sqlite.NewTable("users")
	sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(users, sqlite.Text("emailAddress").NotNull())
	return sqlite.NewSchema(users)
}

func TestDetectRenamesFindsAColumnPair(t *testing.T) {
	got := sqlite.DetectRenames(
		sqlite.BuildSnapshot(renameUsersBefore()),
		sqlite.BuildSnapshot(renameUsersAfter()))
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != sqlite.RenameColumn || c.Table != "users" || c.From != "email" || c.To != "emailAddress" {
		t.Errorf("candidate: %+v", c)
	}
}

// SQLite's own affinity rule folds BOOLEAN and DATETIME into NUMERIC,
// which would offer a dropped flag and an added timestamp as a rename.
// The declared type is what is compared instead.
func TestDetectRenamesIgnoresIncompatibleTypes(t *testing.T) {
	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Boolean("isActive"))
	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Timestamp("seenAt", false))

	got := sqlite.DetectRenames(
		sqlite.BuildSnapshot(sqlite.NewSchema(before)),
		sqlite.BuildSnapshot(sqlite.NewSchema(after)))
	if len(got) != 0 {
		t.Errorf("got %d candidates, want none: %+v", len(got), got)
	}
}

// The bug, in the shape it takes here: a dropped column forces a table
// rebuild, and a rebuild copies only the columns the two snapshots
// agree on — so the renamed column is silently left behind, without a
// DROP COLUMN anywhere in the migration to warn anybody.
func TestDiffRenamesAColumnInsteadOfRebuildingWithoutIt(t *testing.T) {
	prev := sqlite.BuildSnapshot(renameUsersBefore())
	cur := sqlite.BuildSnapshot(renameUsersAfter())

	stmts := sqlite.Diff(prev, cur, sqlite.DiffOptions{Renames: []sqlite.Rename{
		{Kind: sqlite.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
	}})
	want := `ALTER TABLE "users" RENAME COLUMN "email" TO "emailAddress";`
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

// Without the rename the same change is a rebuild whose INSERT ...
// SELECT never mentions the column. This is the statement that loses
// the data; the test above is the one that does not.
func TestDiffWithoutARenameRebuildsWithoutTheColumn(t *testing.T) {
	stmts := sqlite.Diff(
		sqlite.BuildSnapshot(renameUsersBefore()),
		sqlite.BuildSnapshot(renameUsersAfter()))
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, `INSERT INTO "users_new" ("id") SELECT "id" FROM "users";`) {
		t.Fatalf("expected a rebuild that copies only \"id\":\n%s", joined)
	}
}

// A rename that also forces a rebuild — here by adding a NOT NULL
// column with no default — renames first, so the copy that follows
// finds the column under its new name.
func TestDiffRenameSurvivesARebuild(t *testing.T) {
	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("email").NotNull())

	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("emailAddress").NotNull())
	sqlite.Add(after, sqlite.Text("handle").NotNull())

	stmts := sqlite.Diff(
		sqlite.BuildSnapshot(sqlite.NewSchema(before)),
		sqlite.BuildSnapshot(sqlite.NewSchema(after)),
		sqlite.DiffOptions{Renames: []sqlite.Rename{
			{Kind: sqlite.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
		}})
	joined := strings.Join(stmts, "\n")
	if stmts[0] != `ALTER TABLE "users" RENAME COLUMN "email" TO "emailAddress";` {
		t.Fatalf("the rename must come first:\n%s", joined)
	}
	if !strings.Contains(joined, `INSERT INTO "users_new" ("emailAddress", "id") SELECT "emailAddress", "id" FROM "users";`) {
		t.Fatalf("the rebuild did not carry the renamed column across:\n%s", joined)
	}
}

func TestDiffRenamesATableInsteadOfDroppingIt(t *testing.T) {
	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	after := sqlite.NewTable("people")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())

	stmts := sqlite.Diff(
		sqlite.BuildSnapshot(sqlite.NewSchema(before)),
		sqlite.BuildSnapshot(sqlite.NewSchema(after)),
		sqlite.DiffOptions{Renames: []sqlite.Rename{
			{Kind: sqlite.RenameTable, From: "users", To: "people"},
		}})
	want := `ALTER TABLE "users" RENAME TO "people";`
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

func TestDiffDownReversesARename(t *testing.T) {
	stmts := sqlite.DiffDown(
		sqlite.BuildSnapshot(renameUsersBefore()),
		sqlite.BuildSnapshot(renameUsersAfter()),
		sqlite.DiffOptions{Renames: []sqlite.Rename{
			{Kind: sqlite.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
		}})
	want := `ALTER TABLE "users" RENAME COLUMN "emailAddress" TO "email";`
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

// renameFixtureFS builds the migration directory a generator run reads.
func renameFixtureFS(t *testing.T, schema *sqlite.Schema) fstest.MapFS {
	t.Helper()
	body, err := sqlite.BuildSnapshot(schema).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"drizzle/meta/0000_snapshot.json": {Data: body},
		"drizzle/meta/_journal.json": {Data: []byte(
			`{"version":"7","dialect":"sqlite","entries":[` +
				`{"idx":0,"version":"7","when":1,"tag":"0000_init","breakpoints":true}]}`)},
	}
}

// The important one: a run nobody can answer stops rather than
// guessing, and leaves the directory as it found it.
func TestGenerateRefusesAnUnansweredRename(t *testing.T) {
	files := map[string][]byte{}
	res, err := sqlite.GenerateMigration(sqlite.GenerateOptions{
		Schema: renameUsersAfter(),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
	})
	if err == nil {
		t.Fatalf("generated a migration for an ambiguous rename: %s", res.SQL)
	}
	var amb *sqlite.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("error is %T, want *sqlite.RenameAmbiguityError: %v", err, err)
	}
	if !strings.Contains(amb.Error(), "--rename-column users.email=emailAddress") {
		t.Errorf("the refusal does not say how to resolve it:\n%s", amb.Error())
	}
	if len(files) != 0 {
		t.Errorf("a refused run wrote files: %v", files)
	}
}

func TestGenerateAppliesAndRecordsTheAnswer(t *testing.T) {
	files := map[string][]byte{}
	res, err := sqlite.GenerateMigration(sqlite.GenerateOptions{
		Schema: renameUsersAfter(),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
		Renames: []sqlite.RenameDecision{{
			Rename:   sqlite.Rename{Kind: sqlite.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, `RENAME COLUMN "email" TO "emailAddress"`) {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
	if strings.Contains(res.SQL, "-- rebuild") {
		t.Errorf("a plain rename must not rebuild the table:\n%s", res.SQL)
	}
	body, ok := files["meta/_renames.json"]
	if !ok {
		t.Fatalf("the answer was not recorded; wrote: %v", files)
	}
	var log struct {
		Decisions []struct {
			From     string `json:"from"`
			IsRename bool   `json:"rename"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(body, &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Decisions) != 1 || log.Decisions[0].From != "email" || !log.Decisions[0].IsRename {
		t.Errorf("recorded: %s", body)
	}
}

// The recorded answer is the point: a second run, with no flags and no
// terminal, finds it and does not ask.
func TestGenerateReplaysARecordedAnswer(t *testing.T) {
	fsys := renameFixtureFS(t, renameUsersBefore())
	fsys["drizzle/meta/_renames.json"] = &fstest.MapFile{Data: []byte(
		`{"version":"1","dialect":"sqlite","decisions":[` +
			`{"kind":"column","table":"users","from":"email","to":"emailAddress","rename":true}]}`)}

	res, err := sqlite.GenerateMigration(sqlite.GenerateOptions{
		Schema: renameUsersAfter(),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { return nil },
		Now:    func() int64 { return 2 },
	})
	if err != nil {
		t.Fatalf("a recorded answer was not replayed: %v", err)
	}
	if !strings.Contains(res.SQL, `RENAME COLUMN "email" TO "emailAddress"`) {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
}
