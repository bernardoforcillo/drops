package integration_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/sqlite"
	"github.com/bernardoforcillo/drops/stdlib"
)

// querytag_test.go proves the encoding survives the payload an attacker
// would reach for. These are the bytes nobody reaches for and everybody
// eventually hits: a NUL from a truncated C string, a lone 0xFF from a
// mis-decoded Latin-1 header, a value long enough to be a request body.
// They belong on a live server for the same reason the hostile payload
// does — whether a statement parses is the server's opinion, not ours.
//
// Each test also asserts the tag arrived, because a test that passes
// with tagging switched off proves nothing about tagging.

// stressTags is every byte class that could end the comment, plus the
// ones that could end the statement.
func stressTags() []drops.Tag {
	return []drops.Tag{
		{Key: "a_nul", Value: "before\x00after"},
		{Key: "b_bad_utf8", Value: "\xff\xfe\x80raw"},
		{Key: "c_close", Value: "*/ ; DROP TABLE stress; --"},
		{Key: "d_quote", Value: "it's \"q\" `b`"},
		{Key: "e_crlf", Value: "one\ntwo\rthree"},
		{Key: "f_exec", Value: "/*!40101 x*/"},
		{Key: "g_percent", Value: "100%00"},
	}
}

// The comment the first six of those render to. The seventh, the long
// one, is left out of the reach checks: a server's view of the running
// statement is length-capped (track_activity_query_size on PG), so
// asking for it back would test the cap, not the encoding.
const stressComment = `/*a_nul='before%00after',b_bad_utf8='%FF%FE%80raw',` +
	`c_close='%2A%2F%20%3B%20DROP%20TABLE%20stress%3B%20--',` +
	`d_quote='it%27s%20%22q%22%20%60b%60',e_crlf='one%0Atwo%0Dthree',` +
	`f_exec='%2F%2A%2140101%20x%2A%2F',g_percent='100%2500'*/`

func longTag() drops.Tag {
	return drops.Tag{Key: "h_long", Value: strings.Repeat("A", 64*1024)}
}

// ----------------------------------------------------------------------
// PostgreSQL
// ----------------------------------------------------------------------

func TestPGQueryTagSurvivesEveryHostileByte(t *testing.T) {
	db := openPG(t)

	// A 64 KiB tag on top of the hostile bytes: nothing in the
	// encoding is length-sensitive, and a statement that grew past a
	// buffer somewhere would say so here.
	ctx := drops.WithQueryTags(context.Background(), append(stressTags(), longTag())...)
	var n int
	if err := scanOnePG(t, db, ctx, `SELECT 41::int + $1::int`, &n, 1); err != nil {
		t.Fatalf("PostgreSQL rejected a statement tagged with hostile bytes: %v", err)
	}
	if n != 42 {
		t.Fatalf("n = %d, want 42 — the tagged statement did not mean what it meant", n)
	}

	// And the encoded form is what the server received, byte for byte.
	short := drops.WithQueryTags(context.Background(), stressTags()...)
	var seen string
	if err := scanOnePG(t, db, short,
		`SELECT query FROM pg_stat_activity WHERE pid = pg_backend_pid()`, &seen); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(seen), stressComment) {
		t.Fatalf("PostgreSQL saw\n %q\nwhich does not end in\n %s", seen, stressComment)
	}
	// Byte-wise, not rune-wise: 0xFF is not valid UTF-8, which is the
	// whole point of putting it in a tag.
	if strings.IndexByte(seen, 0x00) >= 0 || strings.IndexByte(seen, 0xff) >= 0 {
		t.Fatalf("a raw control byte reached the server: %q", seen)
	}
}

