package mysql_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/mysql"
)

// The two revisions of one table every test below moves between: the
// same table, the same column, a different name.
func renameUsersBefore() *mysql.Schema {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(users, mysql.Varchar("email", 190).NotNull())
	return mysql.NewSchema(users)
}

func renameUsersAfter() *mysql.Schema {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(users, mysql.Varchar("emailAddress", 190).NotNull())
	return mysql.NewSchema(users)
}

func TestDetectRenamesFindsAColumnPair(t *testing.T) {
	got := mysql.DetectRenames(
		mysql.BuildSnapshot(renameUsersBefore()),
		mysql.BuildSnapshot(renameUsersAfter()))
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != mysql.RenameColumn || c.Table != "users" || c.From != "email" || c.To != "emailAddress" {
		t.Errorf("candidate: %+v", c)
	}
}

func TestDetectRenamesIgnoresIncompatibleTypes(t *testing.T) {
	before := mysql.NewTable("users")
	mysql.Add(before, mysql.BigInt("id").PrimaryKey())
	mysql.Add(before, mysql.Timestamp("lastSeen", false))
	after := mysql.NewTable("users")
	mysql.Add(after, mysql.BigInt("id").PrimaryKey())
	mysql.Add(after, mysql.Text("bio"))

	got := mysql.DetectRenames(
		mysql.BuildSnapshot(mysql.NewSchema(before)),
		mysql.BuildSnapshot(mysql.NewSchema(after)))
	if len(got) != 0 {
		t.Errorf("got %d candidates, want none: %+v", len(got), got)
	}
}

