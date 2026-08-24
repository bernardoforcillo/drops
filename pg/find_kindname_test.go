package pg_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// A MorphMany bound to a non-slice field is the same mistake a HasMany
// bound to one is, and the guard catches both — but it used to report
// either as a HasMany, so the one thing the message told you about the
// relation was wrong. A reader who trusts it goes looking for a
// HasMany declaration that does not exist.
type morphUserWithOneComment struct {
	ID       int64         `drop:"id"`
	Name     string        `drop:"name"`
	Comments *morphComment `dropRel:"comments"`
}

func TestSliceGuardNamesTheRelationKindItActuallySaw(t *testing.T) {
	users, _ := morphSchema()

	fd := &fakeDriver{handler: func(sql string, _ []any) (drops.Rows, error) {
		if strings.Contains(sql, `FROM "users"`) {
			return &fakeRows{
				cols: []string{"id", "name"},
				data: [][]any{{int64(1), "Alice"}},
			}, nil
		}
		return &fakeRows{}, nil
	}}
	db := pg.New(fd)

	var us []morphUserWithOneComment
	err := db.Find(users).With("comments").All(context.Background(), &us)
	if err == nil {
		t.Fatalf("a MorphMany bound to a pointer field must be refused")
	}
	if !strings.Contains(err.Error(), "MorphMany") {
		t.Errorf("error must name the kind it saw, got: %v", err)
	}
	if strings.Contains(err.Error(), "HasMany") {
		t.Errorf("error calls a MorphMany a HasMany: %v", err)
	}
}
