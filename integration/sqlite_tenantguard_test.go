package integration_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"
	_ "modernc.org/sqlite"
)

// Everything sqlite/tenantguard.go claims, asked of the engine.
//
// This suite exists because a guard that renders is worth nothing: the
// claim is about what SQLite DOES when a statement reaches a table, and
// no unit test can make that claim. SQLite runs in-process through
// modernc.org/sqlite, so unlike the MySQL and ClickHouse halves of this
// phase these answers cost no container and are measured on every CI
// run rather than on the runs that have one.
//
// Several of the tests below assert a NEGATIVE — that a guard does NOT
// hold somewhere. Those are the load-bearing ones. They are the reasons
// the package refuses to ship a request-scoped guard and refuses
// INSERT OR REPLACE on a scoped table, and if one of them ever starts
// passing the other way, the doc comment it defends is what has to
// change first.

// applyGuard runs every statement of a guard, failing on the first the
// engine refuses.
func applyGuard(t *testing.T, db *sqlite.DB, exprs []drops.Expression) {
	t.Helper()
	for _, e := range exprs {
		exec(t, db, e)
	}
}

// openSQLiteFile opens a file-backed database and returns both handles.
// A file rather than :memory: because two of the tests below need a
// SECOND connection to the same database, which ":memory:" does not
// give: each connection to it gets a database of its own.
func openSQLiteFile(t *testing.T) (*sqlite.DB, *sql.DB, string) {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "guard.db")
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlite.New(stdlib.New(sqlDB)), sqlDB, dsn
}

// wantRefused asserts that a statement was refused and that the refusal
// carries the guard's own message — so a test cannot pass on some
// unrelated error (a typo in the SQL, a missing table) and be read as
// the guard having fired.
func wantRefused(t *testing.T, err error, fragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("statement was accepted; the guard should have refused it")
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("refused, but not by the guard: %v (want a message containing %q)", err, fragment)
	}
}

// scalar reads a one-column, one-row result. drops has no QueryRow, and
// every assertion below that counts rows needs one.
func scalar(t *testing.T, db *sqlite.DB, sql string, dest any) {
	t.Helper()
	rows, err := db.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("%s: no rows: %v", sql, rows.Err())
	}
	if err := rows.Scan(dest); err != nil {
		t.Fatalf("%s: scan: %v", sql, err)
	}
}

type guardDoc struct {
	ID       int64  `drop:"id"`
	AuthorID int64  `drop:"authorId"`
	Body     string `drop:"body"`
	TenantID string `drop:"tenantId"`
}

// The trigger holds for SQL drops did not build. This is the whole
// claim: the predicates reach the statements this package renders, and
// a raw statement carries what the caller wrote and nothing else —
// which is the first entry in "Where the automatic scoping stops".
func TestATenantGuardRefusesRawStatementsDropsNeverSaw(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_raw")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	sqlite.Add(docs, sqlite.Text("body"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)))

	if _, err := db.Exec(ctx, `INSERT INTO "gd_raw" ("id", "tenantId", "body") VALUES (1, 'acme', 'a')`); err != nil {
		t.Fatalf("a row carrying its tenant was refused: %v", err)
	}

	// The row that belongs to nobody.
	_, err := db.Exec(ctx, `INSERT INTO "gd_raw" ("id", "body") VALUES (2, 'b')`)
	wantRefused(t, err, `"gd_raw"."tenantId" is null`)

	_, err = db.Exec(ctx, `INSERT INTO "gd_raw" ("id", "tenantId", "body") VALUES (3, NULL, 'c')`)
	wantRefused(t, err, `"gd_raw"."tenantId" is null`)

	// The statement that moves a row between tenants.
	_, err = db.Exec(ctx, `UPDATE "gd_raw" SET "tenantId" = 'globex' WHERE "id" = 1`)
	wantRefused(t, err, `"gd_raw"."tenantId" is immutable`)

	// An unrelated update on the same row is untouched: BEFORE UPDATE OF
	// fires only for a SET list that names the axis.
	if _, err := db.Exec(ctx, `UPDATE "gd_raw" SET "body" = 'edited' WHERE "id" = 1`); err != nil {
		t.Fatalf("an update that does not name the axis was refused: %v", err)
	}

	// An upsert whose DO UPDATE assigns the axis is an UPDATE as far as
	// the trigger is concerned, and is refused. This one is worth
	// asserting because the statement never says the word UPDATE.
	_, err = db.Exec(ctx,
		`INSERT INTO "gd_raw" ("id", "tenantId", "body") VALUES (1, 'acme', 'x') `+
			`ON CONFLICT ("id") DO UPDATE SET "tenantId" = 'globex'`)
	wantRefused(t, err, `"gd_raw"."tenantId" is immutable`)

	// INSERT OR IGNORE does not swallow the abort. A conflict clause
	// tells SQLite what to do about a CONSTRAINT; RAISE(ABORT) is
	// explicit and wins.
	_, err = db.Exec(ctx, `INSERT OR IGNORE INTO "gd_raw" ("id", "body") VALUES (9, 'i')`)
	wantRefused(t, err, `"gd_raw"."tenantId" is null`)

	var rows int64
	scalar(t, db, `SELECT count(*) FROM "gd_raw"`, &rows)
	if rows != 1 {
		t.Fatalf("table holds %d rows, want 1: a refused statement wrote something", rows)
	}
}

