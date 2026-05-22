package pg_test

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/pg"
)

// helper: pretty-print snapshot to see in test output.
func dumpJSON(t *testing.T, label string, v any) {
	t.Helper()
	body, _ := json.MarshalIndent(v, "", "  ")
	t.Logf("%s:\n%s", label, body)
}

func TestBuildSnapshotShape(t *testing.T) {
	users := pg.NewTable("users")
	uid := pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	pg.Add(users, pg.Text("email").NotNull().Unique())
	pg.Add(users, pg.Integer("age").Default("0"))

	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigSerial("id").PrimaryKey())
	pg.Add(posts, pg.BigInt("user_id").NotNull().References(uid, pg.OnDelete("CASCADE")))
	pg.Add(posts, pg.Text("title").NotNull())

	snap := pg.BuildSnapshot(pg.NewSchema(users, posts))

	if snap.Version != "7" || snap.Dialect != "postgresql" {
		t.Fatalf("version/dialect: got %s/%s", snap.Version, snap.Dialect)
	}
	if len(snap.Tables) != 2 {
		t.Fatalf("got %d tables, want 2", len(snap.Tables))
	}

	usersT, ok := snap.Tables["public.users"]
	if !ok {
		dumpJSON(t, "tables", snap.Tables)
		t.Fatal(`missing "public.users" entry`)
	}
	if usersT.Name != "users" {
		t.Errorf("users name: %s", usersT.Name)
	}
	if c, ok := usersT.Columns["id"]; !ok || c.Type != "bigserial" || !c.PrimaryKey || !c.NotNull {
		t.Errorf(`bad "id" column: %+v`, c)
	}
	if c, ok := usersT.Columns["age"]; !ok || c.Default == nil || *c.Default != "0" {
		t.Errorf(`bad "age" column default: %+v`, c)
	}
	uc, ok := usersT.UniqueConstraints["users_email_unique"]
	if !ok {
		dumpJSON(t, "unique constraints", usersT.UniqueConstraints)
		t.Fatal(`missing "users_email_unique"`)
	}
	if len(uc.Columns) != 1 || uc.Columns[0] != "email" {
		t.Errorf("unique columns: %v", uc.Columns)
	}

	postsT := snap.Tables["public.posts"]
	fk, ok := postsT.ForeignKeys["posts_user_id_users_id_fk"]
	if !ok {
		dumpJSON(t, "foreign keys", postsT.ForeignKeys)
		t.Fatal(`missing "posts_user_id_users_id_fk"`)
	}
	if fk.TableTo != "users" || fk.ColumnsTo[0] != "id" {
		t.Errorf("fk target: %+v", fk)
	}
	if fk.OnDelete != "cascade" {
		t.Errorf("fk onDelete: %q, want cascade", fk.OnDelete)
	}
}

func TestSnapshotJSONRoundTrip(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	original := pg.BuildSnapshot(pg.NewSchema(users))
	bytes, err := original.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	round, err := pg.UnmarshalSnapshot(bytes)
	if err != nil {
		t.Fatal(err)
	}
	if round.ID != original.ID {
		t.Errorf("id round trip: %s vs %s", round.ID, original.ID)
	}
	if len(round.Tables) != len(original.Tables) {
		t.Errorf("tables count: %d vs %d", len(round.Tables), len(original.Tables))
	}
	if round.Tables["public.users"].Columns["id"].Type != "bigserial" {
		t.Error("column type lost in round trip")
	}
}

func TestDiffFromEmptyEmitsCreateTable(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	cur := pg.BuildSnapshot(pg.NewSchema(users))
	stmts := pg.Diff(pg.EmptySnapshot(), cur)
	if len(stmts) != 1 {
		t.Fatalf("got %d statements, want 1: %v", len(stmts), stmts)
	}
	want := "CREATE TABLE \"users\" (\n\t\"id\" bigserial PRIMARY KEY NOT NULL,\n\t\"name\" text NOT NULL\n);"
	if stmts[0] != want {
		t.Errorf("CREATE TABLE mismatch:\n--- got ---\n%s\n--- want ---\n%s", stmts[0], want)
	}
}

func TestDiffAddColumn(t *testing.T) {
	beforeSchema := pg.NewTable("users")
	pg.Add(beforeSchema, pg.BigSerial("id").PrimaryKey())
	pg.Add(beforeSchema, pg.Text("name").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(beforeSchema))

	afterSchema := pg.NewTable("users")
	pg.Add(afterSchema, pg.BigSerial("id").PrimaryKey())
	pg.Add(afterSchema, pg.Text("name").NotNull())
	pg.Add(afterSchema, pg.Integer("age"))
	cur := pg.BuildSnapshot(pg.NewSchema(afterSchema))

	stmts := pg.Diff(prev, cur)
	if len(stmts) != 1 {
		t.Fatalf("got %d statements, want 1: %v", len(stmts), stmts)
	}
	want := `ALTER TABLE "users" ADD COLUMN "age" integer;`
	if stmts[0] != want {
		t.Errorf("add column:\n  got: %s\n want: %s", stmts[0], want)
	}
}

