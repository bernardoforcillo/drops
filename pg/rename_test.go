package pg_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/pg"
)

// renameSetup lays out a migration directory holding one previous
// snapshot, which is all GenerateMigration reads to decide what the
// database currently looks like.
func renameSetup(t *testing.T, prev *pg.Snapshot) *captureFS {
	t.Helper()
	body, err := prev.Marshal()
	if err != nil {
		t.Fatalf("marshal previous snapshot: %v", err)
	}
	return newCaptureFS(fstest.MapFS{
		"drizzle/0000_init.sql":           {Data: []byte("/* placeholder */")},
		"drizzle/meta/0000_snapshot.json": {Data: body},
		"drizzle/meta/_journal.json": {Data: []byte(`{
			"version": "7",
			"dialect": "postgresql",
			"entries": [
				{"idx": 0, "version": "7", "when": 1700000000000, "tag": "0000_init", "breakpoints": true}
			]
		}`)},
	})
}

func generateWithRenames(t *testing.T, capt *captureFS, schema *pg.Schema, opts pg.GenerateOptions) (*pg.GenerateResult, error) {
	t.Helper()
	opts.Schema = schema
	opts.Dir = "drizzle"
	if opts.Name == "" {
		opts.Name = "rename"
	}
	opts.FS = capt.read
	opts.Write = capt.write
	opts.Now = func() int64 { return 1700000100000 }
	return pg.GenerateMigration(opts)
}

// usersWithEmail is the schema before the rename: one table, one key,
// one column called email.
func usersWithEmail() *pg.Schema {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	pg.Add(users, pg.Text("email").NotNull())
	return pg.NewSchema(users)
}

func TestGenerateRenamesRatherThanDroppingAndAdding(t *testing.T) {
	cases := []struct {
		name    string
		before  func() *pg.Schema
		after   func() *pg.Schema
		renames []pg.Rename
		want    string
	}{
		{
			name:   "column",
			before: usersWithEmail,
			after: func() *pg.Schema {
				users := pg.NewTable("users")
				pg.Add(users, pg.BigSerial("id").PrimaryKey())
				pg.Add(users, pg.Text("email_address").NotNull())
				return pg.NewSchema(users)
			},
			renames: []pg.Rename{{Kind: pg.RenameColumn, Table: "users", From: "email", To: "email_address"}},
			want:    `ALTER TABLE "users" RENAME COLUMN "email" TO "email_address";`,
		},
		{
			name:   "table",
			before: usersWithEmail,
			after: func() *pg.Schema {
				people := pg.NewTable("people")
				pg.Add(people, pg.BigSerial("id").PrimaryKey())
				pg.Add(people, pg.Text("email").NotNull())
				return pg.NewSchema(people)
			},
			renames: []pg.Rename{{Kind: pg.RenameTable, From: "users", To: "people"}},
			want:    `ALTER TABLE "users" RENAME TO "people";`,
		},
		{
			name: "schema",
			before: func() *pg.Schema {
				users := pg.NewSchemaTable("app", "users")
				pg.Add(users, pg.BigSerial("id").PrimaryKey())
				return pg.NewSchema(users)
			},
			after: func() *pg.Schema {
				users := pg.NewSchemaTable("tenant", "users")
				pg.Add(users, pg.BigSerial("id").PrimaryKey())
				return pg.NewSchema(users)
			},
			renames: []pg.Rename{{Kind: pg.RenameSchema, From: "app", To: "tenant"}},
			want:    `ALTER SCHEMA "app" RENAME TO "tenant";`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capt := renameSetup(t, pg.BuildSnapshot(tc.before()))
			res, err := generateWithRenames(t, capt, tc.after(), pg.GenerateOptions{Renames: tc.renames})
			if err != nil {
				t.Fatalf("GenerateMigration: %v", err)
			}
			if !strings.Contains(res.SQL, tc.want) {
				t.Fatalf("SQL =\n%s\nwant it to contain %v", res.SQL, tc.want)
			}
			for _, unwanted := range []string{"DROP COLUMN", "ADD COLUMN", "DROP TABLE", "CREATE TABLE"} {
				if strings.Contains(res.SQL, unwanted) {
					t.Errorf("SQL =\n%s\nwant no %v — the object was renamed, not destroyed", res.SQL, unwanted)
				}
			}
		})
	}
}

