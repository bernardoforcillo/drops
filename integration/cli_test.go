package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
)

// The `drops` binary, end to end.
//
// Every assertion here is about the database or about a file on disk.
// The CLI's stdout is checked only where the output is the deliverable
// — a refusal has to name what it refused — because a test that greps
// the tool's own report proves the report, not the migration.
//
// Each test gets a database of its own. `push` and `drift` compare a
// whole schema, so a table another test left behind is a difference
// they are right to report and this suite would be wrong to see.

// ----------------------------------------------------------------------
// Harness
// ----------------------------------------------------------------------

var (
	buildOnce sync.Once
	builtCLI  string
	buildDir  string
	buildErr  error
)

// TestMain exists to take the CLI's build directory away again. It is
// eighteen megabytes of linked binary per run of this suite, and
// os.MkdirTemp does not clean up after itself — a few weeks of CI on
// one machine filled a disk that way, which fails every test here for
// a reason none of them mention.
func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// cliBinary builds cmd/drops once per test run and returns its path.
func cliBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "drops-cli-")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		builtCLI = filepath.Join(dir, "drops")
		// cmd/drops is a module of its own — the root ./... does not
		// reach it, and it has to be built from inside its own
		// directory or its go.mod (and its pgx) are not in scope.
		cmd := osexec.Command("go", "build", "-o", builtCLI, ".")
		cmd.Dir = filepath.Join(repoRoot(t), "cmd", "drops")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/drops: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building the CLI: %v", buildErr)
	}
	return builtCLI
}

// repoRoot is the drops module's directory — the integration module
// sits one level inside it.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// project is a throwaway Go module holding a schema package, which is
// what the CLI expects to be pointed at.
type project struct {
	dir string
	dsn string
	t   *testing.T
}

// newProject creates the module, replacing drops with this checkout so
// the generated program compiles against the code under test.
//
// It requires pgx as well, because `drops push` compiles a program
// *inside this module* and that program opens the connection: the CLI
// carries its own driver, but `go run` resolves the generated
// program's imports here. newBareProject is the same module without
// it, which is what most people's modules look like.
func newProject(t *testing.T, dsn string) *project {
	t.Helper()
	return makeProject(t, dsn, true)
}

func newBareProject(t *testing.T, dsn string) *project {
	t.Helper()
	return makeProject(t, dsn, false)
}

func makeProject(t *testing.T, dsn string, withDriver bool) *project {
	t.Helper()
	dir := t.TempDir()
	if !withDriver {
		writeFile(t, filepath.Join(dir, "go.mod"), fmt.Sprintf(`module dropscli.test

go 1.25.0

require github.com/bernardoforcillo/drops v0.0.0

replace github.com/bernardoforcillo/drops => %s
`, repoRoot(t)))
		return &project{dir: dir, dsn: dsn, t: t}
	}
	// The generated program is built in readonly mode like any other,
	// so pgx has to be verifiable without a network, and the go.sum
	// that verifies it is the CLI's. A go.sum only covers the versions
	// its own module selects, though, and the CLI's graph is wider than
	// a bare drops+pgx one — the linter brings golang.org/x/tools in,
	// which raises golang.org/x/sync above what pgx alone asks for. So
	// this module borrows the CLI's requirements as well as its sums:
	// the two resolve to the same versions because they are the same
	// graph under a different name.
	mod, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "drops", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.mod"), rehomeModule(t, string(mod), repoRoot(t)))
	sum, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "drops", "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "go.sum"), string(sum))
	return &project{dir: dir, dsn: dsn, t: t}
}

// rehomeModule renames the CLI's module to the throwaway one and points
// its relative replace at the checkout, leaving every requirement
// alone. Only those two lines change: anything else would let the two
// module graphs drift apart again, which is the whole reason the go.sum
// is shared.
func rehomeModule(t *testing.T, mod, root string) string {
	t.Helper()
	lines := strings.Split(mod, "\n")
	renamed, rehomed := false, false
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "module "):
			lines[i] = "module dropscli.test"
			renamed = true
		case strings.HasPrefix(line, "replace github.com/bernardoforcillo/drops =>"):
			lines[i] = "replace github.com/bernardoforcillo/drops => " + root
			rehomed = true
		}
	}
	if !renamed || !rehomed {
		t.Fatalf("cmd/drops/go.mod no longer has the module and replace lines this rewrite expects:\n%s", mod)
	}
	return strings.Join(lines, "\n")
}

// schema writes the schema package, replacing whatever was there.
func (p *project) schema(body string) {
	p.t.Helper()
	dir := filepath.Join(p.dir, "schema")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		p.t.Fatal(err)
	}
	writeFile(p.t, filepath.Join(dir, "schema.go"), body)
}

// run invokes the CLI in the project directory and returns its output
// and exit code.
func (p *project) run(args ...string) (stdout, stderr string, code int) {
	p.t.Helper()
	cmd := osexec.Command(cliBinary(p.t), args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "DROPS_PG_DSN="+p.dsn)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if exit, ok := err.(*osexec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		p.t.Fatalf("running drops %s: %v", strings.Join(args, " "), err)
	}
	p.t.Logf("drops %s → exit %d\n%s%s", strings.Join(args, " "), code, out.String(), errb.String())
	return out.String(), errb.String(), code
}

