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

// A table renamed and a column inside it renamed in the same migration.
//
// This is the case the detector was blind to: while the two snapshots
// file the table under different names nothing in it pairs with
// anything, so the generator asked nothing and the diff dropped the
// table and built a new one. The row below is what that cost.
func TestPGRenamedTableAndColumnKeepTheData(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	oldName := integration.UniqueName(t, "old")
	newName := integration.UniqueName(t, "new")

	before := pg.NewTable(oldName)
	pg.Add(before, pg.BigInt("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())
	after := pg.NewTable(newName)
	pg.Add(after, pg.BigInt("id").PrimaryKey())
	pg.Add(after, pg.Text("emailAddress").NotNull())
	dropPG(t, db, before)
	dropPG(t, db, after)
	execPG(t, db, pg.CreateTable(before))

	if _, err := db.Exec(ctx,
		`INSERT INTO `+drops.StdQuoteIdent(oldName)+` (id, email) VALUES (1, $1)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevSnapshot, err := pg.BuildSnapshot(pg.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys := migrationDirFor(t, "postgresql", prevSnapshot)
	opts := func(renames ...pg.RenameDecision) pg.GenerateOptions {
		return pg.GenerateOptions{
			Schema:  pg.NewSchema(after),
			Dir:     "m",
			Name:    "rename",
			FS:      fsys,
			Write:   func(string, []byte) error { return nil },
			Renames: renames,
		}
	}

	// Nothing answered: a question, not a DROP TABLE.
	var amb *pg.RenameAmbiguityError
	if _, err := pg.GenerateMigration(opts()); !errors.As(err, &amb) {
		t.Fatalf("an unanswered table rename generated a migration: %v", err)
	}

	// The table answered but not the column: still a question, because
	// answering the first is what made the second one visible.
	tableAnswer := pg.RenameDecision{
		Rename:   pg.Rename{Kind: pg.RenameTable, From: oldName, To: newName},
		IsRename: true,
	}
	if _, err := pg.GenerateMigration(opts(tableAnswer)); !errors.As(err, &amb) {
		t.Fatalf("the column inside the renamed table was dropped without a question: %v", err)
	}
	if len(amb.Candidates) != 1 || amb.Candidates[0].Table != newName || amb.Candidates[0].From != "email" {
		t.Fatalf("candidates: %+v", amb.Candidates)
	}

	withDown := opts(tableAnswer, pg.RenameDecision{
		Rename:   pg.Rename{Kind: pg.RenameColumn, Table: newName, From: "email", To: "emailAddress"},
		IsRename: true,
	})
	withDown.WithDown = true
	res, err := pg.GenerateMigration(withDown)
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if strings.Contains(res.SQL, "DROP") {
		t.Fatalf("a migration made entirely of renames drops something:\n%s", res.SQL)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("PostgreSQL rejected %q: %v", stmt, err)
		}
	}

	var got string
	rows, err := db.Query(ctx,
		`SELECT "emailAddress" FROM `+drops.StdQuoteIdent(newName)+` WHERE id = 1`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone: the table was replaced rather than renamed")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Errorf("emailAddress = %q after the rename", got)
	}
	rows.Close()

	// And back again. A rollback that dropped the table would lose the
	// row just as thoroughly as the migration would have, and the
	// column names its table by the table's new name in both
	// directions, so the down script has to move that back too.
	for _, stmt := range migrationStatements(res.DownSQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("PostgreSQL rejected the rollback %q: %v\nwhole script:\n%s", stmt, err, res.DownSQL)
		}
	}
	back, err := db.Query(ctx,
		`SELECT "email" FROM `+drops.StdQuoteIdent(oldName)+` WHERE id = 1`)
	if err != nil {
		t.Fatalf("read back after the rollback: %v", err)
	}
	defer back.Close()
	if !back.Next() {
		t.Fatal("the row is gone after the rollback")
	}
	if err := back.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Errorf("email = %q after the rollback", got)
	}
}

// Renaming a column that the primary key, an index, a foreign key, a
// CHECK constraint and a view all name. Every one of those is a way for
// a migration that renamed the column correctly to still stop at the
// server, so the test is whether PostgreSQL accepts the whole file —
// and whether the rows are all still there when it has.
func TestPGRenameOfALoadBearingColumnApplies(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	parent := integration.UniqueName(t, "parent")
	child := integration.UniqueName(t, "child")
	view := integration.UniqueName(t, "v")

	build := func(col string) (*pg.Table, *pg.Table, *pg.PgView) {
		p := pg.NewTable(parent)
		tenant := pg.Add(p, pg.Text(col).NotNull())
		pid := pg.Add(p, pg.BigInt("id").NotNull())
		p.PrimaryKey(tenant, pid)
		p.AddCheck(parent+"_nonempty", `"`+col+`" <> ''`)
		p.AddIndex(pg.NewIndex(parent+"_idx", p, tenant))

		c := pg.NewTable(child)
		pg.Add(c, pg.BigInt("id").PrimaryKey())
		ct := pg.Add(c, pg.Text(col).NotNull())
		cid := pg.Add(c, pg.BigInt("parentId").NotNull())
		c.ForeignKeyN(
			[]pg.ColRef{ct, cid}, p,
			[]pg.ColRef{tenant, pid})

		// No alias: the view's output column is named after the table
		// column, so renaming the column renames the view's column too
		// — which is the thing CREATE OR REPLACE VIEW will not do.
		v := pg.NewView(view, `SELECT "`+col+`", "id" FROM `+drops.StdQuoteIdent(parent))
		return p, c, v
	}

	beforeP, beforeC, beforeV := build("tenant")
	afterP, afterC, afterV := build("tenantId")
	dropPG(t, db, beforeC)
	dropPG(t, db, beforeP)
	t.Cleanup(func() { _, _ = db.Exec(ctx, `DROP VIEW IF EXISTS `+drops.StdQuoteIdent(view)) })
	_, _ = db.Exec(ctx, `DROP VIEW IF EXISTS `+drops.StdQuoteIdent(view))
	_, _ = db.Exec(ctx, `DROP TABLE IF EXISTS `+drops.StdQuoteIdent(child))
	_, _ = db.Exec(ctx, `DROP TABLE IF EXISTS `+drops.StdQuoteIdent(parent))
	for _, stmt := range pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(pg.NewSchema(beforeP, beforeC).AddView(beforeV))) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("setting up: PostgreSQL rejected %q: %v", stmt, err)
		}
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO `+drops.StdQuoteIdent(parent)+` ("tenant", id) VALUES ($1, 1)`, "acme"); err != nil {
		t.Fatalf("insert parent: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO `+drops.StdQuoteIdent(child)+` (id, "tenant", "parentId") VALUES (1, $1, 1)`, "acme"); err != nil {
		t.Fatalf("insert child: %v", err)
	}

	prevSnapshot, err := pg.BuildSnapshot(pg.NewSchema(beforeP, beforeC).AddView(beforeV)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(afterP, afterC).AddView(afterV),
		Dir:    "m",
		Name:   "rename",
		FS:     migrationDirFor(t, "postgresql", prevSnapshot),
		Write:  func(string, []byte) error { return nil },
		Renames: []pg.RenameDecision{
			{Rename: pg.Rename{Kind: pg.RenameColumn, Table: parent, From: "tenant", To: "tenantId"}, IsRename: true},
			{Rename: pg.Rename{Kind: pg.RenameColumn, Table: child, From: "tenant", To: "tenantId"}, IsRename: true},
		},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if strings.Contains(res.SQL, "DROP COLUMN") {
		t.Fatalf("a renamed column was dropped:\n%s", res.SQL)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("PostgreSQL rejected %q: %v\nwhole migration:\n%s", stmt, err, res.SQL)
		}
	}

	for _, q := range []string{
		`SELECT "tenantId" FROM ` + drops.StdQuoteIdent(parent) + ` WHERE id = 1`,
		`SELECT "tenantId" FROM ` + drops.StdQuoteIdent(child) + ` WHERE id = 1`,
		`SELECT "tenantId" FROM ` + drops.StdQuoteIdent(view),
	} {
		var got string
		rows, err := db.Query(ctx, q)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if !rows.Next() {
			rows.Close()
			t.Fatalf("%s: no rows; the rename took the data with it", q)
		}
		if err := rows.Scan(&got); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		if got != "acme" {
			t.Errorf("%s = %q, want %q", q, got, "acme")
		}
	}
}

// A rename that also retypes the column, against a row that has to
// survive both.
func TestPGRenameWithATypeChangeKeepsTheData(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "retyped")

	before := pg.NewTable(name)
	pg.Add(before, pg.BigInt("id").PrimaryKey())
	pg.Add(before, pg.Varchar("email", 120).NotNull())
	after := pg.NewTable(name)
	pg.Add(after, pg.BigInt("id").PrimaryKey())
	pg.Add(after, pg.Text("emailAddress").NotNull())
	dropPG(t, db, before)
	execPG(t, db, pg.CreateTable(before))

	quoted := drops.StdQuoteIdent(name)
	if _, err := db.Exec(ctx,
		`INSERT INTO `+quoted+` (id, email) VALUES (1, $1)`, "ada@example.com"); err != nil {
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
			Rename:   pg.Rename{Kind: pg.RenameColumn, Table: name, From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	})
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	if !strings.Contains(res.SQL, "SET DATA TYPE text") {
		t.Fatalf("the residual type change is missing:\n%s", res.SQL)
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
		t.Fatal("the row is gone")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Errorf("emailAddress = %q", got)
	}
}

// Two renames on one table, each of which could have been paired with
// the other's new name. Both rows have to end up under the name the
// answer gave them, which is the part a wrong pairing gets wrong
// without losing anything — the data is still there, in the wrong
// column.
func TestPGTwoRenamesAtOncePairAsAnswered(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	name := integration.UniqueName(t, "pairs")

	before := pg.NewTable(name)
	pg.Add(before, pg.BigInt("id").PrimaryKey())
	pg.Add(before, pg.Text("a").NotNull())
	pg.Add(before, pg.Text("b").NotNull())
	after := pg.NewTable(name)
	pg.Add(after, pg.BigInt("id").PrimaryKey())
	pg.Add(after, pg.Text("x").NotNull())
	pg.Add(after, pg.Text("y").NotNull())
	dropPG(t, db, before)
	execPG(t, db, pg.CreateTable(before))

	quoted := drops.StdQuoteIdent(name)
	if _, err := db.Exec(ctx,
		`INSERT INTO `+quoted+` (id, a, b) VALUES (1, $1, $2)`, "left", "right"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevSnapshot, err := pg.BuildSnapshot(pg.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys := migrationDirFor(t, "postgresql", prevSnapshot)

	// Four candidates, since either could be either. Answering one
	// settles the pairs that name it, and the other is still asked.
	one := pg.RenameDecision{
		Rename:   pg.Rename{Kind: pg.RenameColumn, Table: name, From: "a", To: "x"},
		IsRename: true,
	}
	var amb *pg.RenameAmbiguityError
	if _, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after), Dir: "m", Name: "rename", FS: fsys,
		Write:   func(string, []byte) error { return nil },
		Renames: []pg.RenameDecision{one},
	}); !errors.As(err, &amb) {
		t.Fatalf("half an answer generated a migration: %v", err)
	}
	if len(amb.Candidates) != 1 || amb.Candidates[0].From != "b" || amb.Candidates[0].To != "y" {
		t.Fatalf("candidates after one answer: %+v", amb.Candidates)
	}

	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after), Dir: "m", Name: "rename", FS: fsys,
		Write: func(string, []byte) error { return nil },
		Renames: []pg.RenameDecision{one, {
			Rename:   pg.Rename{Kind: pg.RenameColumn, Table: name, From: "b", To: "y"},
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

	var x, y string
	rows, err := db.Query(ctx, `SELECT "x", "y" FROM `+quoted+` WHERE id = 1`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone")
	}
	if err := rows.Scan(&x, &y); err != nil {
		t.Fatal(err)
	}
	if x != "left" || y != "right" {
		t.Errorf("x, y = %q, %q — the two renames were paired the other way round", x, y)
	}
}

// A column that really is being dropped, with nothing arriving to be
// mistaken for it, is not a question at all.
func TestPGAGenuineDropIsNotAQuestion(t *testing.T) {
	name := integration.UniqueName(t, "dropped")
	before := pg.NewTable(name)
	pg.Add(before, pg.BigInt("id").PrimaryKey())
	pg.Add(before, pg.Text("legacy"))
	after := pg.NewTable(name)
	pg.Add(after, pg.BigInt("id").PrimaryKey())

	prevSnapshot, err := pg.BuildSnapshot(pg.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	res, err := pg.GenerateMigration(pg.GenerateOptions{
		Schema: pg.NewSchema(after),
		Dir:    "m",
		Name:   "drop",
		FS:     migrationDirFor(t, "postgresql", prevSnapshot),
		Write:  func(string, []byte) error { return nil },
	})
	if err != nil {
		t.Fatalf("an unambiguous drop was refused: %v", err)
	}
	if !strings.Contains(res.SQL, `DROP COLUMN "legacy"`) {
		t.Errorf("migration:\n%s", res.SQL)
	}
}

// The MySQL half of the table-plus-column rename.
func TestMySQLRenamedTableAndColumnKeepTheData(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()
	oldName := integration.UniqueName(t, "old")
	newName := integration.UniqueName(t, "new")

	before := mysql.NewTable(oldName)
	mysql.Add(before, mysql.BigInt("id").PrimaryKey())
	mysql.Add(before, mysql.Varchar("email", 190).NotNull())
	after := mysql.NewTable(newName)
	mysql.Add(after, mysql.BigInt("id").PrimaryKey())
	mysql.Add(after, mysql.Varchar("emailAddress", 190).NotNull())
	dropMySQL(t, db, before)
	dropMySQL(t, db, after)
	execMySQL(t, db, mysql.CreateTable(before))

	if _, err := db.Exec(ctx,
		"INSERT INTO "+drops.BacktickQuoteIdent(oldName)+" (id, email) VALUES (1, ?)", "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	prevSnapshot, err := mysql.BuildSnapshot(mysql.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys := migrationDirFor(t, "mysql", prevSnapshot)
	server, err := mysql.ServerVersion(ctx, db)
	if err != nil {
		t.Fatalf("server version: %v", err)
	}
	opts := func(renames ...mysql.RenameDecision) mysql.GenerateOptions {
		return mysql.GenerateOptions{
			Schema:  mysql.NewSchema(after),
			Dir:     "m",
			Name:    "rename",
			FS:      fsys,
			Write:   func(string, []byte) error { return nil },
			Server:  server,
			Renames: renames,
		}
	}
	tableAnswer := mysql.RenameDecision{
		Rename:   mysql.Rename{Kind: mysql.RenameTable, From: oldName, To: newName},
		IsRename: true,
	}
	var amb *mysql.RenameAmbiguityError
	if _, err := mysql.GenerateMigration(opts(tableAnswer)); !errors.As(err, &amb) {
		t.Fatalf("the column inside the renamed table was dropped without a question: %v", err)
	}

	res, err := mysql.GenerateMigration(opts(tableAnswer, mysql.RenameDecision{
		Rename:   mysql.Rename{Kind: mysql.RenameColumn, Table: newName, From: "email", To: "emailAddress"},
		IsRename: true,
	}))
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
	}
	for _, stmt := range migrationStatements(res.SQL) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s rejected %q: %v", server, stmt, err)
		}
	}

	var got string
	rows, err := db.Query(ctx,
		"SELECT `emailAddress` FROM "+drops.BacktickQuoteIdent(newName)+" WHERE id = 1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone: the table was replaced rather than renamed")
	}
	if err := rows.Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Errorf("emailAddress = %q", got)
	}
}