// The wrong-tenant write no predicate has a form of.
//
// Every statement here is correctly scoped. The ctx carries acme, the
// entity stamps acme, the WHERE clause would carry acme — and the row
// still points at a user belonging to globex, so acme reads globex's
// user through its own post. The first half of this test shows drops
// accepting it, which is the point: the layer cannot see it. The second
// half installs the guard and shows the same call refused.
func TestATenantGuardRefusesAPostWhoseAuthorBelongsToAnotherTenant(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	users := sqlite.NewTable("gd_users")
	userID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	userTenant := sqlite.Add(users, sqlite.Text("tenantId").NotNull())
	users.ScopeWritesByTenant(userTenant)

	posts := sqlite.NewTable("gd_posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	authorID := sqlite.Add(posts, sqlite.BigInt("authorId"))
	sqlite.Add(posts, sqlite.Text("body"))
	postTenant := sqlite.Add(posts, sqlite.Text("tenantId").NotNull())
	posts.ScopeWritesByTenant(postTenant)

	exec(t, db, sqlite.CreateTable(users))
	exec(t, db, sqlite.CreateTable(posts))
	if _, err := db.Exec(ctx,
		`INSERT INTO "gd_users" ("id", "tenantId") VALUES (1, 'acme'), (2, 'globex')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	Posts := sqlite.NewEntity[guardDoc](posts).ScopeByTenant(postTenant)
	acme := sqlite.WithTenant(ctx, "acme")

	// Before the guard: accepted, stamped acme, pointing at globex's
	// user. No error anywhere, which is the failure this exists for.
	crossed := guardDoc{ID: 10, AuthorID: 2, Body: "leak"}
	if err := Posts.Create(db, acme, &crossed); err != nil {
		t.Fatalf("unguarded create: %v", err)
	}
	if crossed.TenantID != "acme" {
		t.Fatalf("stamped tenant = %q, want acme", crossed.TenantID)
	}
	if _, err := db.Exec(ctx, `DELETE FROM "gd_posts"`); err != nil {
		t.Fatalf("clear: %v", err)
	}

	applyGuard(t, db, sqlite.CreateTenantGuard(
		sqlite.TenantGuardFor(posts).MatchingParent(authorID, userID, userTenant)))

	// After: the same call, refused by the database.
	again := guardDoc{ID: 11, AuthorID: 2, Body: "leak"}
	err := Posts.Create(db, acme, &again)
	wantRefused(t, err, `"gd_posts"."tenantId" disagrees with "gd_users"."tenantId"`)

	// acme's own user is fine.
	ok := guardDoc{ID: 12, AuthorID: 1, Body: "fine"}
	if err := Posts.Create(db, acme, &ok); err != nil {
		t.Fatalf("a post for this tenant's own user was refused: %v", err)
	}

	// An UPDATE that re-points an existing post at another tenant's
	// user is refused too — the guard fires on the foreign key as well
	// as on the axis.
	_, err = db.Exec(ctx, `UPDATE "gd_posts" SET "authorId" = 2 WHERE "id" = 12`)
	wantRefused(t, err, `"gd_posts"."tenantId" disagrees with "gd_users"."tenantId"`)

	// A post whose author does not exist is refused as well: a row
	// pointing at nothing has no tenant to agree with. Fail-closed, and
	// documented on MatchingParent as deliberate.
	orphan := guardDoc{ID: 13, AuthorID: 99, Body: "orphan"}
	err = Posts.Create(db, acme, &orphan)
	wantRefused(t, err, `"gd_posts"."tenantId" disagrees with "gd_users"."tenantId"`)
}

// The file-per-tenant deployment, made checkable. In a database that
// holds one tenant, the axis has one legal value, and the guard says so
// in the schema rather than in a runbook.
func TestAPinnedTenantGuardRefusesEveryOtherTenant(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_pinned")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs).PinnedTo("acme")))

	if _, err := db.Exec(ctx, `INSERT INTO "gd_pinned" VALUES (1, 'acme')`); err != nil {
		t.Fatalf("this file's own tenant was refused: %v", err)
	}
	_, err := db.Exec(ctx, `INSERT INTO "gd_pinned" VALUES (2, 'globex')`)
	wantRefused(t, err, `"gd_pinned"."tenantId" must be 'acme' in this database`)

	// The pin subsumes the null guard: IS NOT 'acme' is true for NULL.
	_, err = db.Exec(ctx, `INSERT INTO "gd_pinned" VALUES (3, NULL)`)
	wantRefused(t, err, `"gd_pinned"."tenantId" must be 'acme' in this database`)

	_, err = db.Exec(ctx, `UPDATE "gd_pinned" SET "tenantId" = 'globex' WHERE "id" = 1`)
	wantRefused(t, err, `"gd_pinned"."tenantId" must be 'acme' in this database`)
}

// A tenant value carrying a quote survives the round trip into stored
// DDL. The rendering is asserted in the unit test; what this asks is
// whether the trigger SQLite stored scopes the tenant drops named.
func TestAPinnedTenantGuardKeepsAQuotedTenantIntact(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_quote")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs).PinnedTo("o'brien")))

	if _, err := db.Exec(ctx, `INSERT INTO "gd_quote" VALUES (1, 'o''brien')`); err != nil {
		t.Fatalf("the pinned tenant was refused by its own guard: %v", err)
	}
	_, err := db.Exec(ctx, `INSERT INTO "gd_quote" VALUES (2, 'obrien')`)
	wantRefused(t, err, "must be")
}

// A guard is in the schema, so it applies to a connection that never
// loaded drops — and any connection can remove it. Both halves of the
// same fact, and together they are why the file comment calls this a
// guard and not a boundary.
func TestATenantGuardBindsEveryConnectionAndIsRemovableByAnyOfThem(t *testing.T) {
	db, sqlDB, dsn := openSQLiteFile(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_conn")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)))
	_ = sqlDB

	other, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("second handle: %v", err)
	}
	defer other.Close()

	_, err = other.ExecContext(ctx, `INSERT INTO "gd_conn" VALUES (1, NULL)`)
	wantRefused(t, err, `"gd_conn"."tenantId" is null`)

	// And the other half: nothing stops that connection from taking the
	// guard off. There is no privilege in SQLite to withhold.
	if _, err := other.ExecContext(ctx, `DROP TRIGGER "gd_conn_tenantGuard_ins"`); err != nil {
		t.Fatalf("DROP TRIGGER: %v", err)
	}
	if _, err := other.ExecContext(ctx, `INSERT INTO "gd_conn" VALUES (1, NULL)`); err != nil {
		t.Fatalf("after DROP TRIGGER the write should be unguarded: %v", err)
	}
}

// The same fact by the other route, because a deployment that
// withholds DROP TRIGGER by convention has withheld nothing:
// PRAGMA writable_schema = ON makes sqlite_master an ordinary table
// and the guard an ordinary row to delete.
//
// The one nuance worth recording: the deletion takes effect when the
// schema is next parsed. The connection that ran it kept firing the
// trigger it had already loaded, and the guard was gone for the next
// connection to open the file.
func TestWritableSchemaRemovesAGuardLikeAnyOtherRow(t *testing.T) {
	db, sqlDB, dsn := openSQLiteFile(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_ws")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)))

	_, err := db.Exec(ctx, `INSERT INTO "gd_ws" VALUES (1, NULL)`)
	wantRefused(t, err, `"gd_ws"."tenantId" is null`)

	for _, stmt := range []string{
		`PRAGMA writable_schema = ON`,
		`DELETE FROM sqlite_master WHERE "type" = 'trigger' AND "name" = 'gd_ws_tenantGuard_ins'`,
		`PRAGMA writable_schema = OFF`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// Still refused on this connection: it holds the schema it parsed.
	_, err = db.Exec(ctx, `INSERT INTO "gd_ws" VALUES (1, NULL)`)
	wantRefused(t, err, `"gd_ws"."tenantId" is null`)

	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.ExecContext(ctx, `INSERT INTO "gd_ws" VALUES (2, NULL)`); err != nil {
		t.Fatalf("the guard survived being deleted out of sqlite_master: %v", err)
	}
}

// Why a trigger and not a CHECK, part one: a CHECK is switchable at
// runtime by the code it constrains, and a trigger is not.
//
// PRAGMA ignore_check_constraints is an ordinary statement. Any code on
// the connection may run it, and from then on every CHECK in the
// database is off. There is no pragma that does this to triggers.
func TestAPragmaTurnsEveryCheckOffAndLeavesTheTriggerOn(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx,
		`CREATE TABLE "gd_check" ("id" INTEGER PRIMARY KEY, "tenantId" TEXT CHECK ("tenantId" IS NOT NULL))`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "gd_check" VALUES (1, NULL)`); err == nil {
		t.Fatalf("the CHECK did not fire")
	}

	if _, err := db.Exec(ctx, `PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "gd_check" VALUES (2, NULL)`); err != nil {
		t.Fatalf("the CHECK still fired after the pragma turned it off: %v", err)
	}

	// The same rule as a trigger, with the pragma still on.
	docs := sqlite.NewTable("gd_check")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	docs.ScopeWritesByTenant(tenant)
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)))

	_, err := db.Exec(ctx, `INSERT INTO "gd_check" VALUES (3, NULL)`)
	wantRefused(t, err, `"gd_check"."tenantId" is null`)
}

