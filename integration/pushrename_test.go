package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
)

// Push and renames, against the three engines that have a Push.
//
// A structural diff cannot tell a renamed column from a dropped one and
// an added one. GenerateMigration has refused to guess since renames
// landed; Push reached the same comparison and guessed anyway, which
// made it a second door into the same data loss — and the quieter one,
// because a push has no migration file for anyone to read before it
// runs.
//
// Every test here puts a row in first and reads it back afterwards.
// Comparing statement lists would prove nothing about SQLite, where the
// bug is a statement that is absent: the rebuild copies the columns
// both sides name and simply leaves the renamed one out, so the data
// goes with no DROP COLUMN anywhere for a destructive-statement guard
// to catch.

// --- SQLite ------------------------------------------------------------

// sqliteUsersWithEmail creates the "before" table and seeds one row.
func sqliteUsersWithEmail(t *testing.T, db *sqlite.DB) {
	t.Helper()
	ctx := context.Background()
	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("email").NotNull())
	if _, err := db.ExecExpr(ctx, sqlite.CreateTable(before)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "users" (id, email) VALUES (1, ?)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}
}

// sqliteColumn reads one column of the seeded row back, reporting
// whether the table has that column at all.
//
// The existence check is a PRAGMA rather than a SELECT that fails,
// because on SQLite it would not fail: a double-quoted name that
// matches no column is read as a string literal, so `SELECT "email"`
// against a table with no email column returns the word "email" for
// every row. A test asking whether a column survived a rename is
// exactly the test that misreading would pass.
func sqliteColumn(t *testing.T, db *sqlite.DB, table, name string) (string, bool) {
	t.Helper()
	ctx := context.Background()
	cols, err := db.Query(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("pragma_table_info(%s): %v", table, err)
	}
	found := false
	for cols.Next() {
		var got string
		if err := cols.Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got == name {
			found = true
		}
	}
	cols.Close()
	if !found {
		return "", false
	}

	rows, err := db.Query(ctx, `SELECT "`+name+`" FROM "`+table+`" WHERE id = 1`)
	if err != nil {
		t.Fatalf("read %s.%s: %v", table, name, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the row is gone entirely")
	}
	var v sql.NullString
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("scan %s: %v", name, err)
	}
	// The bool answers "does the table have this column", not "is the
	// value non-NULL". A rename that was applied as a drop-and-add
	// leaves the column there and the value NULL, and the two failures
	// have to be distinguishable.
	return v.String, true
}

func TestSQLitePushRefusesARenameAndKeepsTheData(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	sqliteUsersWithEmail(t, db)

	// The nullable new column is the case with teeth: with NOT NULL the
	// rebuild's INSERT ... SELECT fails and the push at least stops
	// loudly. Nullable, it succeeds, and every row's email becomes NULL.
	unanswered := sqlite.NewTable("users")
	sqlite.Add(unanswered, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(unanswered, sqlite.Text("emailAddress"))

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(unanswered))
	if err == nil {
		t.Fatalf("push applied an unanswered rename:\n%s", strings.Join(res.Statements, "\n"))
	}
	var amb *sqlite.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("error is %T, want *sqlite.RenameAmbiguityError: %v", err, err)
	}
	if len(amb.Candidates) != 1 || amb.Candidates[0].From != "email" || amb.Candidates[0].To != "emailAddress" {
		t.Errorf("candidates: %+v", amb.Candidates)
	}
	if !strings.Contains(amb.Error(), `RenamedFrom("email")`) {
		t.Errorf("the refusal does not say how to answer it:\n%s", amb.Error())
	}
	if got, ok := sqliteColumn(t, db, "users", "email"); !ok || got != "ada@example.com" {
		t.Fatalf("the refused push touched the table: email = %q, present = %v", got, ok)
	}

	// The schema states the answer, and the same push goes through.
	answered := sqlite.NewTable("users")
	sqlite.Add(answered, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(answered, sqlite.Text("emailAddress").RenamedFrom("email"))

	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(answered)); err != nil {
		t.Fatalf("push with the rename declared: %v", err)
	}
	got, ok := sqliteColumn(t, db, "users", "emailAddress")
	if !ok || got != "ada@example.com" {
		t.Fatalf("the row did not survive the rename: emailAddress = %q, present = %v", got, ok)
	}
	if _, ok := sqliteColumn(t, db, "users", "email"); ok {
		t.Error("the old column is still there, so the rename was an add")
	}

	// And the declaration is inert once the rename has happened: the
	// same schema pushed again has nothing left to do, rather than
	// looking for an "email" no database has had since.
	again, err := sqlite.Push(ctx, db, sqlite.NewSchema(answered))
	if err != nil {
		t.Fatalf("a second push of the settled schema: %v", err)
	}
	if len(again.Statements) != 0 {
		t.Errorf("a settled schema still wanted to change something:\n%s", strings.Join(again.Statements, "\n"))
	}
}

