package mysql_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bernardoforcillo/drops/mysql"
)

// blogSchema is the fixture most of these tests diff against: two
// tables, an auto-increment key, a unique column, a foreign key, a
// secondary index and a CHECK constraint.
func blogSchema(t *testing.T) *mysql.Schema {
	t.Helper()
	users := mysql.NewTable("users")
	userID := mysql.Add(users, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(users, mysql.Varchar("email", 190).NotNull().Unique())
	mysql.Add(users, mysql.Integer("age").Default("0"))
	users.AddCheck("ck_users_age", "age >= 0")

	posts := mysql.NewTable("posts")
	mysql.Add(posts, mysql.BigSerial("id").PrimaryKey())
	author := mysql.Add(posts, mysql.BigInt("author").References(userID, mysql.OnDelete("CASCADE")))
	slug := mysql.Add(posts, mysql.Varchar("slug", 190).NotNull())
	posts.AddIndex(mysql.NewIndex("idx_posts_slug", posts, slug).Unique())
	posts.AddIndex(mysql.NewIndex("idx_posts_author", posts, author))

	return mysql.NewSchema(users, posts)
}

func TestBuildSnapshotShape(t *testing.T) {
	snap := mysql.BuildSnapshot(blogSchema(t))

	users, ok := snap.Tables["users"]
	if !ok {
		t.Fatalf("users missing from snapshot: %v", snap.Tables)
	}
	// The key is table-level even though it spans one column: MySQL
	// names every key PRIMARY and reports it that way.
	if users.PrimaryKey == nil || users.PrimaryKey.Name != "PRIMARY" {
		t.Fatalf("primary key = %+v, want one named PRIMARY", users.PrimaryKey)
	}
	if got := users.PrimaryKey.Columns; len(got) != 1 || got[0] != "id" {
		t.Fatalf("primary key columns = %v", got)
	}
	if id := users.Columns["id"]; !id.AutoIncrement || !id.NotNull || id.Type != "bigint" {
		t.Fatalf("id column = %+v", id)
	}
	// A UNIQUE column is an index here, not a constraint of its own.
	uq, ok := users.Indexes["uq_users_email"]
	if !ok || !uq.IsUnique {
		t.Fatalf("unique index = %+v, want uq_users_email unique", uq)
	}
	if age := users.Columns["age"]; age.Default == nil || *age.Default != "0" {
		t.Fatalf("age default = %v", age.Default)
	}
	if ck := users.CheckConstraints["ck_users_age"]; ck == nil || ck.Value != "age >= 0" {
		t.Fatalf("check = %+v", ck)
	}

	posts := snap.Tables["posts"]
	fk, ok := posts.ForeignKeys["fk_posts_author"]
	if !ok {
		t.Fatalf("foreign keys = %v", posts.ForeignKeys)
	}
	if fk.TableTo != "users" || fk.ColumnsTo[0] != "id" || fk.OnDelete != "cascade" {
		t.Fatalf("fk = %+v", fk)
	}
	if fk.OnUpdate != "no action" {
		t.Fatalf("unset ON UPDATE = %q, want the normalised %q", fk.OnUpdate, "no action")
	}
	if len(posts.Indexes) != 2 {
		t.Fatalf("indexes = %v, want the two declared", posts.Indexes)
	}
}

func TestSnapshotRoundTripsThroughJSON(t *testing.T) {
	snap := mysql.BuildSnapshot(blogSchema(t))
	body, err := snap.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	back, err := mysql.UnmarshalSnapshot(body)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if stmts := mysql.Diff(snap, back); len(stmts) != 0 {
		t.Fatalf("a snapshot diffed against its own JSON is not empty: %v", stmts)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("snapshot is not valid JSON: %v", err)
	}
	if raw["dialect"] != "mysql" {
		t.Fatalf("dialect = %v", raw["dialect"])
	}
}

func TestDiffAgainstItselfIsEmpty(t *testing.T) {
	snap := mysql.BuildSnapshot(blogSchema(t))
	if stmts := mysql.Diff(snap, snap); len(stmts) != 0 {
		t.Fatalf("Diff(s, s) = %v, want nothing", stmts)
	}
}

func TestDiffCreatesTables(t *testing.T) {
	stmts := mysql.Diff(nil, mysql.BuildSnapshot(blogSchema(t)))
	joined := strings.Join(stmts, "\n")

	// The PRIMARY KEY is inline; every other constraint is its own
	// statement.
	if !strings.Contains(joined, "CREATE TABLE `users` (") ||
		!strings.Contains(joined, "PRIMARY KEY (`id`)") {
		t.Fatalf("CREATE TABLE missing or keyless:\n%s", joined)
	}
	if strings.Contains(joined, "FOREIGN KEY (`author`)") &&
		strings.Index(joined, "FOREIGN KEY (`author`)") < strings.Index(joined, "CREATE TABLE `users`") {
		t.Fatalf("the foreign key was emitted before the table it points at:\n%s", joined)
	}
	for _, want := range []string{
		"CREATE UNIQUE INDEX `uq_users_email` ON `users` (`email`);",
		"CREATE INDEX `idx_posts_author` ON `posts` (`author`);",
		"ALTER TABLE `users` ADD CONSTRAINT `ck_users_age` CHECK (age >= 0);",
		"ALTER TABLE `posts` ADD CONSTRAINT `fk_posts_author` FOREIGN KEY (`author`) REFERENCES `users` (`id`) ON DELETE CASCADE;",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestDiffBatchesColumnChangesIntoOneAlter(t *testing.T) {
	before := mysql.NewTable("t")
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(before, mysql.Varchar("gone", 10))
	mysql.Add(before, mysql.Integer("kept"))

	after := mysql.NewTable("t")
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Integer("kept").NotNull())
	mysql.Add(after, mysql.Varchar("added", 10))

	prev := mysql.BuildSnapshot(mysql.NewSchema(before))
	cur := mysql.BuildSnapshot(mysql.NewSchema(after))

	batched := mysql.Diff(prev, cur)
	if len(batched) != 1 {
		t.Fatalf("batched diff = %d statements, want 1:\n%s", len(batched), strings.Join(batched, "\n"))
	}
	for _, want := range []string{"DROP COLUMN `gone`", "ADD COLUMN `added`", "MODIFY COLUMN `kept` int NOT NULL"} {
		if !strings.Contains(batched[0], want) {
			t.Fatalf("missing %q in %q", want, batched[0])
		}
	}

	split := mysql.Diff(prev, cur, mysql.DiffOptions{SplitAlters: true})
	if len(split) != 3 {
		t.Fatalf("split diff = %d statements, want 3:\n%s", len(split), strings.Join(split, "\n"))
	}
	for _, s := range split {
		if strings.Count(s, "ALTER TABLE") != 1 || strings.Contains(s, ",\n") {
			t.Fatalf("split statement carries more than one action: %q", s)
		}
	}
}

// A change confined to the default must not restate the column: MODIFY
// COLUMN can rebuild the table, ALTER COLUMN … SET DEFAULT cannot.
func TestDiffUsesSetDefaultForADefaultOnlyChange(t *testing.T) {
	mk := func(def string) *mysql.Snapshot {
		tbl := mysql.NewTable("t")
		c := mysql.Add(tbl, mysql.Integer("n"))
		if def != "" {
			c.Default(def)
		}
		return mysql.BuildSnapshot(mysql.NewSchema(tbl))
	}
	stmts := mysql.Diff(mk("1"), mk("2"))
	if len(stmts) != 1 || stmts[0] != "ALTER TABLE `t` ALTER COLUMN `n` SET DEFAULT 2;" {
		t.Fatalf("changed default = %v", stmts)
	}
	stmts = mysql.Diff(mk("1"), mk(""))
	if len(stmts) != 1 || stmts[0] != "ALTER TABLE `t` ALTER COLUMN `n` DROP DEFAULT;" {
		t.Fatalf("removed default = %v", stmts)
	}
	// A default alongside anything else falls back to MODIFY, because
	// MySQL has no narrower form for the rest.
	wider := mysql.NewTable("t")
	mysql.Add(wider, mysql.Integer("n").NotNull().Default("2"))
	stmts = mysql.Diff(mk("1"), mysql.BuildSnapshot(mysql.NewSchema(wider)))
	if len(stmts) != 1 || !strings.Contains(stmts[0], "MODIFY COLUMN `n` int NOT NULL DEFAULT 2") {
		t.Fatalf("widened change = %v", stmts)
	}
}

// MODIFY restates the column in full, so every attribute the snapshot
// carries has to reach the statement or the ALTER silently drops it.
func TestModifyColumnRestatesEveryAttribute(t *testing.T) {
	before := mysql.NewTable("t")
	mysql.Add(before, mysql.Timestamp("updated", false))
	after := mysql.NewTable("t")
	mysql.Add(after, mysql.Timestamp("updated", false).
		NotNull().
		Default("CURRENT_TIMESTAMP(6)").
		OnUpdateExpr("CURRENT_TIMESTAMP(6)").
		Comment("last write"))

	stmts := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(before)),
		mysql.BuildSnapshot(mysql.NewSchema(after)))
	want := "ALTER TABLE `t` MODIFY COLUMN `updated` datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6) COMMENT 'last write';"
	if len(stmts) != 1 || stmts[0] != want {
		t.Fatalf("got %v\nwant %q", stmts, want)
	}
}

