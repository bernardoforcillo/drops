package integration_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/sqlite"
)

// SQLite cannot ALTER most things, so drops implements those changes as
// a table rebuild — CREATE the new shape, copy, DROP, RENAME. DROP
// TABLE takes every index and every trigger on the table with it, and
// nothing but the engine can prove they came back: a rebuild that
// quietly discards the index the hot query depends on renders exactly
// like one that does not.
//
// Each test forces the rebuild with a change SQLite's ALTER TABLE
// cannot express: a column with a non-constant default, a dropped
// column, a widened UNIQUE constraint.

// applyDiff runs the migration drops generates for a schema change and
// returns the statements, so a test can assert on them as well as on
// what the engine ended up holding.
func applyDiff(t *testing.T, db *sqlite.DB, after *sqlite.Table) []string {
	t.Helper()
	ctx := context.Background()
	live, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	stmts := sqlite.Diff(live, sqlite.BuildSnapshot(sqlite.NewSchema(after)))
	if len(stmts) == 0 {
		t.Fatal("the schema change produced no migration at all")
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			t.Fatalf("the generated migration was rejected: %v\n%s\n\nfull migration:\n%s",
				err, s, strings.Join(stmts, "\n"))
		}
	}
	return stmts
}

// schemaObjects returns sqlite_master's name→sql for everything
// attached to a table, which is the only account of what survived.
func schemaObjects(t *testing.T, db *sqlite.DB, table string) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(),
		`SELECT type, name, COALESCE(sql, '') FROM sqlite_master WHERE tbl_name = ?`, table)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var kind, name, sql string
		if err := rows.Scan(&kind, &name, &sql); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out[kind+":"+name] = sql
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	return out
}

