package mysql_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// SetNull must bind a parameter rather than splice the literal. The
// lazy implementation is Expr(drops.Raw("NULL")), which is today's
// workaround behind a new name: it turns one statement into two and
// routes the value around the binding drops can see.
func TestSetNullBindsAParameterNotTheToken(t *testing.T) {
	tbl := mysql.NewTable("null_probe")
	id := mysql.Add(tbl, mysql.BigInt("id").NotNull())
	bio := mysql.Add(tbl, mysql.Text("bio").Nullable())

	sqlText, args := mysql.New(nil).Insert(tbl).Row(id.Val(1), bio.SetNull()).ToSQL()
	if strings.Contains(sqlText, "NULL") {
		t.Errorf("SetNull spliced the literal into the SQL: %s", sqlText)
	}
	if !strings.Contains(sqlText, "(?, ?)") {
		t.Errorf("want two placeholders, got: %s", sqlText)
	}
	if len(args) != 2 || args[1] != nil {
		t.Errorf("args = %#v, want [1 <nil>]", args)
	}
}

func TestValPtrAndValNullBindTheUnwrappedValue(t *testing.T) {
	tbl := mysql.NewTable("null_probe")
	bio := mysql.Add(tbl, mysql.Text("bio").Nullable())
	v := "hello"

	cases := []struct {
		name string
		cv   mysql.ColumnValue
		want any
	}{
		{"ValPtr nil", bio.ValPtr(nil), nil},
		{"ValPtr value", bio.ValPtr(&v), "hello"},
		{"ValNull invalid", bio.ValNull(sql.Null[string]{}), nil},
		{"ValNull valid", bio.ValNull(sql.Null[string]{V: "hello", Valid: true}), "hello"},
	}
	for _, c := range cases {
		_, args := mysql.New(nil).Insert(tbl).Row(c.cv).ToSQL()
		if len(args) != 1 || args[0] != c.want {
			t.Errorf("%s: bound %#v, want %#v", c.name, args, c.want)
		}
	}
}

// A MySQL column is nullable unless it says otherwise, so stating it
// must not churn the CREATE TABLE.
func TestNullableRendersNothingInDDL(t *testing.T) {
	plain := mysql.NewTable("t")
	mysql.Add(plain, mysql.BigInt("id").PrimaryKey())
	mysql.Add(plain, mysql.Text("bio"))

	stated := mysql.NewTable("t")
	mysql.Add(stated, mysql.BigInt("id").PrimaryKey())
	mysql.Add(stated, mysql.Text("bio").Nullable())

	a, _ := render(mysql.CreateTable(plain))
	b, _ := render(mysql.CreateTable(stated))
	if a != b {
		t.Errorf("Nullable changed the DDL:\n%s\n%s", a, b)
	}
}

type nullBadRow struct {
	ID  int64  `db:"id"`
	Bio string `db:"bio"`
}

type nullPtrRow struct {
	ID  int64   `db:"id"`
	Bio *string `db:"bio"`
}

func nullBioTable(state string) *mysql.Table {
	tbl := mysql.NewTable("bios")
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	bio := mysql.Text("bio")
	switch state {
	case "nullable":
		bio.Nullable()
	case "notNull":
		bio.NotNull()
	}
	mysql.Add(tbl, bio)
	return tbl
}

func nullMustPanic(t *testing.T, fn func()) (msg string) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic")
		}
		msg, _ = r.(string)
	}()
	fn()
	return ""
}

func TestNewEntityRejectsNullableColumnBoundToUnreceptiveField(t *testing.T) {
	msg := nullMustPanic(t, func() { mysql.NewEntity[nullBadRow](nullBioTable("nullable")) })
	for _, want := range []string{`"bio"`, "field Bio", "*string", "mysql.AllowNullableColumns"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q, got: %s", want, msg)
		}
	}
}

// The case the check turns on: a bare mysql.Text("bio") states
// nothing, so the server accepts NULL.
func TestNewEntityRejectsAnUnstatedColumnToo(t *testing.T) {
	msg := nullMustPanic(t, func() { mysql.NewEntity[nullBadRow](nullBioTable("")) })
	if !strings.Contains(msg, "not declared NOT NULL") {
		t.Errorf("message should say the column never decided, got: %s", msg)
	}
}

func TestNewEntityAcceptsEveryReceptiveShape(t *testing.T) {
	mysql.NewEntity[nullPtrRow](nullBioTable("nullable"))
	mysql.NewEntity[nullBadRow](nullBioTable("notNull"))
	// One-directional: a NOT NULL column bound to a pointer field is
	// the legitimate "distinguish unset from zero" idiom.
	mysql.NewEntity[nullPtrRow](nullBioTable("notNull"))
	mysql.NewEntity[nullBadRow](nullBioTable("nullable"), mysql.AllowNullableColumns("bio"))
	mysql.NewEntity[nullBadRow](nullBioTable("nullable"), mysql.AllowAnyNullableColumn())
}

type nullTwoBadRow struct {
	ID   int64  `db:"id"`
	Bio  string `db:"bio"`
	Note string `db:"note"`
}

// A waiver implemented as a boolean rather than a set would turn the
// whole check off at the first exemption.
func TestAllowNullableColumnsIsNarrow(t *testing.T) {
	tbl := nullBioTable("nullable")
	mysql.Add(tbl, mysql.Text("note").Nullable())

	msg := nullMustPanic(t, func() {
		mysql.NewEntity[nullTwoBadRow](tbl, mysql.AllowNullableColumns("bio"))
	})
	if !strings.Contains(msg, `"note"`) || strings.Contains(msg, `"bio"`) {
		t.Errorf("waiver should exempt only bio, got: %s", msg)
	}
}
