package sqlite

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Sentinel errors for assertable failure modes. They mirror
// drops/pg's error surface so code that switches dialects keeps the
// same errors.Is checks.
var (
	// ErrNoRows is returned by ScanOne when the result set is empty.
	ErrNoRows = errors.New("drops/sqlite: no rows in result set")

	// ErrNoRowsToInsert is returned when an INSERT has no rows to write.
	ErrNoRowsToInsert = errors.New("drops/sqlite: INSERT with no rows")

	// ErrInvalidIdentifier is returned (or panicked, at declaration
	// time) for an identifier that cannot be safely quoted.
	ErrInvalidIdentifier = errors.New("drops/sqlite: invalid identifier")

	// ErrBusy / ErrLocked are the retryable-contention sentinels
	// matching SQLITE_BUSY and SQLITE_LOCKED. A RetryPolicy listing
	// them (as DefaultRetryPolicy does) retries a transaction when the
	// underlying driver reports either — matched by errors.Is or by the
	// driver error's message (drivers rarely wrap a comparable
	// sentinel).
	ErrBusy   = errors.New("drops/sqlite: database is busy (SQLITE_BUSY)")
	ErrLocked = errors.New("drops/sqlite: database is locked (SQLITE_LOCKED)")
)

// Constraint-classified errors. Driver-level failures from Exec /
// Query are wrapped in a [SQLiteError] whose Sentinel field points at
// the matching value below, so callers branch with errors.Is rather
// than matching the driver's message themselves:
//
//	if errors.Is(err, sqlite.ErrUniqueViolation) {
//	    return "email already taken"
//	}
//
// The names mirror drops/pg's and drops/mysql's because the failure
// modes are the same; what differs is where the classification comes
// from. PostgreSQL answers a SQLSTATE and MySQL an error number, both
// carried in the protocol. SQLite has neither: it answers an extended
// result code — SQLITE_CONSTRAINT_UNIQUE is 2067, SQLITE_CONSTRAINT
// (19) with the sub-code in the high byte — and the code reaches Go
// only if the driver chose to expose it. So [SQLiteError] reads the
// code where a driver offers one and falls back to the message text,
// which every driver passes through verbatim from the engine.
var (
	// ErrUniqueViolation — SQLITE_CONSTRAINT_UNIQUE (2067),
	// _PRIMARYKEY (1555) and _ROWID (2579). All three are one
	// failure to a caller: a value that had to be unique was not.
	ErrUniqueViolation = errors.New("drops/sqlite: unique constraint violation")

	// ErrForeignKeyViolation — SQLITE_CONSTRAINT_FOREIGNKEY (787).
	//
	// SQLite enforces foreign keys only when the connection has
	// PRAGMA foreign_keys=ON, which is off by default and per
	// connection, not per database. A schema with references that
	// never fail is the usual sign it was never turned on.
	ErrForeignKeyViolation = errors.New("drops/sqlite: foreign-key constraint violation")

	// ErrCheckViolation — SQLITE_CONSTRAINT_CHECK (275).
	ErrCheckViolation = errors.New("drops/sqlite: check constraint violation")

	// ErrNotNullViolation — SQLITE_CONSTRAINT_NOTNULL (1299).
	ErrNotNullViolation = errors.New("drops/sqlite: not-null constraint violation")

	// ErrUndefinedTable — SQLITE_ERROR (1) with "no such table".
	// SQLite reports every statement-preparation failure under the
	// one generic code, so this is classified from the message.
	ErrUndefinedTable = errors.New("drops/sqlite: undefined table")

	// ErrUndefinedColumn — SQLITE_ERROR (1) with "no such column".
	ErrUndefinedColumn = errors.New("drops/sqlite: undefined column")
)

// SQLite result codes this package classifies. The constraint codes
// are SQLITE_CONSTRAINT (19) with the sub-code in the second byte,
// spelled out here so the mapping reads as a table.
const (
	codeError = 1 // SQLITE_ERROR, the catch-all for a statement that would not prepare

	codeBusy         = 5
	codeBusyRecovery = 261
	codeBusySnapshot = 517
	codeBusyTimeout  = 773

	codeLocked            = 6
	codeLockedSharedCache = 262
	codeLockedVTab        = 518

	codeConstraint           = 19
	codeConstraintCheck      = 275
	codeConstraintForeignKey = 787
	codeConstraintNotNull    = 1299
	codeConstraintPrimaryKey = 1555
	codeConstraintUnique     = 2067
	codeConstraintRowID      = 2579
)