// mustRun fails the test when the CLI does not succeed.
func (p *project) mustRun(args ...string) string {
	p.t.Helper()
	stdout, stderr, code := p.run(args...)
	if code != 0 {
		p.t.Fatalf("drops %s exited %d\n%s%s", strings.Join(args, " "), code, stdout, stderr)
	}
	return stdout
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// freshDatabase creates an empty database for one test and drops it
// afterwards, returning its connection string.
//
// The name carries a random suffix as well as the test's name. Two
// runs of this suite against one server would otherwise pick the same
// name, and the second one's setup would drop the database the first
// one is in the middle of migrating.
func freshDatabase(t *testing.T) string {
	t.Helper()
	base := integration.DSN(t, integration.EnvPostgres)
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := strings.ToLower(integration.UniqueName(t, "cli"))
	if len(name) > 40 {
		name = name[len(name)-40:]
	}
	name = fmt.Sprintf("%s_%x", name, suffix)
	admin := openDB(t, base)
	ctx := context.Background()
	// CREATE DATABASE cannot run inside a transaction block, which is
	// also a check that nothing in this stack quietly wraps a
	// statement in one.
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+quote(name)); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+quote(name)+` WITH (FORCE)`); err != nil {
			t.Logf("dropping %s: %v", name, err)
		}
	})
	return replaceDatabase(t, base, name)
}

// replaceDatabase points a connection string at another database.
func replaceDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		return u.String()
	}
	return dsn + " dbname=" + name
}

