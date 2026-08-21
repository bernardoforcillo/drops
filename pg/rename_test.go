package pg_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/pg"
)

// usersWithEmail / usersWithEmailAddress are the two revisions of one
// table that every test below moves between: the same table, the same
// column, a different name.
func usersWithEmail() *pg.Schema {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("email").NotNull())
	return pg.NewSchema(users)
}

func usersWithEmailAddress() *pg.Schema {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("emailAddress").NotNull())
	return pg.NewSchema(users)
}

func TestDetectRenamesFindsAColumnPair(t *testing.T) {
	prev := pg.BuildSnapshot(usersWithEmail())
	cur := pg.BuildSnapshot(usersWithEmailAddress())

	got := pg.DetectRenames(prev, cur)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.Kind != pg.RenameColumn || c.Table != "users" || c.From != "email" || c.To != "emailAddress" {
		t.Errorf("candidate: %+v", c)
	}
}

// A dropped column and an added one of an unrelated type is a drop and
// an add, and asking about it would be noise nobody would ever answer
// yes to.
func TestDetectRenamesIgnoresIncompatibleTypes(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Timestamp("lastSeen", true))
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Boolean("isActive"))
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	if got := pg.DetectRenames(prev, cur); len(got) != 0 {
		t.Errorf("got %d candidates, want none: %+v", len(got), got)
	}
}

// Renaming and widening in one step is one change, not two, so the
// candidate has to survive the type difference.
func TestDetectRenamesSurvivesAWidening(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Varchar("email", 120))
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("emailAddress"))
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	got := pg.DetectRenames(prev, cur)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].FromType != "varchar(120)" || got[0].ToType != "text" {
		t.Errorf("types: %+v", got[0])
	}
}

func TestDetectRenamesFindsATablePair(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("name").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("people")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("name").NotNull())
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	got := pg.DetectRenames(prev, cur)
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Kind != pg.RenameTable || got[0].From != "users" || got[0].To != "people" {
		t.Errorf("candidate: %+v", got[0])
	}
}

// A table dropped and an unrelated one added do not have the same
// columns, so the pair is never offered.
func TestDetectRenamesIgnoresUnrelatedTables(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("name").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("audit")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("action").NotNull())
	pg.Add(after, pg.Text("actor").NotNull())
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	if got := pg.DetectRenames(prev, cur); len(got) != 0 {
		t.Errorf("got %d candidates, want none: %+v", len(got), got)
	}
}

// The bug this whole file exists for: without an answer the diff turns
// a rename into a DROP COLUMN and an ADD COLUMN, which is the data
// going away. With one it is a RENAME and nothing else.
func TestDiffRenamesAColumnInsteadOfDroppingIt(t *testing.T) {
	prev := pg.BuildSnapshot(usersWithEmail())
	cur := pg.BuildSnapshot(usersWithEmailAddress())

	stmts := pg.Diff(prev, cur, pg.DiffOptions{Renames: []pg.Rename{
		{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
	}})
	want := `ALTER TABLE "users" RENAME COLUMN "email" TO "emailAddress";`
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

func TestDiffRenamesATableInsteadOfDroppingIt(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("name").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("people")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("name").NotNull())
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur, pg.DiffOptions{Renames: []pg.Rename{
		{Kind: pg.RenameTable, From: "users", To: "people"},
	}})
	want := `ALTER TABLE "users" RENAME TO "people";`
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

// A rename that also changes the type is a RENAME and a SET DATA TYPE,
// because the rest of the diff runs against a previous schema in which
// the rename has already happened.
func TestDiffRenameCarriesTheResidualChange(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Varchar("email", 120))
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("emailAddress"))
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur, pg.DiffOptions{Renames: []pg.Rename{
		{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
	}})
	joined := strings.Join(stmts, "\n")
	if len(stmts) != 2 {
		t.Fatalf("got %d statements, want 2:\n%s", len(stmts), joined)
	}
	if stmts[0] != `ALTER TABLE "users" RENAME COLUMN "email" TO "emailAddress";` {
		t.Errorf("first statement: %s", stmts[0])
	}
	if stmts[1] != `ALTER TABLE "users" ALTER COLUMN "emailAddress" SET DATA TYPE text;` {
		t.Errorf("second statement: %s", stmts[1])
	}
	if strings.Contains(joined, "DROP COLUMN") {
		t.Errorf("a renamed column must not be dropped:\n%s", joined)
	}
}

