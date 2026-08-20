package pgwire

import "strings"

// Error is an ErrorResponse from the server.
//
// SQLState and ConstraintName are the two accessors drops/pg looks for
// through errors.As when it classifies a failure, so a violation
// raised through this driver reaches the caller as pg.ErrUniqueViolation
// exactly as it would through pgx.
type Error struct {
	Severity   string
	Code       string
	Message    string
	Detail     string
	Hint       string
	Constraint string
	Position   string
	Where      string
}

func (e *Error) Error() string {
	var b strings.Builder
	if e.Severity != "" {
		b.WriteString(strings.ToLower(e.Severity))
		b.WriteString(": ")
	}
	b.WriteString(e.Message)
	if e.Code != "" {
		b.WriteString(" (SQLSTATE " + e.Code + ")")
	}
	if e.Detail != "" {
		b.WriteString("\n  detail: " + e.Detail)
	}
	if e.Hint != "" {
		b.WriteString("\n  hint: " + e.Hint)
	}
	return b.String()
}

// SQLState returns the five-character SQLSTATE code.
func (e *Error) SQLState() string { return e.Code }

// ConstraintName returns the offending constraint, when the server
// named one.
func (e *Error) ConstraintName() string { return e.Constraint }

// parseErrorResponse decodes the field-tagged body shared by
// ErrorResponse and NoticeResponse.
func parseErrorResponse(r *readBuf) *Error {
	e := &Error{}
	for {
		typ := r.byte()
		if typ == 0 || r.err != nil {
			return e
		}
		value := r.str()
		switch typ {
		case 'S':
			e.Severity = value
		case 'C':
			e.Code = value
		case 'M':
			e.Message = value
		case 'D':
			e.Detail = value
		case 'H':
			e.Hint = value
		case 'n':
			e.Constraint = value
		case 'P':
			e.Position = value
		case 'W':
			e.Where = value
		}
	}
}

// boilerplateCodes are the SQLSTATEs PostgreSQL attaches to the
// notices an IF [NOT] EXISTS clause produces when it does nothing:
// duplicate schema, duplicate table, duplicate object, and their
// counterparts on the way down.
var boilerplateCodes = map[string]bool{
	"42P06": true, // schema "x" already exists, skipping
	"42P07": true, // relation "x" already exists, skipping
	"42710": true, // constraint/object "x" already exists, skipping
	"42704": true, // object "x" does not exist, skipping
	"42P01": true, // table "x" does not exist, skipping
}

// Boilerplate reports whether a notice says only that an
// IF [NOT] EXISTS clause found nothing to do.
//
// Those arrive on every run of an idempotent migration, so printing
// them trains the reader to skip the notice line — which is where the
// one that mattered would have been.
func Boilerplate(e *Error) bool {
	if e == nil {
		return true
	}
	return strings.EqualFold(e.Severity, "notice") && boilerplateCodes[e.Code]
}