// The other answer: no, the column really is going. That is not a fact
// about the schema — once pushed, the old column is gone and the
// question never comes back — so it is stated for the one run.
//
// It takes both answers to get through, and deliberately so: declining
// the rename settles what the change means, AllowDestructive settles
// whether it may run, and neither stands in for the other. The
// declined rename alone leaves Push refusing a drop-column nobody
// permitted; AllowDestructive alone leaves it refusing to guess at a
// rename nobody answered.
func TestSQLitePushTakesADeclinedRename(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	sqliteUsersWithEmail(t, db)

	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("emailAddress"))
	declined := []sqlite.RenameDecision{{
		// No To: a refusal names only the object that is going.
		Rename: sqlite.Rename{Kind: sqlite.RenameColumn, Table: "users", From: "email"},
	}}

	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(after), sqlite.PushOptions{
		Renames: declined,
	}); err == nil {
		t.Fatal("a declined rename applied the drop it declined into; that answers the wrong question")
	} else {
		var destructive *sqlite.DestructiveChangeError
		if !errors.As(err, &destructive) {
			t.Fatalf("error is %T, want *sqlite.DestructiveChangeError: %v", err, err)
		}
	}
	if got, ok := sqliteColumn(t, db, "users", "email"); !ok || got != "ada@example.com" {
		t.Fatalf("the refused push touched the table: email = %q, present = %v", got, ok)
	}

	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(after), sqlite.PushOptions{
		Renames:          declined,
		AllowDestructive: true,
	}); err != nil {
		t.Fatalf("push with the drop stated and permitted: %v", err)
	}
	if _, ok := sqliteColumn(t, db, "users", "email"); ok {
		t.Error("the column the push was told to drop is still there")
	}
	if got, ok := sqliteColumn(t, db, "users", "emailAddress"); !ok || got != "" {
		t.Errorf("emailAddress = %q, present = %v; a declined rename adds an empty column", got, ok)
	}
}

// A table rename on SQLite.
//
// Unstated, this one loses nothing: sqlite.Push leaves a table the
// schema never names alone, so an undeclared "users" simply stays
// where it is and an empty "people" appears beside it. Stated, the old
// table has to come back into view — the schema has claimed it under
// its former name — and the rows have to move with it. That is the
// case renamedAwayTables exists for, and without it the declaration is
// accepted and applied to nothing.
func TestSQLitePushRenamesATableAndKeepsTheRows(t *testing.T) {
	ctx := context.Background()

	// Unstated: users is not a table this schema names, so Push leaves
	// it alone and builds an empty people beside it. Nothing is lost,
	// and nothing has moved either.
	t.Run("unstated", func(t *testing.T) {
		db := openSQLite(t)
		sqliteUsersWithEmail(t, db)
		unstated := sqlite.NewTable("people")
		sqlite.Add(unstated, sqlite.BigInt("id").PrimaryKey())
		sqlite.Add(unstated, sqlite.Text("email").NotNull())
		if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(unstated)); err != nil {
			t.Fatalf("push of a table the schema renamed without saying so: %v", err)
		}
		if got, ok := sqliteColumn(t, db, "users", "email"); !ok || got != "ada@example.com" {
			t.Fatalf("the push touched the table the schema stopped naming: %q, present = %v", got, ok)
		}
	})

	db := openSQLite(t)
	sqliteUsersWithEmail(t, db)
	answered := sqlite.NewTable("people").RenamedFrom("users")
	sqlite.Add(answered, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(answered, sqlite.Text("email").NotNull())
	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(answered)); err != nil {
		t.Fatalf("push with the table rename declared: %v", err)
	}
	rows, err := db.Query(ctx, `SELECT email FROM "people" WHERE id = 1`)
	if err != nil {
		t.Fatalf("read people: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the renamed table is empty: the push dropped and re-created it")
	}
	var email string
	if err := rows.Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email != "ada@example.com" {
		t.Fatalf("people.email = %q", email)
	}
}

