package pg_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// drizzleFakeDB returns a fake driver wired to capture inserts into the
// drizzle migration table and return the captured hashes on subsequent
// SELECT hash queries — the minimum surface needed to drive
// DrizzleMigrator from end to end.
func drizzleFakeDB() (*pg.DB, *fakeDriver, *map[string]bool) {
	applied := map[string]bool{}
	fd := &fakeDriver{
		exec: func(sql string, args []any) (drops.Result, error) {
			if strings.Contains(sql, "INSERT INTO") &&
				strings.Contains(sql, `"drizzle"`) &&
				strings.Contains(sql, `"__drizzle_migrations"`) {
				applied[args[0].(string)] = true
			}
			return fakeResult{affected: 1}, nil
		},
		handler: func(sql string, _ []any) (drops.Rows, error) {
			data := [][]any{}
			if strings.Contains(sql, `"__drizzle_migrations"`) {
				for h := range applied {
					data = append(data, []any{h})
				}
				return &fakeRows{cols: []string{"hash"}, data: data}, nil
			}
			return &fakeRows{cols: []string{"hash"}}, nil
		},
	}
	return pg.New(fd), fd, &applied
}

const journalJSON = `{
  "version": "7",
  "dialect": "postgresql",
  "entries": [
    {"idx": 0, "version": "7", "when": 1700000000000, "tag": "0000_init",  "breakpoints": true},
    {"idx": 1, "version": "7", "when": 1700000100000, "tag": "0001_more",  "breakpoints": true},
    {"idx": 2, "version": "7", "when": 1700000200000, "tag": "0002_solo",  "breakpoints": false}
  ]
}`

// initSQL exercises statement-breakpoint splitting.
const initSQL = `CREATE TABLE "users" (
  "id" serial PRIMARY KEY NOT NULL,
  "name" text NOT NULL
);
--> statement-breakpoint
CREATE INDEX "users_name_idx" ON "users" ("name");`

const moreSQL = `ALTER TABLE "users" ADD COLUMN "age" integer;`

// soloSQL has no breakpoints in the journal entry, so it stays one block.
const soloSQL = `CREATE TABLE "logs" ("id" serial PRIMARY KEY);
ALTER TABLE "logs" ADD COLUMN "msg" text;`

func drizzleFS() fstest.MapFS {
	return fstest.MapFS{
		"drizzle/meta/_journal.json": {Data: []byte(journalJSON)},
		"drizzle/0000_init.sql":      {Data: []byte(initSQL)},
		"drizzle/0001_more.sql":      {Data: []byte(moreSQL)},
		"drizzle/0002_solo.sql":      {Data: []byte(soloSQL)},
	}
}

func TestDrizzleHashMatchesSHA256(t *testing.T) {
	db, _, _ := drizzleFakeDB()
	m := pg.NewDrizzleMigrator(db, drizzleFS(), "drizzle")
	entries, err := m.LoadEntries()
	if err != nil {
		t.Fatalf("LoadEntries: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	want := func(s string) string {
		h := sha256.Sum256([]byte(s))
		return hex.EncodeToString(h[:])
	}
	for _, tc := range []struct {
		tag, sql string
	}{
		{"0000_init", initSQL},
		{"0001_more", moreSQL},
		{"0002_solo", soloSQL},
	} {
		var got pg.DrizzleEntry
		for _, e := range entries {
			if e.Tag == tc.tag {
				got = e
				break
			}
		}
		if got.Hash != want(tc.sql) {
			t.Errorf("%s: hash %s, want %s", tc.tag, got.Hash, want(tc.sql))
		}
	}
}

func TestDrizzleUpAppliesEverythingThenIsIdempotent(t *testing.T) {
	db, fd, applied := drizzleFakeDB()
	m := pg.NewDrizzleMigrator(db, drizzleFS(), "drizzle")
	ctx := context.Background()

	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up #1: %v", err)
	}
	if got := len(*applied); got != 3 {
		t.Fatalf("after first Up, applied = %d, want 3", got)
	}

	// Second Up: every entry's hash is in the applied set, so no
	// migration SQL or INSERT should be emitted — only the schema/table
	// ensure plus the SELECT-hash lookup.
	queryCountBefore := len(fd.queries)
	if err := m.Up(ctx); err != nil {
		t.Fatalf("Up #2: %v", err)
	}
	for _, q := range fd.queries[queryCountBefore:] {
		if strings.Contains(q, "CREATE TABLE \"users\"") || strings.Contains(q, "ALTER TABLE") {
			t.Errorf("idempotent Up replayed migration: %q", q)
		}
		if strings.Contains(q, "INSERT INTO") && strings.Contains(q, "__drizzle_migrations") {
			t.Errorf("idempotent Up wrote a new history row: %q", q)
		}
	}
}

