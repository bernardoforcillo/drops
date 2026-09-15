package integration_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
)

// Everything ported into drops/mysql in one pass, put to a real server.
//
// A port that compiles proves nothing about the SQL it emits: the same
// pass shipped a UUID default built out of SQLite's randomblob() and an
// EXPLAIN QUERY PLAN, neither of which MySQL has. Both were found here
// rather than by a user, and this file is what keeps the next one from
// getting further.

// The mixins and templates emit DDL the server has to accept.
func TestMySQLTemplateDDLIsAccepted(t *testing.T) {
	db := openMySQL(t)

	uuidTbl := mysql.NewTable(integration.UniqueName(t, "tpl_uuid"))
	mysql.UUIDPrimaryKey(uuidTbl)
	dropMySQL(t, db, uuidTbl)
	execMySQL(t, db, mysql.CreateTable(uuidTbl))

	// And the default actually generates one.
	if _, err := db.Exec(context.Background(),
		"INSERT INTO `"+uuidTbl.Name()+"` () VALUES ()"); err != nil {
		t.Fatalf("insert relying on the UUID default: %v", err)
	}
	rows, err := db.Query(context.Background(), "SELECT `id` FROM `"+uuidTbl.Name()+"`")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	var id string
	if rows.Next() {
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("the default did not produce a UUID: %q", id)
	}
}

func TestMySQLTimestampsMixinDDLIsAccepted(t *testing.T) {
	db := openMySQL(t)
	tbl := mysql.NewTable(integration.UniqueName(t, "tpl_ts"))
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	mysql.ApplyMixins(tbl, &mysql.TimestampsMixin{})
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	if _, err := db.Exec(context.Background(),
		"INSERT INTO `"+tbl.Name()+"` () VALUES ()"); err != nil {
		t.Fatalf("insert relying on the timestamp defaults: %v", err)
	}
}

// Explain, which is written for MySQL rather than ported: the columns
// it scans are the server's, so this is the test that says whether it
// read them.
func TestMySQLExplainReadsTheRealPlan(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "ex_users")
	tbl := mysql.NewTable(name)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	email := mysql.Add(tbl, mysql.Varchar("email", 255).NotNull())
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))
	for i := 1; i <= 20; i++ {
		if _, err := db.Insert(tbl).
			Row(id.Val(int64(i)), email.Val(strings.Repeat("a", i)+"@x.com")).
			Exec(ctx); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	// No index on email: a full scan, and the plan has to say so.
	plan, err := mysql.Explain(db, ctx,
		"SELECT * FROM `"+name+"` WHERE `email` = ?", "a@x.com")
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("EXPLAIN returned no rows — the columns were not read")
	}
	if got := plan.SeqScans(); len(got) == 0 {
		t.Errorf("an unindexed predicate did not report a scan: %s", plan)
	}

	// By primary key it uses one, and the fingerprint differs from the
	// scan's — which is the whole point of storing one.
	byKey, err := mysql.Explain(db, ctx, "SELECT * FROM `"+name+"` WHERE `id` = ?", int64(1))
	if err != nil {
		t.Fatalf("Explain by key: %v", err)
	}
	if idx := byKey.UsedIndexes(); len(idx) == 0 {
		t.Errorf("a primary-key lookup reported no index: %s", byKey)
	}
	if byKey.Fingerprint() == plan.Fingerprint() {
		t.Error("two different plans share a fingerprint")
	}
	// And it is stable: the same query twice is the same fingerprint.
	again, err := mysql.Explain(db, ctx, "SELECT * FROM `"+name+"` WHERE `id` = ?", int64(1))
	if err != nil {
		t.Fatalf("Explain again: %v", err)
	}
	if again.Fingerprint() != byKey.Fingerprint() {
		t.Error("the same plan fingerprinted differently twice")
	}
}

// The statement hooks, which had to be wired into three builders here
// rather than copied: a hook that nothing calls is the failure this
// package already shipped once with relations.
func TestMySQLHooksReachTheServer(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "hk_rows")
	tbl := mysql.NewTable(name)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	note := mysql.Add(tbl, mysql.Varchar("note", 255).NotNull())
	stamp := mysql.Add(tbl, mysql.Varchar("stamp", 255).NotNull())
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	// An INSERT hook fills a column the caller never binds.
	tbl.OnInsert(mysql.InsertHookFunc(func(c *mysql.InsertHookCtx) {
		if !c.Has(stamp.Column) {
			c.Set(stamp.Val("inserted"))
		}
	}))
	if _, err := db.Insert(tbl).Row(id.Val(1), note.Val("hello")).Exec(ctx); err != nil {
		t.Fatalf("insert with a hook: %v", err)
	}
	if got := scalarString(t, db, "SELECT `stamp` FROM `"+name+"` WHERE `id` = 1"); got != "inserted" {
		t.Errorf("the INSERT hook did not reach the server: stamp = %q", got)
	}

	// An UPDATE hook does the same for the SET list.
	tbl.OnUpdate(mysql.UpdateHookFunc(func(c *mysql.UpdateHookCtx) {
		if !c.Has(stamp.Column) {
			c.Set(stamp.Val("updated"))
		}
	}))
	if _, err := db.Update(tbl).Set(note.Val("bye")).Where(id.Eq(1)).Exec(ctx); err != nil {
		t.Fatalf("update with a hook: %v", err)
	}
	if got := scalarString(t, db, "SELECT `stamp` FROM `"+name+"` WHERE `id` = 1"); got != "updated" {
		t.Errorf("the UPDATE hook did not reach the server: stamp = %q", got)
	}
	// The caller's own value still wins over the hook's.
	if _, err := db.Update(tbl).Set(stamp.Val("mine")).Where(id.Eq(1)).Exec(ctx); err != nil {
		t.Fatalf("update binding the hooked column: %v", err)
	}
	if got := scalarString(t, db, "SELECT `stamp` FROM `"+name+"` WHERE `id` = 1"); got != "mine" {
		t.Errorf("the hook overrode the caller: stamp = %q", got)
	}
}

