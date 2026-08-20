package mysql

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Server-classified errors. Driver-level failures from Exec / Query are
// wrapped in a [ServerError] whose Sentinel field points at the
// matching value below, so callers can branch with errors.Is without
// depending on the driver's error type:
//
//	if errors.Is(err, mysql.ErrUniqueViolation) {
//	    return "email already taken"
//	}
//
// The names mirror drops/pg's, because the failure modes are the same
// ones; what differs is where the classification comes from. PostgreSQL
// answers a SQLSTATE and drops switches on it. MySQL answers both a
// SQLSTATE and its own error number, and the number is the one that
// identifies the failure: 23000 covers the duplicate key, the missing
// parent row, the NULL in a NOT NULL column and the failed CHECK all at
// once, while 1062, 1452, 1048 and 3819 / 4025 tell them apart. So the
// number is what these map, and [ServerError] carries the SQLSTATE
// alongside it for anything that wants the coarser class.
var (
	// ErrUniqueViolation — error 1062 (and 1586, the same failure
	// reported with the key's name). INSERT or UPDATE collided with a
	// UNIQUE or PRIMARY KEY index.
	ErrUniqueViolation = errors.New("drops/mysql: unique constraint violation")

	// ErrForeignKeyViolation — errors 1451 and 1452. A child row
	// referenced a parent that does not exist (1452), or a parent was
	// deleted or updated while a child still referenced it (1451).
	ErrForeignKeyViolation = errors.New("drops/mysql: foreign-key constraint violation")

	// ErrCheckViolation — error 3819 on MySQL 8, 4025 on MariaDB. A
	// CHECK constraint returned false.
	//
	// The two servers disagree about more than the number: MySQL
	// enforces CHECK from 8.0.16 and parses-then-ignores it before
	// that, while MariaDB has enforced it since 10.2. A schema that
	// leans on CHECK is portable only from MySQL 8.0.16 up.
	ErrCheckViolation = errors.New("drops/mysql: check constraint violation")

	// ErrNotNullViolation — error 1048. A NOT NULL column received
	// NULL.
	ErrNotNullViolation = errors.New("drops/mysql: not-null constraint violation")

	// ErrUndefinedTable — error 1146. The statement named a table that
	// does not exist.
	ErrUndefinedTable = errors.New("drops/mysql: undefined table")

	// ErrUndefinedColumn — error 1054. The statement named a column
	// that does not exist.
	ErrUndefinedColumn = errors.New("drops/mysql: undefined column")

	// ErrDeadlock — error 1213. InnoDB found a lock cycle and chose
	// this transaction as the victim. The server has already rolled
	// the whole transaction back; the work must be re-run. Retryable —
	// see [DefaultRetryPolicy].
	ErrDeadlock = errors.New("drops/mysql: deadlock found when trying to get lock")

	// ErrLockWaitTimeout — error 1205. The statement waited
	// innodb_lock_wait_timeout seconds (50 by default) for a row lock
	// another transaction holds, and gave up.
	//
	// Unlike a deadlock this rolls back only the statement, not the
	// transaction: with the default innodb_rollback_on_timeout=OFF the
	// transaction is still open and still holds everything it locked
	// before. Retrying the statement in place would therefore retry it
	// inside a transaction that is already half-done, which is why the
	// retry in this package is at the transaction level — see
	// [RetryPolicy].
	ErrLockWaitTimeout = errors.New("drops/mysql: lock wait timeout exceeded")
)

// MySQL error numbers this package classifies. They are the identity
// of the failure — the SQLSTATE beside them is far coarser — and are
// spelled out here so the mapping reads as a table rather than as
// magic numbers.
const (
	errDupEntry            = 1062
	errDupEntryWithKeyName = 1586
	errNoReferencedRow     = 1452
	errRowIsReferenced     = 1451
	errCheckConstraint     = 3819 // MySQL 8.0.16+
	errConstraintFailed    = 4025 // MariaDB 10.2+
	errBadNull             = 1048
	errNoSuchTable         = 1146
	errBadField            = 1054
	errLockDeadlock        = 1213
	errLockWaitTimeout     = 1205
)