func TestDrizzleStatementBreakpoints(t *testing.T) {
	db, fd, _ := drizzleFakeDB()
	m := pg.NewDrizzleMigrator(db, drizzleFS(), "drizzle")
	if err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}
	// 0000_init has a --> statement-breakpoint, so we expect both halves
	// to have run as separate Exec calls.
	var ranCreateTableUsers, ranCreateIndexUsers bool
	for _, q := range fd.queries {
		if strings.HasPrefix(strings.TrimSpace(q), `CREATE TABLE "users"`) {
			ranCreateTableUsers = true
		}
		if strings.HasPrefix(strings.TrimSpace(q), `CREATE INDEX "users_name_idx"`) {
			ranCreateIndexUsers = true
		}
	}
	if !ranCreateTableUsers || !ranCreateIndexUsers {
		t.Errorf("expected breakpoints to split into two Exec calls; got %v",
			fd.queries)
	}

	// 0002_solo has breakpoints=false in the journal — the file should
	// be sent as a single Exec containing both statements verbatim.
	foundSolo := false
	for _, q := range fd.queries {
		if strings.Contains(q, `CREATE TABLE "logs"`) &&
			strings.Contains(q, `ADD COLUMN "msg"`) {
			foundSolo = true
			break
		}
	}
	if !foundSolo {
		t.Errorf("expected non-breakpoints migration to run as one Exec; queries: %v",
			fd.queries)
	}
}

func TestDrizzleStatusReportsAppliedFlag(t *testing.T) {
	db, _, _ := drizzleFakeDB()
	m := pg.NewDrizzleMigrator(db, drizzleFS(), "drizzle")
	ctx := context.Background()

	before, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range before {
		if s.Applied {
			t.Errorf("before Up, %s should not be applied", s.Tag)
		}
	}

	if err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := m.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range after {
		if !s.Applied {
			t.Errorf("after Up, %s should be applied", s.Tag)
		}
	}
}

func TestDrizzleWithSchemaAndTable(t *testing.T) {
	applied := map[string]bool{}
	fd := &fakeDriver{
		exec: func(sql string, args []any) (drops.Result, error) {
			if strings.Contains(sql, "INSERT INTO") &&
				strings.Contains(sql, `"my_schema"`) &&
				strings.Contains(sql, `"my_history"`) {
				applied[args[0].(string)] = true
			}
			return fakeResult{1}, nil
		},
		handler: func(sql string, _ []any) (drops.Rows, error) {
			data := [][]any{}
			if strings.Contains(sql, `"my_history"`) {
				for h := range applied {
					data = append(data, []any{h})
				}
				return &fakeRows{cols: []string{"hash"}, data: data}, nil
			}
			return &fakeRows{cols: []string{"hash"}}, nil
		},
	}
	db := pg.New(fd)
	m := pg.NewDrizzleMigrator(db, drizzleFS(), "drizzle").
		WithSchema("my_schema").
		WithTable("my_history")

	if err := m.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(applied) != 3 {
		t.Errorf("custom schema/table: applied = %d, want 3", len(applied))
	}
	// Sanity: assert the CREATE SCHEMA used the override.
	sawCustomSchema := false
	for _, q := range fd.queries {
		if strings.Contains(q, `CREATE SCHEMA IF NOT EXISTS "my_schema"`) {
			sawCustomSchema = true
		}
	}
	if !sawCustomSchema {
		t.Errorf("did not see CREATE SCHEMA for custom name; queries: %v", fd.queries)
	}
}

func TestDrizzleRejectsNonPostgresqlDialect(t *testing.T) {
	fsys := fstest.MapFS{
		"drizzle/meta/_journal.json": {Data: []byte(`{
			"version": "7",
			"dialect": "mysql",
			"entries": []
		}`)},
	}
	db, _, _ := drizzleFakeDB()
	m := pg.NewDrizzleMigrator(db, fsys, "drizzle")
	if err := m.Up(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "mysql") {
		t.Errorf("expected dialect error, got %v", err)
	}
}

func TestDrizzleMissingJournal(t *testing.T) {
	db, _, _ := drizzleFakeDB()
	m := pg.NewDrizzleMigrator(db, fstest.MapFS{}, "drizzle")
	if err := m.Up(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "journal") {
		t.Errorf("expected journal error, got %v", err)
	}
}