// A foreign key pointing at the renamed table must not be dropped and
// re-added: after the RENAME it points at the same table under its new
// name, which is exactly what PostgreSQL does to it.
func TestDiffTableRenameFollowsForeignKeys(t *testing.T) {
	before := pg.NewTable("users")
	beforeID := pg.Add(before, pg.BigSerial("id").PrimaryKey())
	beforePosts := pg.NewTable("posts")
	pg.Add(beforePosts, pg.BigSerial("id").PrimaryKey())
	pg.Add(beforePosts, pg.BigInt("userId").References(beforeID))
	prev := pg.BuildSnapshot(pg.NewSchema(before, beforePosts))

	after := pg.NewTable("people")
	afterID := pg.Add(after, pg.BigSerial("id").PrimaryKey())
	afterPosts := pg.NewTable("posts")
	pg.Add(afterPosts, pg.BigSerial("id").PrimaryKey())
	pg.Add(afterPosts, pg.BigInt("userId").References(afterID))
	cur := pg.BuildSnapshot(pg.NewSchema(after, afterPosts))

	stmts := pg.Diff(prev, cur, pg.DiffOptions{Renames: []pg.Rename{
		{Kind: pg.RenameTable, From: "users", To: "people"},
	}})
	joined := strings.Join(stmts, "\n")
	if strings.Contains(joined, "DROP TABLE") || strings.Contains(joined, "CREATE TABLE") {
		t.Fatalf("a renamed table must not be dropped or created:\n%s", joined)
	}
	// The constraint's generated name is built from the target table's,
	// so it does change — but by being renamed in place, not by the
	// table going away.
	if !strings.Contains(joined, `ALTER TABLE "users" RENAME TO "people";`) {
		t.Errorf("missing the RENAME:\n%s", joined)
	}
}

func TestDiffDownReversesARename(t *testing.T) {
	prev := pg.BuildSnapshot(usersWithEmail())
	cur := pg.BuildSnapshot(usersWithEmailAddress())

	stmts := pg.DiffDown(prev, cur, pg.DiffOptions{Renames: []pg.Rename{
		{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
	}})
	want := `ALTER TABLE "users" RENAME COLUMN "emailAddress" TO "email";`
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got:\n%s\nwant one statement:\n%s", strings.Join(stmts, "\n"), want)
	}
}

// --- The generator ----------------------------------------------------

// withPrevSnapshot builds the migration directory a generator run reads:
// one previous snapshot and a journal naming it.
func withPrevSnapshot(t *testing.T, schema *pg.Schema) fstest.MapFS {
	t.Helper()
	body, err := pg.BuildSnapshot(schema).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"drizzle/0000_init.sql":           {Data: []byte("/* placeholder */")},
		"drizzle/meta/0000_snapshot.json": {Data: body},
		"drizzle/meta/_journal.json": {Data: []byte(
			`{"version":"7","dialect":"postgresql","entries":[` +
				`{"idx":0,"version":"7","when":1700000000000,"tag":"0000_init","breakpoints":true}]}`)},
	}
}

