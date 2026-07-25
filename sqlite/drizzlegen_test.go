package sqlite_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/sqlite"
)

func TestDrizzleMigratorUp(t *testing.T) {
	journal := `{"version":"7","dialect":"sqlite","entries":[` +
		`{"idx":0,"version":"7","when":1,"tag":"0000_init","breakpoints":true}]}`
	fsys := fstest.MapFS{
		"drizzle/meta/_journal.json": {Data: []byte(journal)},
		"drizzle/0000_init.sql":      {Data: []byte(`CREATE TABLE "x" ("id" INTEGER PRIMARY KEY)`)},
	}
	drv := &entDriver{}
	db := sqlite.New(drv)
	m := sqlite.NewDrizzleMigrator(db, fsys, "drizzle")
	if err := m.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(drv.queries, "\n")
	for _, want := range []string{
		`CREATE TABLE IF NOT EXISTS "__drizzle_migrations"`,
		`CREATE TABLE "x"`,
		`INSERT INTO "__drizzle_migrations" (hash, created_at) VALUES (?, ?)`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestDrizzleStatus(t *testing.T) {
	journal := `{"version":"7","dialect":"sqlite","entries":[` +
		`{"idx":0,"version":"7","when":5,"tag":"0000_init","breakpoints":true}]}`
	fsys := fstest.MapFS{
		"drizzle/meta/_journal.json": {Data: []byte(journal)},
		"drizzle/0000_init.sql":      {Data: []byte(`CREATE TABLE "x" ("id" INTEGER)`)},
	}
	db := sqlite.New(&entDriver{})
	st, err := sqlite.NewDrizzleMigrator(db, fsys, "drizzle").Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].Tag != "0000_init" || st[0].Applied {
		t.Errorf("status: %+v", st)
	}
}

func TestGenerateMigration(t *testing.T) {
	tbl := sqlite.NewTable("users")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("name").NotNull())

	writes := map[string][]byte{}
	res, err := sqlite.GenerateMigration(sqlite.GenerateOptions{
		Schema: sqlite.NewSchema(tbl),
		Dir:    "drizzle",
		FS:     fstest.MapFS{}, // no prior migrations → first
		Now:    func() int64 { return 1000 },
		NameFn: func() string { return "init" },
		Write: func(rel string, data []byte) error {
			writes[rel] = data
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.NoOp || res.Tag != "0000_init" {
		t.Fatalf("result: %+v", res)
	}
	if !strings.Contains(res.SQL, `CREATE TABLE "users"`) {
		t.Errorf("migration SQL:\n%s", res.SQL)
	}
	// The migration file, snapshot and journal were written.
	for _, f := range []string{"0000_init.sql", "meta/0000_snapshot.json", "meta/_journal.json"} {
		if _, ok := writes[f]; !ok {
			t.Errorf("missing written file %q (wrote: %v)", f, writtenKeys(writes))
		}
	}
	// The journal records the sqlite dialect.
	var j struct {
		Dialect string `json:"dialect"`
	}
	_ = json.Unmarshal(writes["meta/_journal.json"], &j)
	if j.Dialect != "sqlite" {
		t.Errorf("journal dialect: %q", j.Dialect)
	}
}

func TestGenerateMigrationNoOp(t *testing.T) {
	// An empty schema against an empty prior yields no diff → NoOp.
	res, err := sqlite.GenerateMigration(sqlite.GenerateOptions{
		Schema: sqlite.NewSchema(),
		Dir:    "drizzle",
		FS:     fstest.MapFS{},
		Now:    func() int64 { return 1 },
		NameFn: func() string { return "x" },
		Write:  func(string, []byte) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.NoOp {
		t.Errorf("expected NoOp, got %+v", res)
	}
}

func writtenKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
