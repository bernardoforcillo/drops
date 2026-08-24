package sqlite_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// Until SetNull existed there was no way at all to write NULL into a
// SQLite INSERT through the public API: Col[T] has no Expr, the
// package has no untyped Bind, and ColumnValue's methods are
// unexported, so a caller could not implement one. This is the
// dialect the write half most changes.
//
// And it must bind a parameter rather than splice the literal: the
// lazy implementation would route the value around columnValue, which
// is where PII redaction happens.
func TestSetNullBindsAParameterNotTheToken(t *testing.T) {
	tbl := sqlite.NewTable("null_probe")
	id := sqlite.Add(tbl, sqlite.BigInt("id").NotNull())
	bio := sqlite.Add(tbl, sqlite.Text("bio").Nullable())

	sqlText, args := sqlite.New(nil).Insert(tbl).Values(id.Val(1), bio.SetNull()).ToSQL()
	if strings.Contains(sqlText, "NULL") {
		t.Errorf("SetNull spliced the literal into the SQL: %s", sqlText)
	}
	if !strings.Contains(sqlText, "VALUES (?, ?)") {
		t.Errorf("want two placeholders, got: %s", sqlText)
	}
	if len(args) != 2 || args[1] != nil {
		t.Errorf("args = %#v, want [1 <nil>]", args)
	}
}

func TestValPtrAndValNullBindTheUnwrappedValue(t *testing.T) {
	tbl := sqlite.NewTable("null_probe")
	bio := sqlite.Add(tbl, sqlite.Text("bio").Nullable())
	v := "hello"

	cases := []struct {
		name string
		cv   sqlite.ColumnValue
		want any
	}{
		{"ValPtr nil", bio.ValPtr(nil), nil},
		{"ValPtr value", bio.ValPtr(&v), "hello"},
		{"ValNull invalid", bio.ValNull(sql.Null[string]{}), nil},
		{"ValNull valid", bio.ValNull(sql.Null[string]{V: "hello", Valid: true}), "hello"},
	}
	for _, c := range cases {
		_, args := sqlite.New(nil).Insert(tbl).Values(c.cv).ToSQL()
		if len(args) != 1 || args[0] != c.want {
			t.Errorf("%s: bound %#v, want %#v", c.name, args, c.want)
		}
	}
}

// A SQLite column is nullable unless it says otherwise, so stating it
// must not churn the CREATE TABLE.
func TestNullableRendersNothingInDDL(t *testing.T) {
	plain := sqlite.NewTable("t")
	sqlite.Add(plain, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(plain, sqlite.Text("bio"))

	stated := sqlite.NewTable("t")
	sqlite.Add(stated, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(stated, sqlite.Text("bio").Nullable())

	a, _ := sqlite.ToSQL(sqlite.CreateTable(plain))
	b, _ := sqlite.ToSQL(sqlite.CreateTable(stated))
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

func nullBioTable(state string) *sqlite.Table {
	tbl := sqlite.NewTable("bios")
	sqlite.Add(tbl, sqlite.BigInt("id").PrimaryKey())
	bio := sqlite.Text("bio")
	switch state {
	case "nullable":
		bio.Nullable()
	case "notNull":
		bio.NotNull()
	}
	sqlite.Add(tbl, bio)
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
	msg := nullMustPanic(t, func() { sqlite.NewEntity[nullBadRow](nullBioTable("nullable")) })
	for _, want := range []string{`"bio"`, "field Bio", "*string", "sqlite.AllowNullableColumns"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q, got: %s", want, msg)
		}
	}
}

// The case the check turns on: a bare sqlite.Text("bio") states
// nothing, so the database accepts NULL.
func TestNewEntityRejectsAnUnstatedColumnToo(t *testing.T) {
	msg := nullMustPanic(t, func() { sqlite.NewEntity[nullBadRow](nullBioTable("")) })
	if !strings.Contains(msg, "not declared NOT NULL") {
		t.Errorf("message should say the column never decided, got: %s", msg)
	}
}

func TestNewEntityAcceptsEveryReceptiveShape(t *testing.T) {
	sqlite.NewEntity[nullPtrRow](nullBioTable("nullable"))
	sqlite.NewEntity[nullBadRow](nullBioTable("notNull"))
	// One-directional: a NOT NULL column bound to a pointer field is
	// the legitimate "distinguish unset from zero" idiom.
	sqlite.NewEntity[nullPtrRow](nullBioTable("notNull"))
	sqlite.NewEntity[nullBadRow](nullBioTable("nullable"), sqlite.AllowNullableColumns("bio"))
	sqlite.NewEntity[nullBadRow](nullBioTable("nullable"), sqlite.AllowAnyNullableColumn())
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
	sqlite.Add(tbl, sqlite.Text("note").Nullable())

	msg := nullMustPanic(t, func() {
		sqlite.NewEntity[nullTwoBadRow](tbl, sqlite.AllowNullableColumns("bio"))
	})
	if !strings.Contains(msg, `"note"`) || strings.Contains(msg, `"bio"`) {
		t.Errorf("waiver should exempt only bio, got: %s", msg)
	}
}

type autoNullRow struct {
	ID   int64            `drop:"id,primaryKey,autoIncrement"`
	Name string           `drop:"name"`
	Bio  *string          `drop:"bio"`
	Meta sql.Null[string] `drop:"meta"`
	Blob []byte           `drop:"blob"`
}

func TestAutoTableStatesNullabilityFromTheFieldType(t *testing.T) {
	tbl := sqlite.AutoTable[autoNullRow]("auto_null")
	want := map[string]bool{
		"id": false, "name": false, "bio": true, "meta": true,
		// A []byte can receive NULL, but writing one is not the
		// author saying the column is optional.
		"blob": false,
	}
	for _, c := range tbl.Columns() {
		if got := c.IsNullable(); got != want[c.Name()] {
			t.Errorf("column %q: IsNullable = %v, want %v", c.Name(), got, want[c.Name()])
		}
	}
	// And the loop closes: the derived table passes the check its own
	// NewEntity runs.
	sqlite.NewAutoEntity[autoNullRow]("auto_null")
}
