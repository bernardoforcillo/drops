package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// pgxLikeError exercises the *method* branch of the classifier. Note
// that no real driver has this shape: pgconn.PgError exposes only
// Error() and SQLState(), and carries the constraint name as a plain
// field. The driver-shaped fakes are pgconnLikeError and pqLikeError
// below; this one only pins that a wrapper which does expose accessors
// is still preferred to reflection.
type pgxLikeError struct {
	code       string
	constraint string
	msg        string
}

func (e *pgxLikeError) Error() string          { return e.msg }
func (e *pgxLikeError) SQLState() string       { return e.code }
func (e *pgxLikeError) ConstraintName() string { return e.constraint }

// An error exposing Code() returning a 5-char SQLSTATE — the older
// convention, still followed by some wrappers. (Real lib/pq spells it
// SQLState(); see pqLikeError.)
type coderError struct {
	code string
	msg  string
}

func (e *coderError) Error() string { return e.msg }
func (e *coderError) Code() string  { return e.code }

// pgconnLikeError has the shape of the driver most callers actually
// use: pgconn.PgError has exactly two methods, Error() and SQLState(),
// and reports the constraint and column as plain struct fields. A fake
// with a ConstraintName() method would prove only that a branch no
// driver reaches works.
type pgconnLikeError struct {
	Code           string
	ConstraintName string
	ColumnName     string
	Message        string
}

func (e *pgconnLikeError) Error() string    { return e.Message }
func (e *pgconnLikeError) SQLState() string { return e.Code }

// pqLikeError is lib/pq's shape: SQLState() reading a named-string-type
// Code field, with Constraint and Column as plain fields.
type pqErrorCode string

type pqLikeError struct {
	Code       pqErrorCode
	Constraint string
	Column     string
	Message    string
}

func (e *pqLikeError) Error() string    { return e.Message }
func (e *pqLikeError) SQLState() string { return string(e.Code) }

// A driver exposing SQLState() and nothing else — no fields to read,
// so only the message can name the constraint.
type sqlStateOnlyError struct {
	code string
	msg  string
}

func (e *sqlStateOnlyError) Error() string    { return e.msg }
func (e *sqlStateOnlyError) SQLState() string { return e.code }

// An application error that has nothing to do with PostgreSQL but
// carries a five-character Code field.
type appCodeError struct {
	Code string
	Msg  string
}

func (e *appCodeError) Error() string { return e.Msg }

// pqLikeShortCode returns a non-5-char code so we can assert the
// classifier skips it (the lib/pq Code interface is overloaded for
// short notice codes).
type pqLikeShortCode struct{}

func (*pqLikeShortCode) Error() string { return "short" }
func (*pqLikeShortCode) Code() string  { return "00" }

func TestPgErrorClassifiesPgxSQLStates(t *testing.T) {
	cases := []struct {
		code     string
		wantErr  error
		wantCode string
	}{
		{"23505", pg.ErrUniqueViolation, "23505"},
		{"23503", pg.ErrForeignKeyViolation, "23503"},
		{"23514", pg.ErrCheckViolation, "23514"},
		{"23502", pg.ErrNotNullViolation, "23502"},
		{"42P01", pg.ErrUndefinedTable, "42P01"},
		{"42703", pg.ErrUndefinedColumn, "42703"},
		{"40001", pg.ErrSerializationFailure, "40001"},
		{"40P01", pg.ErrDeadlock, "40P01"},
	}
	for _, tc := range cases {
		raw := &pgxLikeError{code: tc.code, constraint: "users_email_unique", msg: "boom"}
		drv := &errDriver{err: raw}
		db := pg.New(drv)
		_, err := db.Exec(context.Background(), "INSERT INTO users ...")
		if !errors.Is(err, tc.wantErr) {
			t.Errorf("SQLSTATE %s: errors.Is should match %v, got %v", tc.code, tc.wantErr, err)
		}
		var pe *pg.PgError
		if !errors.As(err, &pe) || pe.Code != tc.wantCode {
			t.Errorf("SQLSTATE %s: errors.As(*PgError): got %v", tc.code, err)
		}
	}
}

func TestPgErrorExposesConstraintName(t *testing.T) {
	raw := &pgxLikeError{code: "23505", constraint: "users_email_unique", msg: "dup"}
	drv := &errDriver{err: raw}
	db := pg.New(drv)
	_, err := db.Exec(context.Background(), "INSERT INTO users ...")
	var pe *pg.PgError
	if !errors.As(err, &pe) || pe.Constraint != "users_email_unique" {
		t.Errorf("expected constraint name, got %#v", pe)
	}
}

func TestPgErrorHandlesLibpqStyle(t *testing.T) {
	raw := &coderError{code: "23505", msg: "dup"}
	drv := &errDriver{err: raw}
	db := pg.New(drv)
	_, err := db.Exec(context.Background(), "INSERT INTO users ...")
	if !errors.Is(err, pg.ErrUniqueViolation) {
		t.Errorf("lib/pq-style Code(): expected ErrUniqueViolation, got %v", err)
	}
}

