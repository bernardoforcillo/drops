package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mysql"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"
)

// Query tags are application-supplied strings that land inside a SQL
// comment, which makes them an injection site: a value carrying */
// ends the comment and hands the rest of itself to the parser. These
// tests are the proof that it cannot, and they are here rather than in
// a unit test because "the server parses it" is a claim only a server
// can settle. The payload tries to drop the table the same statement
// is reading, so a failure of the encoding is a failure of the test in
// the most visible way available.
//
// The other half — that the comment survives to the server intact
// rather than being silently mangled — is checked by asking each server
// what statement it is currently running.

// hostileTags returns tags carrying every character that could end the
// comment early, plus a multi-byte rune to pin the byte-wise encoding.
func hostileTags(table string) []drops.Tag {
	return []drops.Tag{
		{Key: "controller", Value: "*/ ; DROP TABLE " + table + "; --"},
		{Key: "action", Value: "sh'ow\nnext"},
		{Key: "service", Value: "café/checkout"},
	}
}

// ----------------------------------------------------------------------
// PostgreSQL
// ----------------------------------------------------------------------

func TestPGQueryTagCannotEscapeTheComment(t *testing.T) {
	db := openPG(t)
	name := integration.UniqueName(t, "qtag")
	tbl := pg.NewTable(name)
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("label").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	plain := context.Background()
	if _, err := db.Exec(plain, `INSERT INTO "`+name+`" ("label") VALUES ($1)`, "kept"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx := drops.WithQueryTags(plain, hostileTags(name)...)

	// The payload is aimed at this very table, and the statement binds
	// a parameter so the trailing comment also has to leave the
	// placeholder numbering alone.
	var label string
	rows, err := db.Query(ctx, `SELECT "label" FROM "`+name+`" WHERE "id" = $1`, 1)
	if err != nil {
		t.Fatalf("PostgreSQL rejected a tagged statement: %v", err)
	}
	if !rows.Next() {
		_ = rows.Close()
		t.Fatal("tagged statement returned no rows")
	}
	if err := rows.Scan(&label); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_ = rows.Close()
	if label != "kept" {
		t.Fatalf("label = %q, want %q", label, "kept")
	}

	// If any of that had been parsed as SQL the table would be gone.
	var n int
	if err := scanOnePG(t, db, plain,
		`SELECT count(*)::int FROM information_schema.tables WHERE table_name = $1`, &n, name); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the tag payload executed: table %s is gone", name)
	}
}

// The comment is only worth anything if it reaches the server, so ask
// the server. pg_stat_activity.query is the statement text of the
// query currently running on this backend — comment included.
func TestPGQueryTagReachesTheServerVerbatim(t *testing.T) {
	db := openPG(t)
	ctx := drops.WithQueryTags(context.Background(),
		drops.Tag{Key: "controller", Value: "users"},
		drops.Tag{Key: "action", Value: "show"},
	)

	var seen string
	if err := scanOnePG(t, db, ctx,
		`SELECT query FROM pg_stat_activity WHERE pid = pg_backend_pid()`, &seen); err != nil {
		t.Fatal(err)
	}
	const want = `/*action='show',controller='users'*/`
	if !strings.HasSuffix(strings.TrimSpace(seen), want) {
		t.Fatalf("PostgreSQL saw %q, which does not end in %s", seen, want)
	}
}

// scanOnePG runs a one-row, one-column query and scans it into dest.
func scanOnePG(t *testing.T, db *pg.DB, ctx context.Context, sql string, dest any, args ...any) error {
	t.Helper()
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return rows.Err()
	}
	return rows.Scan(dest)
}

// ----------------------------------------------------------------------
// MySQL / MariaDB
// ----------------------------------------------------------------------