// --- PostgreSQL --------------------------------------------------------

func TestPGPushRefusesARenameAndKeepsTheData(t *testing.T) {
	ctx := context.Background()
	// Push's previous side is the whole schema, so this one gets a
	// database to itself rather than sharing the suite's.
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())
	if _, err := db.ExecExpr(ctx, pg.CreateTable(before)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (email) VALUES ($1)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Nullable, so the drop-and-add the guess would produce actually
	// commits. NOT NULL would have PostgreSQL reject the ADD COLUMN
	// against the rows already there, which is a loud failure rather
	// than a silent loss and so the easier half of the problem.
	unanswered := pg.NewTable("users")
	pg.Add(unanswered, pg.BigSerial("id").PrimaryKey())
	pg.Add(unanswered, pg.Text("emailAddress"))

	res, err := pg.Push(ctx, db, pg.NewSchema(unanswered))
	if err == nil {
		t.Fatalf("push applied an unanswered rename:\n%s", strings.Join(res.Statements, "\n"))
	}
	var amb *pg.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("error is %T, want *pg.RenameAmbiguityError: %v", err, err)
	}
	if !strings.Contains(amb.Error(), `RenamedFrom("email")`) {
		t.Errorf("the refusal does not say how to answer it:\n%s", amb.Error())
	}
	if cols := columnsOf(t, db, "users"); cols["email"] == "" {
		t.Fatalf("the refused push changed the table: %v", cols)
	}

	answered := pg.NewTable("users")
	pg.Add(answered, pg.BigSerial("id").PrimaryKey())
	pg.Add(answered, pg.Text("emailAddress").RenamedFrom("email"))

	if _, err := pg.Push(ctx, db, pg.NewSchema(answered)); err != nil {
		t.Fatalf("push with the rename declared: %v", err)
	}
	if cols := columnsOf(t, db, "users"); cols["emailAddress"] != "text" || cols["email"] != "" {
		t.Fatalf("users after the push = %v", cols)
	}
	if got := scalar(t, db, `SELECT "emailAddress" FROM users`); got != "ada@example.com" {
		t.Fatalf("the row did not survive the rename: emailAddress = %q", got)
	}

	again, err := pg.Push(ctx, db, pg.NewSchema(answered))
	if err != nil {
		t.Fatalf("a second push of the settled schema: %v", err)
	}
	if len(again.Statements) != 0 {
		t.Errorf("a settled schema still wanted to change something:\n%s", strings.Join(again.Statements, "\n"))
	}
}