// The headline case. A unique index, a plain index, a partial index and
// a trigger, across a rebuild — and the unique index has to still
// *enforce*, which is the part that a surviving sqlite_master row does
// not by itself prove.
func TestRebuildKeepsIndexesAndTriggers(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("accounts")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	email := sqlite.Add(before, sqlite.Text("email").NotNull())
	org := sqlite.Add(before, sqlite.Text("org").NotNull())
	sqlite.Add(before, sqlite.Integer("hits").NotNull().Default("0"))
	exec(t, db, sqlite.CreateTable(before))
	exec(t, db, sqlite.CreateUniqueIndex("accounts_email_uidx", before, email))
	exec(t, db, sqlite.CreateIndex("accounts_org_idx", before, org))
	if _, err := db.Exec(ctx,
		`CREATE INDEX accounts_busy_idx ON accounts (org) WHERE hits > 100`); err != nil {
		t.Fatalf("create partial index: %v", err)
	}
	if _, err := db.Exec(ctx,
		`CREATE TRIGGER accounts_count AFTER UPDATE OF email ON accounts
		 BEGIN UPDATE accounts SET hits = hits + 1 WHERE id = NEW.id; END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO accounts (email, org) VALUES ('a@x', 'x')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A column with a non-constant default cannot be ALTER-added —
	// SQLite refuses "Cannot add a column with non-constant default" —
	// so this ordinary-looking addition is a rebuild.
	after := sqlite.NewTable("accounts")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey().AutoIncrement())
	sqlite.Add(after, sqlite.Text("email").NotNull())
	sqlite.Add(after, sqlite.Text("org").NotNull())
	sqlite.Add(after, sqlite.Integer("hits").NotNull().Default("0"))
	sqlite.Add(after, sqlite.Timestamp("seen_at", false).Default("CURRENT_TIMESTAMP"))
	stmts := applyDiff(t, db, after)
	if !strings.Contains(strings.Join(stmts, "\n"), "DROP TABLE") {
		t.Fatalf("this change was supposed to force a rebuild, but did not:\n%s",
			strings.Join(stmts, "\n"))
	}

	objs := schemaObjects(t, db, "accounts")
	for _, want := range []string{
		"index:accounts_email_uidx",
		"index:accounts_org_idx",
		"index:accounts_busy_idx",
		"trigger:accounts_count",
	} {
		if _, ok := objs[want]; !ok {
			t.Errorf("the rebuild destroyed %s and never put it back", want)
		}
	}
	// The partial index's predicate lives only in its stored DDL —
	// PRAGMA index_info never reports it, so a replay that re-rendered
	// the index from its columns would silently widen it.
	if sql := objs["index:accounts_busy_idx"]; !strings.Contains(strings.ToUpper(sql), "WHERE") {
		t.Errorf("the partial index came back without its predicate: %q", sql)
	}

	// Enforcement, not existence. A UNIQUE index that is present but not
	// enforcing is the failure this whole test exists to catch.
	if _, err := db.Exec(ctx,
		`INSERT INTO accounts (email, org) VALUES ('a@x', 'y')`); err == nil {
		t.Error("the unique index survived in name only: a duplicate email was accepted")
	}

	// And the trigger has to still fire.
	if _, err := db.Exec(ctx,
		`UPDATE accounts SET email = 'b@x' WHERE email = 'a@x'`); err != nil {
		t.Fatalf("update: %v", err)
	}
	var hits int
	hitRows, err := db.Query(ctx, `SELECT hits FROM accounts WHERE email = 'b@x'`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !hitRows.Next() {
		hitRows.Close()
		t.Fatal("the updated row is gone")
	}
	if err := hitRows.Scan(&hits); err != nil {
		t.Fatalf("scan: %v", err)
	}
	hitRows.Close()
	if hits != 1 {
		t.Errorf("the trigger did not fire after the rebuild: hits = %d, want 1", hits)
	}
}

// An index keyed on a column the rebuild removes cannot come back.
// drops drops it deliberately, the way PostgreSQL drops an index with
// its column, and says so in the migration rather than letting the
// engine reject the replay and make the column undroppable.
func TestRebuildDropsAnIndexKeyedOnADroppedColumn(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("notes")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("body"))
	legacy := sqlite.Add(before, sqlite.Text("legacy"))
	exec(t, db, sqlite.CreateTable(before))
	exec(t, db, sqlite.CreateIndex("notes_legacy_idx", before, legacy))
	exec(t, db, sqlite.CreateIndex("notes_body_idx", before, sqlite.Add(before, sqlite.Text("tag"))))

	after := sqlite.NewTable("notes")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("body"))
	sqlite.Add(after, sqlite.Text("tag"))

	stmts := applyDiff(t, db, after)
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, `-- index "notes_legacy_idx" dropped with column "legacy"`) {
		t.Errorf("the migration dropped an index without recording it:\n%s", joined)
	}

	objs := schemaObjects(t, db, "notes")
	if _, ok := objs["index:notes_legacy_idx"]; ok {
		t.Error("an index on a dropped column somehow survived")
	}
	// The unrelated index must not be collateral damage.
	if _, ok := objs["index:notes_body_idx"]; !ok {
		t.Error("the rebuild dropped an index that had nothing to do with the dropped column")
	}
	if _, err := db.Exec(ctx, `INSERT INTO notes (id, body, tag) VALUES (1, 'x', 'y')`); err != nil {
		t.Fatalf("the rebuilt table does not accept a row: %v", err)
	}
}

// A trigger body is arbitrary SQL and SQLite does not resolve the names
// in it until it fires, so drops cannot refuse a trigger that names a
// dropped column — it replays it and warns.
func TestRebuildWarnsAboutATriggerNamingADroppedColumn(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("audits")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("body"))
	sqlite.Add(before, sqlite.Text("legacy"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx,
		`CREATE TRIGGER audits_touch AFTER INSERT ON audits
		 BEGIN UPDATE audits SET legacy = 'seen' WHERE id = NEW.id; END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	after := sqlite.NewTable("audits")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("body"))

	stmts := applyDiff(t, db, after)
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, `-- trigger "audits_touch" names dropped column "legacy"`) {
		t.Errorf("the migration replayed a trigger that can no longer work, silently:\n%s", joined)
	}
	if _, ok := schemaObjects(t, db, "audits")["trigger:audits_touch"]; !ok {
		t.Fatal("the trigger was not replayed at all")
	}
	// The engine accepted CREATE TRIGGER without resolving "legacy" —
	// which is exactly why the warning has to exist. It fails here, on
	// use, not on creation.
	if _, err := db.Exec(ctx, `INSERT INTO audits (id, body) VALUES (1, 'x')`); err == nil {
		t.Error("SQLite has started validating trigger bodies at CREATE time; the warning's premise is stale")
	}
}