func TestPgErrorSkipsUnknownDrivers(t *testing.T) {
	raw := errors.New("some random error")
	drv := &errDriver{err: raw}
	db := pg.New(drv)
	_, err := db.Exec(context.Background(), "INSERT INTO users ...")
	if !errors.Is(err, raw) {
		t.Errorf("unknown driver error must pass through unchanged, got %v", err)
	}
	var pe *pg.PgError
	if errors.As(err, &pe) {
		t.Errorf("unknown driver error must not be wrapped in PgError")
	}
}

func TestPgErrorSkipsShortPqCode(t *testing.T) {
	drv := &errDriver{err: &pqLikeShortCode{}}
	db := pg.New(drv)
	_, err := db.Exec(context.Background(), "INSERT INTO users ...")
	var pe *pg.PgError
	if errors.As(err, &pe) {
		t.Errorf("non-5-char Code() must not classify as PgError")
	}
}

// errDriver returns err on Exec / Query so the classifier sees it.
type errDriver struct {
	err error
}

func (d *errDriver) Exec(_ context.Context, _ string, _ ...any) (drops.Result, error) {
	return nil, d.err
}
func (d *errDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	return nil, d.err
}
func (d *errDriver) Begin(_ context.Context) (drops.Tx, error) { return nil, d.err }

// The fix these pin was a real production bug: the classifier looked
// only for a ConstraintName() method, while pgconn and lib/pq both
// report the name as a plain struct field and neither has ever had an
// accessor — so PgError.Constraint came back empty for every caller of
// either driver.
//
// There are two mechanisms in the fix and the live suite cannot tell
// them apart, because pgx's message happens to contain the name too and
// the message fallback alone satisfies it. So each case here starves
// the mechanism it is not testing: the field cases carry a message with
// no name in it, and the message cases carry no fields.
func TestPgErrorReadsConstraintAndColumnFromDriverStructFields(t *testing.T) {
	cases := []struct {
		name           string
		raw            error
		wantConstraint string
		wantColumn     string
	}{
		{
			// The name is in the field and nowhere else, so only
			// reflection over the struct can find it.
			name: "pgconn field, message says nothing",
			raw: &pgconnLikeError{
				Code:           "23505",
				ConstraintName: "users_email_key",
				Message:        "ERROR: duplicate key value (SQLSTATE 23505)",
			},
			wantConstraint: "users_email_key",
		},
		{
			name: "pgconn not-null names a column, not a constraint",
			raw: &pgconnLikeError{
				Code:       "23502",
				ColumnName: "email",
				Message:    "ERROR: null value violates not-null constraint (SQLSTATE 23502)",
			},
			wantColumn: "email",
		},
		{
			name: "lib/pq field, message says nothing",
			raw: &pqLikeError{
				Code:       "23503",
				Constraint: "accounts_team_id_fkey",
				Message:    "ERROR: foreign key violation (SQLSTATE 23503)",
			},
			wantConstraint: "accounts_team_id_fkey",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := pg.New(&errDriver{err: tc.raw})
			_, err := db.Exec(context.Background(), "INSERT INTO users ...")
			var pe *pg.PgError
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v (%T), want a *pg.PgError", err, err)
			}
			if pe.Constraint != tc.wantConstraint {
				t.Errorf("Constraint = %q, want %q", pe.Constraint, tc.wantConstraint)
			}
			if pe.Column != tc.wantColumn {
				t.Errorf("Column = %q, want %q", pe.Column, tc.wantColumn)
			}
		})
	}
}

// The other half of the fix, starved of fields: a driver that only
// formats the name into its text is still read, which is what keeps
// the classifier working for wrappers that flatten the error to a
// string before drops ever sees it.
func TestPgErrorFallsBackToTheMessageWhenThereIsNoField(t *testing.T) {
	raw := &sqlStateOnlyError{
		code: "23505",
		msg:  `ERROR: duplicate key value violates unique constraint "users_email_key" (SQLSTATE 23505)`,
	}
	db := pg.New(&errDriver{err: raw})
	_, err := db.Exec(context.Background(), "INSERT INTO users ...")
	var pe *pg.PgError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %v (%T), want a *pg.PgError", err, err)
	}
	if pe.Constraint != "users_email_key" {
		t.Errorf("Constraint = %q, want it read out of the message", pe.Constraint)
	}
}

// A SQLSTATE is what proves an error came from PostgreSQL, so it is
// only ever read from a method a driver chose to expose. Guessing at a
// struct field named Code claims unrelated errors: this one would be
// classified as a serialization failure and then silently retried by
// the default RetryPolicy, turning someone else's failure into a
// repeated non-idempotent write.
func TestPgErrorIgnoresAnUnrelatedCodeField(t *testing.T) {
	raw := &appCodeError{Code: "40001", Msg: "payment gateway rejected: code 40001"}
	db := pg.New(&errDriver{err: raw})
	_, err := db.Exec(context.Background(), "SELECT 1")

	var pe *pg.PgError
	if errors.As(err, &pe) {
		t.Fatalf("an unrelated error was classified as %v", pe)
	}
	if errors.Is(err, pg.ErrSerializationFailure) {
		t.Fatal("an unrelated error would be retried as a serialization failure")
	}
	if !errors.Is(err, raw) {
		t.Fatalf("err = %v, want the original error unchanged", err)
	}
}
