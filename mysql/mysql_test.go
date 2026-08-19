package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
)

// --- test driver -------------------------------------------------------

type fakeDriver struct {
	queries  []string
	args     [][]any
	rows     func() drops.Rows
	lastID   int64
	hasLast  bool
	execFail error
}

func (d *fakeDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if d.execFail != nil {
		return nil, d.execFail
	}
	if d.hasLast {
		return lastIDResult{id: d.lastID}, nil
	}
	return plainResult{}, nil
}

func (d *fakeDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.queries = append(d.queries, sql)
	d.args = append(d.args, args)
	if d.rows != nil {
		return d.rows(), nil
	}
	return &fakeRows{}, nil
}

func (d *fakeDriver) Begin(context.Context) (drops.Tx, error) { return &fakeTx{d}, nil }

type fakeTx struct{ *fakeDriver }

func (*fakeTx) Commit(context.Context) error   { return nil }
func (*fakeTx) Rollback(context.Context) error { return nil }

// plainResult offers only what drops.Result requires.
type plainResult struct{}

func (plainResult) RowsAffected() (int64, error) { return 1, nil }

// lastIDResult additionally offers LastInsertId, the way database/sql
// does.
type lastIDResult struct{ id int64 }

func (lastIDResult) RowsAffected() (int64, error)   { return 1, nil }
func (r lastIDResult) LastInsertId() (int64, error) { return r.id, nil }

type fakeRows struct {
	cols []string
	data [][]any
	pos  int
}

func (r *fakeRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *fakeRows) Scan(dest ...any) error     { return nil }
func (r *fakeRows) Columns() ([]string, error) { return r.cols, nil }
func (r *fakeRows) Close() error               { return nil }
func (r *fakeRows) Err() error                 { return nil }

// --- schema ------------------------------------------------------------

func usersTable() (*mysql.Table, *mysql.Col[int64], *mysql.Col[string], *mysql.Col[string]) {
	t := mysql.NewTable("users")
	id := mysql.Add(t, mysql.BigSerial("id").PrimaryKey())
	name := mysql.Add(t, mysql.Varchar("name", 255).NotNull())
	email := mysql.Add(t, mysql.Varchar("email", 255).NotNull().Unique())
	return t, id, name, email
}

func sqlOf(e drops.Expression) (string, []any) {
	return drops.StringWithDialect(mysql.Dialect, e)
}

// --- dialect -----------------------------------------------------------

func TestDialectBasics(t *testing.T) {
	if got := mysql.Dialect.Placeholder(3); got != "?" {
		t.Errorf("Placeholder = %q, want ?", got)
	}
	if got := mysql.Dialect.QuoteIdent("tbl"); got != "`tbl`" {
		t.Errorf("QuoteIdent = %q, want backticks", got)
	}
	// A backtick inside an identifier must be doubled, not escaped
	// with a backslash — MySQL's rule.
	if got := mysql.Dialect.QuoteIdent("we`ird"); got != "`we``ird`" {
		t.Errorf("QuoteIdent = %q", got)
	}
	// MySQL has no RETURNING and MariaDB's is partial, so builders
	// must never emit one.
	if mysql.Dialect.SupportsReturning() {
		t.Error("SupportsReturning must be false — see the package doc")
	}
}

// --- DDL ---------------------------------------------------------------

func TestCreateTable(t *testing.T) {
	tbl, _, _, _ := usersTable()
	tbl.Engine("InnoDB").Charset("utf8mb4")
	got, _ := sqlOf(mysql.CreateTable(tbl))

	for _, want := range []string{
		"CREATE TABLE `users`",
		"`id` BIGINT NOT NULL AUTO_INCREMENT",
		"`name` VARCHAR(255) NOT NULL",
		"PRIMARY KEY (`id`)",
		"UNIQUE KEY `uq_users_email` (`email`)",
		"ENGINE=InnoDB",
		"DEFAULT CHARSET=utf8mb4",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DDL missing %s\ngot:\n%s", want, got)
		}
	}
	// Column references inside CREATE TABLE must not be qualified —
	// the table does not exist yet for the server to resolve against.
	if strings.Contains(got, "`users`.`id`") {
		t.Errorf("qualified reference inside CREATE TABLE:\n%s", got)
	}
}

func TestCreateTableCompositePrimaryKey(t *testing.T) {
	tbl := mysql.NewTable("memberships")
	mysql.Add(tbl, mysql.BigInt("orgId").PrimaryKey())
	mysql.Add(tbl, mysql.BigInt("userId").PrimaryKey())
	got, _ := sqlOf(mysql.CreateTable(tbl))
	if !strings.Contains(got, "PRIMARY KEY (`orgId`, `userId`)") {
		t.Errorf("composite key not rendered:\n%s", got)
	}
}