// A generated migration cannot preserve anything: it is computed from
// snapshot files, and a snapshot file records no index and no trigger,
// because BuildSnapshot has nothing to record — the schema DSL cannot
// declare either. So the rebuild Push survives is the rebuild the
// Migrator does not, and the only honest thing left is to say so in the
// file. This runs the generated migration against the engine, because
// the claim being tested is about what the engine is left holding.
func TestAGeneratedMigrationLosesIndexesAndSaysSo(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("ledger")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("ref").NotNull())
	sqlite.Add(before, sqlite.Text("legacy"))

	after := sqlite.NewTable("ledger")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("ref").NotNull())

	files := map[string][]byte{}
	gen := func(tbl *sqlite.Table, name string) *sqlite.GenerateResult {
		t.Helper()
		fsys := fstest.MapFS{}
		for k, v := range files {
			fsys["m/"+k] = &fstest.MapFile{Data: v}
		}
		res, err := sqlite.GenerateMigration(sqlite.GenerateOptions{
			Schema: sqlite.NewSchema(tbl), Dir: "m", Name: name,
			FS: fsys, Now: func() int64 { return 1 },
			Write: func(rel string, data []byte) error { files[rel] = data; return nil },
		})
		if err != nil {
			t.Fatalf("GenerateMigration: %v", err)
		}
		return res
	}

	// Migration 0000 creates the table; an operator then indexes it.
	if _, err := db.Exec(ctx, gen(before, "init").SQL); err != nil {
		t.Fatalf("apply 0000: %v", err)
	}
	if _, err := db.Exec(ctx, `CREATE UNIQUE INDEX ledger_ref_uidx ON ledger (ref)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO ledger (id, ref) VALUES (1, 'r1')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Migration 0001 drops a column, which is a rebuild.
	res := gen(after, "drop_legacy")
	if !strings.Contains(res.SQL, "-- rebuild") {
		t.Fatalf("dropping a column was supposed to be a rebuild:\n%s", res.SQL)
	}
	if !strings.Contains(res.SQL, "every index and trigger on it") {
		t.Errorf("the migration destroys the index and says nothing about it:\n%s", res.SQL)
	}
	var flagged bool
	for _, w := range sqlite.AnalyzeMigration(res.SQL) {
		if w.Rule == "rebuild-loses-indexes" {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("AnalyzeMigration did not flag a rebuild that loses its indexes:\n%s", res.SQL)
	}

	if _, err := db.Exec(ctx, res.SQL); err != nil {
		t.Fatalf("apply 0001: %v\n%s", err, res.SQL)
	}
	// The engine's account: the index really is gone, which is what the
	// comment above is for. If a later change makes the generated path
	// able to preserve it, this test fails and the comment has to go.
	if _, ok := schemaObjects(t, db, "ledger")["index:ledger_ref_uidx"]; ok {
		t.Error("the generated migration kept the index; the note in it is now a lie")
	}
	if _, err := db.Exec(ctx, `INSERT INTO ledger (id, ref) VALUES (2, 'r1')`); err != nil {
		t.Errorf("the unique index is somehow still enforcing: %v", err)
	}
}

// An index that reaches a dropped column through a partial predicate is
// invisible to PRAGMA index_info, so drops replays it and the engine
// refuses. The migration has to fail rather than quietly lose the index.
func TestRebuildRefusesWhenAPartialPredicateNamesADroppedColumn(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("jobs")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("state"))
	sqlite.Add(before, sqlite.Integer("legacy_flag"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx,
		`CREATE INDEX jobs_open_idx ON jobs (state) WHERE legacy_flag = 1`); err != nil {
		t.Fatalf("create partial index: %v", err)
	}

	after := sqlite.NewTable("jobs")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("state"))

	live, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	stmts := sqlite.Diff(live, sqlite.BuildSnapshot(sqlite.NewSchema(after)))

	var lastErr error
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatalf("the migration was accepted, so the index was lost silently after all:\n%s",
			strings.Join(stmts, "\n"))
	}
	if !strings.Contains(lastErr.Error(), "legacy_flag") {
		t.Errorf("expected the engine to refuse the replayed index on the missing column, got: %v", lastErr)
	}
}

// "The engine's refusal fails the migration and rolls it back" is the
// documented outcome, and a refusal is only tolerable because of the
// second half. Running the statements one at a time, as the test above
// does, proves the refusal and nothing about the rollback: it leaves
// the table already dropped and renamed. Push wraps the sequence in one
// transaction, so this is where the claim is actually testable.
func TestPushRollsTheWholeRebuildBackWhenAnIndexCannotBeReplayed(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("tasks")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("state"))
	sqlite.Add(before, sqlite.Integer("legacy_flag"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx,
		`CREATE INDEX tasks_open_idx ON tasks (state) WHERE legacy_flag = 1`); err != nil {
		t.Fatalf("create partial index: %v", err)
	}
	if _, err := db.Exec(ctx,
		`INSERT INTO tasks (id, state, legacy_flag) VALUES (1, 'open', 1)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("tasks")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("state"))

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	if err == nil {
		t.Fatalf("Push swallowed the engine's refusal and reported success: %+v", res)
	}
	if !strings.Contains(err.Error(), "legacy_flag") {
		t.Errorf("Push failed for some other reason than the unreplayable index: %v", err)
	}

	// Nothing may have moved: the column, the index and the row are all
	// still there, or the rollback did not happen.
	objs := schemaObjects(t, db, "tasks")
	if _, ok := objs["index:tasks_open_idx"]; !ok {
		t.Error("the partial index is gone, so the failed rebuild was not rolled back")
	}
	if _, ok := objs["table:tasks_new"]; ok {
		t.Error("the half-built table was left behind")
	}
	var flag int
	rows, err := db.Query(ctx, `SELECT legacy_flag FROM tasks WHERE id = 1`)
	if err != nil {
		t.Fatalf("the dropped column did not come back: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the seeded row is gone")
	}
	if err := rows.Scan(&flag); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if flag != 1 {
		t.Errorf("legacy_flag = %d, want 1", flag)
	}
}