// The sharp edge, asserted rather than described.
//
// INSERT OR REPLACE resolves a collision by DELETING the rows in the
// way, and SQLite fires delete triggers for that implicit delete only
// when recursive_triggers is on. It is off by default. So a guard on
// the deleting side does not see it, another tenant's row is destroyed,
// and the statement reports success.
//
// This is why sqlite/insert.go refuses OR REPLACE on a scoped table
// (ErrReplaceScoped) — a refusal in drops is the only thing standing in
// front of it, because the guard cannot.
func TestReplaceDestroysAnotherTenantsRowWithoutFiringTheDeleteGuard(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	var recursive int64
	scalar(t, db, `PRAGMA recursive_triggers`, &recursive)
	if recursive != 0 {
		t.Fatalf("recursive_triggers = %d; this test is about the DEFAULT, which is off", recursive)
	}

	if _, err := db.Exec(ctx,
		`CREATE TABLE "gd_repl" ("id" INTEGER PRIMARY KEY, "tenantId" TEXT NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx,
		`CREATE TRIGGER "gd_repl_del" BEFORE DELETE ON "gd_repl" FOR EACH ROW `+
			`BEGIN SELECT RAISE(ABORT, 'a delete guard that refuses everything'); END`); err != nil {
		t.Fatalf("delete guard: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "gd_repl" VALUES (1, 'globex')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A plain DELETE is refused, so the guard is real and installed.
	if _, err := db.Exec(ctx, `DELETE FROM "gd_repl" WHERE "id" = 1`); err == nil {
		t.Fatalf("the delete guard did not fire for a plain DELETE")
	}

	// The same row, destroyed by a REPLACE, with no error.
	if _, err := db.Exec(ctx, `INSERT OR REPLACE INTO "gd_repl" VALUES (1, 'acme')`); err != nil {
		t.Fatalf("REPLACE was refused; if SQLite has started firing delete triggers for "+
			"the implicit delete by default, the doc comments that say otherwise are what "+
			"must change: %v", err)
	}
	var tenant string
	scalar(t, db, `SELECT "tenantId" FROM "gd_repl" WHERE "id" = 1`, &tenant)
	if tenant != "acme" {
		t.Fatalf("tenant = %q, want acme: the REPLACE did not take the row over", tenant)
	}

	// With recursive_triggers on, the same statement is refused. The
	// pragma is the difference, and it is per connection.
	if _, err := db.Exec(ctx, `PRAGMA recursive_triggers = ON`); err != nil {
		t.Fatalf("pragma: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT OR REPLACE INTO "gd_repl" VALUES (1, 'globex')`); err == nil {
		t.Fatalf("with recursive_triggers on, the implicit delete should have fired the guard")
	}
}

