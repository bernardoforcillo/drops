package pg

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Package-level sentinel errors. Callers may check them with errors.Is
// to branch on a specific failure mode without inspecting the message.
//
// The decorating wrappers used by builders use fmt.Errorf("...: %w", err)
// so the chain stays intact when extra context is added.
var (
	// ErrReturningRequired is returned by *Builder.All / *Builder.One on
	// INSERT/UPDATE/DELETE statements that were not paired with a
	// Returning(...) clause.
	ErrReturningRequired = errors.New("drops/pg: builder.All/One requires Returning(...)")

	// ErrNoRowsToInsert is returned by InsertBuilder.Exec when no row has
	// been added via Row / Rows.
	ErrNoRowsToInsert = errors.New("drops/pg: INSERT with no rows")

	// ErrNoUpdateAssignments is returned by UpdateBuilder.Exec when no
	// Set assignment has been added.
	ErrNoUpdateAssignments = errors.New("drops/pg: UPDATE with no Set assignments")

	// ErrSchemaRequired is returned by Push when schema is nil.
	ErrSchemaRequired = errors.New("drops/pg: Schema is required")

	// ErrInvalidIdentifier is returned when a SQL identifier (table,
	// column, schema) contains characters that would break out of
	// double-quoted identifier safety (NUL, embedded double quotes).
	ErrInvalidIdentifier = errors.New("drops/pg: invalid SQL identifier")
)

// SQLSTATE-classified errors. Driver-level failures from Exec / Query
// are wrapped in a PgError whose Sentinel field points at the matching
// value below, so callers can branch with errors.Is without depending
// on driver-specific types:
//
//	if errors.Is(err, pg.ErrUniqueViolation) {
//	    return "email already taken"
//	}
//
// PgError exposes the SQLSTATE code and the constraint name when the
// driver reports them.
var (
	// ErrUniqueViolation — SQLSTATE 23505. INSERT / UPDATE collided
	// with a UNIQUE or PRIMARY KEY constraint.
	ErrUniqueViolation = errors.New("drops/pg: unique constraint violation")

	// ErrForeignKeyViolation — SQLSTATE 23503. The referenced row
	// is missing, or a referenced row is being deleted while still
	// referenced.
	ErrForeignKeyViolation = errors.New("drops/pg: foreign-key constraint violation")

	// ErrCheckViolation — SQLSTATE 23514. A CHECK constraint
	// returned false for the supplied value.
	ErrCheckViolation = errors.New("drops/pg: check constraint violation")

	// ErrNotNullViolation — SQLSTATE 23502. A NOT NULL column
	// received NULL.
	ErrNotNullViolation = errors.New("drops/pg: not-null constraint violation")

	// ErrUndefinedTable — SQLSTATE 42P01. Query referenced an
	// unknown table.
	ErrUndefinedTable = errors.New("drops/pg: undefined table")

	// ErrUndefinedColumn — SQLSTATE 42703. Query referenced an
	// unknown column.
	ErrUndefinedColumn = errors.New("drops/pg: undefined column")

	// ErrSerializationFailure — SQLSTATE 40001. A SERIALIZABLE
	// transaction must be retried.
	ErrSerializationFailure = errors.New("drops/pg: serialization failure")

	// ErrDeadlock — SQLSTATE 40P01. The transaction was aborted
	// because it was selected as a deadlock victim.
	ErrDeadlock = errors.New("drops/pg: deadlock detected")
)

// PgError wraps a driver-level error with the matching typed
// Sentinel, the SQLSTATE class, and (when available) the constraint
// name. Use errors.Is(err, pg.ErrXxx) to branch on the failure mode,
// or type-assert through errors.As to read Code / Constraint.
type PgError struct {
	// Code is the five-character SQLSTATE class returned by the
	// driver, e.g. "23505".
	Code string

	// Constraint is the offending constraint name when the server
	// reports one, e.g. "users_email_key". Empty for failures
	// unrelated to a constraint.
	Constraint string

	// Column is the column the server blamed, when it blamed a
	// column rather than a constraint: a not-null violation reports
	// "null value in column \"email\"" and names no constraint,
	// because NOT NULL is a column property and has no name of its
	// own. Empty otherwise.
	Column string

	// Sentinel is the package-level Err* value classifying the
	// error. nil when the SQLSTATE was recognised but is not
	// mapped, or when no SQLSTATE was reported at all.
	Sentinel error

	// Err is the original driver error. Use errors.Unwrap to reach
	// it directly.
	Err error
}