// The rebuild only has anything to preserve because a table with a
// UNIQUE constraint used to rebuild itself on every push: SQLite does
// not store a UNIQUE constraint's name, so comparing names reported a
// constraint change against the table's own declaration, forever.
func TestAUniqueConstraintConvergesInsteadOfRebuildingForever(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		build func() *sqlite.Table
	}{
		{"inline column UNIQUE", func() *sqlite.Table {
			tbl := sqlite.NewTable("members")
			sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
			sqlite.Add(tbl, sqlite.Text("email").NotNull().Unique())
			return tbl
		}},
		{"named composite UNIQUE", func() *sqlite.Table {
			tbl := sqlite.NewTable("members")
			sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
			e := sqlite.Add(tbl, sqlite.Text("email").NotNull())
			o := sqlite.Add(tbl, sqlite.Text("org").NotNull())
			tbl.AddUnique("members_org_email_uq", o, e)
			return tbl
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tbl := tc.build()
			if _, err := db.Exec(ctx, `DROP TABLE IF EXISTS members`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			exec(t, db, sqlite.CreateTable(tbl))

			live, err := sqlite.Introspect(ctx, db)
			if err != nil {
				t.Fatalf("Introspect: %v", err)
			}
			if stmts := sqlite.Diff(live, sqlite.BuildSnapshot(sqlite.NewSchema(tbl))); len(stmts) != 0 {
				t.Errorf("a freshly created table diffs against its own declaration:\n%s",
					strings.Join(stmts, "\n"))
			}
		})
	}
}

