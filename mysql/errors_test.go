package mysql_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mysql"
)

// driverError has the shape go-sql-driver/mysql gives *MySQLError:
// plain fields, no accessor methods. drops cannot import the driver, so
// this is the shape the classifier has to read by reflection — the
// integration suite is where the real one is put through it.
type driverError struct {
	Number   uint16
	SQLState [5]byte
	Message  string
}

func (e *driverError) Error() string {
	if e.SQLState != [5]byte{} {
		return fmt.Sprintf("Error %d (%s): %s", e.Number, e.SQLState, e.Message)
	}
	return fmt.Sprintf("Error %d: %s", e.Number, e.Message)
}

func newDriverError(number uint16, state, msg string) *driverError {
	e := &driverError{Number: number, Message: msg}
	copy(e.SQLState[:], state)
	return e
}

// classifyThroughDB runs a statement whose driver fails with err, and
// returns what the caller sees. Classification happens inside DB.Exec,
// so this is the only honest way to observe it from outside the
// package — and it is also where the sentinels have to work.
func classifyThroughDB(t *testing.T, err error) error {
	t.Helper()
	_, got := mysql.New(&fakeDriver{execFail: err}).Exec(context.Background(), "SELECT 1")
	if got == nil {
		t.Fatal("Exec swallowed the driver error")
	}
	return got
}

func TestClassifyMapsEveryNumberToItsSentinel(t *testing.T) {
	cases := []struct {
		number   uint16
		state    string
		message  string
		sentinel error
	}{
		{1062, "23000", "Duplicate entry 'a@example.com' for key 'uniq_email'", mysql.ErrUniqueViolation},
		{1586, "23000", "Duplicate entry 'x' for key 'uniq_email'", mysql.ErrUniqueViolation},
		{1452, "23000", "Cannot add or update a child row: a foreign key constraint fails (`drops`.`c`, CONSTRAINT `fk_c_p` FOREIGN KEY (`p_id`) REFERENCES `p` (`id`))", mysql.ErrForeignKeyViolation},
		{1451, "23000", "Cannot delete or update a parent row: a foreign key constraint fails (`drops`.`c`, CONSTRAINT `fk_c_p` FOREIGN KEY (`p_id`) REFERENCES `p` (`id`))", mysql.ErrForeignKeyViolation},
		{3819, "HY000", "Check constraint 'chk_positive' is violated.", mysql.ErrCheckViolation},
		{4025, "23000", "CONSTRAINT `chk_positive` failed for `drops`.`t`", mysql.ErrCheckViolation},
		{1048, "23000", "Column 'email' cannot be null", mysql.ErrNotNullViolation},
		{1146, "42S02", "Table 'drops.nope' doesn't exist", mysql.ErrUndefinedTable},
		{1054, "42S22", "Unknown column 'nope' in 'SELECT'", mysql.ErrUndefinedColumn},
		{1213, "40001", "Deadlock found when trying to get lock; try restarting transaction", mysql.ErrDeadlock},
		{1205, "HY000", "Lock wait timeout exceeded; try restarting transaction", mysql.ErrLockWaitTimeout},
	}
	for _, tc := range cases {
		err := classifyThroughDB(t, newDriverError(tc.number, tc.state, tc.message))
		if !errors.Is(err, tc.sentinel) {
			t.Errorf("%d: errors.Is(%v, %v) = false", tc.number, err, tc.sentinel)
		}
		var se *mysql.ServerError
		if !errors.As(err, &se) {
			t.Fatalf("%d: not a *ServerError: %v", tc.number, err)
		}
		if se.Number != tc.number || se.Code != tc.state {
			t.Errorf("%d: got number %d code %q", tc.number, se.Number, se.Code)
		}
	}
}

