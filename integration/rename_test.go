package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
)

// Renaming a column, against real servers.
//
// A generated migration that compares SQL text to SQL text proves that
// drops wrote what drops meant to write. It proves nothing about the
// thing at stake here, which is whether the row that was in the column
// before the migration is in it afterwards. So each test below puts a
// row in, runs the migration the generator produced, and reads the row
// back out under the new name.
//
// The other half of each test is the refusal: the same schema change
// with no answer generates nothing at all, which is what stands between
// a rename and a DROP COLUMN in a pipeline nobody is watching.

// migrationDirFor builds the in-memory migration directory a generator
// run reads: one previous snapshot and a journal naming it.
func migrationDirFor(t *testing.T, dialect string, snapshot []byte) fstest.MapFS {
	t.Helper()
	version := "7"
	if dialect == "mysql" {
		version = "5"
	}
	return fstest.MapFS{
		"m/meta/0000_snapshot.json": {Data: snapshot},
		"m/meta/_journal.json": {Data: []byte(
			`{"version":"` + version + `","dialect":"` + dialect + `","entries":[` +
				`{"idx":0,"version":"` + version + `","when":1,"tag":"0000_init","breakpoints":true}]}`)},
	}
}

// migrationStatements splits a generated migration back into the
// statements a server can be handed one at a time.
func migrationStatements(sql string) []string {
	var out []string
	for _, s := range strings.Split(sql, pg.StatementBreakpoint) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func TestPGGeneratedRenameKeepsTheData(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "renamed")

	before := pg.NewTable(name)
	pg.Add(before, pg.BigInt("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())
	dropPG(t, db, before)
	execPG(t, db, pg.CreateTable(before))

	quoted := drops.StdQuoteIdent(name)
	if _, err := db.Exec(ctx,
		`INSERT INTO `+quoted+` (id, email) VALUES (1, $1)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	after := pg.NewTable(name)
	pg.Add(after, pg.BigInt("id").PrimaryKey())
	pg.Add(after, pg.Text("emailAddress").NotNull())

	prevSnapshot, err := pg.BuildSnapshot(pg.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys := migrationDirFor(t, "postgresql", prevSnapshot)

	// Without an answer nothing is generated at all. This is the run a
	// pipeline makes, and stopping is the whole point of it.
	_, err = pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { t.Error("a refused run wrote a file"); return nil },
	})
	var amb *pg.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("an unanswered rename generated a migration: %v", err)
	}

	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { return nil },
		Renames: []pg.RenameDecision{{
			Rename:   pg.Rename{Kind: pg.RenameColumn, Table: name, From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("PostgreSQL rejected %q: %v", stmt, err)
		}
	}

	var got string
	rows, err := db.Query(ctx, `SELECT "emailAddress" FROM `+quoted+` WHERE id = 1`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone: the migration did not rename the column, it replaced it")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Errorf(`emailAddress = %q after the rename, want "ada@example.com"`, got)
	}
}

func TestPGGeneratedTableRenameKeepsTheData(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	oldName := integration.UniqueName(t, "old")
	newName := integration.UniqueName(t, "new")

	before := pg.NewTable(oldName)
	pg.Add(before, pg.BigInt("id").PrimaryKey())
	pg.Add(before, pg.Text("label").NotNull())
	after := pg.NewTable(newName)
	pg.Add(after, pg.BigInt("id").PrimaryKey())
	pg.Add(after, pg.Text("label").NotNull())
	dropPG(t, db, before)
	dropPG(t, db, after)
	execPG(t, db, pg.CreateTable(before))

	if _, err := db.Exec(ctx,
		`INSERT INTO `+drops.StdQuoteIdent(oldName)+` (id, label) VALUES (1, $1)`, "keep me"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevSnapshot, err := pg.BuildSnapshot(pg.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     migrationDirFor(t, "postgresql", prevSnapshot),
		Write:  func(string, []byte) error { return nil },
		Renames: []pg.RenameDecision{{
			Rename:   pg.Rename{Kind: pg.RenameTable, From: oldName, To: newName},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("PostgreSQL rejected %q: %v", stmt, err)
		}
	}

	var got string
	rows, err := db.Query(ctx, `SELECT label FROM `+drops.StdQuoteIdent(newName)+` WHERE id = 1`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone: the migration dropped the table and made a new one")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "keep me" {
		t.Errorf("label = %q after the rename, want %q", got, "keep me")
	}
}

func TestMySQLGeneratedRenameKeepsTheData(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "renamed")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigInt("id").PrimaryKey())
	mysql.Add(before, mysql.Varchar("email", 190).NotNull())
	dropMySQL(t, db, before)
	execMySQL(t, db, mysql.CreateTable(before))

	quoted := drops.BacktickQuoteIdent(name)
	if _, err := db.Exec(ctx,
		"INSERT INTO "+quoted+" (id, email) VALUES (1, ?)", "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigInt("id").PrimaryKey())
	mysql.Add(after, mysql.Varchar("emailAddress", 190).NotNull())

	prevSnapshot, err := mysql.BuildSnapshot(mysql.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys := migrationDirFor(t, "mysql", prevSnapshot)

	_, err = mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: mysql.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { t.Error("a refused run wrote a file"); return nil },
	})
	var amb *mysql.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("an unanswered rename generated a migration: %v", err)
	}

	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("server version: %v", err)
	}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: mysql.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { return nil },
		Server: server,
		Renames: []mysql.RenameDecision{{
			Rename:   mysql.Rename{Kind: mysql.RenameColumn, Table: name, From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s rejected %q: %v", server, stmt, err)
		}
	}

	var got string
	rows, err := db.Query(ctx, "SELECT `emailAddress` FROM "+quoted+" WHERE id = 1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone: the migration did not rename the column, it replaced it")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Errorf(`emailAddress = %q after the rename, want "ada@example.com"`, got)
	}
}

// The generated migration a server too old for RENAME COLUMN would get.
// MariaDB understands CHANGE COLUMN as well, so the spelling drops
// falls back to can be run against the live server here rather than
// only inspected as text.
func TestMySQLGeneratedRenameKeepsTheDataWithChangeColumn(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "renamed")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigInt("id").PrimaryKey())
	mysql.Add(before, mysql.Varchar("email", 190).NotNull())
	dropMySQL(t, db, before)
	execMySQL(t, db, mysql.CreateTable(before))

	quoted := drops.BacktickQuoteIdent(name)
	if _, err := db.Exec(ctx,
		"INSERT INTO "+quoted+" (id, email) VALUES (1, ?)", "grace@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	after := mysql.NewTable(name)
	mysql.Add(after, mysql.BigInt("id").PrimaryKey())
	mysql.Add(after, mysql.Varchar("emailAddress", 190).NotNull())

	prevSnapshot, err := mysql.BuildSnapshot(mysql.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// No Server: a server of unknown version, which gets the spelling
	// every server understands.
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: mysql.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     migrationDirFor(t, "mysql", prevSnapshot),
		Write:  func(string, []byte) error { return nil },
		Renames: []mysql.RenameDecision{{
			Rename:   mysql.Rename{Kind: mysql.RenameColumn, Table: name, From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, "CHANGE COLUMN") {
		t.Fatalf("expected the portable spelling:\n%s", res.SQL)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("the server rejected %q: %v", stmt, err)
		}
	}

	var got string
	rows, err := db.Query(ctx, "SELECT `emailAddress` FROM "+quoted+" WHERE id = 1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "grace@example.com" {
		t.Errorf("emailAddress = %q after the rename", got)
	}
}

// SQLite is where an unstated rename does its damage quietly: the
// change forces a table rebuild, the rebuild copies the columns the two
// snapshots agree on, and the renamed one is not among them — so the
// data goes without a DROP COLUMN anywhere in the file. This runs both
// migrations against a real database and looks at what is left.
func TestSQLiteGeneratedRenameKeepsTheDataAcrossARebuild(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("email").NotNull())
	if _, err := db.ExecExpr(ctx, sqlite.CreateTable(before)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "users" (id, email) VALUES (1, ?)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A rename that also forces a rebuild: SQLite's ALTER TABLE ADD
	// COLUMN refuses a UNIQUE column, so this change cannot be made in
	// place. The added column is also a second candidate for the
	// dropped "email" — same table, same type — and answering the first
	// pair settles it, which is the resolution rule this exercises.
	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("emailAddress").NotNull())
	sqlite.Add(after, sqlite.Text("handle").Unique())

	prevSnapshot, err := sqlite.BuildSnapshot(sqlite.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys := migrationDirFor(t, "sqlite", prevSnapshot)

	_, err = sqlite.GenerateMigration(sqlite.GenerateOptions{
		Schema: sqlite.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { t.Error("a refused run wrote a file"); return nil },
	})
	var amb *sqlite.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("an unanswered rename generated a migration: %v", err)
	}

	res, err := sqlite.GenerateMigration(sqlite.GenerateOptions{
		Schema: sqlite.NewSchema(after),
		Dir:    "m",
		Name:   "rename",
		FS:     fsys,
		Write:  func(string, []byte) error { return nil },
		Renames: []sqlite.RenameDecision{{
			Rename:   sqlite.Rename{Kind: sqlite.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, "-- rebuild") {
		t.Fatalf("this test is meant to exercise the rebuild path:\n%s", res.SQL)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if strings.HasPrefix(stmt, "--") {
			continue
		}
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("SQLite rejected %q: %v", stmt, err)
		}
	}

	var got string
	rows, err := db.Query(ctx, `SELECT "emailAddress" FROM "users" WHERE id = 1`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone: the rebuild copied the table without the renamed column")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Errorf(`emailAddress = %q after the rebuild, want "ada@example.com"`, got)
	}
}