// The other direction: a UNIQUE constraint that genuinely changes still
// has to produce a migration. Comparing by column tuple must not be so
// loose that it stops noticing.
func TestAChangedUniqueConstraintStillRebuilds(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("seats")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	e := sqlite.Add(before, sqlite.Text("email").NotNull())
	sqlite.Add(before, sqlite.Text("org").NotNull())
	before.AddUnique("seats_email_uq", e)
	exec(t, db, sqlite.CreateTable(before))

	after := sqlite.NewTable("seats")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	e2 := sqlite.Add(after, sqlite.Text("email").NotNull())
	o2 := sqlite.Add(after, sqlite.Text("org").NotNull())
	after.AddUnique("seats_email_uq", o2, e2)

	stmts := applyDiff(t, db, after)
	if !strings.Contains(strings.Join(stmts, "\n"), "DROP TABLE") {
		t.Fatalf("widening a UNIQUE constraint produced no rebuild:\n%s", strings.Join(stmts, "\n"))
	}

	// The widened constraint has to be the one now enforcing.
	if _, err := db.Exec(ctx, `INSERT INTO seats (id, email, org) VALUES (1, 'a@x', 'p')`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO seats (id, email, org) VALUES (2, 'a@x', 'q')`); err != nil {
		t.Errorf("the old single-column UNIQUE is still enforcing after the rebuild: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO seats (id, email, org) VALUES (3, 'a@x', 'p')`); err == nil {
		t.Error("the widened UNIQUE constraint is not enforcing")
	}
	live, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	if again := sqlite.Diff(live, sqlite.BuildSnapshot(sqlite.NewSchema(after))); len(again) != 0 {
		t.Errorf("the migration did not converge; a redeploy would emit:\n%s", strings.Join(again, "\n"))
	}
}

// Push must never treat an index it was not told about as drift. The Go
// schema DSL cannot declare an index at all, so every index in every
// database is undeclared — a Push that dropped those would drop all of
// them.
func TestPushLeavesUndeclaredIndexesAlone(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("catalog")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sku := sqlite.Add(tbl, sqlite.Text("sku").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))
	exec(t, db, sqlite.CreateUniqueIndex("catalog_sku_uidx", tbl, sku))

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(tbl), sqlite.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Errorf("a hand-made index made Push want to change the schema:\n%s",
			strings.Join(res.Statements, "\n"))
	}
	if _, ok := schemaObjects(t, db, "catalog")["index:catalog_sku_uidx"]; !ok {
		t.Fatal("the index is not there to begin with")
	}
}

