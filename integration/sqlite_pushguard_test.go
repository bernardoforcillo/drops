package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// sqlite.Push and the changes a table rebuild cannot carry the data
// through.
//
// Every assertion here reads the rows back. Reading the statement list
// would prove nothing: SQLite implements a schema change as a rebuild —
// CREATE "t_new", INSERT ... SELECT, DROP TABLE, RENAME — and those
// four statements are byte-for-byte the same whether the rebuild widens
// the table or empties a column out of it. The whole reason the guard
// is computed from the diff is that the SQL does not carry the fact, so
// a test that read the SQL would be testing the wrong artefact.
//
// Each case is run twice: refused, with the rows still there, and then
// permitted, with the loss actually happening. The second half matters
// as much as the first — an override that quietly did nothing would
// pass a test that only checked the refusal.

// mustRefuse asserts that err is the destructive-change refusal and
// returns it, so a caller can look at what it named.
func mustRefuse(t *testing.T, err error) *sqlite.DestructiveChangeError {
	t.Helper()
	if err == nil {
		t.Fatal("the push applied a destructive change nobody permitted")
	}
	var refusal *sqlite.DestructiveChangeError
	if !errors.As(err, &refusal) {
		t.Fatalf("error is %T, want *sqlite.DestructiveChangeError: %v", err, err)
	}
	return refusal
}

// hasRule reports whether the refusal named a rule against an object.
func hasRule(refusal *sqlite.DestructiveChangeError, rule, object string) bool {
	for _, c := range refusal.Changes {
		if c.Rule == rule && c.Object == object {
			return true
		}
	}
	return false
}

// The headline case, and the one the whole guard exists for: a column
// the schema stopped naming.
//
// Before this, the push applied, the rebuild copied the columns both
// sides named, the data was gone and err was nil.
func TestSQLitePushRefusesADroppedColumn(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	sqliteUsersWithEmail(t, db)

	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())

	_, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	refusal := mustRefuse(t, err)
	if !hasRule(refusal, "drop-column", "email") {
		t.Errorf("the refusal did not name the column: %v", refusal.Changes)
	}
	// The rule name is drop-column because that is what it is called on
	// PostgreSQL. Nobody should have to learn a second word for it.
	if !strings.Contains(refusal.Error(), "drop-column") {
		t.Errorf("the refusal does not spell the rule:\n%s", refusal.Error())
	}
	if got, ok := sqliteColumn(t, db, "users", "email"); !ok || got != "ada@example.com" {
		t.Fatalf("the refused push touched the table: email = %q, present = %v", got, ok)
	}

	// Permitted, the loss happens — the option is a permission, not a
	// second opinion.
	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(after),
		sqlite.PushOptions{AllowDestructive: true}); err != nil {
		t.Fatalf("push with the loss permitted: %v", err)
	}
	if _, ok := sqliteColumn(t, db, "users", "email"); ok {
		t.Error("AllowDestructive did not apply the drop it was given permission for")
	}
}

// A narrowed type. SQLite is dynamically typed, so most re-declarations
// cost nothing — but text moving into a numeric affinity is converted
// by the rebuild's copy, and the conversion is not reversible.
//
// The value is '007' because it makes the loss legible: a leading zero
// is data, and after the copy it is not there. A test using '7' would
// have passed either way.
func TestSQLitePushRefusesANarrowedColumnType(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("parts")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("code"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `INSERT INTO "parts" (id, code) VALUES (1, '007')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("parts")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.BigInt("code"))

	_, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	refusal := mustRefuse(t, err)
	if !hasRule(refusal, "alter-column-type", "code") {
		t.Errorf("the refusal did not name the type change: %v", refusal.Changes)
	}
	if got, ok := sqliteColumn(t, db, "parts", "code"); !ok || got != "007" {
		t.Fatalf("the refused push touched the value: code = %q, present = %v", got, ok)
	}

	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(after),
		sqlite.PushOptions{AllowDestructive: true}); err != nil {
		t.Fatalf("push with the conversion permitted: %v", err)
	}
	// The engine, not drops, decides what the copy does to the value.
	// This is the assertion that the guard's message is true.
	got, ok := sqliteColumn(t, db, "parts", "code")
	if !ok {
		t.Fatal("the column is gone; the rebuild was supposed to convert it, not drop it")
	}
	if got != "7" {
		t.Errorf("code = %q; SQLite no longer converts numeric-looking text into a numeric affinity, "+
			"so the alter-column-type message is stale", got)
	}
}

// A type change that is not a narrowing is not refused.
//
// SQLite is dynamically typed and a declared type is only an affinity,
// so most re-declarations cost nothing at all: an affinity converts a
// value only when it considers the conversion faithful, and REAL into
// INTEGER is not one of those. The engine is the authority on that, so
// this asks it — if SQLite ever starts truncating here, the rule in
// narrowsAffinity is too narrow and this test says so.
func TestSQLitePushDoesNotRefuseAWideningTypeChange(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("readings")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Real("level"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `INSERT INTO "readings" (id, level) VALUES (1, 1.5)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("readings")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.BigInt("level"))

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	if err != nil {
		t.Fatalf("a type change that converts nothing was refused: %v", err)
	}
	if len(res.Destructive) != 0 {
		t.Errorf("the push reported a loss it did not have: %v", res.Destructive)
	}
	if got, ok := sqliteColumn(t, db, "readings", "level"); !ok || got != "1.5" {
		t.Fatalf("level = %q, present = %v; INTEGER affinity now truncates a REAL, "+
			"so narrowsAffinity is missing a case", got, ok)
	}
}