func TestDiffDropColumn(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("name").NotNull())
	pg.Add(before, pg.Text("nickname"))
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("name").NotNull())
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur)
	want := `ALTER TABLE "users" DROP COLUMN "nickname";`
	if len(stmts) != 1 || stmts[0] != want {
		t.Errorf("drop column: %v", stmts)
	}
}

func TestDiffAlterColumnNotNullAndDefault(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Integer("age"))
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Integer("age").NotNull().Default("0"))
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur)
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		`ALTER TABLE "users" ALTER COLUMN "age" SET NOT NULL;`,
		`ALTER TABLE "users" ALTER COLUMN "age" SET DEFAULT 0;`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing: %s\nactual:\n%s", want, joined)
		}
	}
}

func TestDiffAlterColumnType(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.Integer("count"))
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigInt("count"))
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur)
	want := `ALTER TABLE "users" ALTER COLUMN "count" SET DATA TYPE bigint;`
	if len(stmts) != 1 || stmts[0] != want {
		t.Errorf("alter type: %v", stmts)
	}
}

func TestDiffAddForeignKey(t *testing.T) {
	usersBefore := pg.NewTable("users")
	pg.Add(usersBefore, pg.BigSerial("id").PrimaryKey())
	postsBefore := pg.NewTable("posts")
	pg.Add(postsBefore, pg.BigSerial("id").PrimaryKey())
	pg.Add(postsBefore, pg.BigInt("user_id").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(usersBefore, postsBefore))

	usersAfter := pg.NewTable("users")
	uid := pg.Add(usersAfter, pg.BigSerial("id").PrimaryKey())
	postsAfter := pg.NewTable("posts")
	pg.Add(postsAfter, pg.BigSerial("id").PrimaryKey())
	pg.Add(postsAfter, pg.BigInt("user_id").NotNull().References(uid, pg.OnDelete("CASCADE")))
	cur := pg.BuildSnapshot(pg.NewSchema(usersAfter, postsAfter))

	stmts := pg.Diff(prev, cur)
	want := `ALTER TABLE "posts" ADD CONSTRAINT "posts_user_id_users_id_fk" FOREIGN KEY ("user_id") REFERENCES "users"("id") ON DELETE cascade ON UPDATE no action;`
	for _, s := range stmts {
		if s == want {
			return
		}
	}
	t.Errorf("missing FK add statement\n  want: %s\n  got:\n%s", want, strings.Join(stmts, "\n"))
}

func TestDiffAddAndDropUnique(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("nickname").NotNull().Unique())
	prev := pg.BuildSnapshot(pg.NewSchema(before))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("nickname").NotNull()) // unique removed
	pg.Add(after, pg.Text("email").NotNull().Unique())
	cur := pg.BuildSnapshot(pg.NewSchema(after))

	stmts := pg.Diff(prev, cur)
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		`ALTER TABLE "users" DROP CONSTRAINT "users_nickname_unique";`,
		`ALTER TABLE "users" ADD CONSTRAINT "users_email_unique" UNIQUE("email");`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing: %s\nactual:\n%s", want, joined)
		}
	}
}

func TestDiffNoChanges(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	snap1 := pg.BuildSnapshot(pg.NewSchema(users))

	// Round-trip through JSON so IDs differ but content matches.
	body, _ := snap1.Marshal()
	snap2, _ := pg.UnmarshalSnapshot(body)
	if stmts := pg.Diff(snap1, snap2); len(stmts) != 0 {
		t.Errorf("identical snapshots: got %d statements: %v", len(stmts), stmts)
	}
}

// --- End-to-end generator tests ---------------------------------------

// captureFS wraps fstest.MapFS so we can verify what GenerateMigration
// would write without touching the disk.
type captureFS struct {
	read    fstest.MapFS
	written map[string][]byte
}

func newCaptureFS(initial fstest.MapFS) *captureFS {
	return &captureFS{read: initial, written: map[string][]byte{}}
}

func (c *captureFS) write(rel string, data []byte) error {
	c.written[rel] = append([]byte(nil), data...)
	return nil
}

