package drift_test

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/internal/drift"
)

// scannable is a user type that receives NULL through its own Scan
// method — the shape a domain type behind a nullable column takes.
type scannable struct{ set bool }

func (s *scannable) Scan(any) error { s.set = true; return nil }

func TestAcceptsNullFollowsDatabaseSQL(t *testing.T) {
	cases := []struct {
		typ  any
		want bool
	}{
		{(*string)(nil), true},
		{sql.Null[string]{}, true},
		{sql.NullString{}, true},
		{scannable{}, true},
		{json.RawMessage(nil), true},
		{[]byte(nil), true},
		{map[string]string(nil), true},
		{"", false},
		{int64(0), false},
		{false, false},
		{time.Time{}, false},
		{[4]byte{}, false},
	}
	for _, c := range cases {
		rt := reflect.TypeOf(c.typ)
		if got := drift.AcceptsNull(rt); got != c.want {
			t.Errorf("AcceptsNull(%s) = %v, want %v", rt, got, c.want)
		}
	}
	// any is spelled as an interface type, which reflect.TypeOf on a
	// value can never hand back.
	anyType := reflect.TypeOf((*any)(nil)).Elem()
	if !drift.AcceptsNull(anyType) {
		t.Error("AcceptsNull(any) = false, want true")
	}
}

func TestInferNullableIsNarrowerThanAcceptsNull(t *testing.T) {
	cases := []struct {
		typ  any
		want bool
	}{
		{(*string)(nil), true},
		{(*time.Time)(nil), true},
		{sql.Null[string]{}, true},
		{sql.NullString{}, true},
		// A []byte can receive NULL, but writing one is not the
		// author saying the column is optional.
		{[]byte(nil), false},
		{json.RawMessage(nil), false},
		{"", false},
		{time.Time{}, false},
		{scannable{}, false},
	}
	for _, c := range cases {
		rt := reflect.TypeOf(c.typ)
		if got := drift.InferNullable(rt); got != c.want {
			t.Errorf("InferNullable(%s) = %v, want %v", rt, got, c.want)
		}
	}
}

// The invariant the two generators and the checker rest on. A future
// edit to either predicate that breaks it produces a generator whose
// own output trips its own check, and this is where that must fail.
func TestInferNullableImpliesAcceptsNull(t *testing.T) {
	for _, v := range []any{
		(*string)(nil), sql.Null[string]{}, sql.NullString{}, (*time.Time)(nil),
		"", []byte(nil), time.Time{}, json.RawMessage(nil), int64(0), scannable{},
	} {
		rt := reflect.TypeOf(v)
		if drift.InferNullable(rt) && !drift.AcceptsNull(rt) {
			t.Errorf("%s: InferNullable but not AcceptsNull", rt)
		}
	}
}

type outer struct {
	inner
	Name string
}

type inner struct{ ID int64 }

func TestFieldPathDotsThroughEmbedding(t *testing.T) {
	rt := reflect.TypeOf(outer{})
	if got := drift.FieldPath(rt, []int{0, 0}); got != "inner.ID" {
		t.Errorf("FieldPath = %q, want inner.ID", got)
	}
	if got := drift.FieldPath(rt, []int{1}); got != "Name" {
		t.Errorf("FieldPath = %q, want Name", got)
	}
}

func TestReportNullableNilWhenNothingMismatched(t *testing.T) {
	if err := drift.ReportNullable("drops/pg", "T", "t", nil, "pg.AllowNullableColumns"); err != nil {
		t.Errorf("no mismatches should be no error, got %v", err)
	}
}

func TestReportNullableNamesColumnFieldAndBothFixes(t *testing.T) {
	err := drift.ReportNullable("drops/pg", "User", "users", []drift.NullMismatch{
		{Column: "bio", Field: "Bio", FieldType: "string"},
		{Column: "note", Field: "Note", FieldType: "time.Time", Stated: true},
	}, "pg.AllowNullableColumns")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"drops/pg", "NewEntity[User]", `"users"`,
		`"bio"`, "field Bio is string", ".NotNull()", "make the field *string",
		`"note"`, "declared Nullable", "field Note is time.Time",
		"*time.Time", "sql.Null[time.Time]",
		`pg.AllowNullableColumns("bio", "note")`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\ngot: %s", want, msg)
		}
	}
}

// A column that said nothing is a schema that never decided, and
// tightening it is a legitimate answer. A column that said Nullable
// has decided, so only the field can move — the message must not
// suggest otherwise.
func TestReportNullableDoesNotOfferNotNullForAStatedColumn(t *testing.T) {
	err := drift.ReportNullable("drops/pg", "User", "users", []drift.NullMismatch{
		{Column: "bio", Field: "Bio", FieldType: "string", Stated: true},
	}, "pg.AllowNullableColumns")
	if strings.Contains(err.Error(), ".NotNull()") {
		t.Errorf("stated column should not be told to add .NotNull(): %s", err)
	}
}