// ServerError wraps a driver-level error with the matching typed
// Sentinel, the server's error number, the SQLSTATE class, and — when
// the message names one — the offending index or constraint. Use
// errors.Is(err, mysql.ErrXxx) to branch on the failure mode, or
// errors.As to read Number / Code / Constraint.
//
// PostgreSQL's equivalent is called PgError; this one is not called
// MySQLError because the driver already owns that name, and code that
// imports both would be reading two different types spelled the same
// way three lines apart.
type ServerError struct {
	// Number is the MySQL error number, e.g. 1062. Zero when the
	// error carried none.
	Number uint16

	// Code is the five-character SQLSTATE, e.g. "23000". Empty when
	// the driver did not report one.
	Code string

	// Constraint is the index or constraint the server named, when it
	// named one: the key for a duplicate entry, the foreign key for a
	// referential failure, the constraint for a failed CHECK.
	//
	// It is parsed out of the message text, because that is the only
	// place MySQL puts it — there is no structured field for it in the
	// protocol. Treat it as a hint for a log line, not as something to
	// branch on. MySQL 8.0.19 and later qualify a duplicate key with
	// its table ("users.uniq_email"); the qualifier is stripped so the
	// value matches what MariaDB and older MySQL report.
	Constraint string

	// Sentinel is the package-level Err* value classifying the error.
	// nil when the number was read but is not one this package maps.
	Sentinel error

	// Err is the original driver error. Use errors.Unwrap to reach it.
	Err error
}

// Error implements error. The original message is kept verbatim at the
// end so anything already matching on the server's own text — and
// there is a lot of such code in the wild — goes on working.
func (e *ServerError) Error() string {
	switch {
	case e.Constraint != "":
		return fmt.Sprintf("drops/mysql: error %d (%s) on %s: %v", e.Number, e.Code, e.Constraint, e.Err)
	case e.Code != "":
		return fmt.Sprintf("drops/mysql: error %d (%s): %v", e.Number, e.Code, e.Err)
	default:
		return fmt.Sprintf("drops/mysql: error %d: %v", e.Number, e.Err)
	}
}

// Unwrap returns the original driver error so errors.As can walk the
// chain down to it.
func (e *ServerError) Unwrap() error { return e.Err }

// Is matches the package sentinel so errors.Is(err, mysql.ErrXxx)
// returns true for the appropriate failure mode.
func (e *ServerError) Is(target error) bool {
	return e.Sentinel != nil && target == e.Sentinel
}

// classifyError wraps err in a *ServerError when it carries a MySQL
// error number. Errors that do not — a context cancellation, a broken
// connection, a scan failure — pass through untouched so existing error
// handling keeps working.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	number, state, ok := serverErrorFields(err)
	if !ok {
		return err
	}
	se := &ServerError{Number: number, Code: state, Err: err}
	switch number {
	case errDupEntry, errDupEntryWithKeyName:
		se.Sentinel = ErrUniqueViolation
		se.Constraint = duplicateKeyName(err.Error())
	case errNoReferencedRow, errRowIsReferenced:
		se.Sentinel = ErrForeignKeyViolation
		se.Constraint = backtickedName(err.Error(), "CONSTRAINT ")
	case errCheckConstraint:
		se.Sentinel = ErrCheckViolation
		se.Constraint = quotedName(err.Error(), "Check constraint ")
	case errConstraintFailed:
		se.Sentinel = ErrCheckViolation
		se.Constraint = backtickedName(err.Error(), "CONSTRAINT ")
	case errBadNull:
		se.Sentinel = ErrNotNullViolation
	case errNoSuchTable:
		se.Sentinel = ErrUndefinedTable
	case errBadField:
		se.Sentinel = ErrUndefinedColumn
	case errLockDeadlock:
		se.Sentinel = ErrDeadlock
	case errLockWaitTimeout:
		se.Sentinel = ErrLockWaitTimeout
	}
	return se
}