// The sentinels must not collapse into each other: a caller branching
// on a duplicate key must not catch a deadlock.
func TestSentinelsDoNotOverlap(t *testing.T) {
	err := classifyThroughDB(t, newDriverError(1062, "23000", "Duplicate entry '1' for key 'PRIMARY'"))
	for _, other := range []error{
		mysql.ErrForeignKeyViolation, mysql.ErrCheckViolation, mysql.ErrNotNullViolation,
		mysql.ErrUndefinedTable, mysql.ErrUndefinedColumn, mysql.ErrDeadlock, mysql.ErrLockWaitTimeout,
	} {
		if errors.Is(err, other) {
			t.Errorf("1062 also matched %v", other)
		}
	}
}

func TestServerErrorReportsTheNamedConstraint(t *testing.T) {
	cases := []struct {
		message string
		number  uint16
		want    string
	}{
		{"Duplicate entry 'a' for key 'uniq_email'", 1062, "uniq_email"},
		// MySQL 8.0.19+ qualifies the key with its table; MariaDB and
		// older MySQL do not, and callers should see one spelling.
		{"Duplicate entry 'a' for key 'users.uniq_email'", 1062, "uniq_email"},
		{"Cannot add or update a child row: a foreign key constraint fails (`drops`.`c`, CONSTRAINT `fk_c_p` FOREIGN KEY (`p_id`) REFERENCES `p` (`id`))", 1452, "fk_c_p"},
		{"CONSTRAINT `chk_positive` failed for `drops`.`t`", 4025, "chk_positive"},
		{"Check constraint 'chk_positive' is violated.", 3819, "chk_positive"},
	}
	for _, tc := range cases {
		err := classifyThroughDB(t, newDriverError(tc.number, "23000", tc.message))
		var se *mysql.ServerError
		if !errors.As(err, &se) {
			t.Fatalf("not a *ServerError: %v", err)
		}
		if se.Constraint != tc.want {
			t.Errorf("Constraint = %q, want %q (from %q)", se.Constraint, tc.want, tc.message)
		}
	}
}

// A driver that hides its error behind a plain wrapper, or that offers
// no struct fields at all, must still be classified: the number is in
// the message either way.
func TestClassifyReadsWrappedAndTextOnlyErrors(t *testing.T) {
	wrapped := fmt.Errorf("insert users: %w", newDriverError(1062, "23000", "Duplicate entry '1' for key 'PRIMARY'"))
	if err := classifyThroughDB(t, wrapped); !errors.Is(err, mysql.ErrUniqueViolation) {
		t.Errorf("wrapped: %v", err)
	}

	textOnly := errors.New("Error 1213 (40001): Deadlock found when trying to get lock; try restarting transaction")
	if err := classifyThroughDB(t, textOnly); !errors.Is(err, mysql.ErrDeadlock) {
		t.Errorf("text-only: %v", err)
	}
}

// Errors that are not the server's must pass through untouched —
// wrapping a cancelled context in a ServerError would make every
// caller's errors.Is(err, context.Canceled) fail.
func TestClassifyLeavesNonServerErrorsAlone(t *testing.T) {
	for _, err := range []error{context.Canceled, errors.New("driver: bad connection")} {
		got := classifyThroughDB(t, err)
		if !errors.Is(got, err) {
			t.Errorf("%v was not preserved: %v", err, got)
		}
		var se *mysql.ServerError
		if errors.As(got, &se) {
			t.Errorf("%v was wrongly classified as %v", err, se)
		}
	}
}

// An unmapped number still yields a ServerError, so a caller can read
// the number drops does not name.
func TestUnmappedNumberStillCarriesTheNumber(t *testing.T) {
	err := classifyThroughDB(t, newDriverError(1364, "HY000", "Field 'name' doesn't have a default value"))
	var se *mysql.ServerError
	if !errors.As(err, &se) {
		t.Fatalf("not a *ServerError: %v", err)
	}
	if se.Number != 1364 || se.Sentinel != nil {
		t.Errorf("number = %d, sentinel = %v", se.Number, se.Sentinel)
	}
	// And the server's own text is still in the message, for the code
	// that greps for it.
	if !strings.Contains(se.Error(), "doesn't have a default value") {
		t.Errorf("message lost the server's text: %s", se.Error())
	}
}
