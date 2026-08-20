package pg

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/internal/drift"
)

// A column's Go type is the T of the handle that declared it, and
// nothing else recovers it: T appears in no field of Col[T], so
// reflection over the handle sees only the embedded *Column, and the
// SQL type is lossy in both directions — uuid and citext are both
// text-backed strings, and one Go type backs several SQL ones.
func TestColumnGoTypeIsTheHandlesTypeArgument(t *testing.T) {
	cases := []struct {
		col  *Column
		want reflect.Type
	}{
		{Text("a").Column, reflect.TypeOf("")},
		{Varchar("b", 10).Column, reflect.TypeOf("")},
		{BigSerial("c").Column, reflect.TypeOf(int64(0))},
		{Integer("d").Column, reflect.TypeOf(int32(0))},
		{Boolean("e").Column, reflect.TypeOf(false)},
		{DoublePrecision("f").Column, reflect.TypeOf(float64(0))},
		{Timestamp("g", true).Column, reflect.TypeOf(time.Time{})},
		{JSONB("h").Column, reflect.TypeOf(json.RawMessage(nil))},
		{Bytea("i").Column, reflect.TypeOf([]byte(nil))},
		// The escape hatch is the case that matters most: uuid is
		// text at the SQL level and a uuid.UUID in Go, and only the
		// handle knows.
		{Custom[sql.NullString]("j", "text").Column, reflect.TypeOf(sql.NullString{})},
	}
	for _, c := range cases {
		if got := c.col.GoType(); got != c.want {
			t.Errorf("column %q: GoType = %v, want %v", c.col.Name(), got, c.want)
		}
	}
}

// Registration and the builder chain both hand the column on; neither
// may lose the type on the way.
func TestColumnGoTypeSurvivesTheBuilderChainAndAdd(t *testing.T) {
	tbl := NewTable("t")
	Add(tbl, Timestamp("at", true).NotNull().Default("now()"))
	got := tbl.Col("at").GoType()
	if got != reflect.TypeOf(time.Time{}) {
		t.Fatalf("GoType after Add = %v, want time.Time", got)
	}
}

// An alias is a second handle on one column, so it answers the same
// question the same way.
func TestColumnGoTypeSurvivesAliasing(t *testing.T) {
	tbl := NewTable("t")
	Add(tbl, Text("name"))
	if got := tbl.As("u").Col("name").GoType(); got != reflect.TypeOf("") {
		t.Fatalf("aliased GoType = %v, want string", got)
	}
}

// The invariant a generator leans on: a nullable column's Go type is
// the *value* type, so the generator is the one that has to add the
// pointer — GoType never reports one on its own for a plain column,
// and the field it emits is what drift.InferNullable must then agree
// with. Reading a pointer back out of GoType would mean adding a
// second one.
func TestColumnGoTypeIsTheValueTypeNotTheFieldType(t *testing.T) {
	c := Text("bio").Nullable().Column
	if !c.IsNullable() {
		t.Fatal("column should be nullable")
	}
	if drift.InferNullable(c.GoType()) {
		t.Fatalf("GoType = %v: a nullable column still carries its value type", c.GoType())
	}
}

// AutoTable builds its columns from a struct that already has the Go
// types, so it never sets one. A generator meeting nil has to say so
// rather than guess, and this pins the contract GoType's doc states.
func TestColumnGoTypeIsNilForAutoTableColumns(t *testing.T) {
	type row struct {
		Name string `drop:"name"`
	}
	tbl := AutoTable[row]("rows")
	if got := tbl.Col("name").GoType(); got != nil {
		t.Fatalf("AutoTable column GoType = %v, want nil", got)
	}
}