func quote(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

// openDB connects the way the binary does: pgx behind drops/stdlib.
// Every assertion in this file is read back through it, so a pairing
// that cannot read the database says so here rather than in one
// confusing test.
func openDB(t *testing.T, dsn string) *pg.DB {
	t.Helper()
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open %s: %v", dsn, err)
	}
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("connect to %s: %v", dsn, err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return pg.New(stdlib.New(sqlDB))
}

// columnsOf reads a table's columns straight from the catalogue.
func columnsOf(t *testing.T, db *pg.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT column_name, data_type FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1`, table)
	if err != nil {
		t.Fatalf("read columns of %s: %v", table, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		out[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func tableExists(t *testing.T, db *pg.DB, table string) bool {
	t.Helper()
	return len(columnsOf(t, db, table)) > 0
}

func scalar(t *testing.T, db *pg.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("%s returned no rows", query)
	}
	var v string
	if err := rows.Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// ----------------------------------------------------------------------
// The connection the binary opens
// ----------------------------------------------------------------------

// writeMigrationSet writes a one-entry drizzle journal and its SQL,
// which is all `drops migrate` reads.
func writeMigrationSet(t *testing.T, p *project, tag, body string) {
	t.Helper()
	dir := filepath.Join(p.dir, "drizzle")
	if err := os.MkdirAll(filepath.Join(dir, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, tag+".sql"), body)
	writeFile(t, filepath.Join(dir, "meta", "_journal.json"), fmt.Sprintf(`{
  "version": "7",
  "dialect": "postgresql",
  "entries": [
    {"idx": 0, "version": "7", "when": 1700000000000, "tag": %q, "breakpoints": true}
  ]
}`, tag))
}

// Which constraint fired is the part of a violation an operator acts
// on: a table has several uniques, and "duplicate key" alone does not
// say which one to go and look at.
//
// The wire client this replaced carried the name through a
// ConstraintName method. pgx carries it as a struct field instead, and
// drops/pg reads that by reflection — so the port needs no adapter
// between the two, and this is the test that says so rather than
// assuming it. It asserts on the classified form,
// "SQLSTATE 23505 (people_email_uq)", because the server's own message
// carries the name as well: an assertion that only looked for the name
// would pass with the classification gone entirely.
func TestCLIFailedMigrationNamesTheConstraint(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	ctx := context.Background()
	if _, err := db.Exec(ctx, `CREATE TABLE people (email text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO people VALUES ('a@b'), ('a@b')`); err != nil {
		t.Fatal(err)
	}

	p := newBareProject(t, dsn)
	writeMigrationSet(t, p, "0000_unique_email",
		`ALTER TABLE "people" ADD CONSTRAINT "people_email_uq" UNIQUE ("email");`)

	stdout, stderr, code := p.run("migrate")
	if code != 1 {
		t.Fatalf("migrate exited %d, want 1\n%s%s", code, stdout, stderr)
	}
	out := stdout + stderr
	for _, want := range []string{"23505", "(people_email_uq)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not report %s — the constraint name did not survive the driver:\n%s", want, out)
		}
	}
}

// A migration is one transaction. If its second statement is rejected
// the first must not survive, or a failed migrate leaves a database no
// re-run can fix.
func TestCLIFailedMigrationRollsBack(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newBareProject(t, dsn)
	writeMigrationSet(t, p, "0000_half_applied", `CREATE TABLE "half_applied" ("id" bigint);
--> statement-breakpoint
CREATE TABLE "half_applied" ("id" bigint);
`)
	stdout, stderr, code := p.run("migrate")
	if code != 1 {
		t.Fatalf("migrate exited %d, want 1\n%s%s", code, stdout, stderr)
	}
	if tableExists(t, db, "half_applied") {
		t.Fatal("the table created before the failing statement is still there: the migration did not roll back")
	}
}

// Ctrl-C has to reach the server. The CLI cancels its context, which
// cancels the statement and rolls back the transaction around it — an
// operator who interrupts a migration and is left with half of it
// applied has been failed twice.
func TestCLIInterruptRollsBackTheMigrationInFlight(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newBareProject(t, dsn)
	writeMigrationSet(t, p, "0000_slow", `CREATE TABLE "interrupted" ("id" bigint);
--> statement-breakpoint
SELECT pg_sleep(60);
`)
	cmd := osexec.Command(cliBinary(t), "migrate")
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "DROPS_PG_DSN="+p.dsn)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the CLI: %v", err)
	}
	// Long enough for the process to have reached pg_sleep, short
	// enough that the test is not the slow part of the suite.
	time.Sleep(3 * time.Second)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupting the CLI: %v", err)
	}
	start := time.Now()
	err := cmd.Wait()
	if err == nil {
		t.Fatalf("the interrupted migration reported success:\n%s", out.String())
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the CLI took %s to give up after Ctrl-C:\n%s", elapsed, out.String())
	}
	// The rollback on its own proves nothing about this binary. A
	// process that takes the default disposition for SIGINT dies
	// where it stands, its socket closes, and the server rolls the
	// open transaction back without being asked — so the table is
	// gone either way. What says the signal was *handled* is that the
	// process chose its own exit: code 1, the documented failure
	// code, and a line saying why.
	exit, ok := err.(*osexec.ExitError)
	if !ok || !exit.Exited() {
		t.Fatalf("the CLI was killed by the signal instead of handling it (%v): Ctrl-C never reached the migration\n%s", err, out.String())
	}
	if exit.ExitCode() != 1 {
		t.Fatalf("the interrupted migration exited %d, want 1\n%s", exit.ExitCode(), out.String())
	}
	if !strings.Contains(out.String(), "drops: interrupted") {
		t.Errorf("the CLI gave no sign it had been interrupted:\n%s", out.String())
	}
	if tableExists(t, db, "interrupted") {
		t.Fatal("the interrupted migration left its first statement applied")
	}
}

// push is the one command whose work happens inside the user's module,
// so it is the one command that needs a driver there. A module without
// one has to be told which, and how — the alternative is a compiler
// error inside a file the user never wrote.
func TestCLIPushWithoutADriverSaysWhatToInstall(t *testing.T) {
	dsn := freshDatabase(t)
	p := newBareProject(t, dsn)
	p.schema(schemaV1)

	stdout, stderr, code := p.run("push", "--schema", "./schema")
	if code != 1 {
		t.Fatalf("push exited %d, want 1\n%s%s", code, stdout, stderr)
	}
	// The remedy alone is not enough to assert on: when the preflight
	// is gone the compiler names the same `go get` from inside the
	// generated file, so a test looking only for that passes while
	// the user reads a build error about a path in a temporary
	// directory they have never seen. The preflight's own sentence,
	// and the absence of that directory, are what separate the two.
	out := stdout + stderr
	for _, want := range []string{
		"push needs a PostgreSQL driver in the module that declares the schema",
		"go get github.com/jackc/pgx/v5",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the failure does not mention %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".drops-bridge-") {
		t.Errorf("the failure is a compiler error inside the generated program, not the preflight:\n%s", out)
	}
	// generate, which compiles the same program without the
	// connection, still works in that module.
	p.mustRun("generate", "--schema", "./schema", "--name", "init")
}

// ----------------------------------------------------------------------
// generate → migrate → down
// ----------------------------------------------------------------------

const schemaV1 = `package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`

const schemaV2 = `package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull().Unique())
	UserAge   = pg.Add(Users, pg.Integer("age"))
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`

func TestCLIGenerateThenMigrate(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(schemaV1)

	p.mustRun("generate", "--schema", "./schema", "--name", "init")
	for _, name := range []string{
		"drizzle/0000_init.sql",
		"drizzle/0000_init.down.sql",
		"drizzle/meta/0000_snapshot.json",
		"drizzle/meta/_journal.json",
	} {
		if _, err := os.Stat(filepath.Join(p.dir, name)); err != nil {
			t.Fatalf("generate did not write %s: %v", name, err)
		}
	}
	if tableExists(t, db, "users") {
		t.Fatal("generate touched the database")
	}

	p.mustRun("migrate")
	cols := columnsOf(t, db, "users")
	if len(cols) != 2 || cols["id"] != "bigint" || cols["email"] != "text" {
		t.Fatalf("users after migrate = %v", cols)
	}

	// A second run is a no-op, which is the property the hash-based
	// history exists to provide.
	p.mustRun("migrate")
	if got := scalar(t, db, `SELECT count(*)::text FROM drizzle.__drizzle_migrations`); got != "1" {
		t.Fatalf("migration history holds %s rows after two runs", got)
	}

	// Evolve the schema and do it again.
	p.schema(schemaV2)
	p.mustRun("generate", "--schema", "./schema", "--name", "add_age")
	p.mustRun("migrate")
	if cols := columnsOf(t, db, "users"); cols["age"] != "integer" {
		t.Fatalf("users after the second migration = %v", cols)
	}
}

func TestCLIMigrateDownNeedsPermissionAndThenWorks(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(schemaV1)
	p.mustRun("generate", "--schema", "./schema", "--name", "init")
	p.schema(schemaV2)
	p.mustRun("generate", "--schema", "./schema", "--name", "add_age")
	p.mustRun("migrate")

	stdout, _, code := p.run("migrate", "down")
	if code != 3 {
		t.Fatalf("migrate down exited %d, want 3 (refused)", code)
	}
	if !strings.Contains(stdout, `DROP COLUMN "age"`) {
		t.Errorf("the refusal does not name the statement it refused:\n%s", stdout)
	}
	if cols := columnsOf(t, db, "users"); cols["age"] == "" {
		t.Fatal("the refused rollback dropped the column anyway")
	}

	p.mustRun("migrate", "down", "--allow-destructive")
	if cols := columnsOf(t, db, "users"); cols["age"] != "" {
		t.Fatalf("age survived the rollback: %v", cols)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM drizzle.__drizzle_migrations`); got != "1" {
		t.Fatalf("history holds %s rows after rolling one back", got)
	}
	// And the rolled-back migration is pending again, not lost.
	p.mustRun("migrate")
	if cols := columnsOf(t, db, "users"); cols["age"] != "integer" {
		t.Fatalf("re-applying the rolled-back migration left %v", cols)
	}
}

