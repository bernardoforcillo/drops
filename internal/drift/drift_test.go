package drift_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/internal/drift"
)

type row struct {
	ID       int64
	Name     string
	Skipped  string   `drop:"-"`
	Children []string `dropRel:"children"`
	Stray    string
	private  string //nolint:unused // present to prove unexported fields are ignored
}

func TestSpareFieldsIgnoresOptedOutAndRelations(t *testing.T) {
	rt := reflect.TypeOf(row{})
	// Pretend ID (field 0) and Name (field 1) are bound to columns.
	bound := map[string]bool{drift.FieldKey([]int{0}): true, drift.FieldKey([]int{1}): true}

	got := drift.SpareFields(rt, bound)
	if len(got) != 1 || got[0] != "Stray" {
		t.Errorf("SpareFields = %v, want only [Stray]", got)
	}
}

func TestFieldKeyDistinguishesPaths(t *testing.T) {
	if drift.FieldKey([]int{1, 2}) == drift.FieldKey([]int{12}) {
		t.Error("nested and flat paths must not collide")
	}
}

func TestReportNilWhenNothingMissing(t *testing.T) {
	if err := drift.Report("drops/pg", "T", "t", nil, []string{"Stray"}, "pg.AllowUnmappedColumns"); err != nil {
		t.Errorf("no missing columns should be no error, got %v", err)
	}
}

func TestReportNamesColumnsCauseAndRemedy(t *testing.T) {
	err := drift.Report("drops/pg", "User", "users",
		[]string{"email", "bio"}, []string{"Emial"}, "pg.AllowUnmappedColumns")
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"drops/pg", "NewEntity[User]", `"users"`,
		`"email"`, `"bio"`,
		"Emial",
		`pg.AllowUnmappedColumns("email", "bio")`,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q\ngot: %s", want, msg)
		}
	}
}

// With no unbound fields there is nothing to suggest, and the message
// should not invent a cause.
func TestReportOmitsSuspectsWhenThereAreNone(t *testing.T) {
	err := drift.Report("drops/sqlite", "T", "t", []string{"x"}, nil, "sqlite.AllowUnmappedColumns")
	if strings.Contains(err.Error(), "looks like the cause") {
		t.Errorf("should not speculate with no unbound fields: %s", err)
	}
}

func TestReportAgreesWithItselfOnNumber(t *testing.T) {
	one := drift.Report("p", "T", "t", []string{"a"}, nil, "e").Error()
	two := drift.Report("p", "T", "t", []string{"a", "b"}, nil, "e").Error()
	if !strings.Contains(one, "owns it") {
		t.Errorf("singular: %s", one)
	}
	if !strings.Contains(two, "owns them") {
		t.Errorf("plural: %s", two)
	}
}
