package stdlib_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/stdlib"
)

// The suite needs a driver but drops has no dependencies, so it brings
// its own: a driver small enough to be read in one screen, which fails
// any statement whose text contains "fail" and otherwise returns one
// row of one column.

// pgError stands in for the rich error a real driver returns — the
// thing a caller reaches for with errors.As to read a SQLSTATE.
type pgError struct{ code string }

func (e *pgError) Error() string { return "fake: SQLSTATE " + e.code }

var errUnique = &pgError{code: "23505"}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return &fakeConn{}, nil }

type fakeConn struct{}

func (c *fakeConn) Prepare(q string) (driver.Stmt, error) { return &fakeStmt{q: q}, nil }
func (c *fakeConn) Close() error                          { return nil }
func (c *fakeConn) Begin() (driver.Tx, error)             { return fakeTx{}, nil }

type fakeTx struct{}

func (fakeTx) Commit() error   { return nil }
func (fakeTx) Rollback() error { return nil }

type fakeStmt struct{ q string }

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }

func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	if strings.Contains(s.q, "fail") {
		return nil, errUnique
	}
	return driver.RowsAffected(1), nil
}

func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	if strings.Contains(s.q, "fail") {
		return nil, errUnique
	}
	return &fakeRows{}, nil
}

type fakeRows struct{ done bool }

func (r *fakeRows) Columns() []string { return []string{"n"} }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

func init() { sql.Register("dropsfake", fakeDriver{}) }

func openFake(t *testing.T) drops.Driver {
	t.Helper()
	db, err := sql.Open("dropsfake", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return stdlib.New(db)
}

// A failed Query must hand back a nil cursor, not a non-nil
// drops.Rows wrapping a nil *sql.Rows: closing that panics, and
// `if rows != nil { rows.Close() }` is the shape every caller in this
// repository writes.
func TestQueryReturnsNilRowsOnError(t *testing.T) {
	d := openFake(t)
	rows, err := d.Query(context.Background(), "SELECT fail")
	if err == nil {
		t.Fatal("expected the query to fail")
	}
	if rows != nil {
		t.Fatalf("rows should be nil alongside an error, got %#v", rows)
	}
}

func TestTxQueryReturnsNilRowsOnError(t *testing.T) {
	d := openFake(t)
	tx, err := d.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(context.Background(), "SELECT fail")
	if err == nil {
		t.Fatal("expected the query to fail")
	}
	if rows != nil {
		t.Fatalf("rows should be nil alongside an error, got %#v", rows)
	}
}

// The driver's own error has to reach the caller intact, or the
// SQLSTATE a dialect switches on is lost in the adapter.
func TestDriverErrorSurvivesTheAdapter(t *testing.T) {
	d := openFake(t)
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{"query", func() error { _, err := d.Query(context.Background(), "SELECT fail"); return err }},
		{"exec", func() error { _, err := d.Exec(context.Background(), "UPDATE fail"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if !errors.Is(err, errUnique) {
				t.Errorf("errors.Is lost the driver error: %v", err)
			}
			var pgErr *pgError
			if !errors.As(err, &pgErr) || pgErr.code != "23505" {
				t.Errorf("errors.As could not reach the SQLSTATE: %v", err)
			}
		})
	}
}

func TestQuerySucceeds(t *testing.T) {
	d := openFake(t)
	rows, err := d.Query(context.Background(), "SELECT n")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var n int64
	if !rows.Next() {
		t.Fatal("expected a row")
	}
	if err := rows.Scan(&n); err != nil || n != 1 {
		t.Fatalf("Scan = %d, %v", n, err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestNestedBeginIsRefused(t *testing.T) {
	d := openFake(t)
	tx, err := d.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Begin(context.Background()); err == nil {
		t.Error("nested Begin should be refused, not silently reuse the outer transaction")
	}
}
