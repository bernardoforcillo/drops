package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

type ctUser struct {
	ID    int64  `drop:"id"`
	Email string `drop:"email"`
	Alt   string `drop:"alt"`
}

func ctTable(name string) *pg.Table {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("email").NotNull().Unique())
	pg.Add(t, pg.Text("alt"))
	return t
}

// A mapping that can never fire is a typo, and a package-level
// declaration is where it should be reported.
func TestMapConstraintPanicsOnAFieldThatDoesNotExist(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("MapConstraint accepted a field that does not exist")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "Emial") {
			t.Fatalf("panic = %v, want it to name the field", r)
		}
	}()
	pg.NewEntity[ctUser](ctTable("ct_panic")).MapConstraint("ct_panic_email_key", "Emial")
}

// The derived default is a convention, and a convention has to be
// overridable: a constraint whose name follows PostgreSQL's rules can
// still belong to a different field — a generated column, a value the
// form collects under another name.
func TestMapConstraintOverridesTheDerivedName(t *testing.T) {
	tbl := ctTable("ct_override")
	raw := &pgxLikeError{
		code:       "23505",
		constraint: "ct_override_email_key",
		msg:        `duplicate key value violates unique constraint "ct_override_email_key"`,
	}
	db := pg.New(&errDriver{err: raw})

	derived := pg.NewEntity[ctUser](tbl)
	row := ctUser{Email: "a@example.com"}
	fe, ok := drops.AsFieldError(derived.Create(db, context.Background(), &row))
	if !ok {
		t.Fatalf("no field error from the derived mapping")
	}
	if fe.Field != "Email" {
		t.Fatalf("derived Field = %q, want Email", fe.Field)
	}

	overridden := pg.NewEntity[ctUser](tbl).MapConstraint("ct_override_email_key", "Alt")
	row = ctUser{Email: "a@example.com"}
	fe, ok = drops.AsFieldError(overridden.Create(db, context.Background(), &row))
	if !ok {
		t.Fatalf("no field error from the explicit mapping")
	}
	if fe.Field != "Alt" || fe.Column != "alt" {
		t.Fatalf("Field = %q Column = %q, want Alt/alt", fe.Field, fe.Column)
	}
	if fe.Rule != drops.RuleUnique {
		t.Fatalf("Rule = %q, want unique", fe.Rule)
	}
	if !errors.Is(fe, pg.ErrUniqueViolation) {
		t.Fatal("the classified error is no longer reachable under the FieldError")
	}
}

// A constraint nothing on the table claims leaves the error alone: a
// FieldError that cannot name a field would be worse than the driver
// error it replaced.
func TestFieldErrorLeavesAnUnmappedConstraintAlone(t *testing.T) {
	raw := &pgxLikeError{
		code:       "23505",
		constraint: "some_other_tables_key",
		msg:        `duplicate key value violates unique constraint "some_other_tables_key"`,
	}
	db := pg.New(&errDriver{err: raw})
	e := pg.NewEntity[ctUser](ctTable("ct_unmapped"))

	row := ctUser{Email: "a@example.com"}
	err := e.Create(db, context.Background(), &row)
	if _, ok := drops.AsFieldError(err); ok {
		t.Fatalf("err = %v became a FieldError", err)
	}
	if !errors.Is(err, pg.ErrUniqueViolation) {
		t.Fatalf("errors.Is(err, ErrUniqueViolation) = false for %v", err)
	}
}