// The whole story through the entry point people actually call. Push is
// where the production incident happens: a schema change lands, the
// rebuild runs inside one transaction, and the index the hot query
// needs is either there afterwards or it is not.
func TestPushThroughARebuildKeepsTheIndex(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("orders")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	ref := sqlite.Add(before, sqlite.Text("ref").NotNull())
	sqlite.Add(before, sqlite.Integer("total").NotNull().Default("0"))
	exec(t, db, sqlite.CreateTable(before))
	exec(t, db, sqlite.CreateUniqueIndex("orders_ref_uidx", before, ref))
	if _, err := db.Exec(ctx, `INSERT INTO orders (id, ref) VALUES (1, 'r1')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("orders")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("ref").NotNull())
	sqlite.Add(after, sqlite.Text("total").NotNull().Default("0")) // INTEGER -> TEXT forces the rebuild

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !res.Applied {
		t.Fatalf("Push applied nothing:\n%s", strings.Join(res.Statements, "\n"))
	}
	if !strings.Contains(strings.Join(res.Statements, "\n"), "DROP TABLE") {
		t.Fatalf("the type change did not force a rebuild:\n%s", strings.Join(res.Statements, "\n"))
	}

	if _, ok := schemaObjects(t, db, "orders")["index:orders_ref_uidx"]; !ok {
		t.Fatal("Push destroyed the unique index and never put it back")
	}
	if _, err := db.Exec(ctx, `INSERT INTO orders (id, ref) VALUES (2, 'r1')`); err == nil {
		t.Error("the unique index is present but no longer enforcing")
	}
	// And the push has to converge, or the next deploy rebuilds again.
	again, err := sqlite.Push(ctx, db, sqlite.NewSchema(after), sqlite.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("second Push: %v", err)
	}
	if len(again.Statements) != 0 {
		t.Errorf("a second push still wants to change the schema:\n%s",
			strings.Join(again.Statements, "\n"))
	}
}

// Drift is blind to indexes, and that is the decision rather than an
// oversight: `Diff` compares no index, so `DetectDrift` reports none in
// either direction. It used to report a hand-made *unique* index — as a
// UNIQUE constraint the repo had not declared — and not a plain one,
// and the migration it proposed was a rebuild that would have destroyed
// the index it was complaining about. This pins the quieter behaviour,
// which is the one documented.
func TestDriftIsBlindToAHandMadeIndex(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	tbl := sqlite.NewTable("people")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(tbl, sqlite.Text("email").NotNull())
	exec(t, db, sqlite.CreateTable(tbl))
	for _, ddl := range []string{
		`CREATE UNIQUE INDEX people_email_uidx ON people (email)`,
		`CREATE INDEX people_email_idx ON people (email, id)`,
	} {
		if _, err := db.Exec(ctx, ddl); err != nil {
			t.Fatalf("create index: %v", err)
		}
	}

	live, err := sqlite.Introspect(ctx, db)
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	report := sqlite.DetectDrift(sqlite.BuildSnapshot(sqlite.NewSchema(tbl)), live)
	if !report.InSync {
		t.Errorf("a hand-made index registered as drift:\npending:\n%s\nunauthorized:\n%s",
			strings.Join(report.PendingMigrations, "\n"),
			strings.Join(report.UnauthorizedChanges, "\n"))
	}
}

// A table referenced by a view (or by a trigger on another table)
// cannot be rebuilt at all. SQLite resolves those bodies during
// ALTER TABLE ... RENAME TO, and between the DROP and the RENAME the
// old name does not exist. This asserts the engine's behaviour, not
// drops' — if SQLite ever stops resolving them there, the paragraph in
// docs/dialects.md is stale and this test says so. What drops owes the
// caller meanwhile is that the failure is total: Push rolls back.
func TestARebuildIsImpossibleUnderADependentView(t *testing.T) {
	db := openSQLite(t)
	ctx := context.Background()

	before := sqlite.NewTable("invoices")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("state"))
	sqlite.Add(before, sqlite.Text("legacy"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `CREATE VIEW open_invoices AS SELECT id FROM invoices`); err != nil {
		t.Fatalf("create view: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO invoices (id, state, legacy) VALUES (1, 'open', 'x')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("invoices")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("state"))

	// AllowDestructive because the change drops "legacy", which Push
	// now refuses on its own. That refusal is not what this test is
	// about: getting past it is what lets the engine's refusal be the
	// one under test.
	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(after),
		sqlite.PushOptions{AllowDestructive: true}); err == nil {
		t.Fatal("SQLite accepted the rename under a dependent view; docs/dialects.md now says otherwise")
	} else if !strings.Contains(err.Error(), "open_invoices") {
		t.Errorf("the rebuild failed for some other reason than the view: %v", err)
	}

	// Total failure, not partial: the table is untouched.
	rows, err := db.Query(ctx, `SELECT legacy FROM invoices WHERE id = 1`)
	if err != nil {
		t.Fatalf("the table did not survive the failed rebuild: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("the seeded row is gone")
	}
}