// Why there is no request-scoped guard: the only shape that expresses
// one fails OPEN, silently, on every connection but the one that
// installed it.
//
// A persistent trigger cannot name the temp schema at all — SQLite
// refuses to create it. A TEMP trigger can, and works. But a temp
// trigger belongs to one connection, database/sql hands out whichever
// connection is free, and a write on any other one runs with no trigger
// in the schema it consults.
//
// If drops shipped a WithRequestTenant helper around this, it would be
// a name that says "guard" over a mechanism that is absent for most of
// the writes it claims to cover.
func TestATempTriggerGuardsOneConnectionAndFailsOpenOnTheRest(t *testing.T) {
	_, sqlDB, _ := openSQLiteFile(t)
	ctx := context.Background()
	sqlDB.SetMaxOpenConns(4)

	if _, err := sqlDB.ExecContext(ctx,
		`CREATE TABLE "gd_temp" ("id" INTEGER PRIMARY KEY AUTOINCREMENT, "tenantId" TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	// A persistent trigger may not reference the temp schema.
	_, err := sqlDB.ExecContext(ctx,
		`CREATE TRIGGER "gd_temp_g" BEFORE INSERT ON "gd_temp" FOR EACH ROW `+
			`WHEN NEW."tenantId" IS NOT (SELECT "v" FROM temp."_tenant") `+
			`BEGIN SELECT RAISE(ABORT, 'wrong tenant'); END`)
	if err == nil {
		t.Fatalf("a persistent trigger naming the temp schema was accepted; " +
			"tenantguard.go says SQLite refuses it")
	}
	if !strings.Contains(err.Error(), "temp") {
		t.Fatalf("refused for some other reason: %v", err)
	}

	// A temp trigger can, on the connection that holds it.
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	for _, stmt := range []string{
		`CREATE TEMP TABLE "_tenant" ("v" TEXT)`,
		`INSERT INTO "_tenant" VALUES ('acme')`,
		`CREATE TEMP TRIGGER "gd_temp_g" BEFORE INSERT ON "gd_temp" FOR EACH ROW ` +
			`WHEN NEW."tenantId" IS NOT (SELECT "v" FROM temp."_tenant") ` +
			`BEGIN SELECT RAISE(ABORT, 'wrong tenant'); END`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO "gd_temp" ("tenantId") VALUES ('globex')`); err == nil {
		t.Fatalf("the temp trigger did not fire on its own connection")
	}

	// And through the pool, where it is not installed: accepted, every
	// time, with no error to read.
	const writes = 6
	for i := 0; i < writes; i++ {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO "gd_temp" ("tenantId") VALUES ('globex')`); err != nil {
			t.Fatalf("pooled write %d: %v (the temp trigger is not supposed to be "+
				"reachable from another connection)", i, err)
		}
	}
	var leaked int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM "gd_temp" WHERE "tenantId" = 'globex'`).Scan(&leaked); err != nil {
		t.Fatalf("count: %v", err)
	}
	if leaked != writes {
		t.Fatalf("%d of %d pooled writes landed; this test measures a fail-OPEN and "+
			"wants all of them", leaked, writes)
	}
}