func TestCreateTableForeignKey(t *testing.T) {
	users, id, _, _ := usersTable()
	_ = users
	posts := mysql.NewTable("posts")
	mysql.Add(posts, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(posts, mysql.BigInt("userId").NotNull().References(id, mysql.OnDelete("CASCADE")))

	got, _ := sqlOf(mysql.CreateTable(posts))
	for _, want := range []string{
		"CONSTRAINT `fk_posts_userId` FOREIGN KEY (`userId`)",
		"REFERENCES `users` (`id`)",
		"ON DELETE CASCADE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("DDL missing %s\ngot:\n%s", want, got)
		}
	}
}

// MySQL cannot index a TEXT column without a prefix length, so the
// builder has to be able to express one.
func TestIndexPrefixLength(t *testing.T) {
	tbl := mysql.NewTable("docs")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	body := mysql.Add(tbl, mysql.Text("body"))
	got, _ := sqlOf(mysql.NewIndex("idx_docs_body", tbl, body).Prefix(body, 64))
	if !strings.Contains(got, "(`body`(64))") {
		t.Errorf("prefix length not rendered: %s", got)
	}
}

// MySQL scopes index names to their table, so DROP INDEX needs one.
func TestDropIndexNamesTheTable(t *testing.T) {
	tbl, _, _, _ := usersTable()
	got, _ := sqlOf(mysql.DropIndex("idx_users_name", tbl))
	if !strings.Contains(got, "ON `users`") {
		t.Errorf("DROP INDEX must name the table: %s", got)
	}
}

// --- SELECT ------------------------------------------------------------

func TestSelectRendersMySQLSyntax(t *testing.T) {
	tbl, id, name, _ := usersTable()
	db := mysql.New(&fakeDriver{})
	sel := db.Select(id, name).From(tbl).
		Where(name.Eq("ada")).
		OrderBy(id.Desc()).
		Limit(10)

	got, args := sel.ToSQL()
	want := "SELECT `users`.`id`, `users`.`name` FROM `users` WHERE (`users`.`name` = ?) ORDER BY `users`.`id` DESC LIMIT ?"
	if got != want {
		t.Errorf("SQL\n got %s\nwant %s", got, want)
	}
	if len(args) != 2 || args[0] != "ada" || args[1] != int64(10) {
		t.Errorf("args = %v", args)
	}
}

// MySQL rejects OFFSET without LIMIT, so an offset alone has to carry
// one.
func TestSelectOffsetWithoutLimit(t *testing.T) {
	tbl, _, _, _ := usersTable()
	got, _ := mysql.New(&fakeDriver{}).Select().From(tbl).Offset(20).ToSQL()
	if !strings.Contains(got, "LIMIT 18446744073709551615") {
		t.Errorf("offset without limit needs a limit: %s", got)
	}
}

func TestSelectDefaultFilterAndUnscoped(t *testing.T) {
	tbl := mysql.NewTable("users")
	mysql.Add(tbl, mysql.BigSerial("id").PrimaryKey())
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false))
	tbl.DefaultFilter(deleted.IsNull())

	db := mysql.New(&fakeDriver{})
	scoped, _ := db.Select().From(tbl).ToSQL()
	if !strings.Contains(scoped, "deletedAt") {
		t.Errorf("default filter missing: %s", scoped)
	}
	unscoped, _ := db.Select().From(tbl).Unscoped().ToSQL()
	if strings.Contains(unscoped, "deletedAt") {
		t.Errorf("Unscoped should bypass the filter: %s", unscoped)
	}
}

func TestSelectForUpdateSkipLocked(t *testing.T) {
	tbl, _, _, _ := usersTable()
	got, _ := mysql.New(&fakeDriver{}).Select().From(tbl).ForUpdateSkipLocked().ToSQL()
	if !strings.HasSuffix(got, "FOR UPDATE SKIP LOCKED") {
		t.Errorf("got %s", got)
	}
}

// --- INSERT ------------------------------------------------------------