// The two statement endings that meet the appended comment: a
// semicolon, which PostgreSQL's extended protocol counts commands
// around, and a line comment, which would swallow anything put on its
// line.
func TestPGTaggedStatementEndings(t *testing.T) {
	db := openPG(t)
	ctx := drops.WithQueryTag(context.Background(), "controller", "users")

	var n int
	if err := scanOnePG(t, db, ctx, `SELECT 7::int;`, &n); err != nil {
		t.Fatalf("PostgreSQL rejected a tagged statement ending in a semicolon: %v", err)
	}
	if n != 7 {
		t.Fatalf("n = %d, want 7", n)
	}

	var seen string
	if err := scanOnePG(t, db, ctx,
		"SELECT query FROM pg_stat_activity WHERE pid = pg_backend_pid() -- trailing\n", &seen); err != nil {
		t.Fatalf("PostgreSQL rejected a tagged statement ending in a line comment: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(seen), `/*controller='users'*/`) {
		t.Fatalf("the line comment swallowed the tag; server saw %q", seen)
	}
}

// Writes happen inside transactions, and a transaction runs through a
// tx-bound DB. If that path skipped tagging then reads would be
// attributable and writes would not.
func TestPGQueryTagInsideTransaction(t *testing.T) {
	db := openPG(t)
	ctx := drops.WithQueryTag(context.Background(), "controller", "users")

	var seen string
	err := db.InTx(ctx, func(tx *pg.DB) error {
		return scanOnePG(t, tx, ctx,
			`SELECT query FROM pg_stat_activity WHERE pid = pg_backend_pid()`, &seen)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(seen), `/*controller='users'*/`) {
		t.Fatalf("a statement inside a transaction was untagged: %q", seen)
	}
}

// Every other live test here tags a raw statement. The statement worth
// attributing is the one a builder rendered — it is the one nobody can
// recognise in a slow-query log — so run one against the server and
// check both that it carried the tag and that it still wrote the row.
func TestPGBuilderStatementIsTaggedOnTheServer(t *testing.T) {
	db := openPG(t)
	name := integration.UniqueName(t, "qtagb")
	tbl := pg.NewTable(name)
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	label := pg.Add(tbl, pg.Text("label").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))

	var sent string
	tagged := db.WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.Kind == "exec" && strings.Contains(e.SQL, "INSERT") {
			sent = e.SQL
		}
	})
	ctx := drops.WithQueryTag(context.Background(), "controller", "users")
	if _, err := tagged.Insert(tbl).Row(label.Val("x")).Exec(ctx); err != nil {
		t.Fatalf("PostgreSQL rejected a tagged builder INSERT: %v", err)
	}
	if !strings.HasSuffix(sent, `/*controller='users'*/`) {
		t.Fatalf("a builder-rendered INSERT reached the server untagged: %q", sent)
	}
	var n int
	if err := scanOnePG(t, db, context.Background(),
		`SELECT count(*)::int FROM "`+name+`"`, &n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the tagged INSERT wrote %d rows, want 1", n)
	}
}

// ----------------------------------------------------------------------
// MySQL / MariaDB
// ----------------------------------------------------------------------

func TestMySQLQueryTagSurvivesEveryHostileByte(t *testing.T) {
	db := openMySQL(t)

	ctx := drops.WithQueryTags(context.Background(), append(stressTags(), longTag())...)
	var n int
	if err := scanOneMySQL(t, db, ctx, "SELECT 41 + ?", &n, 1); err != nil {
		t.Fatalf("MariaDB rejected a statement tagged with hostile bytes: %v", err)
	}
	if n != 42 {
		t.Fatalf("n = %d, want 42 — the tagged statement did not mean what it meant", n)
	}

	short := drops.WithQueryTags(context.Background(), stressTags()...)
	var seen string
	if err := scanOneMySQL(t, db, short,
		"SELECT info FROM information_schema.processlist WHERE id = CONNECTION_ID()", &seen); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(seen), stressComment) {
		t.Fatalf("MariaDB saw\n %q\nwhich does not end in\n %s", seen, stressComment)
	}
	// Byte-wise, not rune-wise: 0xFF is not valid UTF-8, which is the
	// whole point of putting it in a tag.
	if strings.IndexByte(seen, 0x00) >= 0 || strings.IndexByte(seen, 0xff) >= 0 {
		t.Fatalf("a raw control byte reached the server: %q", seen)
	}
}

