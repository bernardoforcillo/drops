package clickhouse_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/clickhouse"
)

// SetNull must bind a parameter rather than splice the literal.
// clickhouse/insert.go already splices drops.Raw("NULL") to align a
// short row in a batch, and that path is deliberately different: it
// fills a column the caller never mentioned, where this one is the
// caller saying NULL.
func TestSetNullBindsAParameterNotTheToken(t *testing.T) {
	tbl := clickhouse.NewTable("null_probe")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	bio := clickhouse.Add(tbl, clickhouse.String("bio").Nullable())

	sqlText, args := clickhouse.ToSQL(
		clickhouse.New(nil).Insert(tbl).Row(id.Val(1), bio.SetNull()))
	if strings.Contains(sqlText, "NULL") {
		t.Errorf("SetNull spliced the literal into the SQL: %s", sqlText)
	}
	if len(args) != 2 || args[1] != nil {
		t.Errorf("args = %#v, want [1 <nil>]", args)
	}
}

func TestValPtrAndValNullBindTheUnwrappedValue(t *testing.T) {
	tbl := clickhouse.NewTable("null_probe")
	bio := clickhouse.Add(tbl, clickhouse.String("bio").Nullable())
	v := "hello"

	cases := []struct {
		name string
		cv   clickhouse.ColumnValue
		want any
	}{
		{"ValPtr nil", bio.ValPtr(nil), nil},
		{"ValPtr value", bio.ValPtr(&v), "hello"},
		{"ValNull invalid", bio.ValNull(sql.Null[string]{}), nil},
		{"ValNull valid", bio.ValNull(sql.Null[string]{V: "hello", Valid: true}), "hello"},
	}
	for _, c := range cases {
		_, args := clickhouse.ToSQL(clickhouse.New(nil).Insert(tbl).Row(c.cv))
		if len(args) != 1 || args[0] != c.want {
			t.Errorf("%s: bound %#v, want %#v", c.name, args, c.want)
		}
	}
}

type chNullBadRow struct {
	ID  uint64 `db:"id"`
	Bio string `db:"bio"`
}

type chNullPtrRow struct {
	ID  uint64  `db:"id"`
	Bio *string `db:"bio"`
}

func chNullBioTable(nullable bool) *clickhouse.Table {
	tbl := clickhouse.NewTable("bios")
	clickhouse.Add(tbl, clickhouse.UInt64("id"))
	bio := clickhouse.String("bio")
	if nullable {
		bio.Nullable()
	}
	clickhouse.Add(tbl, bio)
	return tbl
}

func chNullMustPanic(t *testing.T, fn func()) (msg string) {
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
	msg := chNullMustPanic(t, func() { clickhouse.NewEntity[chNullBadRow](chNullBioTable(true)) })
	for _, want := range []string{`"bio"`, "field Bio", "*string", "clickhouse.AllowNullableColumns"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q, got: %s", want, msg)
		}
	}
}

// ClickHouse is the dialect that already had this right: nullability
// is an opt-in that changes the column's type, so a column that said
// nothing is NOT NULL and no existing CH schema trips the check. That
// default being the safe one is the strongest evidence the rule is
// the right one.
func TestUnstatedClickHouseColumnIsNotNullable(t *testing.T) {
	clickhouse.NewEntity[chNullBadRow](chNullBioTable(false))
}

func TestNewEntityAcceptsEveryReceptiveShape(t *testing.T) {
	clickhouse.NewEntity[chNullPtrRow](chNullBioTable(true))
	// One-directional: a non-nullable column bound to a pointer field
	// is not an error.
	clickhouse.NewEntity[chNullPtrRow](chNullBioTable(false))
	clickhouse.NewEntity[chNullBadRow](chNullBioTable(true), clickhouse.AllowNullableColumns("bio"))
	clickhouse.NewEntity[chNullBadRow](chNullBioTable(true), clickhouse.AllowAnyNullableColumn())
}

type chNullTwoBadRow struct {
	ID   uint64 `db:"id"`
	Bio  string `db:"bio"`
	Note string `db:"note"`
}

// A waiver implemented as a boolean rather than a set would turn the
// whole check off at the first exemption.
func TestAllowNullableColumnsIsNarrow(t *testing.T) {
	tbl := chNullBioTable(true)
	clickhouse.Add(tbl, clickhouse.String("note").Nullable())

	msg := chNullMustPanic(t, func() {
		clickhouse.NewEntity[chNullTwoBadRow](tbl, clickhouse.AllowNullableColumns("bio"))
	})
	if !strings.Contains(msg, `"note"`) || strings.Contains(msg, `"bio"`) {
		t.Errorf("waiver should exempt only bio, got: %s", msg)
	}
}
