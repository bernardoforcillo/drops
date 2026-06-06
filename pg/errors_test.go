package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// pgxLikeError mimics the pgx / pgconn surface — SQLState() and
// ConstraintName() methods — so the classifier picks it up via
// errors.As.
type pgxLikeError struct {
	code       string
	constraint string
	msg        string
}

func (e *pgxLikeError) Error() string         { return e.msg }
func (e *pgxLikeError) SQLState() string      { return e.code }
func (e *pgxLikeError) ConstraintName() string { return e.constraint }

// lib/pq-style error: exposes Code() returning a 5-char SQLSTATE.
type pqLikeError struct {
	code string
	msg  string
}

func (e *pqLikeError) Error() string { return e.msg }
func (e *pqLikeError) Code() string  { return e.code }

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
	raw := &pqLikeError{code: "23505", msg: "dup"}
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