// SQLiteError wraps a driver-level error with the matching typed
// Sentinel, the extended result code where the driver exposed one, and
// the detail SQLite wrote into the message. Use errors.Is(err,
// sqlite.ErrXxx) to branch on the failure mode, or errors.As to read
// Code / Table / Columns.
type SQLiteError struct {
	// Code is the extended result code, e.g. 2067. Zero when the
	// driver exposed none and the classification came from the
	// message.
	Code int

	// Detail is what SQLite wrote after "<kind> constraint failed:"
	// — the qualified column list for UNIQUE and NOT NULL, the
	// constraint's name for a named CHECK, and the check expression
	// for an unnamed one. Empty where SQLite gave no detail, which
	// is the case for every foreign-key failure: the engine does
	// not say which key it was.
	Detail string

	// Table and Columns are Detail parsed, for the failures where
	// it is a column list. This is the one thing SQLite gives that
	// its siblings do not — PostgreSQL and MySQL name a constraint
	// and leave the columns to be looked up.
	Table   string
	Columns []string

	// Sentinel is the package-level Err* value classifying the
	// error.
	Sentinel error

	// Err is the original driver error. Use errors.Unwrap to reach
	// it.
	Err error
}

// Error implements error. The driver's own message is kept at the end,
// because it is the only place a foreign-key failure says anything at
// all and because code already matching on it goes on working.
func (e *SQLiteError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("drops/sqlite: result code %d: %v", e.Code, e.Err)
	}
	return fmt.Sprintf("drops/sqlite: %v", e.Err)
}

// Unwrap returns the original driver error so errors.As can walk the
// chain down to it.
func (e *SQLiteError) Unwrap() error { return e.Err }

// Is matches the package sentinel so errors.Is(err, sqlite.ErrXxx)
// returns true for the appropriate failure mode.
func (e *SQLiteError) Is(target error) bool {
	return e.Sentinel != nil && target == e.Sentinel
}

// classifyError wraps err in a *SQLiteError when it is a failure this
// package recognises.
//
// Unlike its siblings it wraps only what it classified. pg wraps
// anything carrying a SQLSTATE and mysql anything carrying an error
// number, because the presence of either proves the error came from the
// server. SQLite offers no such proof — a driver error is a plain error
// like any other — so wrapping on suspicion would swallow context
// cancellations and drops' own sentinels into a type that claims the
// engine spoke.
func classifyError(err error) error {
	if err == nil {
		return nil
	}
	code := resultCode(err)
	msg := err.Error()
	sentinel := sentinelFor(code, msg)
	if sentinel == nil {
		return err
	}
	se := &SQLiteError{Code: code, Sentinel: sentinel, Err: err}
	se.Detail = constraintDetail(msg)
	if sentinel == ErrUniqueViolation || sentinel == ErrNotNullViolation {
		se.Table, se.Columns = splitQualifiedColumns(se.Detail)
	}
	return se
}

// sentinelFor classifies by result code where there is one, and by
// message where there is not — either because the driver hides the code
// or because SQLite folded the failure into the generic SQLITE_ERROR.
func sentinelFor(code int, msg string) error {
	switch code {
	case codeConstraintUnique, codeConstraintPrimaryKey, codeConstraintRowID:
		return ErrUniqueViolation
	case codeConstraintForeignKey:
		return ErrForeignKeyViolation
	case codeConstraintCheck:
		return ErrCheckViolation
	case codeConstraintNotNull:
		return ErrNotNullViolation
	case codeBusy, codeBusyRecovery, codeBusySnapshot, codeBusyTimeout:
		return ErrBusy
	case codeLocked, codeLockedSharedCache, codeLockedVTab:
		return ErrLocked
	case codeError, codeConstraint:
		// Both are generic on purpose: SQLITE_ERROR covers every
		// statement that would not prepare, and a bare
		// SQLITE_CONSTRAINT is a constraint failure the engine did
		// not sub-classify. Only the message tells them apart.
		return sentinelFromMessage(msg)
	}
	return sentinelFromMessage(msg)
}