// The per-run answer, for a caller that has one and no schema to put it
// in — the shape `drops push --rename-column` travels in.
func TestPGPushTakesARenameFromPushOptions(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, freshDatabase(t))

	before := pg.NewTable("users")
	pg.Add(before, pg.BigSerial("id").PrimaryKey())
	pg.Add(before, pg.Text("email").NotNull())
	if _, err := db.ExecExpr(ctx, pg.CreateTable(before)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO users (email) VALUES ($1)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	after := pg.NewTable("users")
	pg.Add(after, pg.BigSerial("id").PrimaryKey())
	pg.Add(after, pg.Text("emailAddress").NotNull())

	if _, err := pg.Push(ctx, db, pg.NewSchema(after), pg.PushOptions{
		Renames: []pg.RenameDecision{{
			Rename:   pg.Rename{Kind: pg.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
			IsRename: true,
		}},
	}); err != nil {
		t.Fatalf("push with the rename given: %v", err)
	}
	if got := scalar(t, db, `SELECT "emailAddress" FROM users`); got != "ada@example.com" {
		t.Fatalf("the row did not survive the rename: emailAddress = %q", got)
	}
}

// --- MySQL / MariaDB ---------------------------------------------------

func TestMySQLPushRefusesARenameAndKeepsTheData(t *testing.T) {
	ctx := context.Background()
	db := openMySQL(t)
	name := integration.UniqueName(t, "pr")

	before := mysql.NewTable(name)
	mysql.Add(before, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(before, mysql.Varchar("email", 190).NotNull())
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), "DROP TABLE IF EXISTS `"+name+"`")
	})
	if _, err := db.ExecExpr(ctx, mysql.CreateTable(before)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO `"+name+"` (email) VALUES (?)", "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	unanswered := mysql.NewTable(name)
	mysql.Add(unanswered, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(unanswered, mysql.Varchar("emailAddress", 190).NotNull())

	res, err := mysql.Push(ctx, db, mysql.NewSchema(unanswered))
	if err == nil {
		t.Fatalf("push applied an unanswered rename:\n%s", strings.Join(res.Statements, "\n"))
	}
	var amb *mysql.RenameAmbiguityError
	if !errors.As(err, &amb) {
		t.Fatalf("error is %T, want *mysql.RenameAmbiguityError: %v", err, err)
	}
	if !strings.Contains(amb.Error(), `RenamedFrom("email")`) {
		t.Errorf("the refusal does not say how to answer it:\n%s", amb.Error())
	}
	if got := mysqlScalar(t, db, "SELECT email FROM `"+name+"`"); got != "ada@example.com" {
		t.Fatalf("the refused push changed the table: email = %q", got)
	}

	answered := mysql.NewTable(name)
	mysql.Add(answered, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(answered, mysql.Varchar("emailAddress", 190).NotNull().RenamedFrom("email"))

	if _, err := mysql.Push(ctx, db, mysql.NewSchema(answered)); err != nil {
		t.Fatalf("push with the rename declared: %v", err)
	}
	if got := mysqlScalar(t, db, "SELECT emailAddress FROM `"+name+"`"); got != "ada@example.com" {
		t.Fatalf("the row did not survive the rename: emailAddress = %q", got)
	}

	again, err := mysql.Push(ctx, db, mysql.NewSchema(answered))
	if err != nil {
		t.Fatalf("a second push of the settled schema: %v", err)
	}
	if len(again.Statements) != 0 {
		t.Errorf("a settled schema still wanted to change something:\n%s", strings.Join(again.Statements, "\n"))
	}
}

// The table-level answer against MariaDB, and with it the narrowing
// mysql.Push does before it diffs: a table the schema no longer names
// is somebody else's and is filtered out of the previous side, which
// would have filtered out the very table the declared rename is about.
func TestMySQLPushRenamesATableAndKeepsTheRows(t *testing.T) {
	ctx := context.Background()
	db := openMySQL(t)
	from := integration.UniqueName(t, "prf")
	to := integration.UniqueName(t, "prt")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = db.Exec(bg, "DROP TABLE IF EXISTS `"+from+"`")
		_, _ = db.Exec(bg, "DROP TABLE IF EXISTS `"+to+"`")
	})

	before := mysql.NewTable(from)
	mysql.Add(before, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(before, mysql.Varchar("email", 190).NotNull())
	if _, err := db.ExecExpr(ctx, mysql.CreateTable(before)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, "INSERT INTO `"+from+"` (email) VALUES (?)", "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	after := mysql.NewTable(to).RenamedFrom(from)
	mysql.Add(after, mysql.BigInt("id").PrimaryKey().AutoIncrement())
	mysql.Add(after, mysql.Varchar("email", 190).NotNull())
	if _, err := mysql.Push(ctx, db, mysql.NewSchema(after)); err != nil {
		t.Fatalf("push with the table rename declared: %v", err)
	}
	if got := mysqlScalar(t, db, "SELECT email FROM `"+to+"`"); got != "ada@example.com" {
		t.Fatalf("the rows did not move with the table: email = %q", got)
	}
}

// --- the answer given for this run, against the one in the schema ----

// The two states of one project. The old column carries a UNIQUE, so
// the answer under test is given about a column whose drop takes a
// constraint with it — the shape that used to make the plain diff emit
// a DROP CONSTRAINT after the DROP COLUMN that had already removed it,
// which PostgreSQL rejects. See pg.Diff on the order the drops run in.
const schemaPlainEmail = `package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`

// schemaDeclaresRename is the same project with the rename stated on
// the column, as `drops push` would find it once it has moved on.
const schemaDeclaresRename = `package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("emailAddress").RenamedFrom("email"))
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`

// --drop-column outranks a RenamedFrom naming the same column.
//
// The refusal push prints says a per-run answer wins over the schema's,
// and names --drop-column as the way to say the other thing. Both were
// true only while the schema said nothing: a declaration and a refusal
// are not the same shape — one names a pair, one names only the column
// that is going — so the refusal could not outrank the declaration by
// carrying the same key, and was simply not heard. An operator who had
// decided the column really was going had nothing to say.
func TestCLIPushDropColumnOutranksTheSchemasDeclaration(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(schemaPlainEmail)
	p.mustRun("push", "--schema", "./schema")
	if _, err := db.Exec(context.Background(),
		`INSERT INTO users (email) VALUES ($1)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	p.schema(schemaDeclaresRename)
	// The rename is destructive of nothing, so without the refusal this
	// push renames and succeeds. With it, the DROP COLUMN stands and
	// the destructive guard is what stops the run.
	stdout, stderr, code := p.run("push", "--schema", "./schema", "--drop-column", "users.email")
	if code != 3 {
		t.Fatalf("push exited %d, want 3 (the DROP is destructive)\n%s%s", code, stdout, stderr)
	}
	out := stdout + stderr
	if strings.Contains(out, "RENAME COLUMN") {
		t.Fatalf("the declaration outvoted --drop-column:\n%s", out)
	}
	if !strings.Contains(out, `DROP COLUMN "email"`) {
		t.Fatalf("the column the operator said was going is not being dropped:\n%s", out)
	}
	// The safety analyser reads statements and cannot see that the
	// question was answered on the command line. Still asking it, right
	// under the answer, is how a tool teaches people to stop reading it.
	if strings.Contains(out, "unstated-column-rename") {
		t.Fatalf("push asked about a rename --drop-column had already answered:\n%s", out)
	}

	p.mustRun("push", "--schema", "./schema", "--drop-column", "users.email", "--allow-destructive")
	cols := columnsOf(t, db, "users")
	if cols["email"] != "" {
		t.Errorf("email survived the drop it was told to do: %v", cols)
	}
	if cols["emailAddress"] == "" {
		t.Errorf("emailAddress was not added: %v", cols)
	}
}

// A table rename declaration is meant to be left in the schema until
// every database has caught up, so it outlives the rename. What it must
// not outlive is its claim on the old name.
//
// sqlite.Push narrows the live database to the tables the Schema
// declares, because the file is shared: another service's table is not
// something a push may drop. renamedAwayTables punches a hole in that
// narrowing so a declared rename can still see the table it is about.
// Once the rename has happened the hole has to close again — the new
// name is in the database, the declaration has nothing left to do, and
// anything that later takes the old name belongs to somebody else.
func TestSQLitePushLetsGoOfARenamedAwayName(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	people := func() *sqlite.Schema {
		t := sqlite.NewTable("people").RenamedFrom("users")
		sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
		sqlite.Add(t, sqlite.Text("email").NotNull())
		return sqlite.NewSchema(t)
	}
	if _, err := sqlite.Push(ctx, db, people()); err != nil {
		t.Fatalf("the push that performs the rename: %v", err)
	}

	// Another service now keeps a table under the name this schema used
	// to use. The declaration is still in the source, waiting for the
	// deployments that have not caught up.
	if _, err := db.Exec(ctx, `CREATE TABLE "users" (id INTEGER PRIMARY KEY, secret TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "users" VALUES (1, 'not yours')`); err != nil {
		t.Fatal(err)
	}

	res, err := sqlite.Push(ctx, db, people())
	if err != nil {
		t.Fatalf("the settled schema stopped on somebody else's table: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Errorf("the settled schema wanted to change something:\n%s", strings.Join(res.Statements, "\n"))
	}
	rows, err := db.Query(ctx, `SELECT secret FROM "users" WHERE id = 1`)
	if err != nil {
		t.Fatalf("read the other service's table: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the push dropped a table the Schema does not declare")
	}
}