// A column that gains NOT NULL over rows that hold NULL. SQLite rejects
// the rebuild's copy, so nothing is lost — what the guard buys is that
// the push stops before it starts writing, naming the column and how
// many rows are in the way, instead of dying halfway through a
// transaction on a constraint message.
func TestSQLitePushRefusesNotNullOverExistingNulls(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("contacts")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("phone"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `INSERT INTO "contacts" (id, phone) VALUES (1, NULL), (2, '555')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("contacts")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("phone").NotNull())

	_, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	refusal := mustRefuse(t, err)
	if !hasRule(refusal, "alter-column-set-not-null", "phone") {
		t.Errorf("the refusal did not name the column: %v", refusal.Changes)
	}
	// The count comes from the rows, not from the schemas, which is the
	// whole point of asking the database before reporting this one.
	if !strings.Contains(refusal.Error(), "1 row(s) hold NULL") {
		t.Errorf("the refusal does not say how many rows are in the way:\n%s", refusal.Error())
	}
	if got, ok := sqliteColumn(t, db, "contacts", "phone"); !ok || got != "" {
		t.Fatalf("the refused push touched the table: phone = %q, present = %v", got, ok)
	}

	// Permitted, the old behaviour is what you get: the engine refuses
	// the copy and the transaction rolls back whole.
	_, err = sqlite.Push(ctx, db, sqlite.NewSchema(after), sqlite.PushOptions{AllowDestructive: true})
	if err == nil {
		t.Fatal("SQLite accepted a NOT NULL column fed a NULL; the rule's premise is stale")
	}
	if errors.As(err, new(*sqlite.DestructiveChangeError)) {
		t.Errorf("AllowDestructive did not get past the guard: %v", err)
	}
	if _, ok := sqliteColumn(t, db, "contacts", "phone"); !ok {
		t.Error("the failed rebuild did not roll back")
	}
}

// The same tightening over rows that hold no NULL is not refused.
//
// This is the case that keeps the option meaning something. Adding NOT
// NULL to a column that was never actually null is an ordinary change;
// a guard that stopped it would teach people to set AllowDestructive by
// reflex, and it would then be set on the day it mattered too.
func TestSQLitePushAllowsNotNullWhereNoRowIsNull(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("contacts")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("phone"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `INSERT INTO "contacts" (id, phone) VALUES (1, '555')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("contacts")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("phone").NotNull())

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	if err != nil {
		t.Fatalf("a tightening no row contradicts was refused: %v", err)
	}
	if len(res.Destructive) != 0 {
		t.Errorf("the push reported a loss it did not have: %v", res.Destructive)
	}
	if got, ok := sqliteColumn(t, db, "contacts", "phone"); !ok || got != "555" {
		t.Fatalf("phone = %q, present = %v", got, ok)
	}
	// And the constraint is really there afterwards.
	if _, err := db.Exec(ctx, `INSERT INTO "contacts" (id, phone) VALUES (2, NULL)`); err == nil {
		t.Error("the rebuild did not apply the NOT NULL it was pushed for")
	}
}

// A rebuild that widens the table loses nothing, and must not be
// refused. Without this the guard could be "refuse every rebuild",
// which would pass every other test in the file and be useless.
func TestSQLitePushDoesNotRefuseAHarmlessRebuild(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	sqliteUsersWithEmail(t, db)

	// A UNIQUE constraint is a table-level change, so it forces the
	// full rebuild — DROP TABLE and all — without costing anything.
	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("email").NotNull().Unique())

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	if err != nil {
		t.Fatalf("a rebuild that loses nothing was refused: %v", err)
	}
	if len(res.Destructive) != 0 {
		t.Errorf("the push reported a loss it did not have: %v", res.Destructive)
	}
	if got, ok := sqliteColumn(t, db, "users", "email"); !ok || got != "ada@example.com" {
		t.Fatalf("email = %q, present = %v", got, ok)
	}
}