// ----------------------------------------------------------------------
// push
// ----------------------------------------------------------------------

func TestCLIPushAppliesAndRefusesDestruction(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(`package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull())

	Posts       = pg.NewTable("posts")
	PostID      = pg.Add(Posts, pg.BigSerial("id").PrimaryKey())
	PostUserID  = pg.Add(Posts, pg.BigInt("user_id").NotNull().References(UserID, pg.OnDelete("CASCADE")))
	PostTitle   = pg.Add(Posts, pg.Text("title").NotNull())
)

func Schema() *pg.Schema { return pg.NewSchema(Users, Posts) }
`)

	// --dry-run says what it would do and does none of it.
	p.mustRun("push", "--schema", "./schema", "--dry-run")
	if tableExists(t, db, "users") {
		t.Fatal("--dry-run created a table")
	}

	p.mustRun("push", "--schema", "./schema")
	if !tableExists(t, db, "users") || !tableExists(t, db, "posts") {
		t.Fatal("push did not create the tables")
	}
	// The foreign key reached the catalogue, not just the SQL.
	if got := scalar(t, db, `SELECT count(*)::text FROM pg_constraint
		WHERE contype = 'f' AND conrelid = 'posts'::regclass`); got != "1" {
		t.Fatalf("posts carries %s foreign keys", got)
	}

	// Pushing again finds nothing to do: the diff has to be stable
	// against a database it just created, or push churns for ever.
	stdout := p.mustRun("push", "--schema", "./schema")
	if !strings.Contains(stdout, "no changes") {
		t.Fatalf("a second push wanted to change something:\n%s", stdout)
	}

	// Dropping a table from the schema is a destructive push.
	p.schema(`package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("email").NotNull())
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`)
	stdout, _, code := p.run("push", "--schema", "./schema")
	if code != 3 {
		t.Fatalf("push exited %d, want 3 (refused)", code)
	}
	if !strings.Contains(stdout, `DROP TABLE "posts"`) {
		t.Errorf("the refusal does not name the table it would drop:\n%s", stdout)
	}
	if !tableExists(t, db, "posts") {
		t.Fatal("the refused push dropped the table anyway")
	}

	p.mustRun("push", "--schema", "./schema", "--allow-destructive")
	if tableExists(t, db, "posts") {
		t.Fatal("posts survived a push that was allowed to drop it")
	}
}

// ----------------------------------------------------------------------
// drift
// ----------------------------------------------------------------------

func TestCLIDriftSeesAManualChange(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(schemaV1)
	p.mustRun("push", "--schema", "./schema")

	if _, _, code := p.run("drift", "--schema", "./schema"); code != 0 {
		t.Fatalf("drift reported a difference right after a push (exit %d)", code)
	}

	// Somebody adds a column by hand, the way it actually happens.
	if _, err := db.Exec(context.Background(), `ALTER TABLE users ADD COLUMN hotfix text`); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := p.run("drift", "--schema", "./schema")
	if code != 3 {
		t.Fatalf("drift exited %d after a manual change, want 3", code)
	}
	if !strings.Contains(stdout, "hotfix") {
		t.Errorf("drift does not name the column that appeared:\n%s", stdout)
	}
}

// ----------------------------------------------------------------------
// baseline and pull
// ----------------------------------------------------------------------

// legacyDDL is a database that predates drops, with rows in it.
const legacyDDL = `
CREATE TABLE legacy_authors (
	id bigserial PRIMARY KEY,
	email text NOT NULL UNIQUE,
	rank integer NOT NULL DEFAULT 0
);
CREATE TABLE legacy_books (
	id bigserial PRIMARY KEY,
	author_id bigint NOT NULL REFERENCES legacy_authors(id) ON DELETE CASCADE,
	title text NOT NULL
);
CREATE INDEX legacy_books_author_idx ON legacy_books (author_id);
INSERT INTO legacy_authors (email) VALUES ('someone@example.com');
`

func TestCLIBaselineAdoptsALiveDatabase(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	if _, err := db.Exec(context.Background(), legacyDDL); err != nil {
		t.Fatal(err)
	}
	p := newProject(t, dsn)

	p.mustRun("baseline")
	if _, err := os.Stat(filepath.Join(p.dir, "drizzle", "meta", "0000_snapshot.json")); err != nil {
		t.Fatalf("baseline wrote no snapshot: %v", err)
	}
	// The migration it wrote is recorded as applied, so migrating does
	// not try to create tables that are already there.
	p.mustRun("migrate")
	if got := scalar(t, db, `SELECT count(*)::text FROM legacy_authors`); got != "1" {
		t.Fatalf("the adopted table holds %s rows; baseline was not a no-op", got)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM drizzle.__drizzle_migrations`); got != "1" {
		t.Fatalf("history holds %s rows after baseline + migrate", got)
	}

	// Baselining a directory that already has a history is refused.
	if _, _, code := p.run("baseline"); code == 0 {
		t.Fatal("baseline overwrote an existing history")
	}
}

// The proof that a pulled schema is right is not that it looks right:
// it is that pushing it back changes nothing but the names drops
// derives for itself, and that drift then finds nothing — which also
// proves the file compiles.
func TestCLIPullRoundTrips(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	if _, err := db.Exec(context.Background(), legacyDDL); err != nil {
		t.Fatal(err)
	}
	p := newProject(t, dsn)
	if err := os.MkdirAll(filepath.Join(p.dir, "pulled"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.mustRun("pull", "--out", filepath.Join(p.dir, "pulled", "schema.go"), "--package", "pulled")

	// The one thing a pulled schema cannot carry is a foreign key's
	// name: drops derives that, so pushing renames the constraint
	// PostgreSQL named. Everything else has to survive untouched, which
	// is what the columns and the row below check.
	p.mustRun("push", "--schema", "./pulled", "--allow-destructive")

	authors := columnsOf(t, db, "legacy_authors")
	if authors["id"] != "bigint" || authors["email"] != "text" || authors["rank"] != "integer" {
		t.Fatalf("legacy_authors after pushing the pulled schema = %v", authors)
	}
	books := columnsOf(t, db, "legacy_books")
	if books["id"] != "bigint" || books["author_id"] != "bigint" || books["title"] != "text" {
		t.Fatalf("legacy_books after pushing the pulled schema = %v", books)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM legacy_authors`); got != "1" {
		t.Fatalf("the row that was there before the round trip is gone (%s left)", got)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'legacy_books_author_idx'`); got != "1" {
		t.Fatal("the index did not survive the round trip")
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM pg_constraint
		WHERE contype = 'f' AND conrelid = 'legacy_books'::regclass`); got != "1" {
		t.Fatalf("legacy_books carries %s foreign keys after the round trip", got)
	}
	if got := scalar(t, db, `SELECT count(*)::text FROM pg_constraint
		WHERE contype = 'u' AND conrelid = 'legacy_authors'::regclass`); got != "1" {
		t.Fatalf("legacy_authors carries %s unique constraints after the round trip", got)
	}

	if stdout, stderr, code := p.run("drift", "--schema", "./pulled"); code != 0 {
		t.Fatalf("drift after pushing the pulled schema exited %d\n%s%s", code, stdout, stderr)
	}
}