func TestDiffDropsForeignKeysBeforeTheTableTheyPointAt(t *testing.T) {
	prev := mysql.BuildSnapshot(blogSchema(t))
	// The new schema keeps posts and its foreign key column but drops
	// users entirely.
	posts := mysql.NewTable("posts")
	mysql.Add(posts, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(posts, mysql.BigInt("author"))
	slug := mysql.Add(posts, mysql.Varchar("slug", 190).NotNull())
	posts.AddIndex(mysql.NewIndex("idx_posts_slug", posts, slug).Unique())
	posts.AddIndex(mysql.NewIndex("idx_posts_author", posts, mysql.Add(posts, mysql.Integer("x"))))

	stmts := mysql.Diff(prev, mysql.BuildSnapshot(mysql.NewSchema(posts)))
	dropFK, dropTable := -1, -1
	for i, s := range stmts {
		if strings.HasPrefix(s, "ALTER TABLE `posts` DROP FOREIGN KEY") {
			dropFK = i
		}
		if strings.HasPrefix(s, "DROP TABLE `users`") {
			dropTable = i
		}
	}
	if dropFK < 0 || dropTable < 0 {
		t.Fatalf("expected both a DROP FOREIGN KEY and a DROP TABLE:\n%s", strings.Join(stmts, "\n"))
	}
	if dropFK > dropTable {
		t.Fatalf("InnoDB refuses to drop a referenced table; the FK must go first:\n%s", strings.Join(stmts, "\n"))
	}
}

// The harder half of the same rule. When the table holding the foreign
// key is itself being dropped, the reference still blocks the DROP
// TABLE of its target, and the drops are emitted in sorted-name order —
// so a parent that sorts first is dropped first and the migration stops
// there. MySQL has no DROP TABLE … CASCADE to fall back on, which is
// what drops/pg uses. See
// TestMySQLDroppingTwoTablesAtOnceSurvivesTheReference.
func TestDiffDropsForeignKeysBetweenTwoTablesBothGoing(t *testing.T) {
	parent := mysql.NewTable("aaa_parent")
	parentID := mysql.Add(parent, mysql.BigSerial("id").PrimaryKey())
	child := mysql.NewTable("zzz_child")
	mysql.Add(child, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(child, mysql.BigInt("pid").References(parentID))

	prev := mysql.BuildSnapshot(mysql.NewSchema(parent, child))
	stmts := mysql.Diff(prev, mysql.EmptySnapshot())

	dropFK, dropParent := -1, -1
	for i, s := range stmts {
		if strings.HasPrefix(s, "ALTER TABLE `zzz_child` DROP FOREIGN KEY") {
			dropFK = i
		}
		if s == "DROP TABLE `aaa_parent`;" {
			dropParent = i
		}
	}
	if dropParent < 0 {
		t.Fatalf("the parent was never dropped:\n%s", strings.Join(stmts, "\n"))
	}
	if dropFK < 0 || dropFK > dropParent {
		t.Fatalf("the reference from the table that is also going has to be cleared first, or DROP TABLE `aaa_parent` fails:\n%s",
			strings.Join(stmts, "\n"))
	}
}

// InnoDB accepts USING HASH on a CREATE INDEX and builds a B-tree, so
// the catalogue reports BTREE for an index the schema declared HASH.
// Treating that as a difference would drop and recreate the index on
// every push and never close it. See
// TestMySQLDeclaredHashIndexDoesNotChurn.
func TestDiffDoesNotChaseAnAccessMethodTheEngineDiscarded(t *testing.T) {
	declared := mysql.NewTable("t")
	c := mysql.Add(declared, mysql.Integer("n"))
	declared.AddIndex(mysql.NewIndex("idx_n", declared, c).Using("HASH"))
	desired := mysql.BuildSnapshot(mysql.NewSchema(declared))

	// What Introspect reads back afterwards: the same index, with the
	// method folded to the engine's default.
	live := mysql.BuildSnapshot(mysql.NewSchema(declared))
	live.Tables["t"].Indexes["idx_n"].Method = ""

	if stmts := mysql.Diff(live, desired); len(stmts) != 0 {
		t.Fatalf("an access method the engine discarded is not a migration:\n%s", strings.Join(stmts, "\n"))
	}
	// A method the server does report, and that differs, still is.
	live.Tables["t"].Indexes["idx_n"].Method = "RTREE"
	if stmts := mysql.Diff(live, desired); len(stmts) != 2 {
		t.Fatalf("a method the catalogue reports and that differs has to be acted on: %v", stmts)
	}
}

func TestDiffChangesIndexesByDropAndRecreate(t *testing.T) {
	mk := func(unique bool) *mysql.Snapshot {
		tbl := mysql.NewTable("t")
		c := mysql.Add(tbl, mysql.Varchar("slug", 190))
		idx := mysql.NewIndex("idx_slug", tbl, c)
		if unique {
			idx.Unique()
		}
		tbl.AddIndex(idx)
		return mysql.BuildSnapshot(mysql.NewSchema(tbl))
	}
	stmts := mysql.Diff(mk(false), mk(true))
	want := []string{
		"DROP INDEX `idx_slug` ON `t`;",
		"CREATE UNIQUE INDEX `idx_slug` ON `t` (`slug`);",
	}
	if len(stmts) != 2 || stmts[0] != want[0] || stmts[1] != want[1] {
		t.Fatalf("got %v\nwant %v", stmts, want)
	}
}

// A TEXT column cannot enter an index without a prefix length, so the
// prefix has to survive into the CREATE INDEX.
func TestIndexPrefixLengthReachesTheStatement(t *testing.T) {
	tbl := mysql.NewTable("t")
	body := mysql.Add(tbl, mysql.Text("body"))
	tbl.AddIndex(mysql.NewIndex("idx_body", tbl, body).Prefix(body, 32))

	stmts := mysql.Diff(nil, mysql.BuildSnapshot(mysql.NewSchema(tbl)))
	if !strings.Contains(strings.Join(stmts, "\n"), "CREATE INDEX `idx_body` ON `t` (`body`(32));") {
		t.Fatalf("prefix length lost:\n%s", strings.Join(stmts, "\n"))
	}
}

func TestDiffReplacesAChangedCheck(t *testing.T) {
	mk := func(expr string) *mysql.Snapshot {
		tbl := mysql.NewTable("t")
		mysql.Add(tbl, mysql.Integer("age"))
		tbl.AddCheck("ck_age", expr)
		return mysql.BuildSnapshot(mysql.NewSchema(tbl))
	}
	stmts := mysql.Diff(mk("age >= 0"), mk("age >= 18"))
	if len(stmts) != 2 ||
		stmts[0] != "ALTER TABLE `t` DROP CONSTRAINT `ck_age`;" ||
		stmts[1] != "ALTER TABLE `t` ADD CONSTRAINT `ck_age` CHECK (age >= 18);" {
		t.Fatalf("tightening a check under the same name = %v", stmts)
	}
}

// MySQL learned the portable DROP CONSTRAINT in 8.0.19; before that it
// only had DROP CHECK, which MariaDB has never accepted.
func TestDropCheckSpellingFollowsTheServer(t *testing.T) {
	mk := func(withCheck bool) *mysql.Snapshot {
		tbl := mysql.NewTable("t")
		mysql.Add(tbl, mysql.Integer("age"))
		if withCheck {
			tbl.AddCheck("ck_age", "age >= 0")
		}
		return mysql.BuildSnapshot(mysql.NewSchema(tbl))
	}
	old := mysql.Diff(mk(true), mk(false), mysql.DiffOptions{
		Server: mysql.ServerInfo{Version: "8.0.17", Major: 8, Patch: 17},
	})
	if len(old) != 1 || !strings.Contains(old[0], "DROP CHECK") {
		t.Fatalf("8.0.17 = %v, want DROP CHECK", old)
	}
	modern := mysql.Diff(mk(true), mk(false), mysql.DiffOptions{
		Server: mysql.ServerInfo{Version: "8.4.0", Major: 8, Minor: 4},
	})
	if len(modern) != 1 || !strings.Contains(modern[0], "DROP CONSTRAINT") {
		t.Fatalf("8.4 = %v, want DROP CONSTRAINT", modern)
	}
}

func TestDiffDownReversesTheMigration(t *testing.T) {
	prev := mysql.BuildSnapshot(mysql.NewSchema(mysql.NewTable("keep").
		AddCheck("ck", "1 = 1")))
	// A table with no columns is not a table; give it one.
	keep := mysql.NewTable("keep")
	mysql.Add(keep, mysql.BigSerial("id").PrimaryKey())
	prev = mysql.BuildSnapshot(mysql.NewSchema(keep))

	added := mysql.NewTable("added")
	mysql.Add(added, mysql.BigSerial("id").PrimaryKey())
	cur := mysql.BuildSnapshot(mysql.NewSchema(keep, added))

	up := mysql.Diff(prev, cur)
	down := mysql.DiffDown(prev, cur)
	if len(up) != 1 || !strings.HasPrefix(up[0], "CREATE TABLE `added`") {
		t.Fatalf("up = %v", up)
	}
	if len(down) != 1 || down[0] != "DROP TABLE `added`;" {
		t.Fatalf("down = %v", down)
	}
}

// Safe means what MySQL can express, which is CREATE TABLE and DROP
// TABLE. MariaDB accepts IF NOT EXISTS on a column too; MySQL does not,
// so drops emits it on neither.
func TestSafeOnlyTouchesTheStatementsMySQLCanGuard(t *testing.T) {
	before := mysql.NewTable("t")
	mysql.Add(before, mysql.BigSerial("id").PrimaryKey())
	after := mysql.NewTable("t")
	mysql.Add(after, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(after, mysql.Integer("n"))
	gone := mysql.NewTable("gone")
	mysql.Add(gone, mysql.BigSerial("id").PrimaryKey())

	stmts := mysql.Diff(
		mysql.BuildSnapshot(mysql.NewSchema(before, gone)),
		mysql.BuildSnapshot(mysql.NewSchema(after)),
		mysql.DiffOptions{Safe: true})
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, "DROP TABLE IF EXISTS `gone`;") {
		t.Fatalf("DROP TABLE was not guarded:\n%s", joined)
	}
	if strings.Contains(joined, "ADD COLUMN IF NOT EXISTS") {
		t.Fatalf("ADD COLUMN IF NOT EXISTS is MariaDB-only and must not be emitted:\n%s", joined)
	}
	created := mysql.Diff(nil, mysql.BuildSnapshot(mysql.NewSchema(after)), mysql.DiffOptions{Safe: true})
	if !strings.Contains(created[0], "CREATE TABLE IF NOT EXISTS") {
		t.Fatalf("CREATE TABLE was not guarded: %v", created)
	}
}

// ----------------------------------------------------------------------
// Generation
// ----------------------------------------------------------------------

func TestGenerateMigrationWritesTheDrizzleLayout(t *testing.T) {
	files := map[string][]byte{}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema:   blogSchema(t),
		Dir:      "migrations",
		Name:     "init",
		FS:       fstest.MapFS{},
		Write:    func(rel string, data []byte) error { files[rel] = data; return nil },
		Now:      func() int64 { return 1700000000000 },
		WithDown: true,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res.Tag != "0000_init" {
		t.Fatalf("tag = %q", res.Tag)
	}
	for _, want := range []string{"0000_init.sql", "0000_init.down.sql", "meta/0000_snapshot.json", "meta/_journal.json"} {
		if _, ok := files[want]; !ok {
			t.Fatalf("%q was not written; got %v", want, files)
		}
	}
	if !strings.Contains(res.SQL, "--> statement-breakpoint") {
		t.Fatalf("statements were not separated:\n%s", res.SQL)
	}
	var journal struct {
		Version string
		Dialect string
		Entries []struct {
			Idx int
			Tag string
		}
	}
	if err := json.Unmarshal(files["meta/_journal.json"], &journal); err != nil {
		t.Fatalf("journal: %v", err)
	}
	if journal.Dialect != "mysql" || len(journal.Entries) != 1 || journal.Entries[0].Tag != "0000_init" {
		t.Fatalf("journal = %+v", journal)
	}
	if !strings.Contains(res.DownSQL, "DROP TABLE `users`;") {
		t.Fatalf("down SQL = %q", res.DownSQL)
	}
}

func TestGenerateMigrationIsANoOpWhenNothingChanged(t *testing.T) {
	schema := blogSchema(t)
	snapshot, err := mysql.BuildSnapshot(schema).Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fsys := fstest.MapFS{
		"migrations/meta/0000_snapshot.json": {Data: snapshot},
		"migrations/meta/_journal.json": {Data: []byte(
			`{"version":"5","dialect":"mysql","entries":[{"idx":0,"version":"5","when":1,"tag":"0000_init","breakpoints":true}]}`)},
	}
	res, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: schema,
		Dir:    "migrations",
		FS:     fsys,
		Write:  func(string, []byte) error { t.Fatal("a no-op migration wrote a file"); return nil },
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !res.NoOp {
		t.Fatalf("second generate against an unchanged schema = %+v", res)
	}
}

func TestGenerateMigrationRefusesAForeignJournal(t *testing.T) {
	fsys := fstest.MapFS{
		"migrations/meta/_journal.json": {Data: []byte(`{"version":"7","dialect":"postgresql","entries":[]}`)},
	}
	_, err := mysql.GenerateMigration(mysql.GenerateOptions{
		Schema: blogSchema(t),
		Dir:    "migrations",
		FS:     fsys,
		Write:  func(string, []byte) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "postgresql") {
		t.Fatalf("err = %v, want a complaint about the dialect", err)
	}
}

// ----------------------------------------------------------------------
// Safety
// ----------------------------------------------------------------------

func TestAnalyzeMigrationGradesTheDestructiveStatements(t *testing.T) {
	sql := strings.Join([]string{
		"ALTER TABLE `users` DROP COLUMN `email`;",
		"DROP TABLE `posts`;",
		"ALTER TABLE `users` MODIFY COLUMN `age` bigint NOT NULL;",
		"CREATE INDEX `idx` ON `users` (`age`);",
	}, "\n--> statement-breakpoint\n")

	byRule := map[string]mysql.SafetyWarning{}
	for _, w := range mysql.AnalyzeMigration(sql) {
		byRule[w.Rule] = w
	}
	if w, ok := byRule["drop-column"]; !ok || w.Severity != mysql.SeverityError {
		t.Fatalf("drop-column = %+v; on MySQL it is unrecoverable, not reviewable", w)
	}
	if w, ok := byRule["drop-table"]; !ok || w.Severity != mysql.SeverityError {
		t.Fatalf("drop-table = %+v", w)
	}
	if _, ok := byRule["modify-column"]; !ok {
		t.Fatalf("MODIFY COLUMN went unreported: %v", byRule)
	}
	if w, ok := byRule["copy-table-alter"]; !ok || w.Severity != mysql.SeverityWarn {
		t.Fatalf("the table-copying ALTER went unreported: %+v", w)
	}
	// The migration-level finding: four statements, no transaction.
	if w, ok := byRule["no-transactional-ddl"]; !ok || !strings.Contains(w.Message, "4 statements") {
		t.Fatalf("no-transactional-ddl = %+v", w)
	}
}

func TestAnalyzeMigrationHonoursIgnore(t *testing.T) {
	sql := "DROP TABLE `posts`;"
	if got := mysql.AnalyzeMigration(sql); len(got) != 1 {
		t.Fatalf("got %d warnings, want 1: %+v", len(got), got)
	}
	if got := mysql.AnalyzeMigration(sql, mysql.SafetyOptions{Ignore: []string{"drop-table"}}); len(got) != 0 {
		t.Fatalf("ignored rule still reported: %+v", got)
	}
}

func TestClassifyDDL(t *testing.T) {
	cases := []struct {
		stmt string
		want mysql.DDLAlgorithm
	}{
		{"ALTER TABLE `t` ADD COLUMN `n` int NULL", mysql.AlgorithmInstant},
		{"ALTER TABLE `t` ALTER COLUMN `n` SET DEFAULT 1", mysql.AlgorithmInstant},
		{"ALTER TABLE `t` ADD UNIQUE KEY `u` (`n`)", mysql.AlgorithmInplace},
		{"ALTER TABLE `t` DROP COLUMN `n`", mysql.AlgorithmInplace},
		{"ALTER TABLE `t` MODIFY COLUMN `n` bigint NOT NULL", mysql.AlgorithmCopy},
		{"ALTER TABLE `t` DROP PRIMARY KEY", mysql.AlgorithmCopy},
		{"ALTER TABLE `t` ENGINE=InnoDB", mysql.AlgorithmCopy},
		// Neither server can add a CHECK in place. MariaDB 10.11
		// answers error 1845 and MySQL documents the rebuild; see
		// TestMySQLServerAgreesWithTheDDLClassification, which asks.
		{"ALTER TABLE `t` ADD CONSTRAINT `c` CHECK (`n` >= 0)", mysql.AlgorithmCopy},
		{"CREATE INDEX `i` ON `t` (`n`)", mysql.AlgorithmUnknown},
		// One copying action in a batch makes the whole statement one.
		{"ALTER TABLE `t` ADD COLUMN `a` int NULL,\n\tMODIFY COLUMN `n` bigint NOT NULL", mysql.AlgorithmCopy},
	}
	for _, c := range cases {
		if got := mysql.ClassifyDDL(c.stmt); got != c.want {
			t.Errorf("ClassifyDDL(%q) = %v, want %v", c.stmt, got, c.want)
		}
	}
}

// A statement that already names an algorithm is the author's to
// answer for: the server will refuse it if the algorithm is impossible.
func TestCopyTableRuleDefersToAnExplicitAlgorithm(t *testing.T) {
	stmt := "ALTER TABLE `t` MODIFY COLUMN `n` bigint NOT NULL, ALGORITHM=INPLACE, LOCK=NONE"
	for _, w := range mysql.AnalyzeStatements([]string{stmt}) {
		if w.Rule == "copy-table-alter" {
			t.Fatalf("still warned about an explicit algorithm: %+v", w)
		}
	}
}

func TestServerInfoCapabilities(t *testing.T) {
	maria := mysql.ServerInfo{MariaDB: true, Major: 10, Minor: 11, Patch: 14}
	if !maria.SupportsCheckConstraints() || !maria.SupportsDropConstraint() {
		t.Fatalf("MariaDB 10.11 = %+v", maria)
	}
	old := mysql.ServerInfo{Major: 5, Minor: 7, Patch: 44}
	if old.SupportsCheckConstraints() {
		t.Fatalf("MySQL 5.7 parses CHECK constraints and discards them; it must not report support")
	}
	if !(mysql.ServerInfo{Major: 8, Minor: 0, Patch: 16}).SupportsCheckConstraints() {
		t.Fatalf("MySQL 8.0.16 enforces CHECK constraints")
	}
	if (mysql.ServerInfo{Major: 8, Minor: 0, Patch: 18}).SupportsDropConstraint() {
		t.Fatalf("MySQL learned DROP CONSTRAINT in 8.0.19")
	}
}

func TestPushRequiresASchema(t *testing.T) {
	if _, err := mysql.Push(t.Context(), nil, nil); err != mysql.ErrSchemaRequired {
		t.Fatalf("err = %v, want ErrSchemaRequired", err)
	}
}

// ExampleDiff shows the shape of a first migration: the primary key
// rides inside the CREATE TABLE, because an AUTO_INCREMENT column has
// to be part of a key from the moment it exists, and every other
// constraint follows as its own statement.
func ExampleDiff() {
	users := mysql.NewTable("users")
	mysql.Add(users, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(users, mysql.Varchar("email", 190).NotNull().Unique())
	users.AddCheck("ck_users_email", "email <> ''")

	for _, stmt := range mysql.Diff(nil, mysql.BuildSnapshot(mysql.NewSchema(users))) {
		fmt.Println(stmt)
	}
	// Output:
	// CREATE TABLE `users` (
	// 	`email` varchar(190) NOT NULL,
	// 	`id` bigint NOT NULL AUTO_INCREMENT,
	// 	PRIMARY KEY (`id`)
	// );
	// CREATE UNIQUE INDEX `uq_users_email` ON `users` (`email`);
	// ALTER TABLE `users` ADD CONSTRAINT `ck_users_email` CHECK (email <> '');
}