// An index keyed on a column that is going. The rebuild drops the table
// and does not put this one back — deliberately, because replaying it
// would fail on "no such column" — so the index is gone and the only
// record of it was a comment in a statement list nobody prints.
func TestSQLitePushRefusesAnIndexTheRebuildWillNotReplace(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("orders")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("state"))
	sqlite.Add(before, sqlite.Text("legacy"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `CREATE INDEX orders_legacy_idx ON orders (legacy)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO orders (id, state, legacy) VALUES (1, 'open', 'x')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("orders")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("state"))

	_, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	refusal := mustRefuse(t, err)
	if !hasRule(refusal, "rebuild-drops-index", "orders_legacy_idx") {
		t.Errorf("the refusal did not name the index: %v", refusal.Changes)
	}
	if _, ok := schemaObjects(t, db, "orders")["index:orders_legacy_idx"]; !ok {
		t.Fatal("the refused push dropped the index anyway")
	}

	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(after),
		sqlite.PushOptions{AllowDestructive: true}); err != nil {
		t.Fatalf("push with the loss permitted: %v", err)
	}
	if _, ok := schemaObjects(t, db, "orders")["index:orders_legacy_idx"]; ok {
		t.Error("the index survived a rebuild that removed the column it keys; the rule's premise is stale")
	}
}

// A trigger whose body names a column that is going. SQLite does not
// resolve a trigger body at CREATE TRIGGER time, so the rebuild puts
// the trigger back, the engine accepts it, and the failure waits for
// the first write that fires it.
func TestSQLitePushRefusesATriggerTheRebuildWillBreak(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

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

	_, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	refusal := mustRefuse(t, err)
	if !hasRule(refusal, "rebuild-stale-trigger", "audits_touch") {
		t.Errorf("the refusal did not name the trigger: %v", refusal.Changes)
	}

	if _, err := sqlite.Push(ctx, db, sqlite.NewSchema(after),
		sqlite.PushOptions{AllowDestructive: true}); err != nil {
		t.Fatalf("push with the loss permitted: %v", err)
	}
	if _, ok := schemaObjects(t, db, "audits")["trigger:audits_touch"]; !ok {
		t.Fatal("the trigger was not replayed at all")
	}
	// Accepted at creation, broken on use — which is why nothing
	// downstream of the push can catch this and the diff has to.
	if _, err := db.Exec(ctx, `INSERT INTO audits (id, body) VALUES (1, 'x')`); err == nil {
		t.Error("SQLite has started validating trigger bodies at CREATE time; the rule's premise is stale")
	}
}

// A dry run does not refuse. It reports.
//
// The refusal belongs on the run that would have written; a preview
// that will not show you the plan you would have to permit is no
// preview. This is also how a caller learns what to put in front of a
// human before setting the option.
func TestSQLitePushDryRunReportsWithoutRefusing(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	sqliteUsersWithEmail(t, db)

	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(after), sqlite.PushOptions{DryRun: true})
	if err != nil {
		t.Fatalf("a dry run refused: %v", err)
	}
	if res.Applied {
		t.Error("a dry run applied")
	}
	if len(res.Destructive) != 1 || res.Destructive[0].Rule != "drop-column" {
		t.Fatalf("the dry run did not report the loss: %v", res.Destructive)
	}
	// Nothing the engine will execute names the column. That is the
	// premise the whole design rests on, so it is asserted rather than
	// assumed. Diff does write a `-- rebuild "users": drop column
	// "email"` comment above the sequence, and that is not a
	// counter-example: the comment is there because Diff read the same
	// diff this guard reads. The fact travels from the diff into the
	// text, never the other way.
	for _, s := range res.Statements {
		if strings.HasPrefix(strings.TrimSpace(s), "--") {
			continue
		}
		if strings.Contains(strings.ToLower(s), "email") {
			t.Errorf("an executable statement names the dropped column after all; "+
				"the guard could have read the SQL:\n%s", s)
		}
	}
	if got, ok := sqliteColumn(t, db, "users", "email"); !ok || got != "ada@example.com" {
		t.Fatalf("the dry run touched the table: email = %q, present = %v", got, ok)
	}
}

// A stated rename is not a drop. The guard reads the diff after the
// renames have been applied to it, exactly as Diff does, so the two
// agree on what the migration means — otherwise answering the rename
// question would land you in front of a second refusal for the column
// you just rescued.
func TestSQLitePushDoesNotCallAStatedRenameADrop(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)
	sqliteUsersWithEmail(t, db)

	answered := sqlite.NewTable("users")
	sqlite.Add(answered, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(answered, sqlite.Text("emailAddress").NotNull().RenamedFrom("email"))

	res, err := sqlite.Push(ctx, db, sqlite.NewSchema(answered))
	if err != nil {
		t.Fatalf("a stated rename was refused as a destructive change: %v", err)
	}
	if len(res.Destructive) != 0 {
		t.Errorf("the push reported a loss it did not have: %v", res.Destructive)
	}
	if got, ok := sqliteColumn(t, db, "users", "emailAddress"); !ok || got != "ada@example.com" {
		t.Fatalf("emailAddress = %q, present = %v", got, ok)
	}
}

// The affinity rule reads declared type strings, and the two sides of a
// push spell them from different places: the schema renders them, the
// database reports back what its CREATE TABLE said. If those ever drift
// apart — a Timestamp declared DATETIME read back as TEXT, say — the
// rule would see TEXT becoming NUMERIC and refuse an idempotent push,
// on every run, for a schema nobody changed. That is the worst failure
// this guard could have, so it is pinned.
func TestSQLitePushIsIdempotentAcrossTheAwkwardTypes(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	events := sqlite.NewTable("events")
	sqlite.Add(events, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(events, sqlite.Timestamp("at", false))
	sqlite.Add(events, sqlite.Date("on"))
	sqlite.Add(events, sqlite.Boolean("live"))
	sqlite.Add(events, sqlite.Numeric("amount", 12, 2))
	sqlite.Add(events, sqlite.Text("note"))
	sqlite.Add(events, sqlite.Blob("payload"))
	schema := sqlite.NewSchema(events)

	if _, err := sqlite.Push(ctx, db, schema); err != nil {
		t.Fatalf("first push: %v", err)
	}
	res, err := sqlite.Push(ctx, db, schema)
	if err != nil {
		t.Fatalf("pushing the same schema twice: %v", err)
	}
	if len(res.Statements) != 0 {
		t.Errorf("a settled schema still wanted to change something:\n%s", strings.Join(res.Statements, "\n"))
	}
	if len(res.Destructive) != 0 {
		t.Errorf("a settled schema reported a loss: %v", res.Destructive)
	}
}

// The NULL probe asks the live database, and the live database has not
// been renamed yet. The findings name a column by its new name, because
// they are computed against a previous snapshot the renames have
// already been applied to; the rows are still under the old one.
//
// Getting this backwards does not produce an error, which is why it
// needs a test of its own. `SELECT count(*) FROM "users" WHERE
// "emailAddress" IS NULL` against a table with no emailAddress column
// is not a failure on SQLite: a double-quoted name that resolves to
// nothing is re-read as a string literal, a literal is never NULL, and
// the count comes back 0. The finding is then discarded as acquitted,
// the push proceeds, and the engine kills it halfway through the
// rebuild on the constraint the guard existed to get in front of.
func TestSQLitePushCountsNullsUnderTheNameTheDatabaseStillHas(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("email"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `INSERT INTO "users" (id, email) VALUES (1, NULL)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Renamed and tightened in one push: the finding is on
	// "emailAddress", the rows are still in "email".
	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("emailAddress").NotNull().RenamedFrom("email"))

	_, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	refusal := mustRefuse(t, err)
	if !hasRule(refusal, "alter-column-set-not-null", "emailAddress") {
		t.Fatalf("the refusal did not name the column: %v", refusal.Changes)
	}
	if !strings.Contains(refusal.Error(), "1 row(s) hold NULL") {
		t.Errorf("the probe read the wrong column — it found no NULL where there is one:\n%s", refusal.Error())
	}
	if got, ok := sqliteColumn(t, db, "users", "email"); ok && got != "" {
		t.Fatalf("the refused push touched the table: email = %q", got)
	}
}

// The same unwinding, one level out: a table renamed in the push that
// also tightens one of its columns. Here reading the finding's name
// literally does fail loudly — there is no "people" yet — but loudly in
// the wrong place, as an error from counting rather than the refusal
// the operator needs.
func TestSQLitePushCountsNullsBeforeTheTableIsRenamed(t *testing.T) {
	ctx := context.Background()
	db := openSQLite(t)

	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("email"))
	exec(t, db, sqlite.CreateTable(before))
	if _, err := db.Exec(ctx, `INSERT INTO "users" (id, email) VALUES (1, NULL)`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	after := sqlite.NewTable("people").RenamedFrom("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("email").NotNull())

	_, err := sqlite.Push(ctx, db, sqlite.NewSchema(after))
	refusal := mustRefuse(t, err)
	if !hasRule(refusal, "alter-column-set-not-null", "email") {
		t.Fatalf("the refusal did not name the column: %v", refusal.Changes)
	}
	if !strings.Contains(refusal.Error(), "1 row(s) hold NULL") {
		t.Errorf("the probe did not count the rows it was pointed at:\n%s", refusal.Error())
	}
	if got, ok := sqliteColumn(t, db, "users", "email"); !ok || got != "" {
		t.Fatalf("the refused push touched the table: email = %q, present = %v", got, ok)
	}
}