// pg.Diff writes no schema qualifier — a table declared for one
// schema is emitted as CREATE TABLE "users" and lands wherever
// search_path points. push introspects the schema it was given and
// then applies SQL that ignores it, so `--pg-schema reporting` used to
// report four statements applied and put every table in public, and
// the next run wedged on "relation already exists". The CLI refuses
// the combination it cannot honour rather than misplacing the tables.
func TestCLIRefusesToPushIntoANonPublicSchema(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(schemaV1)

	if _, err := db.Exec(context.Background(), `CREATE SCHEMA reporting`); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := p.run("push", "--schema", "./schema", "--pg-schema", "reporting")
	if code != 2 {
		t.Fatalf("push --pg-schema reporting exited %d, want 2\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "public") {
		t.Errorf("the refusal does not name the schema drops can write to:\n%s", stderr)
	}
	if tableExists(t, db, "users") {
		t.Fatal("push put the table in public after being asked for another schema")
	}
}

// ----------------------------------------------------------------------
// drizzle-kit interoperability
// ----------------------------------------------------------------------

// A migration set exactly as drizzle-kit writes one: a journal, a .sql
// file per entry with statement breakpoints, and no down direction.
func TestCLIAppliesADrizzleKitMigrationSet(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)

	const first = `CREATE TABLE "kit_users" (
	"id" bigserial PRIMARY KEY NOT NULL,
	"email" text NOT NULL
);
--> statement-breakpoint
CREATE UNIQUE INDEX "kit_users_email_idx" ON "kit_users" ("email");
`
	const second = `ALTER TABLE "kit_users" ADD COLUMN "nickname" text;`

	dir := filepath.Join(p.dir, "drizzle")
	if err := os.MkdirAll(filepath.Join(dir, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "0000_kit_init.sql"), first)
	writeFile(t, filepath.Join(dir, "0001_kit_nickname.sql"), second)
	writeFile(t, filepath.Join(dir, "meta", "_journal.json"), `{
  "version": "7",
  "dialect": "postgresql",
  "entries": [
    {"idx": 0, "version": "7", "when": 1700000000000, "tag": "0000_kit_init", "breakpoints": true},
    {"idx": 1, "version": "7", "when": 1700000001000, "tag": "0001_kit_nickname", "breakpoints": true}
  ]
}`)

	p.mustRun("migrate")
	cols := columnsOf(t, db, "kit_users")
	if cols["id"] != "bigint" || cols["email"] != "text" || cols["nickname"] != "text" {
		t.Fatalf("kit_users after applying drizzle-kit's migrations = %v", cols)
	}

	// drizzle-orm identifies a migration by the SHA-256 of the file.
	// Recording anything else would make drizzle-orm re-run the file
	// against tables that already exist.
	sum := sha256.Sum256([]byte(first))
	if got := scalar(t, db, `SELECT count(*)::text FROM drizzle.__drizzle_migrations WHERE hash = $1`,
		hex.EncodeToString(sum[:])); got != "1" {
		t.Fatalf("the history has no row for the file's own hash (found %s)", got)
	}

	// Re-running applies nothing: the hashes match.
	p.mustRun("migrate")
	if got := scalar(t, db, `SELECT count(*)::text FROM drizzle.__drizzle_migrations`); got != "2" {
		t.Fatalf("history holds %s rows after two migrate runs", got)
	}

	// And `drops status` reads that same history back.
	stdout := p.mustRun("status")
	if !strings.Contains(stdout, "2 applied, 0 pending") {
		t.Errorf("status does not agree with the database:\n%s", stdout)
	}
}