func TestMySQLQueryTagCannotEscapeTheComment(t *testing.T) {
	db := openMySQL(t)
	name := integration.UniqueName(t, "qtag")
	plain := context.Background()

	if _, err := db.Exec(plain, "DROP TABLE IF EXISTS `"+name+"`"); err != nil {
		t.Fatalf("pre-drop: %v", err)
	}
	if _, err := db.Exec(plain,
		"CREATE TABLE `"+name+"` (id BIGINT PRIMARY KEY, label TEXT NOT NULL)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(plain, "DROP TABLE IF EXISTS `"+name+"`") })
	if _, err := db.Exec(plain, "INSERT INTO `"+name+"` (id, label) VALUES (1, ?)", "kept"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx := drops.WithQueryTags(plain, hostileTags(name)...)

	var label string
	if err := scanOneMySQL(t, db, ctx,
		"SELECT label FROM `"+name+"` WHERE id = ?", &label, 1); err != nil {
		t.Fatalf("MariaDB rejected a tagged statement: %v", err)
	}
	if label != "kept" {
		t.Fatalf("label = %q, want %q", label, "kept")
	}

	var n int
	if err := scanOneMySQL(t, db, plain,
		"SELECT count(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?",
		&n, name); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the tag payload executed: table %s is gone", name)
	}
}

// information_schema.processlist reports the statement this connection
// is running, which is how we see what the server received. MySQL is
// the dialect where a leading comment would have been actively
// dangerous — /*! and /*+ are executable there — so it is also the one
// worth confirming the comment landed at the end.
func TestMySQLQueryTagReachesTheServerVerbatim(t *testing.T) {
	db := openMySQL(t)
	ctx := drops.WithQueryTags(context.Background(),
		drops.Tag{Key: "controller", Value: "users"},
		drops.Tag{Key: "action", Value: "show"},
	)

	var seen string
	if err := scanOneMySQL(t, db, ctx,
		"SELECT info FROM information_schema.processlist WHERE id = CONNECTION_ID()", &seen); err != nil {
		t.Fatal(err)
	}
	const want = `/*action='show',controller='users'*/`
	if !strings.HasSuffix(strings.TrimSpace(seen), want) {
		t.Fatalf("MariaDB saw %q, which does not end in %s", seen, want)
	}
}

func scanOneMySQL(t *testing.T, db *mysql.DB, ctx context.Context, sql string, dest any, args ...any) error {
	t.Helper()
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return rows.Err()
	}
	return rows.Scan(dest)
}

// ----------------------------------------------------------------------
// SQLite
// ----------------------------------------------------------------------

// SQLite has no view of the statement it is running, so the hook
// stands in for it: what the hook reports is the exact string handed
// to the driver. Without that check the test would pass just as well
// with tagging switched off entirely, and prove nothing about it.
func TestSQLiteQueryTagCannotEscapeTheComment(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	var sent []string
	db := sqlite.New(stdlib.New(sqlDB)).WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.SQL != "" {
			sent = append(sent, e.SQL)
		}
	})

	plain := context.Background()
	const name = "qtag_sqlite"
	if _, err := db.Exec(plain, `CREATE TABLE "`+name+`" (id INTEGER PRIMARY KEY, label TEXT NOT NULL)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(plain, `INSERT INTO "`+name+`" (id, label) VALUES (1, ?)`, "kept"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ctx := drops.WithQueryTags(plain, hostileTags(name)...)

	sent = sent[:0]
	rows, err := db.Query(ctx, `SELECT "label" FROM "`+name+`" WHERE "id" = ?`, 1)
	if err != nil {
		t.Fatalf("SQLite rejected a tagged statement: %v", err)
	}
	if len(sent) != 1 || !strings.HasSuffix(sent[0], "*/") {
		t.Fatalf("the statement reached the driver untagged, so the payload was never tested: %q", sent)
	}
	var label string
	if !rows.Next() {
		_ = rows.Close()
		t.Fatal("tagged statement returned no rows")
	}
	if err := rows.Scan(&label); err != nil {
		t.Fatalf("scan: %v", err)
	}
	_ = rows.Close()
	if label != "kept" {
		t.Fatalf("label = %q, want %q", label, "kept")
	}

	var n int
	trows, err := db.Query(plain,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name)
	if err != nil {
		t.Fatal(err)
	}
	if trows.Next() {
		if err := trows.Scan(&n); err != nil {
			t.Fatal(err)
		}
	}
	_ = trows.Close()
	if n != 1 {
		t.Fatalf("the tag payload executed: table %s is gone", name)
	}
}

// A tagged DDL statement is the case where an early comment
// terminator would be least visible — nothing scans a result set
// afterwards — so run one and then use the table it made.
func TestSQLiteTaggedDDLStillRuns(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	var sent []string
	db := sqlite.New(stdlib.New(sqlDB)).WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.SQL != "" {
			sent = append(sent, e.SQL)
		}
	})

	ctx := drops.WithQueryTags(context.Background(), hostileTags("ddl_target")...)
	if _, err := db.Exec(ctx, `CREATE TABLE "ddl_target" (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("SQLite rejected tagged DDL: %v", err)
	}
	if len(sent) != 1 || !strings.HasSuffix(sent[0], "*/") {
		t.Fatalf("the DDL reached the driver untagged, so the payload was never tested: %q", sent)
	}
	if _, err := db.Exec(ctx, `INSERT INTO "ddl_target" (id) VALUES (?)`, 7); err != nil {
		t.Fatalf("the table the tagged DDL created is not usable: %v", err)
	}
}