func TestGenerateFirstMigration(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())

	cap := newCaptureFS(fstest.MapFS{})
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(users),
		Dir:    "drizzle",
		Name:   "init",
		FS:     cap.read,
		Write:  cap.write,
		Now:    func() int64 { return 1700000000000 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if res.NoOp {
		t.Fatal("first migration: NoOp = true")
	}
	if res.Tag != "0000_init" {
		t.Errorf("tag: %s", res.Tag)
	}
	if _, ok := cap.written["0000_init.sql"]; !ok {
		t.Errorf("missing 0000_init.sql; wrote: %v", keysOf(cap.written))
	}
	if _, ok := cap.written["meta/0000_snapshot.json"]; !ok {
		t.Errorf("missing meta/0000_snapshot.json; wrote: %v", keysOf(cap.written))
	}
	if _, ok := cap.written["meta/_journal.json"]; !ok {
		t.Errorf("missing meta/_journal.json; wrote: %v", keysOf(cap.written))
	}

	// Journal should have one entry pointing at our tag.
	var j struct {
		Version string `json:"version"`
		Dialect string `json:"dialect"`
		Entries []struct {
			Idx  int    `json:"idx"`
			Tag  string `json:"tag"`
			When int64  `json:"when"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(cap.written["meta/_journal.json"], &j); err != nil {
		t.Fatal(err)
	}
	if j.Dialect != "postgresql" || j.Version != "7" {
		t.Errorf("journal version/dialect: %s/%s", j.Version, j.Dialect)
	}
	if len(j.Entries) != 1 || j.Entries[0].Tag != "0000_init" ||
		j.Entries[0].When != 1700000000000 {
		t.Errorf("journal entry: %+v", j.Entries)
	}
}

func TestGenerateSecondMigrationDiff(t *testing.T) {
	// Step 1: build the "before" snapshot and stage it as the previous
	// migration on disk.
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("name").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(before))
	prevBytes, _ := prev.Marshal()

	initial := fstest.MapFS{
		"drizzle/0000_init.sql":           {Data: []byte("/* placeholder */")},
		"drizzle/meta/0000_snapshot.json": {Data: prevBytes},
		"drizzle/meta/_journal.json": {Data: []byte(`{
			"version": "7",
			"dialect": "postgresql",
			"entries": [
				{"idx": 0, "version": "7", "when": 1700000000000, "tag": "0000_init", "breakpoints": true}
			]
		}`)},
	}

	// Step 2: change the Go schema (add an age column) and generate.
	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("name").NotNull())
	pg.Add(after, pg.Integer("age"))

	cap := newCaptureFS(initial)
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after),
		Dir:    "drizzle",
		Name:   "add_age",
		FS:     cap.read,
		Write:  cap.write,
		Now:    func() int64 { return 1700000100000 },
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if res.Tag != "0001_add_age" {
		t.Errorf("tag: %s", res.Tag)
	}
	sql := string(cap.written["0001_add_age.sql"])
	if !strings.Contains(sql, `ADD COLUMN "age" integer`) {
		t.Errorf("expected ADD COLUMN in SQL, got:\n%s", sql)
	}

	// Journal should now have 2 entries with the new one appended.
	var j struct {
		Entries []struct {
			Idx int    `json:"idx"`
			Tag string `json:"tag"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(cap.written["meta/_journal.json"], &j); err != nil {
		t.Fatal(err)
	}
	if len(j.Entries) != 2 ||
		j.Entries[0].Tag != "0000_init" ||
		j.Entries[1].Tag != "0001_add_age" {
		t.Errorf("journal entries: %+v", j.Entries)
	}
}

func TestGenerateNoOpWhenSchemaUnchanged(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("name").NotNull())
	prev := pg.BuildSnapshot(pg.NewSchema(users))
	prevBytes, _ := prev.Marshal()

	initial := fstest.MapFS{
		"drizzle/0000_init.sql":           {Data: []byte("/* placeholder */")},
		"drizzle/meta/0000_snapshot.json": {Data: prevBytes},
		"drizzle/meta/_journal.json": {Data: []byte(`{
			"version": "7",
			"dialect": "postgresql",
			"entries": [{"idx": 0, "version": "7", "when": 1, "tag": "0000_init", "breakpoints": true}]
		}`)},
	}

	// Same schema as before.
	cap := newCaptureFS(initial)
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(users),
		Dir:    "drizzle",
		Name:   "noop",
		FS:     cap.read,
		Write:  cap.write,
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !res.NoOp {
		t.Errorf("expected NoOp = true, got %+v", res)
	}
	if len(cap.written) != 0 {
		t.Errorf("NoOp should not write files; got: %v", keysOf(cap.written))
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