// The bug: without an answer the diff drops the column and adds the
// other one, and on a server with no transactional DDL there is nothing
// to undo it with.
func TestDiffRenamesAColumnInsteadOfDroppingIt(t *testing.T) {
	prev := mysql.BuildSnapshot(renameUsersBefore())
	cur := mysql.BuildSnapshot(renameUsersAfter())

	stmts := mysql.Diff(prev, cur, mysql.DiffOptions{
		Server:  mysql.ServerInfo{MariaDB: true, Major: 10, Minor: 11},
		Renames: []mysql.Rename{{Kind: mysql.RenameColumn, Table: "users", From: "email", To: "emailAddress"}},
	})
	want := "ALTER TABLE `users` RENAME COLUMN `email` TO `emailAddress`;"
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

// A server too old for RENAME COLUMN — and a DiffOptions with no server
// in it, which means one of unknown version — gets CHANGE COLUMN, which
// every server understands and which also preserves the data.
func TestDiffRenamesWithChangeColumnOnAnOlderServer(t *testing.T) {
	prev := mysql.BuildSnapshot(renameUsersBefore())
	cur := mysql.BuildSnapshot(renameUsersAfter())

	stmts := mysql.Diff(prev, cur, mysql.DiffOptions{
		Renames: []mysql.Rename{{Kind: mysql.RenameColumn, Table: "users", From: "email", To: "emailAddress"}},
	})
	want := "ALTER TABLE `users` CHANGE COLUMN `email` `emailAddress` varchar(190) NOT NULL;"
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

func TestDiffRenamesATableInsteadOfDroppingIt(t *testing.T) {
	before := mysql.NewTable("users")
	mysql.Add(before, mysql.BigInt("id").PrimaryKey())
	after := mysql.NewTable("people")
	mysql.Add(after, mysql.BigInt("id").PrimaryKey())

	stmts := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(before)),
		mysql.BuildSnapshot(mysql.NewSchema(after)),
		mysql.DiffOptions{Renames: []mysql.Rename{{Kind: mysql.RenameTable, From: "users", To: "people"}}})
	want := "ALTER TABLE `users` RENAME TO `people`;"
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

func TestDiffDownReversesARename(t *testing.T) {
	stmts := mysql.DiffDown(
		mysql.BuildSnapshot(renameUsersBefore()),
		mysql.BuildSnapshot(renameUsersAfter()),
		mysql.DiffOptions{
			Server:  mysql.ServerInfo{MariaDB: true, Major: 10, Minor: 11},
			Renames: []mysql.Rename{{Kind: mysql.RenameColumn, Table: "users", From: "email", To: "emailAddress"}},
		})
	want := "ALTER TABLE `users` RENAME COLUMN `emailAddress` TO `email`;"
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

// renameFixtureFS builds the migration directory a generator run reads.
func renameFixtureFS(t *testing.T, schema *mysql.Schema) fstest.MapFS {
	t.Helper()
	body, err := mysql.BuildSnapshot(schema).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"migrations/meta/0000_snapshot.json": {Data: body},
		"migrations/meta/_journal.json": {Data: []byte(
			`{"version":"5","dialect":"mysql","entries":[` +
				`{"idx":0,"version":"5","when":1,"tag":"0000_init","breakpoints":true}]}`)},
	}
}

// The important one: a run nobody can answer stops rather than
// guessing, and leaves the directory as it found it.
func TestGenerateRefusesAnUnansweredRename(t *testing.T) {
	files := map[string][]byte{}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: renameUsersAfter(),
		Dir:    "migrations",
		Name:   "rename",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
	})
	if err == nil {
		t.Fatalf("generated a migration for an ambiguous rename: %s", res.SQL)
	}
	var amb *mysql.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("error is %T, want *mysql.RenameAmbiguityError: %v", err, err)
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
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: renameUsersAfter(),
		Dir:    "migrations",
		Name:   "rename",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
		Server: mysql.ServerInfo{MariaDB: true, Major: 10, Minor: 11},
		Renames: []mysql.RenameDecision{{
			Rename:   mysql.Rename{Kind: mysql.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, "RENAME COLUMN `email` TO `emailAddress`") {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
	if strings.Contains(res.SQL, "DROP COLUMN") {
		t.Errorf("the migration drops the renamed column:\n%s", res.SQL)
	}
	body, ok := files["meta/_renames.json"]
	if !ok {
		t.Fatalf("the answer was not recorded; wrote: %v", files)
	}
	var log struct {
		Decisions []struct {
			From     string `json:"from"`
			To       string `json:"to"`
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
	fsys["migrations/meta/_renames.json"] = &fstest.MapFile{Data: []byte(
		`{"version":"1","dialect":"mysql","decisions":[` +
			`{"kind":"column","table":"users","from":"email","to":"emailAddress","rename":true}]}`)}

	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: renameUsersAfter(),
		Dir:    "migrations",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { return nil },
		Now:    func() int64 { return 2 },
		Server: mysql.ServerInfo{MariaDB: true, Major: 10, Minor: 11},
	})
	if err != nil {
		t.Fatalf("a recorded answer was not replayed: %v", err)
	}
	if !strings.Contains(res.SQL, "RENAME COLUMN `email` TO `emailAddress`") {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
}

// --- A rename inside a rename -----------------------------------------

// The same table under two names, with one column also renamed: the
// shape a migration has when a table is renamed and a column inside it
// tidied in one commit.
func renamePeopleAfter() *mysql.Schema {
	people := mysql.NewTable("people")
	mysql.Add(people, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(people, mysql.Varchar("emailAddress", 190).NotNull())
	return mysql.NewSchema(people)
}

func TestDetectRenamesFindsATablePairThatAlsoChangedAColumn(t *testing.T) {
	got := mysql.DetectRenames(
		mysql.BuildSnapshot(renameUsersBefore()),
		mysql.BuildSnapshot(renamePeopleAfter()))
	var found bool
	for _, c := range got {
		if c.Kind == mysql.RenameTable && c.From == "users" && c.To == "people" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no table candidate for users/people: %+v", got)
	}
}

// Once the table rename is answered, the column inside it is a question
// in its own right rather than a DROP COLUMN nobody was asked about.
func TestResolveRenamesAsksAboutAColumnInsideARenamedTable(t *testing.T) {
	prev := mysql.BuildSnapshot(renameUsersBefore())
	cur := mysql.BuildSnapshot(renamePeopleAfter())

	applied, unresolved := mysql.ResolveRenames(prev, cur, []mysql.RenameDecision{{
		Rename:   mysql.Rename{Kind: mysql.RenameTable, From: "users", To: "people"},
		IsRename: true,
	}})
	if len(applied) != 1 || applied[0].Kind != mysql.RenameTable {
		t.Fatalf("applied: %+v", applied)
	}
	if len(unresolved) != 1 {
		t.Fatalf("the column inside the renamed table was not asked about: %+v", unresolved)
	}
	c := unresolved[0]
	if c.Kind != mysql.RenameColumn || c.Table != "people" || c.From != "email" || c.To != "emailAddress" {
		t.Errorf("candidate: %+v", c)
	}
}

func TestGenerateRefusesARenameInsideARenamedTable(t *testing.T) {
	files := map[string][]byte{}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: renamePeopleAfter(),
		Dir:    "migrations",
		Name:   "rename",
		FS:     renameFixtureFS(t, renameUsersBefore()),
		Write:  func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:    func() int64 { return 2 },
		Renames: []mysql.RenameDecision{{
			Rename:   mysql.Rename{Kind: mysql.RenameTable, From: "users", To: "people"},
			IsRename: true,
		}},
	})
	var amb *mysql.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("generated a migration that drops the renamed column: %v\n%s", err, res.SQL)
	}
	if len(files) != 0 {
		t.Errorf("a refused run wrote files: %v", files)
	}
}

// --- Renaming onto a name that is already taken ------------------------

func TestGenerateRejectsARenameOntoAnOccupiedColumnName(t *testing.T) {
	before := mysql.NewTable("users")
	mysql.Add(before, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(before, mysql.Varchar("email", 190).NotNull())
	mysql.Add(before, mysql.Varchar("alias", 190))
	after := mysql.NewTable("users")
	mysql.Add(after, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(after, mysql.Varchar("alias", 190))

	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: mysql.NewSchema(after),
		Dir:    "migrations",
		Name:   "rename",
		FS:     renameFixtureFS(t, mysql.NewSchema(before)),
		Write:  func(string, []byte) error { return nil },
		Now:    func() int64 { return 2 },
		Renames: []mysql.RenameDecision{{
			Rename:   mysql.Rename{Kind: mysql.RenameColumn, Table: "users", From: "email", To: "alias"},
			IsRename: true,
		}},
	})
	if err == nil {
		t.Fatalf("generated a migration the server will reject:\n%s", res.SQL)
	}
	if !strings.Contains(err.Error(), `already has a column "alias"`) {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}

func TestGenerateRejectsATableRenameOntoAnOccupiedName(t *testing.T) {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	people := mysql.NewTable("people")
	mysql.Add(people, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	after := mysql.NewTable("people")
	mysql.Add(after, mysql.BigInt("id").PrimaryKey().AutoIncrement())

	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: mysql.NewSchema(after),
		Dir:    "migrations",
		Name:   "rename",
		FS:     renameFixtureFS(t, mysql.NewSchema(users, people)),
		Write:  func(string, []byte) error { return nil },
		Now:    func() int64 { return 2 },
		Renames: []mysql.RenameDecision{{
			Rename:   mysql.Rename{Kind: mysql.RenameTable, From: "users", To: "people"},
			IsRename: true,
		}},
	})
	if err == nil {
		t.Fatalf("generated a migration the server will reject:\n%s", res.SQL)
	}
	if !strings.Contains(err.Error(), `already holds a table "people"`) {
		t.Errorf("error does not say what is wrong: %v", err)
	}
}
