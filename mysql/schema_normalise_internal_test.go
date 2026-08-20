package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// Every case here is a spelling one of the two servers actually
// returns; they are the reason a diff of two identical schemas can come
// out non-empty.
func TestNormaliseType(t *testing.T) {
	cases := []struct{ in, want string }{
		// MariaDB 10.11 still reports a display width; MySQL 8.4 does
		// not, and the same schema has to normalise to one answer.
		{"bigint(20)", "bigint"},
		{"int(11)", "int"},
		{"bigint(20) unsigned", "bigint unsigned"},
		{"tinyint(4)", "tinyint"},
		// Not a width: this is how both servers spell BOOLEAN.
		{"tinyint(1)", "tinyint(1)"},
		{"BOOLEAN", "tinyint(1)"},
		{"BOOL", "tinyint(1)"},
		// The declared side, as the type constructors render it.
		{"VARCHAR(255)", "varchar(255)"},
		{"DATETIME(6)", "datetime(6)"},
		{"DECIMAL(10,2)", "decimal(10,2)"},
		{"ENUM('draft', 'live')", "enum('draft','live')"},
		{"DOUBLE PRECISION", "double"},
		{"NUMERIC", "decimal(10,0)"},
		{"NUMERIC(10,2)", "decimal(10,2)"},
		{"INTEGER", "int"},
		{"CHAR(36)", "char(36)"},
		{"json", "json"},
	}
	for _, c := range cases {
		if got := normaliseType(c.in); got != c.want {
			t.Errorf("normaliseType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// An ENUM or SET keeps its members in its type, and those members are
// the column's values rather than a spelling of them. Folding their
// case would leave both sides of a diff agreeing — each was folded the
// same way — while the CREATE TABLE rendered from the snapshot built a
// column that stores something else. See
// TestMySQLEnumMembersSurviveTheMigration, which asks the server.
func TestNormaliseTypeLeavesQuotedMembersAlone(t *testing.T) {
	cases := []struct{ in, want string }{
		{"ENUM('Draft', 'Published')", "enum('Draft','Published')"},
		{"enum('Draft','Published')", "enum('Draft','Published')"},
		// The space is inside the member, so it is part of the value.
		{"ENUM('a, b', 'c')", "enum('a, b','c')"},
		{"SET('Read', 'Write')", "set('Read','Write')"},
		// A doubled quote is one character of the member, not the end
		// of it: the run has to be scanned, not searched for.
		{"ENUM('it''s', 'B')", "enum('it''s','B')"},
		// Everything outside the quotes still folds.
		{"VARCHAR(20)  CHARACTER SET   utf8mb4", "varchar(20) character set utf8mb4"},
	}
	for _, c := range cases {
		if got := normaliseType(c.in); got != c.want {
			t.Errorf("normaliseType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// MariaDB has no JSON type: it is LONGTEXT plus a generated
// json_valid() constraint, and that is what the catalogue reports. On
// MySQL the two are different types and must stay different.
func TestTypeEqualJSONIsLongtextOnMariaDB(t *testing.T) {
	maria := ServerInfo{MariaDB: true, Major: 10, Minor: 11}
	mysql8 := ServerInfo{Major: 8, Minor: 4}
	if !typeEqual("json", "longtext", maria) {
		t.Fatalf("on MariaDB a declared JSON column reads back longtext; they must compare equal")
	}
	if typeEqual("json", "longtext", mysql8) {
		t.Fatalf("on MySQL json and longtext are different types")
	}
	if !typeEqual("varchar(10)", "varchar(10)", mysql8) {
		t.Fatalf("identical types must compare equal on any server")
	}
}

func TestCanonicalDefault(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		// MariaDB reports the four-character string NULL for a column
		// that never had a default.
		{"NULL", ""},
		{"null", ""},
		{"0", "0"},
		{"'draft'", "'draft'"},
		{"TRUE", "1"},
		{"false", "0"},
		// The one default that is a function call, in each of the
		// spellings the two servers use.
		{"CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP"},
		{"current_timestamp()", "CURRENT_TIMESTAMP"},
		{"current_timestamp(6)", "CURRENT_TIMESTAMP(6)"},
		{"CURRENT_TIMESTAMP(6)", "CURRENT_TIMESTAMP(6)"},
		{"now()", "CURRENT_TIMESTAMP"},
	}
	for _, c := range cases {
		if got := canonicalDefault(c.in); got != c.want {
			t.Errorf("canonicalDefault(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// MySQL 8 strips the quotes off a string default on the way out;
// MariaDB keeps them. Only the catalogue knows the column was a string.
func TestIntrospectedDefault(t *testing.T) {
	cases := []struct {
		name     string
		def      sql.NullString
		dataType string
		extra    string
		want     string
	}{
		{"no default", sql.NullString{}, "varchar", "", ""},
		{"mariadb spells a missing default NULL", sql.NullString{String: "NULL", Valid: true}, "varchar", "", ""},
		{"mysql reports a string bare", sql.NullString{String: "draft", Valid: true}, "enum", "", "'draft'"},
		{"mariadb reports it quoted", sql.NullString{String: "'draft'", Valid: true}, "enum", "", "'draft'"},
		{"an expression default is not a string", sql.NullString{String: "(1 + 1)", Valid: true}, "varchar", "default_generated", "(1 + 1)"},
		{"numbers are left alone", sql.NullString{String: "0", Valid: true}, "int", "", "0"},
		{"the clock is canonicalised", sql.NullString{String: "current_timestamp(6)", Valid: true}, "datetime", "", "CURRENT_TIMESTAMP(6)"},
	}
	for _, c := range cases {
		if got := introspectedDefault(c.def, c.dataType, c.extra); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestOnUpdateFromExtra(t *testing.T) {
	if got := onUpdateFromExtra("on update current_timestamp(6)"); got != "CURRENT_TIMESTAMP(6)" {
		t.Errorf("got %q", got)
	}
	if got := onUpdateFromExtra("auto_increment"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestParseServerVersion(t *testing.T) {
	maria := parseServerVersion("10.11.14-MariaDB-0ubuntu0.24.04.1")
	if !maria.MariaDB || maria.Major != 10 || maria.Minor != 11 || maria.Patch != 14 {
		t.Fatalf("MariaDB = %+v", maria)
	}
	if maria.String() != "MariaDB 10.11.14" {
		t.Fatalf("String() = %q", maria.String())
	}
	my := parseServerVersion("8.4.0")
	if my.MariaDB || my.Major != 8 || my.Minor != 4 {
		t.Fatalf("MySQL = %+v", my)
	}
}

// MySQL 8 wraps a stored CHECK in parentheses and MariaDB does not; the
// probe respells the declared side through the same server, so both
// sides pass through here and meet.
func TestUnwrapCheckClause(t *testing.T) {
	cases := []struct{ in, want string }{
		{"(`age` >= 0)", "`age` >= 0"},
		{"`age` >= 0", "`age` >= 0"},
		{"(`a` = 1) and (`b` = 2)", "(`a` = 1) and (`b` = 2)"},
		{"(`note` <> ')')", "`note` <> ')'"},
	}
	for _, c := range cases {
		if got := unwrapCheckClause(c.in); got != c.want {
			t.Errorf("unwrapCheckClause(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// driverError imitates go-sql-driver/mysql's MySQLError, which carries
// its SQLSTATE in a struct field rather than behind a method — there is
// no interface to assert, which is why the reader uses reflection.
type driverError struct {
	Number   uint16
	SQLState [5]byte
	Message  string
}

func (e *driverError) Error() string { return e.Message }

func TestServerSQLState(t *testing.T) {
	e := &driverError{Number: 1054, SQLState: [5]byte{'4', '2', 'S', '2', '2'}, Message: "Unknown column"}
	if got := serverSQLState(e); got != "42S22" {
		t.Fatalf("by field: got %q", got)
	}
	if got := serverSQLState(errors.New("Error 1054 (42S22): Unknown column 'x' in 'CHECK'")); got != "42S22" {
		t.Fatalf("by message: got %q", got)
	}
	if got := serverSQLState(errors.New("connection refused")); got != "" {
		t.Fatalf("got %q, want empty for an error carrying no state", got)
	}
}

// A probe fails for two very different reasons, and only one of them
// says anything about the expression.
func TestRefusedExpression(t *testing.T) {
	syntax := &driverError{Number: 1064, SQLState: [5]byte{'4', '2', '0', '0', '0'}, Message: "syntax"}
	if !refusedExpression(syntax) {
		t.Fatalf("a class-42 error is the server refusing the expression")
	}
	// The case the class list missed: MariaDB rejects a function it
	// will not allow in a CHECK with 1901, in the catch-all HY000
	// class, and so do MySQL's check-constraint errors. See
	// TestMySQLARefusedCheckExpressionIsReportedNotChurned, which
	// provokes exactly this one on the live server.
	notAllowed := &driverError{Number: 1901, SQLState: [5]byte{'H', 'Y', '0', '0', '0'},
		Message: "Function or expression cannot be used in the CHECK clause"}
	if !refusedExpression(notAllowed) {
		t.Fatalf("the server answered about the expression; the push must report it, not abort")
	}
	// Nothing reached a server that could have an opinion.
	if refusedExpression(errors.New("connection refused")) {
		t.Fatalf("an error carrying no server answer must fail the push")
	}
	if refusedExpression(context.Canceled) {
		t.Fatalf("a cancelled context is not the server refusing anything")
	}
}

func TestServerErrorNumber(t *testing.T) {
	e := &driverError{Number: 1901, SQLState: [5]byte{'H', 'Y', '0', '0', '0'}, Message: "nope"}
	if got := serverErrorNumber(e); got != 1901 {
		t.Fatalf("by field: got %d", got)
	}
	if got := serverErrorNumber(fmt.Errorf("wrapped: %w", e)); got != 1901 {
		t.Fatalf("through a wrapper: got %d", got)
	}
	if got := serverErrorNumber(errors.New("Error 1901 (HY000): nope")); got != 1901 {
		t.Fatalf("by message: got %d", got)
	}
	if got := serverErrorNumber(errors.New("connection refused")); got != 0 {
		t.Fatalf("got %d, want 0 for an error the server never sent", got)
	}
}

func TestOwnedByLeavesForeignTablesAlone(t *testing.T) {
	live := EmptySnapshot()
	live.Tables["mine"] = newTableSnapshot("mine")
	live.Tables["someone_elses"] = newTableSnapshot("someone_elses")

	declared := EmptySnapshot()
	declared.Tables["mine"] = newTableSnapshot("mine")

	narrowed, withheld := ownedBy(live, declared)
	if _, ok := narrowed.Tables["someone_elses"]; ok {
		t.Fatalf("a table the schema never declared reached the diff and would be dropped")
	}
	if stmts := Diff(narrowed, declared); len(stmts) != 0 {
		t.Fatalf("diff over the narrowed side = %v", stmts)
	}
	if len(withheld) != 1 || withheld[0] != "someone_elses" {
		t.Fatalf("withheld = %v, want [someone_elses]", withheld)
	}
	notices := unmanagedTableNotices(live, withheld, PushOptions{})
	if len(notices) != 1 || notices[0].Rule != "unmanaged-table" || notices[0].Table != "someone_elses" {
		t.Fatalf("notices = %v, want one unmanaged-table notice for someone_elses", notices)
	}
	if notices[0].SQL != `DROP TABLE `+"`someone_elses`"+`;` {
		t.Fatalf("notice SQL = %v, want the DROP it withheld", notices[0].SQL)
	}
}