// RAISE(ABORT) undoes the statement and leaves the transaction alone.
// Asserted because the alternative — RAISE(ROLLBACK) — is one word away
// in tenantguard.go, and the difference is whether a caller that
// ignores the error loses work it had already done.
func TestAGuardAbortsTheStatementAndNotTheTransaction(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_abort")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)))

	tx, drvTx, err := db.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO "gd_abort" VALUES (1, 'acme')`); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO "gd_abort" VALUES (2, NULL)`)
	wantRefused(t, err, `"gd_abort"."tenantId" is null`)
	if _, err := tx.Exec(ctx, `INSERT INTO "gd_abort" VALUES (3, 'acme')`); err != nil {
		t.Fatalf("the transaction did not survive the abort: %v", err)
	}
	if err := drvTx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var n int64
	scalar(t, db, `SELECT count(*) FROM "gd_abort"`, &n)
	if n != 2 {
		t.Fatalf("committed %d rows, want 2 (the two that were not refused)", n)
	}
}

// A guard whose body names something the database does not have fails
// CLOSED. SQLite does not resolve the names in a trigger body at CREATE
// TRIGGER time, so a guard applied against the wrong schema is accepted
// and then refuses every write to the table — loudly, rather than
// quietly guarding nothing.
func TestAGuardNamingAMissingParentRefusesEveryWrite(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	posts := sqlite.NewTable("gd_missing")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	authorID := sqlite.Add(posts, sqlite.BigInt("authorId"))
	tenant := sqlite.Add(posts, sqlite.Text("tenantId"))
	posts.ScopeWritesByTenant(tenant)

	// A parent table that is declared in Go and never created.
	users := sqlite.NewTable("gd_missing_users")
	userID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	userTenant := sqlite.Add(users, sqlite.Text("tenantId"))

	exec(t, db, sqlite.CreateTable(posts))
	applyGuard(t, db, sqlite.CreateTenantGuard(
		sqlite.TenantGuardFor(posts).MatchingParent(authorID, userID, userTenant)))

	_, err := db.Exec(ctx, `INSERT INTO "gd_missing" VALUES (1, 1, 'acme')`)
	if err == nil {
		t.Fatalf("a guard naming a table that does not exist accepted a write")
	}
	if !strings.Contains(err.Error(), "gd_missing_users") {
		t.Fatalf("refused for some other reason: %v", err)
	}
}

// Why a trigger and not a CHECK, part two — and a correction to what
// sqlite/ddl.go said about ALTER TABLE.
//
// The usual reason for emitting every constraint inside CREATE TABLE is
// that SQLite's ALTER TABLE cannot add one, so a CHECK on an existing
// table means the twelve-step rebuild. That is true of UNIQUE and of
// FOREIGN KEY, and it is NOT true of a named CHECK: the statement is
// accepted, appended to the stored schema text, enforced immediately,
// still enforced after the file is reopened, and integrity_check is
// happy about it.
//
// The form is outside the ALTER TABLE grammar SQLite documents, which
// is why drops measures it here and does not emit it. It is recorded
// because a comment saying the statement does not exist would send a
// reader to rebuild a table they did not have to.
func TestAlterTableAddsANamedCheckAndNothingElse(t *testing.T) {
	db, sqlDB, dsn := openSQLiteFile(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx,
		`CREATE TABLE "gd_alter" ("id" INTEGER PRIMARY KEY, "tenantId" TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(ctx,
		`ALTER TABLE "gd_alter" ADD CONSTRAINT "gd_alter_ck" CHECK ("tenantId" IS NOT NULL)`); err != nil {
		t.Fatalf("ADD CONSTRAINT ... CHECK was refused; sqlite/ddl.go's note that this "+
			"form is accepted is what must change: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "gd_alter" VALUES (1, NULL)`); err == nil {
		t.Fatalf("the added CHECK was stored and not enforced")
	}

	// It adds no column, whatever the ADD COLUMN grammar would suggest.
	var cols int64
	scalar(t, db, `SELECT count(*) FROM pragma_table_info('gd_alter')`, &cols)
	if cols != 2 {
		t.Fatalf("table has %d columns, want 2: ADD CONSTRAINT added one", cols)
	}

	// And it survives the file being closed and reopened.
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if _, err := reopened.ExecContext(ctx, `INSERT INTO "gd_alter" VALUES (2, NULL)`); err == nil {
		t.Fatalf("the CHECK was not enforced after reopening the file")
	}
	var integrity string
	if err := reopened.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q", integrity)
	}

	// The other two constraint kinds are syntax errors, so the inline
	// rule CREATE TABLE follows is still the rule.
	for _, stmt := range []string{
		`ALTER TABLE "gd_alter" ADD CONSTRAINT "gd_alter_u" UNIQUE ("tenantId")`,
		`ALTER TABLE "gd_alter" ADD CONSTRAINT "gd_alter_fk" FOREIGN KEY ("tenantId") REFERENCES "gd_alter" ("id")`,
	} {
		if _, err := reopened.ExecContext(ctx, stmt); err == nil {
			t.Fatalf("SQLite accepted %q; ddl.go says it does not", stmt)
		}
	}
}