// The SQLite half, where the table rename and the rebuild meet.
func TestSQLiteRenamedTableAndColumnKeepTheData(t *testing.T) {
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

	after := sqlite.NewTable("people")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("emailAddress").NotNull())

	prevSnapshot, err := sqlite.BuildSnapshot(sqlite.NewSchema(before)).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	fsys := migrationDirFor(t, "sqlite", prevSnapshot)
	opts := func(renames ...sqlite.RenameDecision) sqlite.GenerateOptions {
		return sqlite.GenerateOptions{
			Schema:  sqlite.NewSchema(after),
			Dir:     "m",
			Name:    "rename",
			FS:      fsys,
			Write:   func(string, []byte) error { return nil },
			Renames: renames,
		}
	}
	tableAnswer := sqlite.RenameDecision{
		Rename:   sqlite.Rename{Kind: sqlite.RenameTable, From: "users", To: "people"},
		IsRename: true,
	}
	var amb *sqlite.RenameAmbiguityError
	if _, err := sqlite.GenerateMigration(opts()); !errors.As(err, &amb) {
		t.Fatalf("an unanswered table rename generated a migration: %v", err)
	}
	if _, err := sqlite.GenerateMigration(opts(tableAnswer)); !errors.As(err, &amb) {
		t.Fatalf("the column inside the renamed table went without a question: %v", err)
	}

	res, err := sqlite.GenerateMigration(opts(tableAnswer, sqlite.RenameDecision{
		Rename:   sqlite.Rename{Kind: sqlite.RenameColumn, Table: "people", From: "email", To: "emailAddress"},
		IsRename: true,
	}))
	if err != nil {
		t.Fatalf("GenerateMigration: %v", err)
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
	rows, err := db.Query(ctx, `SELECT "emailAddress" FROM "people" WHERE id = 1`)
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
	if got != "ada@example.com" {
		t.Errorf("emailAddress = %q", got)
	}
}
