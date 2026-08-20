package drops_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
)

// fakeColumn has the shape every dialect's *Column has: an Expression
// that writes a qualified quoted identifier, and no Alias method.
type fakeColumn struct{ table, name string }

func (c fakeColumn) Name() string { return c.name }
func (c fakeColumn) WriteSQL(b *drops.Builder) {
	b.WriteQualified(c.table, c.name)
}

// fakeTable has the shape every dialect's *Table has: name, alias, and
// a WriteSQL that renders the FROM form.
type fakeTable struct{ schema, name, alias string }

func (t fakeTable) Name() string   { return t.name }
func (t fakeTable) Schema() string { return t.schema }
func (t fakeTable) Alias() string  { return t.alias }
func (t fakeTable) WriteSQL(b *drops.Builder) {
	b.WriteQualified(t.schema, t.name)
	if t.alias != "" {
		b.WriteString(" AS ")
		b.WriteIdent(t.alias)
	}
}

// fakeCHTable spells its namespace the way MySQL and ClickHouse do.
type fakeCHTable struct{ db, name string }

func (t fakeCHTable) Name() string              { return t.name }
func (t fakeCHTable) Database() string          { return t.db }
func (t fakeCHTable) Alias() string             { return "" }
func (t fakeCHTable) WriteSQL(b *drops.Builder) { b.WriteQualified(t.db, t.name) }
func mustSQL(t *testing.T, e drops.Expression) (string, []any) {
	t.Helper()
	return drops.String(e)
}

// The whole safety argument of SQL in one test: the operator text is
// written verbatim, and the value beside it is bound. A string value
// that reached the text instead is an injection, so this pins the
// direction, not just the rendering.
func TestSQLBindsValuesAndWritesTextVerbatim(t *testing.T) {
	email := fakeColumn{table: "users", name: "email"}
	in := "'; DROP TABLE users; --"

	sql, args := mustSQL(t, drops.SQL("lower(", email, ") = ", drops.Arg(strings.ToLower(in))))

	const want = `lower("users"."email") = $1`
	if sql != want {
		t.Fatalf("sql = %q, want %q", sql, want)
	}
	if len(args) != 1 || args[0] != strings.ToLower(in) {
		t.Fatalf("args = %#v, want the value bound", args)
	}
	if strings.Contains(sql, "DROP TABLE") {
		t.Fatalf("the value reached the SQL text: %q", sql)
	}
}

// Param is the older spelling of the same wrapper and must keep
// working inside a fragment.
func TestSQLAcceptsParamAsWellAsArg(t *testing.T) {
	sql, args := mustSQL(t, drops.SQL("x = ", drops.Param{Value: "text"}))
	if sql != "x = $1" || len(args) != 1 || args[0] != "text" {
		t.Fatalf("sql = %q args = %#v", sql, args)
	}
}

// Anything that is not a string and not an Expression is a value. No
// wrapper needed — an int cannot be confused for SQL.
func TestSQLBindsNonStringValuesWithoutAWrapper(t *testing.T) {
	sql, args := mustSQL(t, drops.SQL("age > ", 18, " AND deleted_at IS ", nil))
	if sql != "age > $1 AND deleted_at IS $2" {
		t.Fatalf("sql = %q", sql)
	}
	if !reflect.DeepEqual(args, []any{18, nil}) {
		t.Fatalf("args = %#v", args)
	}
}

// A named string type is not the string type, so it binds. The
// dangerous direction (text where a value was meant) is reserved for a
// bare string alone.
func TestSQLBindsNamedStringTypes(t *testing.T) {
	type email string
	sql, args := mustSQL(t, drops.SQL("email = ", email("a@example.com")))
	if sql != "email = $1" {
		t.Fatalf("sql = %q", sql)
	}
	if len(args) != 1 || args[0] != email("a@example.com") {
		t.Fatalf("args = %#v", args)
	}
}

// Raw is an Expression, so it inlines as text rather than binding —
// the one string-ish type that stays text on purpose.
func TestSQLInlinesRawAndNestedFragments(t *testing.T) {
	inner := drops.SQL("id = ", drops.Arg(7))
	sql, args := mustSQL(t, drops.SQL("SELECT ", drops.Raw("count(*)"), " WHERE ", inner))
	if sql != "SELECT count(*) WHERE id = $1" {
		t.Fatalf("sql = %q", sql)
	}
	if len(args) != 1 || args[0] != 7 {
		t.Fatalf("args = %#v", args)
	}
}

// A table renders as a reference to itself, not as the FROM item its
// own WriteSQL produces.
func TestSQLRendersTableAsAReference(t *testing.T) {
	cases := []struct {
		name  string
		table drops.Expression
		want  string
	}{
		{"plain", fakeTable{name: "users"}, `"users"`},
		{"schema-qualified", fakeTable{schema: "app", name: "users"}, `"app"."users"`},
		{"aliased", fakeTable{schema: "app", name: "users", alias: "u"}, `"u"`},
		{"database-qualified", fakeCHTable{db: "analytics", name: "events"}, `"analytics"."events"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, _ := mustSQL(t, drops.SQL("SELECT count(*) FROM ", tc.table))
			if sql != "SELECT count(*) FROM "+tc.want {
				t.Fatalf("sql = %q, want %q", sql, "SELECT count(*) FROM "+tc.want)
			}
		})
	}
}

// The fragment holds its parts and renders them per dialect, so one
// declaration serves both placeholder styles.
func TestSQLRendersWithTheInstalledDialect(t *testing.T) {
	frag := drops.SQL("x = ", drops.Arg(1), " AND y = ", drops.Arg(2))
	sql, args := drops.StringWithDialect(qmarkDialect{}, frag)
	if sql != "x = ? AND y = ?" {
		t.Fatalf("sql = %q", sql)
	}
	if len(args) != 2 {
		t.Fatalf("args = %#v", args)
	}
}

// qmarkDialect is a MySQL-shaped dialect: "?" placeholders, backtick
// identifiers.
type qmarkDialect struct{}

func (qmarkDialect) Name() string           { return "qmark" }
func (qmarkDialect) Placeholder(int) string { return "?" }
func (qmarkDialect) QuoteIdent(s string) string {
	return drops.BacktickQuoteIdent(s)
}
func (qmarkDialect) SupportsReturning() bool { return false }

func TestSQLWithNoPartsIsEmpty(t *testing.T) {
	sql, args := mustSQL(t, drops.SQL())
	if sql != "" || len(args) != 0 {
		t.Fatalf("sql = %q args = %#v", sql, args)
	}
}