// serverErrorFields digs the error number and SQLSTATE out of whatever
// the driver returned.
//
// drops has no dependencies, so it cannot name *mysql.MySQLError and
// type-assert on it, and the driver's error type — unlike pgconn's —
// exposes no method to assert on either: Number and SQLState are plain
// struct fields. Reflection over the error chain reads them without the
// import, and works for any driver that shapes its error the same way.
// A driver that shapes it differently still lands on the message
// fallback, since every one of them formats the number into the text.
func serverErrorFields(err error) (uint16, string, bool) {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if n, s, ok := reflectErrorFields(e); ok {
			return n, s, true
		}
	}
	return parseErrorMessage(err.Error())
}

func reflectErrorFields(err error) (uint16, string, bool) {
	v := reflect.ValueOf(err)
	for v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return 0, "", false
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0, "", false
	}
	nf := v.FieldByName("Number")
	if !nf.IsValid() || nf.Kind() != reflect.Uint16 {
		return 0, "", false
	}
	number := uint16(nf.Uint())
	if number == 0 {
		return 0, "", false
	}
	return number, reflectSQLState(v), true
}

// reflectSQLState reads the SQLState field, which go-sql-driver/mysql
// declares as a [5]byte and others as a string. An all-zero array means
// the server sent no SQLSTATE.
func reflectSQLState(v reflect.Value) string {
	f := v.FieldByName("SQLState")
	if !f.IsValid() {
		return ""
	}
	switch f.Kind() {
	case reflect.String:
		return f.String()
	case reflect.Array:
		if f.Type().Elem().Kind() != reflect.Uint8 {
			return ""
		}
		out := make([]byte, f.Len())
		for i := range out {
			out[i] = byte(f.Index(i).Uint())
		}
		if strings.Trim(string(out), "\x00") == "" {
			return ""
		}
		return string(out)
	default:
		return ""
	}
}

// parseErrorMessage reads the number back out of the driver's rendered
// message — "Error 1062 (23000): Duplicate entry …", or the older
// "Error 1062: …" from a driver that predates SQLSTATE reporting.
func parseErrorMessage(msg string) (uint16, string, bool) {
	const prefix = "Error "
	i := strings.Index(msg, prefix)
	if i < 0 {
		return 0, "", false
	}
	rest := msg[i+len(prefix):]
	j := 0
	for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
		j++
	}
	if j == 0 {
		return 0, "", false
	}
	number, err := strconv.ParseUint(rest[:j], 10, 16)
	if err != nil || number == 0 {
		return 0, "", false
	}
	state := ""
	if strings.HasPrefix(rest[j:], " (") {
		if end := strings.Index(rest[j+2:], ")"); end == 5 {
			state = rest[j+2 : j+2+end]
		}
	}
	return uint16(number), state, true
}

// duplicateKeyName pulls the index name out of a 1062 message —
// "Duplicate entry 'a@example.com' for key 'uniq_email'" — dropping
// the table qualifier MySQL 8.0.19+ writes in front of it.
func duplicateKeyName(msg string) string {
	name := quotedName(msg, "for key ")
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// quotedName returns the single-quoted identifier following marker.
func quotedName(msg, marker string) string {
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	return between(msg[i+len(marker):], '\'', '\'')
}

// backtickedName returns the backtick-quoted identifier following
// marker, which is how the foreign-key and MariaDB CHECK messages
// spell it.
func backtickedName(msg, marker string) string {
	i := strings.Index(msg, marker)
	if i < 0 {
		return ""
	}
	return between(msg[i+len(marker):], '`', '`')
}

func between(s string, open, shut byte) string {
	if len(s) == 0 || s[0] != open {
		return ""
	}
	end := strings.IndexByte(s[1:], shut)
	if end < 0 {
		return ""
	}
	return s[1 : 1+end]
}