// SoftDeleteMixin rewrites the DELETE into an UPDATE, which is a
// DeleteHook replacing the statement — the third builder the hooks had
// to reach, and the one whose wiring is hardest to prove from a render
// test.
//
// The mixin, not SoftDelete: SoftDelete alone registers the
// "deletedAt IS NULL" filter, so a DELETE really deletes and the filter
// only hides rows something else marked. The mixin adds the rewrite on
// top. Getting that pair the wrong way round is easy — this test was
// written against SoftDelete first and reported the rewrite missing.
func TestMySQLSoftDeleteMixinRewritesTheDelete(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "sd_rows")
	tbl := mysql.NewTable(name)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	mysql.ApplyMixins(tbl, &mysql.SoftDeleteMixin{})
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	if _, err := db.Insert(tbl).Row(id.Val(1)).Row(id.Val(2)).Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.Delete(tbl).Where(id.Eq(1)).Exec(ctx); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// The row is still there, and the filter hides it.
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM `"+name+"`"); n != 2 {
		t.Errorf("the soft delete removed the row: %d rows left", n)
	}
	var visible []struct{ ID int64 }
	if err := db.Select(id).From(tbl).All(ctx, &visible); err != nil {
		t.Fatalf("select: %v", err)
	}
	if len(visible) != 1 || visible[0].ID != 2 {
		t.Errorf("the soft-delete filter did not hide the row: %+v", visible)
	}
	// Unscoped sees both, and hard-deletes when asked.
	var all []struct{ ID int64 }
	if err := db.Select(id).From(tbl).Unscoped().All(ctx, &all); err != nil {
		t.Fatalf("unscoped select: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("Unscoped did not see the soft-deleted row: %+v", all)
	}
	if _, err := db.Delete(tbl).Unscoped().Where(id.Eq(1)).Exec(ctx); err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	if n := scalarInt(t, db, "SELECT COUNT(*) FROM `"+name+"`"); n != 1 {
		t.Errorf("Unscoped did not hard-delete: %d rows left", n)
	}
}

// PII travels to the server as the real value and to a hook as the
// marker. Both halves matter: redact too little and the password is in
// the log, too much and it is in the database.
func TestMySQLPIIRedactsTheLogAndNotTheWrite(t *testing.T) {
	db := openMySQL(t)
	ctx := context.Background()

	name := integration.UniqueName(t, "pii_users")
	tbl := mysql.NewTable(name)
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	secret := mysql.Add(tbl, mysql.Varchar("secret", 255).NotNull().AsPII())
	dropMySQL(t, db, tbl)
	execMySQL(t, db, mysql.CreateTable(tbl))

	var seen []any
	hooked := db.WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.Kind == "exec" {
			seen = append(seen, e.Args...)
		}
	})
	if _, err := hooked.Insert(tbl).Row(id.Val(1), secret.Val("hunter2")).Exec(ctx); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := scalarString(t, db, "SELECT `secret` FROM `"+name+"` WHERE `id` = 1"); got != "hunter2" {
		t.Errorf("the server stored the redaction marker instead of the value: %q", got)
	}
	var redacted bool
	for _, a := range seen {
		if mysql.IsPII(a) {
			redacted = true
		}
	}
	if !redacted {
		t.Errorf("the hook saw the raw value rather than the marker: %v", seen)
	}
}

func scalarString(t *testing.T, db *mysql.DB, q string) string {
	t.Helper()
	rows, err := db.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out string
	if rows.Next() {
		if err := rows.Scan(&out); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	return out
}

func scalarInt(t *testing.T, db *mysql.DB, q string) int64 {
	t.Helper()
	rows, err := db.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	var out int64
	if rows.Next() {
		if err := rows.Scan(&out); err != nil {
			t.Fatalf("scan: %v", err)
		}
	}
	return out
}
