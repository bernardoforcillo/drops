package d1

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// D1 envelope error codes worth branching on.
const (
	// CodeStatementError — D1 could not prepare or run the
	// statement. The message is SQLite's own, which is what makes
	// [ClassifyError] able to read a constraint out of it.
	CodeStatementError = 7500

	// CodeDatabaseNotFound — no database with that ID on this
	// account.
	CodeDatabaseNotFound = 7404
)

// Constraint-classified errors, mirroring the names
// [github.com/bernardoforcillo/drops/sqlite] uses for the same
// failures.
//
// They are declared here rather than imported from the sqlite package
// because a Driver is a layer below a dialect: drops/stdlib, the
// other adapter in this repository, imports nothing but the root
// package, and this one has no more reason to reach up into a dialect
// than that one does. The same driver is usable from a dialect that
// is not sqlite — a plain [github.com/bernardoforcillo/drops.SQL]
// against D1 needs no dialect at all.
//
// It costs nothing at the call site. A statement issued through
// [github.com/bernardoforcillo/drops/sqlite.DB] is classified twice:
// once here, and once by the dialect, which reads the same SQLite
// message out of the error string and produces its own
// *sqlite.SQLiteError. So errors.Is(err, sqlite.ErrUniqueViolation)
// answers on that path, and so does
// [github.com/bernardoforcillo/drops.AsFieldError] — the entity layer
// never learns which driver it was talking to.
var (
	// ErrUniqueViolation — a UNIQUE index or PRIMARY KEY already
	// held the value.
	ErrUniqueViolation = errors.New("drops/cloudflare/d1: unique constraint violation")

	// ErrForeignKeyViolation — the referenced row does not exist, or
	// is still referenced.
	//
	// Worth knowing on D1 specifically: foreign keys are enforced
	// (there is no PRAGMA foreign_keys to forget), and inside a
	// batch they are deferred to the end of it. A parent and its
	// children can go in either order within one batch; across two
	// requests they cannot.
	ErrForeignKeyViolation = errors.New("drops/cloudflare/d1: foreign-key constraint violation")

	// ErrCheckViolation — a CHECK constraint returned false.
	ErrCheckViolation = errors.New("drops/cloudflare/d1: check constraint violation")

	// ErrNotNullViolation — a NOT NULL column received NULL.
	ErrNotNullViolation = errors.New("drops/cloudflare/d1: not-null constraint violation")

	// ErrUndefinedTable — "no such table".
	ErrUndefinedTable = errors.New("drops/cloudflare/d1: undefined table")

	// ErrUndefinedColumn — "no such column".
	ErrUndefinedColumn = errors.New("drops/cloudflare/d1: undefined column")
)

// Error is a D1 statement failure: the Cloudflare API error
// underneath, plus the reading of it that lets callers branch.
//
// It exists because D1 is two services stacked on one another and a
// failure can come from either. A bad token is Cloudflare's failure
// and arrives as a numeric code; a violated UNIQUE index is SQLite's
// and arrives as a message inside Cloudflare's envelope. Both reach
// the caller through this type, and errors.Is answers for both.
type Error struct {
	// Sentinel is the constraint classification — one of the Err*
	// values above — or nil when the failure was not SQLite's.
	Sentinel error

	// Message is what D1 said, which for a statement failure is
	// SQLite's own text: "UNIQUE constraint failed: users.email".
	Message string

	// Err is the underlying
	// [github.com/bernardoforcillo/drops/cloudflare.APIError].
	Err error
}

// Error implements error.
//
// SQLite's message is reproduced verbatim, and that is load-bearing
// rather than cosmetic: a dialect package reading this error
// classifies it from the text, because the text is the only thing D1
// puts on the wire. Rewording it would break that reading.
func (e *Error) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("drops/cloudflare/d1: %v", e.Err)
	}
	return fmt.Sprintf("drops/cloudflare/d1: %s", e.Message)
}

// Unwrap returns the Cloudflare error so errors.As reaches
// *cloudflare.APIError, and errors.Is reaches
// [github.com/bernardoforcillo/drops/cloudflare.ErrUnauthorized] and
// the rest.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the constraint sentinel.
func (e *Error) Is(target error) bool {
	return e.Sentinel != nil && target == e.Sentinel
}

// ClassifyError reads a SQLite failure out of a Cloudflare API error.
//
// D1 passes SQLite's message through verbatim inside the envelope's
// errors[]. That message is the only classification available: there
// is no SQLSTATE and no extended result code on the wire, so what the
// local dialect reads from a driver's error code has to be read here
// from the text.
//
// The text is more reliable than that makes it sound. SQLite writes
// it — not D1, not a driver — and it has not changed in the lifetime
// of the engine, which is why
// [github.com/bernardoforcillo/drops/sqlite] already falls back to
// matching on it for drivers that expose no code.
//
// Errors that are not statement failures — an expired token, a
// deleted database, a rate limit — pass through unchanged, still
// matching their [github.com/bernardoforcillo/drops/cloudflare]
// sentinels.
func ClassifyError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *cloudflare.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	msg := apiErr.FirstMessage()
	if msg == "" {
		return err
	}
	sentinel := sentinelFromMessage(msg)
	if sentinel == nil {
		return err
	}
	return &Error{Sentinel: sentinel, Message: msg, Err: err}
}

// sentinelFromMessage maps SQLite's own wording onto the sentinels
// above. The phrases are SQLite's, and the mapping is the same one
// drops/sqlite applies to a driver that exposes no result code.
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
	}
	return nil
}

// ConstraintDetail returns what SQLite wrote after "constraint
// failed: " — the qualified column list for a UNIQUE or NOT NULL
// failure, the constraint's name for a named CHECK.
//
// It is what a form layer needs to name the field that was refused,
// and it is empty for a foreign-key failure because SQLite does not
// say which key it was.
func ConstraintDetail(err error) string {
	var d1Err *Error
	if !errors.As(err, &d1Err) {
		return ""
	}
	const marker = "constraint failed: "
	i := strings.LastIndex(d1Err.Message, marker)
	if i < 0 {
		return ""
	}
	detail := strings.TrimSpace(d1Err.Message[i+len(marker):])
	if strings.HasSuffix(detail, "constraint failed") {
		return ""
	}
	return detail
}

// ConstraintColumns parses [ConstraintDetail] into the table and the
// unqualified column names, for the failures where SQLite wrote a
// column list. A detail that is not one — a CHECK constraint's name —
// yields nothing rather than a column that does not exist.
func ConstraintColumns(err error) (table string, columns []string) {
	detail := ConstraintDetail(err)
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
