package pgwire

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rows is a cursor over a buffered result set. The whole result is
// read before Query returns — the protocol has to be drained to
// ReadyForQuery before the next statement anyway, and the result sets
// this client sees are catalogue queries and migration history.
type Rows struct {
	fields []string
	rows   [][][]byte
	pos    int
	err    error
}

// Columns returns the column names in result order.
func (r *Rows) Columns() ([]string, error) { return r.fields, nil }

// Next advances to the following row, reporting false at the end.
func (r *Rows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

// Close releases the cursor. Nothing is held open, so it never fails.
func (r *Rows) Close() error { return nil }

// Err returns the first decoding failure, if any.
func (r *Rows) Err() error { return r.err }

// Scan decodes the current row into dest.
//
// Every value arrives in PostgreSQL's text format, so the conversion
// is the one database/sql performs for a text-format driver: a
// destination implementing sql.Scanner is handed the raw bytes (which
// is what makes sql.NullString and friends work), and the concrete
// pointer types are parsed directly.
func (r *Rows) Scan(dest ...any) error {
	if r.pos == 0 || r.pos > len(r.rows) {
		return fmt.Errorf("drops: Scan called outside a row")
	}
	row := r.rows[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("drops: Scan into %d destinations from a row of %d columns", len(dest), len(row))
	}
	for i, d := range dest {
		if err := convertAssign(d, row[i]); err != nil {
			name := strconv.Itoa(i)
			if i < len(r.fields) {
				name = r.fields[i]
			}
			r.err = fmt.Errorf("drops: column %s: %w", name, err)
			return r.err
		}
	}
	return nil
}

// convertAssign writes one text-format value into one destination.
// src is nil for SQL NULL.
func convertAssign(dest any, src []byte) error {
	if s, ok := dest.(sql.Scanner); ok {
		if src == nil {
			return s.Scan(nil)
		}
		// A copy: database/sql promises a Scanner may keep what it is
		// given unless the type is sql.RawBytes, and the row buffer
		// outlives no one here but the caller might.
		return s.Scan(append([]byte(nil), src...))
	}
	switch d := dest.(type) {
	case *any:
		if src == nil {
			*d = nil
		} else {
			*d = string(src)
		}
		return nil
	case *[]byte:
		if src == nil {
			*d = nil
			return nil
		}
		v, err := decodeBytea(src)
		if err != nil {
			return err
		}
		*d = v
		return nil
	}
	if src == nil {
		return fmt.Errorf("NULL cannot be scanned into %T", dest)
	}
	text := string(src)
	switch d := dest.(type) {
	case *string:
		*d = text
	case *bool:
		v, err := parseBool(text)
		if err != nil {
			return err
		}
		*d = v
	case *int:
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return err
		}
		*d = int(v)
	case *int32:
		v, err := strconv.ParseInt(text, 10, 32)
		if err != nil {
			return err
		}
		*d = int32(v)
	case *int64:
		v, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return err
		}
		*d = v
	case *float32:
		v, err := strconv.ParseFloat(text, 32)
		if err != nil {
			return err
		}
		*d = float32(v)
	case *float64:
		v, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return err
		}
		*d = v
	case *time.Time:
		v, err := parseTimestamp(text)
		if err != nil {
			return err
		}
		*d = v
	default:
		return fmt.Errorf("no conversion from a text-format value to %T", dest)
	}
	return nil
}

func parseBool(s string) (bool, error) {
	switch s {
	case "t", "true", "y", "yes", "on", "1":
		return true, nil
	case "f", "false", "n", "no", "off", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q is not a boolean", s)
}

// timestampLayouts covers what PostgreSQL's text output for the
// date/time family looks like with DateStyle=ISO, which is the
// default and what the startup message leaves in place.
var timestampLayouts = []string{
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999Z07",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02",
	"15:04:05.999999999Z07",
	"15:04:05.999999999",
}

func parseTimestamp(s string) (time.Time, error) {
	for _, layout := range timestampLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a timestamp this client knows how to read", s)
}

// decodeBytea converts PostgreSQL's hex output format back to bytes.
// Anything not in that form is a text value being scanned into a byte
// slice, which is what database/sql does with it too.
func decodeBytea(src []byte) ([]byte, error) {
	if len(src) < 2 || src[0] != '\\' || src[1] != 'x' {
		return append([]byte(nil), src...), nil
	}
	out := make([]byte, (len(src)-2)/2)
	for i := range out {
		hi, err := hexDigit(src[2+i*2])
		if err != nil {
			return nil, err
		}
		lo, err := hexDigit(src[3+i*2])
		if err != nil {
			return nil, err
		}
		out[i] = hi<<4 | lo
	}
	return out, nil
}

func hexDigit(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("%q is not a hex digit", c)
}

// encodeParam renders one bound parameter in the text format. nil
// means SQL NULL.
//
// The parameter's type is left for the server to infer from the
// statement, so a Go string bound to a bigint column arrives as the
// digits the column wants rather than as text the server refuses.
func encodeParam(v any) ([]byte, error) {
	switch a := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(a), nil
	case []byte:
		return []byte(`\x` + hexEncode(a)), nil
	case bool:
		if a {
			return []byte("true"), nil
		}
		return []byte("false"), nil
	case int:
		return []byte(strconv.FormatInt(int64(a), 10)), nil
	case int8:
		return []byte(strconv.FormatInt(int64(a), 10)), nil
	case int16:
		return []byte(strconv.FormatInt(int64(a), 10)), nil
	case int32:
		return []byte(strconv.FormatInt(int64(a), 10)), nil
	case int64:
		return []byte(strconv.FormatInt(a, 10)), nil
	case uint:
		return []byte(strconv.FormatUint(uint64(a), 10)), nil
	case uint8:
		return []byte(strconv.FormatUint(uint64(a), 10)), nil
	case uint16:
		return []byte(strconv.FormatUint(uint64(a), 10)), nil
	case uint32:
		return []byte(strconv.FormatUint(uint64(a), 10)), nil
	case uint64:
		return []byte(strconv.FormatUint(a, 10)), nil
	case float32:
		return []byte(strconv.FormatFloat(float64(a), 'g', -1, 32)), nil
	case float64:
		return []byte(strconv.FormatFloat(a, 'g', -1, 64)), nil
	case time.Time:
		return []byte(a.Format("2006-01-02 15:04:05.999999999Z07:00")), nil
	}
	if valuer, ok := v.(driver.Valuer); ok {
		inner, err := valuer.Value()
		if err != nil {
			return nil, err
		}
		if inner == nil {
			return nil, nil
		}
		return encodeParam(inner)
	}
	return nil, fmt.Errorf("no text encoding for a parameter of type %T", v)
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	var out strings.Builder
	out.Grow(len(b) * 2)
	for _, c := range b {
		out.WriteByte(digits[c>>4])
		out.WriteByte(digits[c&0xf])
	}
	return out.String()
}