func TestMySQLTaggedStatementEndings(t *testing.T) {
	db := openMySQL(t)
	ctx := drops.WithQueryTag(context.Background(), "controller", "users")

	var n int
	if err := scanOneMySQL(t, db, ctx, "SELECT 7;", &n); err != nil {
		t.Fatalf("MariaDB rejected a tagged statement ending in a semicolon: %v", err)
	}
	if n != 7 {
		t.Fatalf("n = %d, want 7", n)
	}

	var seen string
	if err := scanOneMySQL(t, db, ctx,
		"SELECT info FROM information_schema.processlist WHERE id = CONNECTION_ID() -- trailing\n",
		&seen); err != nil {
		t.Fatalf("MariaDB rejected a tagged statement ending in a line comment: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(seen), `/*controller='users'*/`) {
		t.Fatalf("the line comment swallowed the tag; server saw %q", seen)
	}

	// # is MySQL's other line-comment opener, and the newline rule in
	// appendTagComment only knows about --. The tag therefore lands
	// inside the # comment. It still reaches the statement text the
	// server records, which is where attribution is read from, so this
	// pins the behaviour rather than calling it wrong.
	if err := scanOneMySQL(t, db, ctx,
		"SELECT info FROM information_schema.processlist WHERE id = CONNECTION_ID() # trailing\n",
		&seen); err != nil {
		t.Fatalf("MariaDB rejected a tagged statement ending in a # comment: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(seen), `/*controller='users'*/`) {
		t.Fatalf("the tag is missing from the statement MariaDB recorded: %q", seen)
	}
}

// ----------------------------------------------------------------------
// SQLite
// ----------------------------------------------------------------------

// SQLite cannot be asked what it is running, so the hook stands in for
// the server's view: it reports the exact string handed to the driver.
func TestSQLiteQueryTagSurvivesEveryHostileByte(t *testing.T) {
	var sent []string
	db := openSQLiteTagged(t, &sent)

	ctx := drops.WithQueryTags(context.Background(), append(stressTags(), longTag())...)
	rows, err := db.Query(ctx, "SELECT 41 + ?", 1)
	if err != nil {
		t.Fatalf("SQLite rejected a statement tagged with hostile bytes: %v", err)
	}
	var n int
	if !rows.Next() {
		_ = rows.Close()
		t.Fatal("tagged statement returned no rows")
	}
	if err := rows.Scan(&n); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if n != 42 {
		t.Fatalf("n = %d, want 42 — the tagged statement did not mean what it meant", n)
	}
	if len(sent) == 0 || !strings.Contains(sent[0], stressComment[:len(stressComment)-2]) {
		t.Fatalf("the statement reached the driver without the encoded comment: %q", sent)
	}

	if _, err := db.Exec(ctx, "SELECT 1;"); err != nil {
		t.Fatalf("SQLite rejected a tagged statement ending in a semicolon: %v", err)
	}
}

// The tag goes in the comment and the argument goes on the wire, and
// the two never meet: whatever is on the context, the driver's argument
// list is the one the caller passed.
func TestQueryArgumentsAreUntouchedByTagging(t *testing.T) {
	var sent []string
	db := openSQLiteTagged(t, &sent)

	ctx := drops.WithQueryTags(context.Background(), stressTags()...)
	rows, err := db.Query(ctx, "SELECT ?, ?", "secret-value", 99)
	if err != nil {
		t.Fatal(err)
	}
	var s string
	var i int
	if !rows.Next() {
		_ = rows.Close()
		t.Fatal("no rows")
	}
	if err := rows.Scan(&s, &i); err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if s != "secret-value" || i != 99 {
		t.Fatalf("tagging rewrote the arguments: %q, %d", s, i)
	}
	if len(sent) == 0 || !strings.HasSuffix(sent[0], "*/") {
		t.Fatalf("the statement was not tagged, so this proves nothing: %q", sent)
	}
	// The other direction: an argument must not appear in the comment.
	if i := strings.LastIndex(sent[0], "/*"); i < 0 || strings.Contains(sent[0][i:], "secret-value") {
		t.Fatalf("a bound argument reached the query comment: %q", sent[0])
	}
}

// openSQLiteTagged returns an in-memory SQLite DB whose hook appends
// every statement handed to the driver to sent.
func openSQLiteTagged(t *testing.T, sent *[]string) *sqlite.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlite.New(stdlib.New(sqlDB)).WithHook(func(_ context.Context, e drops.QueryEvent) {
		if e.SQL != "" {
			*sent = append(*sent, e.SQL)
		}
	})
}