// Why a trigger and not a CHECK, part three: the two guards a CHECK
// cannot express at all.
//
// tenantguard.go gives three reasons for the guards being triggers.
// The switchable one is above; these are the two structural ones, and
// they are asserted here rather than asserted in a comment.
func TestACheckCannotExpressTheCrossRowOrCrossTableGuards(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	if _, err := db.Exec(ctx,
		`CREATE TABLE "gd_ck_users" ("id" INTEGER PRIMARY KEY, "tenantId" TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The parent-agreement guard has no CHECK form: a CHECK may not
	// contain a subquery, so it cannot look at another row at all.
	_, err := db.Exec(ctx,
		`CREATE TABLE "gd_ck_a" ("id" INTEGER PRIMARY KEY, "tenantId" TEXT, "authorId" INTEGER, `+
			`CHECK ("tenantId" IS (SELECT "tenantId" FROM "gd_ck_users" WHERE "id" IS "authorId")))`)
	if err == nil {
		t.Fatalf("a CHECK containing a subquery was accepted; tenantguard.go says SQLite refuses it")
	}
	if !strings.Contains(err.Error(), "subqueries prohibited in CHECK constraints") {
		t.Fatalf("refused for some other reason: %v", err)
	}

	// The immutability guard has none either: a CHECK sees one row and
	// there is no OLD to compare it against.
	_, err = db.Exec(ctx,
		`CREATE TABLE "gd_ck_b" ("id" INTEGER PRIMARY KEY, "tenantId" TEXT CHECK ("tenantId" IS OLD."tenantId"))`)
	if err == nil {
		t.Fatalf("a CHECK naming OLD was accepted; tenantguard.go says there is no OLD in one")
	}
	if !strings.Contains(err.Error(), "OLD") {
		t.Fatalf("refused for some other reason: %v", err)
	}
}

// A rebuild takes the guard with it, and drops' own Diff puts it back.
//
// SQLite changes a column by the twelve-step rebuild — create the new
// shape, copy, DROP the old table, rename — and DROP TABLE takes every
// trigger on that table with it. So the guard's lifetime is tied to
// the migration tooling, and this asserts both halves of that: the
// engine forgets the trigger when the table goes, and a Diff taken
// against the live database emits the CREATE TRIGGER again after the
// rebuild it wrote.
//
// A rebuild done by hand, or by a tool that does not read the triggers
// first, has no such half. That is the sentence in tenantguard.go this
// pins.
func TestARebuildDropsTheGuardAndDiffReplaysIt(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_rebuild")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	sqlite.Add(docs, sqlite.Text("body"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)))

	live, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("introspect: %v", err)
	}
	if _, ok := live.Tables["gd_rebuild"].Triggers["gd_rebuild_tenantGuard_ins"]; !ok {
		t.Fatalf("introspection did not see the guard: %+v", live.Tables["gd_rebuild"].Triggers)
	}

	// The engine's half: dropping the table forgets the triggers.
	if _, err := db.Exec(ctx, `DROP TABLE "gd_rebuild"`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	var left int64
	scalar(t, db, `SELECT count(*) FROM sqlite_master WHERE "type" = 'trigger'`, &left)
	if left != 0 {
		t.Fatalf("%d triggers survived DROP TABLE, want 0", left)
	}

	// The tooling's half: a Diff that changes the table's shape emits
	// the rebuild AND the CREATE TRIGGER after it.
	narrowed := sqlite.NewTable("gd_rebuild")
	sqlite.Add(narrowed, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(narrowed, sqlite.Text("tenantId"))
	want := sqlite.BuildSnapshot(sqlite.NewSchema(narrowed))

	var rebuilt, replayed bool
	for _, stmt := range sqlite.Diff(live, want) {
		if strings.HasPrefix(stmt, "-- rebuild") {
			rebuilt = true
		}
		if strings.Contains(stmt, `CREATE TRIGGER "gd_rebuild_tenantGuard_ins"`) {
			replayed = true
		}
	}
	if !rebuilt {
		t.Fatalf("dropping a column did not produce a rebuild: %v", sqlite.Diff(live, want))
	}
	if !replayed {
		t.Fatalf("the rebuild did not replay the guard: %v", sqlite.Diff(live, want))
	}
}

// A guard applied over rows that already violate it is ACCEPTED, and
// the rows stay.
//
// This is the trap the sharp-edges list in tenantguard.go names, and
// it is asserted here rather than reasoned about because both halves
// are surprising: CREATE TRIGGER validates nothing, and the obvious
// repair afterwards — an UPDATE putting the right tenant on the row —
// is the exact statement the immutability guard exists to refuse. A
// table can therefore reach a state where a row is wrong and cannot be
// made right in place.
func TestAGuardOverPreexistingViolationsIsAcceptedAndTheRowsCannotBeRepairedInPlace(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_pre")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	sqlite.Add(docs, sqlite.Text("body"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))

	for _, stmt := range []string{
		`INSERT INTO "gd_pre" VALUES (1, 'acme', 'kept')`,
		`INSERT INTO "gd_pre" VALUES (2, NULL, 'orphan')`,
		`INSERT INTO "gd_pre" VALUES (3, NULL, 'orphan too')`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	// The preflight, run where it is meant to be run: before the guard
	// exists. Two rows, and the check names the trigger that will start
	// refusing them.
	checks := sqlite.TenantGuardViolations(sqlite.TenantGuardFor(docs))
	if len(checks) != 1 {
		t.Fatalf("rendered %d checks, want 1", len(checks))
	}
	if checks[0].Trigger != "gd_pre_tenantGuard_ins" {
		t.Fatalf("check names %q, want the INSERT trigger", checks[0].Trigger)
	}
	text, args := drops.StringWithDialect(sqlite.Dialect, checks[0].Count)
	if len(args) != 0 {
		t.Fatalf("preflight bound args: %s %v", text, args)
	}
	var violating int64
	scalar(t, db, text, &violating)
	if violating != 2 {
		t.Fatalf("preflight counted %d violating rows, want 2", violating)
	}

	// The guard goes on anyway, without complaint.
	applyGuard(t, db, sqlite.CreateTenantGuard(sqlite.TenantGuardFor(docs)))

	var left int64
	scalar(t, db, `SELECT count(*) FROM "gd_pre" WHERE "tenantId" IS NULL`, &left)
	if left != 2 {
		t.Fatalf("%d violating rows survived the guard, want 2", left)
	}

	// And now they are stuck: correcting the axis is a move between
	// tenants as far as the trigger can tell, NULL being one of them.
	_, err := db.Exec(ctx, `UPDATE "gd_pre" SET "tenantId" = 'acme' WHERE "id" = 2`)
	wantRefused(t, err, `"gd_pre"."tenantId" is immutable`)

	// The first documented way out: the row can be DELETED, because a
	// guard installs no delete trigger, and inserted again correctly.
	if _, err := db.Exec(ctx, `DELETE FROM "gd_pre" WHERE "id" = 2`); err != nil {
		t.Fatalf("deleting a violating row was refused: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "gd_pre" VALUES (2, 'acme', 'orphan')`); err != nil {
		t.Fatalf("re-inserting the repaired row was refused: %v", err)
	}

	scalar(t, db, text, &violating)
	if violating != 1 {
		t.Fatalf("preflight counts %d after one repair, want 1", violating)
	}
}