func TestInsertOnDuplicateKeyUpdate(t *testing.T) {
	tbl, id, name, email := usersTable()
	db := mysql.New(&fakeDriver{})
	got, _ := db.Insert(tbl).
		Row(id.Val(1), name.Val("ada"), email.Val("ada@example.com")).
		OnDuplicateKeyUpdateAll().
		ToSQL()

	if !strings.Contains(got, "ON DUPLICATE KEY UPDATE") {
		t.Errorf("upsert clause missing: %s", got)
	}
	// The key column is not reassigned...
	if strings.Contains(got, "`id` = VALUES(`id`)") {
		t.Errorf("the key column must not be reassigned: %s", got)
	}
	// ...and the rest are, via the portable VALUES() spelling rather
	// than MySQL 8's row alias, which MariaDB does not accept.
	if !strings.Contains(got, "`name` = VALUES(`name`)") {
		t.Errorf("expected VALUES(col) assignments: %s", got)
	}
}

func TestInsertMultipleRows(t *testing.T) {
	tbl, id, name, email := usersTable()
	db := mysql.New(&fakeDriver{})
	got, args := db.Insert(tbl).
		Row(id.Val(1), name.Val("a"), email.Val("a@x")).
		Row(id.Val(2), name.Val("b"), email.Val("b@x")).
		ToSQL()
	if strings.Count(got, "(?, ?, ?)") != 2 {
		t.Errorf("expected two value tuples: %s", got)
	}
	if len(args) != 6 {
		t.Errorf("args = %v", args)
	}
}

// A row that omits a column inserts DEFAULT for it rather than
// shifting the remaining values into the wrong columns.
func TestInsertAlignsShortRows(t *testing.T) {
	tbl, id, name, email := usersTable()
	db := mysql.New(&fakeDriver{})
	got, _ := db.Insert(tbl).
		Row(id.Val(1), name.Val("a"), email.Val("a@x")).
		Row(id.Val(2), email.Val("b@x")).
		ToSQL()
	if !strings.Contains(got, "(?, DEFAULT, ?)") {
		t.Errorf("short row should default the missing column: %s", got)
	}
}

func TestInsertEmptyIsAnError(t *testing.T) {
	tbl, _, _, _ := usersTable()
	_, err := mysql.New(&fakeDriver{}).Insert(tbl).Exec(context.Background())
	if !errors.Is(err, mysql.ErrNoRows) {
		t.Errorf("err = %v want ErrNoRows", err)
	}
}

// --- UPDATE / DELETE ---------------------------------------------------

func TestUpdateOrderByLimit(t *testing.T) {
	tbl, id, name, _ := usersTable()
	db := mysql.New(&fakeDriver{})
	got, _ := db.Update(tbl).Set(name.Val("x")).OrderBy(id.Asc()).Limit(100).ToSQL()
	if !strings.Contains(got, "ORDER BY `users`.`id` ASC LIMIT ?") {
		t.Errorf("batched UPDATE not rendered: %s", got)
	}
}

func TestUpdateWithoutAssignmentsIsAnError(t *testing.T) {
	tbl, _, _, _ := usersTable()
	_, err := mysql.New(&fakeDriver{}).Update(tbl).Exec(context.Background())
	if !errors.Is(err, mysql.ErrNoAssignments) {
		t.Errorf("err = %v want ErrNoAssignments", err)
	}
}

func TestDeleteLimit(t *testing.T) {
	tbl, id, _, _ := usersTable()
	got, _ := mysql.New(&fakeDriver{}).Delete(tbl).Where(id.Lt(100)).Limit(500).ToSQL()
	if !strings.Contains(got, "DELETE FROM `users` WHERE (`users`.`id` < ?) LIMIT ?") {
		t.Errorf("got %s", got)
	}
}

// --- operators ---------------------------------------------------------

// MySQL rejects "IN ()", so an empty set has to render as the boolean
// the operator means.
func TestEmptyInRendersBoolean(t *testing.T) {
	tbl, id, _, _ := usersTable()
	in, _ := sqlOf(mysql.In(id))
	if in != "(FALSE)" {
		t.Errorf("empty IN = %q, want (FALSE)", in)
	}
	notIn, _ := sqlOf(mysql.NotIn(id))
	if notIn != "(TRUE)" {
		t.Errorf("empty NOT IN = %q, want (TRUE)", notIn)
	}
	typed, _ := sqlOf(id.In())
	if typed != "(FALSE)" {
		t.Errorf("empty typed In = %q", typed)
	}
	_ = tbl
}

func TestInExpandsLoneSlice(t *testing.T) {
	_, id, _, _ := usersTable()
	got, args := sqlOf(mysql.In(id, []int64{1, 2, 3}))
	if !strings.Contains(got, "IN (?, ?, ?)") {
		t.Errorf("slice not expanded: %s", got)
	}
	if len(args) != 3 {
		t.Errorf("args = %v", args)
	}
}