// sentinelFromMessage reads the engine's own words. SQLite writes the
// same text whatever the driver, and for SQLITE_ERROR (1) and a bare
// SQLITE_CONSTRAINT (19) it is the only thing that distinguishes one
// failure from another.
func sentinelFromMessage(msg string) error {
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"),
		strings.Contains(msg, "PRIMARY KEY constraint failed"):
		return ErrUniqueViolation
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return ErrForeignKeyViolation
	case strings.Contains(msg, "CHECK constraint failed"):
		return ErrCheckViolation
	case strings.Contains(msg, "NOT NULL constraint failed"):
		return ErrNotNullViolation
	case strings.Contains(msg, "no such table"):
		return ErrUndefinedTable
	case strings.Contains(msg, "no such column"):
		return ErrUndefinedColumn
	case strings.Contains(msg, "database table is locked"):
		return ErrLocked
	case strings.Contains(msg, "database is locked"):
		return ErrBusy
	}
	return nil
}

// resultCode digs the extended result code out of whatever the driver
// returned.
//
// drops has no dependencies, so it can neither name *sqlite.Error nor
// *sqlite3.Error. modernc.org/sqlite exposes a Code() int method;
// mattn/go-sqlite3 exposes Code and ExtendedCode as struct fields of
// named integer types. The method is asserted first, the fields are
// read by reflection, and a driver that offers neither lands on the
// message classification instead. ExtendedCode wins where both are
// present: the plain code says "a constraint failed", the extended one
// says which.
func resultCode(err error) int {
	type coder interface{ Code() int }
	var c coder
	if errors.As(err, &c) {
		if code := c.Code(); code != 0 {
			return code
		}
	}
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
		for _, name := range []string{"ExtendedCode", "Code"} {
			f := v.FieldByName(name)
			if f.IsValid() && isSignedInt(f) && f.Int() != 0 {
				return int(f.Int())
			}
		}
	}
	return 0
}

func isSignedInt(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

// constraintDetail returns what SQLite wrote after "constraint failed:
// ".
//
// The last occurrence is the one that matters. A driver prepends its
// own rendering of the result code — modernc writes "constraint failed:
// UNIQUE constraint failed: accounts.email (2067)", the words twice and
// the number at the end — so the first marker belongs to the driver and
// the second to the engine. A failure the engine gave no detail for,
// which is every foreign-key violation, ends on the words themselves
// and yields nothing rather than the sentence fragment.
func constraintDetail(msg string) string {
	const marker = "constraint failed: "
	i := strings.LastIndex(msg, marker)
	if i < 0 {
		return ""
	}
	detail := trimResultCodeSuffix(strings.TrimSpace(msg[i+len(marker):]))
	if strings.HasSuffix(detail, "constraint failed") {
		return ""
	}
	return detail
}

// trimResultCodeSuffix removes a trailing " (2067)" — the numeric code
// modernc.org/sqlite appends and mattn/go-sqlite3 does not.
func trimResultCodeSuffix(s string) string {
	if !strings.HasSuffix(s, ")") {
		return s
	}
	open := strings.LastIndex(s, " (")
	if open < 0 {
		return s
	}
	digits := s[open+2 : len(s)-1]
	if digits == "" {
		return s
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return s
		}
	}
	return s[:open]
}

// splitQualifiedColumns parses "users.email, users.nick" into the table
// and the unqualified column names. A detail that is not a column list
// — a CHECK constraint's name, say — yields nothing rather than a
// column that does not exist.
func splitQualifiedColumns(detail string) (table string, columns []string) {
	if detail == "" {
		return "", nil
	}
	for _, part := range strings.Split(detail, ",") {
		part = strings.TrimSpace(part)
		dot := strings.LastIndex(part, ".")
		if dot <= 0 || dot == len(part)-1 {
			return "", nil
		}
		if table == "" {
			table = part[:dot]
		}
		columns = append(columns, part[dot+1:])
	}
	return table, columns
}