// The second documented way out: drop the guard, repair, recreate,
// inside ONE transaction — so the window in which the table is
// unguarded is a window nothing else gets to write in.
//
// This runs against a file rather than ":memory:" because the claim is
// about a transaction holding the database, and it asserts the guard
// is back and holding afterwards: a recovery recipe that leaves the
// table unguarded would be worse than the state it repaired.
func TestDroppingAGuardRepairingAndRecreatingItInOneTransaction(t *testing.T) {
	db, sqlDB, _ := openSQLiteFile(t)
	ctx := context.Background()

	docs := sqlite.NewTable("gd_repair")
	sqlite.Add(docs, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(docs, sqlite.Text("tenantId"))
	docs.ScopeWritesByTenant(tenant)
	exec(t, db, sqlite.CreateTable(docs))
	if _, err := db.Exec(ctx, `INSERT INTO "gd_repair" VALUES (1, NULL), (2, NULL)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	guard := sqlite.TenantGuardFor(docs)
	applyGuard(t, db, sqlite.CreateTenantGuard(guard))

	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	inTx := func(e drops.Expression) {
		t.Helper()
		text, args := drops.StringWithDialect(sqlite.Dialect, e)
		if _, err := tx.ExecContext(ctx, text, args...); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
	}
	for _, e := range sqlite.DropTenantGuardIfExists(guard) {
		inTx(e)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE "gd_repair" SET "tenantId" = 'acme' WHERE "tenantId" IS NULL`); err != nil {
		t.Fatalf("repair: %v", err)
	}
	for _, e := range sqlite.CreateTenantGuard(guard) {
		inTx(e)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var left int64
	scalar(t, db, `SELECT count(*) FROM "gd_repair" WHERE "tenantId" IS NULL`, &left)
	if left != 0 {
		t.Fatalf("%d rows still violate the guard after the repair", left)
	}
	// The guard is back, and holding.
	_, err = db.Exec(ctx, `INSERT INTO "gd_repair" VALUES (3, NULL)`)
	wantRefused(t, err, `"gd_repair"."tenantId" is null`)
}

// Two violations that DO repair in place, and the reason each does.
//
// A pinned guard's condition is about the value a row lands on, not
// about the move, so there is no immutability trigger to trip over. A
// parent disagreement can be settled from the other end: moving the
// foreign key to a parent in the same tenant satisfies the guard, and
// a SET list that does not name the axis never reaches the
// immutability trigger at all.
func TestTheViolationsThatCanBeRepairedInPlace(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	pinned := sqlite.NewTable("gd_pin_repair")
	sqlite.Add(pinned, sqlite.BigInt("id").PrimaryKey())
	pinnedTenant := sqlite.Add(pinned, sqlite.Text("tenantId"))
	pinned.ScopeWritesByTenant(pinnedTenant)
	exec(t, db, sqlite.CreateTable(pinned))
	if _, err := db.Exec(ctx, `INSERT INTO "gd_pin_repair" VALUES (1, 'globex'), (2, NULL)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	pin := sqlite.TenantGuardFor(pinned).PinnedTo("acme")
	pinChecks := sqlite.TenantGuardViolations(pin)
	pinText, _ := drops.StringWithDialect(sqlite.Dialect, pinChecks[0].Count)
	var violating int64
	scalar(t, db, pinText, &violating)
	if violating != 2 {
		t.Fatalf("preflight counted %d rows a pin would refuse, want 2", violating)
	}
	applyGuard(t, db, sqlite.CreateTenantGuard(pin))

	// Another tenant's value, and NULL, both correct in place.
	if _, err := db.Exec(ctx, `UPDATE "gd_pin_repair" SET "tenantId" = 'acme'`); err != nil {
		t.Fatalf("repairing under a pinned guard was refused: %v", err)
	}
	scalar(t, db, pinText, &violating)
	if violating != 0 {
		t.Fatalf("%d rows still violate the pin after the repair", violating)
	}

	users := sqlite.NewTable("gd_par_users")
	userID := sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	userTenant := sqlite.Add(users, sqlite.Text("tenantId"))
	users.ScopeWritesByTenant(userTenant)
	exec(t, db, sqlite.CreateTable(users))

	posts := sqlite.NewTable("gd_par_posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	postTenant := sqlite.Add(posts, sqlite.Text("tenantId"))
	author := sqlite.Add(posts, sqlite.BigInt("authorId"))
	posts.ScopeWritesByTenant(postTenant)
	exec(t, db, sqlite.CreateTable(posts))

	for _, stmt := range []string{
		`INSERT INTO "gd_par_users" VALUES (1, 'acme'), (2, 'globex')`,
		`INSERT INTO "gd_par_posts" VALUES (10, 'acme', 1)`,
		`INSERT INTO "gd_par_posts" VALUES (11, 'acme', 2)`,
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	linked := sqlite.TenantGuardFor(posts).MatchingParent(author, userID, userTenant)
	parentCheck := sqlite.TenantGuardViolations(linked)
	if len(parentCheck) != 2 {
		t.Fatalf("rendered %d checks for a guard with a parent link, want 2", len(parentCheck))
	}
	parentText, _ := drops.StringWithDialect(sqlite.Dialect, parentCheck[1].Count)
	scalar(t, db, parentText, &violating)
	if violating != 1 {
		t.Fatalf("preflight counted %d disagreeing rows, want 1", violating)
	}
	applyGuard(t, db, sqlite.CreateTenantGuard(linked))

	// The axis cannot move, but the foreign key can.
	_, err := db.Exec(ctx, `UPDATE "gd_par_posts" SET "tenantId" = 'globex' WHERE "id" = 11`)
	wantRefused(t, err, `"gd_par_posts"."tenantId"`)
	if _, err := db.Exec(ctx, `UPDATE "gd_par_posts" SET "authorId" = 1 WHERE "id" = 11`); err != nil {
		t.Fatalf("repointing the foreign key at a parent in the same tenant was refused: %v", err)
	}
	scalar(t, db, parentText, &violating)
	if violating != 0 {
		t.Fatalf("%d rows still disagree with their parent after the repair", violating)
	}
}