// The gate has to hold for a set drops did not write.
//
// drops emits `ALTER TABLE ... DROP COLUMN` on one line, so a gate
// that only recognises it there passes every test drops generates and
// still loses a column the first time it is pointed at a migration
// somebody else wrapped — which is the whole point of applying
// drizzle-kit's sets.
func TestCLIRefusesAWrappedDropColumn(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)

	dir := filepath.Join(p.dir, "drizzle")
	if err := os.MkdirAll(filepath.Join(dir, "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "0000_wrap_init.sql"), `CREATE TABLE "wrapped" (
	"id" bigserial PRIMARY KEY NOT NULL,
	"payload" text NOT NULL
);
`)
	writeFile(t, filepath.Join(dir, "0001_wrap_drop.sql"), "ALTER TABLE \"wrapped\"\n\tDROP COLUMN \"payload\";\n")
	writeFile(t, filepath.Join(dir, "meta", "_journal.json"), `{
  "version": "7",
  "dialect": "postgresql",
  "entries": [
    {"idx": 0, "version": "7", "when": 1700000000000, "tag": "0000_wrap_init", "breakpoints": true},
    {"idx": 1, "version": "7", "when": 1700000001000, "tag": "0001_wrap_drop", "breakpoints": true}
  ]
}`)

	stdout, stderr, code := p.run("migrate")
	if code != 3 {
		t.Fatalf("migrate exited %d, want 3 — the wrapped DROP COLUMN was not refused\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "drop-column") {
		t.Errorf("the refusal does not name the rule it fired:\n%s", stdout)
	}
	if tableExists(t, db, "wrapped") {
		t.Fatal("migrate applied the whole set before refusing anything")
	}

	// And with the flag, the same set applies: the gate is a gate, not
	// a statement the CLI cannot run.
	p.mustRun("migrate", "--allow-destructive")
	if cols := columnsOf(t, db, "wrapped"); len(cols) != 1 || cols["id"] != "bigint" {
		t.Fatalf("wrapped after the allowed run = %v", cols)
	}
}

// ----------------------------------------------------------------------
// The shipped example, and the failure modes
// ----------------------------------------------------------------------

// examples/cli is the documented schema package. If it stops
// satisfying the convention, or stops evaluating to a schema, every
// reader who copies it is stuck.
//
// It is reached through a package that re-exports it because the CLI
// writes its generated program into the module that owns the schema,
// and this suite writes nothing into the drops checkout.
func TestCLIRunsAgainstTheShippedExample(t *testing.T) {
	p := newProject(t, "")
	p.schema(`package schema

import (
	example "github.com/bernardoforcillo/drops/examples/cli"
	"github.com/bernardoforcillo/drops/pg"
)

func Schema() *pg.Schema { return example.Schema() }
`)
	p.mustRun("generate", "--schema", "./schema", "--name", "example")

	sql, err := os.ReadFile(filepath.Join(p.dir, "drizzle", "0000_example.sql"))
	if err != nil {
		t.Fatalf("no migration was written: %v", err)
	}
	for _, want := range []string{
		`CREATE TABLE "cli_authors"`,
		`CREATE TABLE "cli_articles"`,
		"FOREIGN KEY",
		`CHECK (word_count >= 0)`,
		`CREATE INDEX "cli_articles_author_idx"`,
	} {
		if !strings.Contains(string(sql), want) {
			t.Errorf("the example's migration is missing %s:\n%s", want, sql)
		}
	}
}

func TestCLIFailureModes(t *testing.T) {
	dsn := freshDatabase(t)
	p := newProject(t, dsn)
	p.schema(schemaV1)

	cases := []struct {
		name string
		args []string
		want int
		says string
	}{
		{
			name: "unreachable database",
			args: []string{"push", "--schema", "./schema", "--dsn", "postgres://drops@127.0.0.1:1/none?sslmode=disable"},
			want: 1,
			says: "connect",
		},
		{
			name: "no database named",
			args: []string{"status", "--dsn", ""},
			want: 2,
			says: "DROPS_PG_DSN",
		},
		{
			name: "unknown flag",
			args: []string{"push", "--wat"},
			want: 2,
		},
		{
			name: "unknown subcommand",
			args: []string{"frobnicate"},
			want: 2,
		},
		{
			name: "missing schema package",
			args: []string{"generate", "--schema", "./not-a-package"},
			want: 1,
			says: "go list",
		},
		{
			// A required flag left off is the command line being
			// wrong, the same as leaving off --dsn, and a script that
			// tells the two apart by exit code has to be able to.
			name: "no schema package named",
			args: []string{"generate"},
			want: 2,
			says: "--schema is required",
		},
		{
			name: "no migration directory",
			args: []string{"migrate", "--dir", "nowhere"},
			want: 1,
			says: "drops generate",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := p.dsn
			if c.name == "no database named" {
				env = ""
			}
			cmd := osexec.Command(cliBinary(t), c.args...)
			cmd.Dir = p.dir
			cmd.Env = append(os.Environ(), "DROPS_PG_DSN="+env, "DATABASE_URL=")
			out, err := cmd.CombinedOutput()
			code := 0
			if exit, ok := err.(*osexec.ExitError); ok {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("running the CLI: %v", err)
			}
			if code != c.want {
				t.Errorf("exit %d, want %d\n%s", code, c.want, out)
			}
			if c.says != "" && !strings.Contains(string(out), c.says) {
				t.Errorf("the message does not mention %q:\n%s", c.says, out)
			}
			if strings.Contains(string(out), "panic:") {
				t.Errorf("the CLI panicked:\n%s", out)
			}
		})
	}
}

// A cancelled context has to reach the server, or Ctrl-C during a long
// migration would leave the operator watching a process that has
// stopped listening.
//
// The lever is drops/stdlib handing the context to database/sql's
// ExecContext, which pgx turns into a cancellation request on the
// connection. An adapter that dropped the context would still return
// eventually — after pg_sleep finished — which is what this test's
// deadline is for.
func TestCancellationReachesTheServer(t *testing.T) {
	db := openDB(t, integration.DSN(t, integration.EnvPostgres))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := db.Exec(ctx, `SELECT pg_sleep(30)`)
	if err == nil {
		t.Fatal("a cancelled query returned successfully")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the cancelled query took %s to give up", elapsed)
	}
	// The error has to say what actually happened. An operator who
	// pressed Ctrl-C and is handed a network error goes and looks at
	// the network.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled query reported %v, which is not a cancellation", err)
	}
}

// ----------------------------------------------------------------------
// generate and renames
// ----------------------------------------------------------------------

// schemaRenamed is schemaV1 with one column under a different name —
// the change a structural diff cannot tell from a drop and an add.
const schemaRenamed = `package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	Users     = pg.NewTable("users")
	UserID    = pg.Add(Users, pg.BigSerial("id").PrimaryKey())
	UserEmail = pg.Add(Users, pg.Text("emailAddress").NotNull().Unique())
)