func TestGenerateRecordsRenamesInSnapshotMeta(t *testing.T) {
	capt := renameSetup(t, pg.BuildSnapshot(usersWithEmail()))
	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("email_address").NotNull())

	res, err := generateWithRenames(t, capt, pg.NewSchema(after), pg.GenerateOptions{
		Renames: []pg.Rename{{Kind: pg.RenameColumn, Table: "users", From: "email", To: "email_address"}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	var snap pg.Snapshot
	if err := json.Unmarshal(res.Snapshot, &snap); err != nil {
		t.Fatalf("parse written snapshot: %v", err)
	}
	got, ok := snap.Meta.Columns["public.users.email"]
	if !ok {
		t.Fatalf("_meta.columns = %v, want the rename recorded", snap.Meta.Columns)
	}
	if want := "public.users.email_address"; got != want {
		t.Errorf("_meta.columns[public.users.email] = %v, want %v", got, want)
	}
}

func TestGenerateRenameKeepsTheIndexOverTheRenamedColumn(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	beforeEmail := pg.Add(before, pg.Text("email").NotNull())
	before.AddIndex(pg.NewIndex("usersEmailIdx", before, beforeEmail))

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	afterEmail := pg.Add(after, pg.Text("email_address").NotNull())
	after.AddIndex(pg.NewIndex("usersEmailIdx", after, afterEmail))

	capt := renameSetup(t, pg.BuildSnapshot(pg.NewSchema(before)))
	res, err := generateWithRenames(t, capt, pg.NewSchema(after), pg.GenerateOptions{
		Renames: []pg.Rename{{Kind: pg.RenameColumn, Table: "users", From: "email", To: "email_address"}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if strings.Contains(res.SQL, "DROP INDEX") || strings.Contains(res.SQL, "CREATE INDEX") {
		t.Errorf("SQL =\n%s\nwant no index churn — PostgreSQL carries the index across a column rename", res.SQL)
	}
}

func TestGenerateRenameDownUndoesInReverse(t *testing.T) {
	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())

	after := pg.NewTable("people")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("email_address").NotNull())

	capt := renameSetup(t, pg.BuildSnapshot(pg.NewSchema(before)))
	res, err := generateWithRenames(t, capt, pg.NewSchema(after), pg.GenerateOptions{
		WithDown: true,
		Renames: []pg.Rename{
			{Kind: pg.RenameTable, From: "users", To: "people"},
			// Named against the table as the first rename left it.
			{Kind: pg.RenameColumn, Table: "people", From: "email", To: "email_address"},
		},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	up := []string{
		`ALTER TABLE "users" RENAME TO "people";`,
		`ALTER TABLE "people" RENAME COLUMN "email" TO "email_address";`,
	}
	for _, want := range up {
		if !strings.Contains(res.SQL, want) {
			t.Fatalf("up SQL =\n%s\nwant it to contain %v", res.SQL, want)
		}
	}
	if strings.Index(res.SQL, up[0]) > strings.Index(res.SQL, up[1]) {
		t.Errorf("up SQL =\n%s\nwant the table renamed before the column that lives on it", res.SQL)
	}
	down := []string{
		`ALTER TABLE "people" RENAME COLUMN "email_address" TO "email";`,
		`ALTER TABLE "people" RENAME TO "users";`,
	}
	for _, want := range down {
		if !strings.Contains(res.DownSQL, want) {
			t.Fatalf("down SQL =\n%s\nwant it to contain %v", res.DownSQL, want)
		}
	}
	if strings.Index(res.DownSQL, down[0]) > strings.Index(res.DownSQL, down[1]) {
		t.Errorf("down SQL =\n%s\nwant the column renamed back before the table it lives on", res.DownSQL)
	}
}

func TestGenerateRefusesARenameThatMatchesNothing(t *testing.T) {
	after := func() *pg.Schema {
		users := pg.NewTable("users")
		pg.Add(users, pg.BigSerial("id").PrimaryKey())
		pg.Add(users, pg.Text("email_address").NotNull())
		return pg.NewSchema(users)
	}
	cases := []struct {
		name   string
		rename pg.Rename
		want   error
	}{
		{
			name:   "already generated last time",
			rename: pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "nickname", To: "handle"},
			want:   pg.ErrRenameNotApplicable,
		},
		{
			name:   "a table that never existed",
			rename: pg.Rename{Kind: pg.RenameColumn, Table: "accounts", From: "email", To: "email_address"},
			want:   pg.ErrRenameNotApplicable,
		},
		{
			name:   "the new name is not declared",
			rename: pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email", To: "mail"},
			want:   pg.ErrRenameNotApplicable,
		},
		{
			name:   "a table rename with no table on the other side",
			rename: pg.Rename{Kind: pg.RenameTable, From: "users", To: "people"},
			want:   pg.ErrRenameNotApplicable,
		},
		{
			name:   "a kind that is not a kind",
			rename: pg.Rename{Kind: "colunm", Table: "users", From: "email", To: "email_address"},
			want:   pg.ErrRenameInvalid,
		},
		{
			name:   "a column rename with no table",
			rename: pg.Rename{Kind: pg.RenameColumn, From: "email", To: "email_address"},
			want:   pg.ErrRenameInvalid,
		},
		{
			name:   "a rename to itself",
			rename: pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email", To: "email"},
			want:   pg.ErrRenameInvalid,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capt := renameSetup(t, pg.BuildSnapshot(usersWithEmail()))
			_, err := generateWithRenames(t, capt, after(), pg.GenerateOptions{
				Renames: []pg.Rename{tc.rename},
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if len(capt.written) != 0 {
				t.Errorf("wrote %v, want nothing written when a rename does not apply", capt.written)
			}
		})
	}
}

func TestGenerateWithoutRenamesStillDropsAndAdds(t *testing.T) {
	// The other half of the contract: drops does not guess. Without a
	// declaration the pair is what it looks like, and the migration
	// says so in the SQL a reviewer reads.
	capt := renameSetup(t, pg.BuildSnapshot(usersWithEmail()))
	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("email_address").NotNull())

	res, err := generateWithRenames(t, capt, pg.NewSchema(after), pg.GenerateOptions{})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	for _, want := range []string{`DROP COLUMN "email"`, `ADD COLUMN "email_address"`} {
		if !strings.Contains(res.SQL, want) {
			t.Errorf("SQL =\n%s\nwant it to contain %v", res.SQL, want)
		}
	}
	if !strings.Contains(res.SQL, "RENAME") {
		return
	}
	t.Errorf("SQL =\n%s\nwant no RENAME — nobody declared one", res.SQL)
}
