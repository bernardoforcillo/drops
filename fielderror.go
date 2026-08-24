package drops

import (
	"errors"
	"fmt"
)

// Rule names the kind of database constraint a value fell foul of. It
// is the vocabulary a form layer branches on — "this address is taken"
// reads differently from "that department no longer exists" — and it is
// deliberately smaller than the set of SQLSTATEs and error numbers the
// dialect packages classify.
type Rule string

// The constraint kinds a violation is reported as.
const (
	// RuleUnique — a UNIQUE index or PRIMARY KEY already holds the
	// value.
	RuleUnique Rule = "unique"

	// RuleForeignKey — the referenced row does not exist, or is
	// still referenced.
	RuleForeignKey Rule = "foreign_key"

	// RuleCheck — a CHECK constraint returned false.
	RuleCheck Rule = "check"

	// RuleNotNull — a NOT NULL column received NULL.
	RuleNotNull Rule = "not_null"
)

// FieldError reports a constraint violation against the struct field
// that caused it, so a form layer can put the message next to the input
// the user typed rather than at the top of the page.
//
// It exists because uniqueness cannot be checked in application code
// without a race: between the SELECT that finds the address free and
// the INSERT that takes it, another request can take it. The only
// authority is the unique index, and the only thing it answers with is
// a driver error naming a constraint. Translating that name back into a
// field is the last step, and it is the one every Go codebase either
// skips (check-then-insert, and lose the race) or improvises with a
// strings.Contains over the driver's message.
//
// The dialect packages produce a FieldError from their own classified
// error and keep the original underneath, so nothing that already
// worked stops working:
//
//	err := UserEntity.Create(db, ctx, &u)
//
//	var fe *drops.FieldError
//	switch {
//	case errors.As(err, &fe) && fe.Rule == drops.RuleUnique:
//	    form.AddError(fe.Field, "is already taken")
//	case errors.Is(err, pg.ErrUniqueViolation):
//	    // still true of the same error: the driver error is intact
//	    // underneath, reachable by Is and As alike.
//	}
type FieldError struct {
	// Field is the Go struct field the violation is reported
	// against, e.g. "Email". Always set — a FieldError is not
	// produced when no field can be identified.
	Field string

	// Column is the database column behind Field, when the mapping
	// named one. Empty for a constraint mapped to a field without a
	// column of its own.
	Column string

	// Constraint is the constraint or index name the server
	// blamed. Empty where the server names a column instead, as
	// PostgreSQL does for a not-null violation.
	Constraint string

	// Rule is the kind of constraint that failed.
	Rule Rule

	// Err is the classified dialect error underneath — *pg.PgError,
	// *mysql.ServerError, *sqlite.SQLiteError. Reachable with
	// errors.As, and its sentinel with errors.Is.
	Err error
}

// Error implements error.
func (e *FieldError) Error() string {
	switch {
	case e.Constraint != "":
		return fmt.Sprintf("drops: %s: %s constraint %q violated: %v", e.Field, e.Rule, e.Constraint, e.Err)
	default:
		return fmt.Sprintf("drops: %s: %s constraint violated: %v", e.Field, e.Rule, e.Err)
	}
}

// Unwrap returns the underlying dialect error so errors.Is and
// errors.As keep reaching it — a FieldError adds a reading of the
// failure, it does not replace one.
func (e *FieldError) Unwrap() error { return e.Err }

// AsFieldError reports whether err carries a [FieldError] and returns
// it. It is errors.As with the target declared for you, for the form
// layer that only wants the one question answered.
func AsFieldError(err error) (*FieldError, bool) {
	var fe *FieldError
	if errors.As(err, &fe) {
		return fe, true
	}
	return nil, false
}