func Schema() *pg.Schema { return pg.NewSchema(Users) }
`

// The whole loop, through the CLI and the program it compiles: a rename
// stops a run nobody can answer, the answer on the command line lets it
// through, the answer is recorded, and the row that was in the column
// is still there under the new name afterwards.
//
// The CLI runs here with its stdin attached to a pipe, which is the
// case that matters: no terminal, so nothing to ask, so the only two
// options are to stop or to guess.
func TestCLIGenerateRefusesARenameAndThenTakesTheAnswer(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(schemaV1)
	p.mustRun("generate", "--schema", "./schema", "--name", "init")
	applyGeneratedSQL(t, db, filepath.Join(p.dir, "drizzle", "0000_init.sql"))

	if _, err := db.Exec(context.Background(),
		`INSERT INTO users (email) VALUES ($1)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	p.schema(schemaRenamed)
	stdout, stderr, code := p.run("generate", "--schema", "./schema", "--name", "rename")
	if code != 3 {
		t.Fatalf("generate exited %d, want 3 (refused)\n%s%s", code, stdout, stderr)
	}
	out := stdout + stderr
	for _, want := range []string{
		"could be a rename or a drop-and-add",
		"--rename-column users.email=emailAddress",
		"--drop-column users.email",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(p.dir, "drizzle", "0001_*.sql")); len(matches) > 0 {
		t.Fatalf("a refused generate wrote %v", matches)
	}

	p.mustRun("generate", "--schema", "./schema", "--name", "rename",
		"--rename-column", "users.email=emailAddress")
	body, err := os.ReadFile(filepath.Join(p.dir, "drizzle", "0001_rename.sql"))
	if err != nil {
		t.Fatalf("generate did not write the migration: %v", err)
	}
	if !strings.Contains(string(body), `RENAME COLUMN "email" TO "emailAddress"`) {
		t.Fatalf("migration:\n%s", body)
	}
	log, err := os.ReadFile(filepath.Join(p.dir, "drizzle", "meta", "_renames.json"))
	if err != nil {
		t.Fatalf("the answer was not recorded: %v", err)
	}
	if !strings.Contains(string(log), `"emailAddress"`) {
		t.Fatalf("meta/_renames.json:\n%s", log)
	}

	applyGeneratedSQL(t, db, filepath.Join(p.dir, "drizzle", "0001_rename.sql"))
	if cols := columnsOf(t, db, "users"); cols["emailAddress"] != "text" || cols["email"] != "" {
		t.Fatalf("users after the rename = %v", cols)
	}
	if got := scalar(t, db, `SELECT "emailAddress" FROM users`); got != "ada@example.com" {
		t.Fatalf("the row did not survive the rename: emailAddress = %q", got)
	}

	// And the recorded answer means the same run, with no flags at all,
	// now has nothing to ask about and nothing to do.
	if out := p.mustRun("generate", "--schema", "./schema", "--name", "again"); !strings.Contains(out, "no changes") {
		t.Errorf("a settled schema still generated something:\n%s", out)
	}
}

// The other way to answer: --interactive, with the answers on stdin.
// A prompt drops decided to show on its own would be one a scripted run
// could receive by accident, so the flag is the whole of what turns it
// on.
func TestCLIGenerateAsksWhenToldTo(t *testing.T) {
	dsn := freshDatabase(t)
	p := newProject(t, dsn)
	p.schema(schemaV1)
	p.mustRun("generate", "--schema", "./schema", "--name", "init")

	p.schema(schemaRenamed)
	stdout, stderr, code := p.runWithStdin("y\n",
		"generate", "--schema", "./schema", "--name", "rename", "--interactive")
	if code != 0 {
		t.Fatalf("generate exited %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "is users.emailAddress a rename of users.email?") {
		t.Errorf("no question was asked:\n%s", stdout)
	}
	body, err := os.ReadFile(filepath.Join(p.dir, "drizzle", "0001_rename.sql"))
	if err != nil {
		t.Fatalf("generate did not write the migration: %v", err)
	}
	if !strings.Contains(string(body), `RENAME COLUMN "email" TO "emailAddress"`) {
		t.Fatalf("migration:\n%s", body)
	}
	if _, err := os.ReadFile(filepath.Join(p.dir, "drizzle", "meta", "_renames.json")); err != nil {
		t.Fatalf("the prompted answer was not recorded: %v", err)
	}
}

// runWithStdin is run with something for the command to read, which is
// what the prompt needs and nothing else in this file does.
func (p *project) runWithStdin(input string, args ...string) (stdout, stderr string, code int) {
	p.t.Helper()
	cmd := osexec.Command(cliBinary(p.t), args...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "DROPS_PG_DSN="+p.dsn)
	cmd.Stdin = strings.NewReader(input)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if exit, ok := err.(*osexec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		p.t.Fatalf("running drops %s: %v", strings.Join(args, " "), err)
	}
	p.t.Logf("drops %s → exit %d\n%s%s", strings.Join(args, " "), code, out.String(), errb.String())
	return out.String(), errb.String(), code
}

// applyGeneratedSQL runs a generated migration file statement by
// statement.
//
// "drops migrate" would do the same thing and is covered by
// TestCLIGenerateThenMigrate; what the rename test is about is what
// generate wrote, and running that is the only way to find out whether
// the row is still in the column afterwards.
func applyGeneratedSQL(t *testing.T, db *pg.DB, path string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, stmt := range strings.Split(string(body), pg.StatementBreakpoint) {
		if stmt = strings.TrimSpace(stmt); stmt == "" {
			continue
		}
		if _, err := db.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("PostgreSQL rejected %q: %v", stmt, err)
		}
	}
}