// The important one: a run nobody can answer stops rather than
// guessing, and leaves the directory as it found it.
func TestGenerateRefusesAnUnansweredRename(t *testing.T) {
	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmailAddress(),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
	})
	if err == nil {
		t.Fatalf("generated a migration for an ambiguous rename: %s", res.SQL)
	}
	var amb *pg.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("error is %T, want *pg.RenameAmbiguityError: %v", err, err)
	}
	if len(amb.Candidates) != 1 || amb.Candidates[0].From != "email" {
		t.Errorf("candidates: %+v", amb.Candidates)
	}
	for _, want := range []string{"--rename-column users.email=emailAddress", "--drop-column users.email"} {
		if !strings.Contains(amb.Error(), want) {
			t.Errorf("the refusal does not say how to resolve it (%q):\n%s", want, amb.Error())
		}
	}
	if len(capt.written) != 0 {
		t.Errorf("a refused run wrote files: %v", keysOf(capt.written))
	}
}

func TestGenerateAppliesAndRecordsTheAnswer(t *testing.T) {
	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmailAddress(),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
		Renames: []pg.RenameDecision{{
			Rename:   pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, `RENAME COLUMN "email" TO "emailAddress"`) {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
	if strings.Contains(res.SQL, "DROP COLUMN") {
		t.Errorf("the migration drops the renamed column:\n%s", res.SQL)
	}
	if len(res.Renames) != 1 {
		t.Errorf("result renames: %+v", res.Renames)
	}

	body, ok := capt.written["meta/_renames.json"]
	if !ok {
		t.Fatalf("the answer was not recorded; wrote: %v", keysOf(capt.written))
	}
	var log struct {
		Decisions []struct {
			Kind     string `json:"kind"`
			Table    string `json:"table"`
			From     string `json:"from"`
			To       string `json:"to"`
			IsRename bool   `json:"rename"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(body, &log); err != nil {
		t.Fatal(err)
	}
	if len(log.Decisions) != 1 {
		t.Fatalf("recorded %d decisions: %s", len(log.Decisions), body)
	}
	d := log.Decisions[0]
	if d.Kind != "column" || d.Table != "users" || d.From != "email" || d.To != "emailAddress" || !d.IsRename {
		t.Errorf("recorded decision: %+v", d)
	}
}

// The recorded answer is the point: a second run, with no flags and no
// terminal, finds it and does not ask.
func TestGenerateReplaysARecordedAnswer(t *testing.T) {
	initial := withPrevSnapshot(t, usersWithEmail())
	initial["drizzle/meta/_renames.json"] = &fstest.MapFile{Data: []byte(
		`{"version":"1","dialect":"postgresql","decisions":[` +
			`{"kind":"column","table":"users","from":"email","to":"emailAddress","rename":true}]}`)}

	capt := newCaptureFS(initial)
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmailAddress(),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
	})
	if err != nil {
		t.Fatalf("a recorded answer was not replayed: %v", err)
	}
	if !strings.Contains(res.SQL, `RENAME COLUMN "email" TO "emailAddress"`) {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
}

// "No, it really is a drop" is an answer too, and produces the
// migration drops used to produce by default.
func TestGenerateTakesADropAndAddAnswer(t *testing.T) {
	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmailAddress(),
		Dir:    "drizzle",
		Name:   "replace",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
		Renames: []pg.RenameDecision{{
			Rename:   pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			IsRename: false,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, "DROP COLUMN") || !strings.Contains(res.SQL, "ADD COLUMN") {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
	if strings.Contains(res.SQL, "RENAME") {
		t.Errorf("a declined rename still emitted a RENAME:\n%s", res.SQL)
	}
}

// A schema change with nothing ambiguous in it must still generate, and
// must not start writing a rename log it has nothing to put in.
func TestGenerateWithoutCandidatesIsUnchanged(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("email"))

	capt := newCaptureFS(withPrevSnapshot(t, pg.NewSchema(before)))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after),
		Dir:    "drizzle",
		Name:   "add",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, "ADD COLUMN") {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
	if _, ok := capt.written["meta/_renames.json"]; ok {
		t.Error("wrote a rename log with no decisions in it")
	}
}

// "That column really is going" is one fact about one column, and the
// person saying it never has to name what the diff thought it might
// have become.
func TestGenerateTakesADropAnswerNamingOnlyTheOldColumn(t *testing.T) {
	capt := newCaptureFS(withPrevSnapshot(t, usersWithEmail()))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmailAddress(),
		Dir:    "drizzle",
		Name:   "replace",
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
	if !strings.Contains(res.SQL, "DROP COLUMN") || strings.Contains(res.SQL, "RENAME") {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
}

// Renaming a table and a column inside it in one migration, and rolling
// it back. The column names its table by the table's new name, in both
// directions, so the down script has to move that name back with it —
// otherwise the RENAME COLUMN names a table the statement above it has
// just renamed away.
func TestDiffDownReversesATableAndColumnRenameTogether(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("people")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("emailAddress").NotNull())
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	opts := pg.DiffOptions{Renames: []pg.Rename{
		{Kind: pg.RenameTable, From: "users", To: "people"},
		{Kind: pg.RenameColumn, Table: "people", From: "email", To: "emailAddress"},
	}}

	up := pg.Diff(prev, cur, opts)
	wantUp := []string{
		`ALTER TABLE "users" RENAME TO "people";`,
		`ALTER TABLE "people" RENAME COLUMN "email" TO "emailAddress";`,
	}
	if !sameSQL(up, wantUp) {
		t.Errorf("up:\n got %s\nwant %s", strings.Join(up, "\n"), strings.Join(wantUp, "\n"))
	}

	down := pg.DiffDown(prev, cur, opts)
	wantDown := []string{
		`ALTER TABLE "people" RENAME TO "users";`,
		`ALTER TABLE "users" RENAME COLUMN "emailAddress" TO "email";`,
	}
	if !sameSQL(down, wantDown) {
		t.Errorf("down:\n got %s\nwant %s", strings.Join(down, "\n"), strings.Join(wantDown, "\n"))
	}
}

func sameSQL(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// The detector's type test decides what drops asks about, not what is
// true. An operator who says two columns of unrelated types are the
// same column knows something the type test cannot, and the answer has
// to be honoured rather than quietly dropped on the floor.
func TestGenerateHonoursARenameTheDetectorWouldNotOffer(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Integer("code"))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("label"))

	if got := pg.DetectRenames(pg.BuildSnapshot(pg.NewSchema(before)), pg.BuildSnapshot(pg.NewSchema(after))); len(got) != 0 {
		t.Fatalf("this test needs a pair the detector does not offer: %+v", got)
	}

	capt := newCaptureFS(withPrevSnapshot(t, pg.NewSchema(before)))
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after),
		Dir:    "drizzle",
		Name:   "rename",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000001000 },
		Renames: []pg.RenameDecision{{
			Rename:   pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "code", To: "label"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, `RENAME COLUMN "code" TO "label"`) {
		t.Errorf("the stated rename was ignored:\n%s", res.SQL)
	}
	if strings.Contains(res.SQL, "DROP COLUMN") {
		t.Errorf("the stated rename still dropped the column:\n%s", res.SQL)
	}
}

// The log accumulates, so it holds answers to questions settled several
// migrations ago. Replaying one of those would emit an ALTER TABLE
// naming a column no database has had since.
func TestGenerateDoesNotReplayASettledAnswer(t *testing.T) {
	initial := withPrevSnapshot(t, usersWithEmailAddress())
	initial["drizzle/meta/_renames.json"] = &fstest.MapFile{Data: []byte(
		`{"version":"1","dialect":"postgresql","decisions":[` +
			`{"kind":"column","table":"users","from":"email","to":"emailAddress","rename":true}]}`)}

	capt := newCaptureFS(initial)
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: usersWithEmailAddress(),
		Dir:    "drizzle",
		Name:   "again",
		FS:     capt.read,
		Write:  capt.write,
		Now:    func() int64 { return 1700000002000 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !res.NoOp {
		t.Errorf("a settled rename was generated again:\n%s", res.SQL)
	}
}