// Error implements error.
func (e *PgError) Error() string {
	if e.Constraint != "" {
		return fmt.Sprintf("drops/pg: SQLSTATE %s (%s): %v", e.Code, e.Constraint, e.Err)
	}
	if e.Code != "" {
		return fmt.Sprintf("drops/pg: SQLSTATE %s: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("drops/pg: %v", e.Err)
}

// Unwrap returns the original driver error so errors.As can walk the
// chain.
func (e *PgError) Unwrap() error { return e.Err }

// Is matches the package sentinel so errors.Is(err, pg.ErrXxx)
// returns true for the appropriate failure mode.
func (e *PgError) Is(target error) bool {
	return e.Sentinel != nil && target == e.Sentinel
}

// classifyError inspects err for SQLSTATE-carrying driver interfaces
// and, when one is found, returns a *PgError wrapping it with the
// matching sentinel. Errors that don't expose a SQLSTATE pass through
// unchanged so existing error handling keeps working.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	code := sqlState(err)
	if code == "" {
		return err
	}
	pe := &PgError{Code: code, Constraint: constraintName(err), Column: columnName(err), Err: err}
	switch code {
	case "23505":
		pe.Sentinel = ErrUniqueViolation
	case "23503":
		pe.Sentinel = ErrForeignKeyViolation
	case "23514":
		pe.Sentinel = ErrCheckViolation
	case "23502":
		pe.Sentinel = ErrNotNullViolation
	case "42P01":
		pe.Sentinel = ErrUndefinedTable
	case "42703":
		pe.Sentinel = ErrUndefinedColumn
	case "40001":
		pe.Sentinel = ErrSerializationFailure
	case "40P01":
		pe.Sentinel = ErrDeadlock
	}
	return pe
}

// sqlState walks err's chain looking for an interface that exposes a
// PostgreSQL SQLSTATE code. Both drivers anyone uses have one —
// pgconn.PgError and pq.Error each spell it SQLState() — and the
// Code() form covers the older convention some wrappers still follow.
// A SQLSTATE is the one thing that proves the error came from the
// server, so it is read only from a method a driver chose to expose,
// never guessed at from a struct field.
func sqlState(err error) string {
	type sqlStater interface{ SQLState() string }
	type coder interface{ Code() string }

	var s sqlStater
	if errors.As(err, &s) {
		return s.SQLState()
	}
	var c coder
	if errors.As(err, &c) {
		if code := c.Code(); len(code) == 5 {
			return code
		}
	}
	// Deliberately no reflective fallback onto a "Code" struct field.
	// It looks like it would help lib/pq, whose SQLSTATE really is a
	// field of a named string type (pq.Error.Code) — but *pq.Error
	// also has SQLState(), so the branch above already classifies it,
	// and pgconn does too. Reading any five-character Code field
	// instead claims every unrelated error that happens to carry one:
	// an application error with Code "40001" would be classified as a
	// serialization failure and silently retried by the default
	// [RetryPolicy], which is a far worse failure than not
	// classifying an exotic driver at all.
	return ""
}

// constraintName extracts the name of the constraint the server
// refused on.
//
// A method assertion is not enough, and for the two drivers anyone
// actually uses it finds nothing at all: pgconn and lib/pq both carry
// the name, and both carry it as a plain struct field —
// pgconn.PgError.ConstraintName, pq.Error.Constraint — with no
// accessor. So the field is read by reflection over the error chain,
// the way drops/mysql already reads MySQL's error number, and the
// message is the last resort for a driver that only formats the name
// into its text. The method case stays first because a wrapper that
// does expose one is answering more cheaply than either.
func constraintName(err error) string {
	type namer interface{ ConstraintName() string }
	var n namer
	if errors.As(err, &n) {
		if s := n.ConstraintName(); s != "" {
			return s
		}
	}
	if s := errorField(err, "ConstraintName", "Constraint"); s != "" {
		return s
	}
	// "… violates unique constraint \"users_email_key\"", and the
	// same shape for foreign key, check and exclusion constraints.
	return quotedAfter(err.Error(), `constraint "`)
}

// columnName extracts the column the server blamed. PostgreSQL reports
// one for a not-null violation (23502) and for an undefined column
// (42703); for everything else it names a constraint instead and this
// is empty.
func columnName(err error) string {
	type namer interface{ ColumnName() string }
	var n namer
	if errors.As(err, &n) {
		if s := n.ColumnName(); s != "" {
			return s
		}
	}
	if s := errorField(err, "ColumnName", "Column"); s != "" {
		return s
	}
	// "null value in column \"email\" of relation \"users\" …"
	return quotedAfter(err.Error(), `column "`)
}

// errorField walks err's chain looking for a struct field named by one
// of names that holds a non-empty string. Field order follows names,
// so the caller decides which spelling wins when a driver has both.
func errorField(err error, names ...string) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		v := reflect.ValueOf(e)
		for v.Kind() == reflect.Ptr {
			if v.IsNil() {
				break
			}
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			continue
		}
		for _, name := range names {
			f := v.FieldByName(name)
			if f.IsValid() && f.Kind() == reflect.String && f.String() != "" {
				return f.String()
			}
		}
	}
	return ""
}

// quotedAfter returns the double-quoted identifier that follows the
// first occurrence of marker, which must itself end with the opening
// quote.
func quotedAfter(msg, marker string) string {
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(marker):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	return rest[:end]
}