// A table renamed and a column inside it renamed in the same commit.
const schemaTableAndColumnRenamed = `package schema

import "github.com/bernardoforcillo/drops/pg"

var (
	People      = pg.NewTable("people")
	PersonID    = pg.Add(People, pg.BigSerial("id").PrimaryKey())
	PersonEmail = pg.Add(People, pg.Text("emailAddress").NotNull().Unique())
)

func Schema() *pg.Schema { return pg.NewSchema(People) }
`

// Renaming a table and a column inside it, through the CLI, with a row
// in the table.
//
// The two are asked in turn — the column inside a renamed table is not
// a question anyone can put until the table has an answer — so this is
// also the test that the prompt keeps going, and that a non-interactive
// run holding only half the answers still refuses rather than emitting
// the DROP TABLE.
func TestCLIGenerateRenamesATableAndAColumnInsideIt(t *testing.T) {
	dsn := freshDatabase(t)
	db := openDB(t, dsn)
	p := newProject(t, dsn)
	p.schema(schemaV1)
	p.mustRun("generate", "--schema", "./schema", "--name", "init")
	applyGeneratedSQL(t, db, filepath.Join(p.dir, "drizzle", "0000_init.sql"))

	if _, err := db.Exec(context.Background(),
		`INSERT INTO users (email) VALUES ($1)`, "ada@example.com"); err != nil {
		t.Fatalf("insert: %v", err)
	}

	p.schema(schemaTableAndColumnRenamed)

	// No answers at all: exit 3, nothing written.
	if _, _, code := p.run("generate", "--schema", "./schema", "--name", "rename"); code != 3 {
		t.Fatalf("generate exited %d, want 3 (refused)", code)
	}
	// The table answered and not the column: still exit 3, because
	// answering the table is what made the column a question.
	stdout, stderr, code := p.run("generate", "--schema", "./schema", "--name", "rename",
		"--rename-table", "users=people")
	if code != 3 {
		t.Fatalf("a half-answered rename exited %d, want 3\n%s%s", code, stdout, stderr)
	}
	if out := stdout + stderr; !strings.Contains(out, "--rename-column people.email=emailAddress") {
		t.Errorf("the refusal does not name the column question:\n%s", out)
	}
	if matches, _ := filepath.Glob(filepath.Join(p.dir, "drizzle", "0001_*.sql")); len(matches) > 0 {
		t.Fatalf("a refused generate wrote %v", matches)
	}

	// Interactive, with a yes for each: the prompt has to come back for
	// the second question after the first is answered.
	stdout, stderr, code = p.runWithStdin("y\ny\n",
		"generate", "--schema", "./schema", "--name", "rename", "--interactive")
	if code != 0 {
		t.Fatalf("generate exited %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "is table people a rename of users?") {
		t.Errorf("the table question was not asked:\n%s", stdout)
	}
	if !strings.Contains(stdout, "is people.emailAddress a rename of people.email?") {
		t.Errorf("the column question was never asked:\n%s", stdout)
	}

	body, err := os.ReadFile(filepath.Join(p.dir, "drizzle", "0001_rename.sql"))
	if err != nil {
		t.Fatalf("generate did not write the migration: %v", err)
	}
	// The UNIQUE constraint is dropped and re-added, because its
	// generated name is built from the table's and the column's and
	// PostgreSQL does not rename it along with them. Nothing holding a
	// row may go.
	for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("a migration made entirely of renames has a %s in it:\n%s", forbidden, body)
		}
	}
	applyGeneratedSQL(t, db, filepath.Join(p.dir, "drizzle", "0001_rename.sql"))
	if got := scalar(t, db, `SELECT "emailAddress" FROM people`); got != "ada@example.com" {
		t.Fatalf("the row did not survive: emailAddress = %q", got)
	}

	// Both answers are on disk now, so the same run finds nothing left
	// to ask and nothing left to do.
	if out := p.mustRun("generate", "--schema", "./schema", "--name", "again"); !strings.Contains(out, "no changes") {
		t.Errorf("a settled schema still generated something:\n%s", out)
	}
}

// --interactive with nothing on stdin is the accident the flag exists to
// make impossible: EOF is not an answer, so the run fails rather than
// reading one into the silence.
func TestCLIInteractiveWithNoStdinDoesNotGuess(t *testing.T) {
	dsn := freshDatabase(t)
	p := newProject(t, dsn)
	p.schema(schemaV1)
	p.mustRun("generate", "--schema", "./schema", "--name", "init")

	p.schema(schemaRenamed)
	stdout, stderr, code := p.runWithStdin("",
		"generate", "--schema", "./schema", "--name", "rename", "--interactive")
	if code == 0 {
		t.Fatalf("generate exited 0 with nothing to read\n%s%s", stdout, stderr)
	}
	if matches, _ := filepath.Glob(filepath.Join(p.dir, "drizzle", "0001_*.sql")); len(matches) > 0 {
		t.Fatalf("a run with no answers wrote %v", matches)
	}
}
