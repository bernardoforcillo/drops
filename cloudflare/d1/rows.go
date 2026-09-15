package d1

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"
)

// Rows is a forward-only cursor over one D1 result set, implementing
// [github.com/bernardoforcillo/drops.Rows].
//
// The whole result set is already in memory by the time a Rows
// exists: D1 answers an HTTP request with the rows in it, so there is
// no streaming to do and Next never blocks. That is also the reason
// [Limits.QueryResponseSize] matters — a SELECT whose answer exceeds
// it fails at the service rather than arriving a page at a time. Page
// with LIMIT, or with the keyset cursors in
// [github.com/bernardoforcillo/drops/sqlite].
type Rows struct {
	columns []string
	rows    [][]any
	pos     int
	closed  bool
	meta    Meta
}

// Next advances to the next row, reporting whether there is one.
func (r *Rows) Next() bool {
	if r.closed || r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

// Columns returns the column names, in the order the statement
// projected them.
func (r *Rows) Columns() ([]string, error) {
	if r.columns == nil {
		return []string{}, nil
	}
	return r.columns, nil
}

// Close releases the cursor. It is idempotent, and cheap: the rows
// are already in memory, so closing only stops further iteration.
func (r *Rows) Close() error {
	r.closed = true
	return nil
}

// Err reports an error encountered during iteration. Always nil: a D1
// result set arrives complete or not at all, so every failure has
// already been returned by Query.
func (r *Rows) Err() error { return nil }

// Meta returns what D1 reported about the statement that produced
// these rows — notably RowsRead, the number the bill is computed
// from.
func (r *Rows) Meta() Meta { return r.meta }

// Scan copies the current row's columns into dest, converting from
// the JSON shapes D1 answers in. See [ConvertAssign] for the
// conversion table.
func (r *Rows) Scan(dest ...any) error {
	if r.closed {
		return errors.New("drops/cloudflare/d1: Scan on a closed Rows")
	}
	if r.pos == 0 {
		return errors.New("drops/cloudflare/d1: Scan called before Next")
	}
	row := r.rows[r.pos-1]
	if len(dest) != len(row) {
		return fmt.Errorf("drops/cloudflare/d1: Scan wants %d destination(s) for %d column(s)", len(dest), len(row))
	}
	for i, d := range dest {
		if err := ConvertAssign(d, row[i]); err != nil {
			return fmt.Errorf("drops/cloudflare/d1: column %d (%s): %w", i, r.columnName(i), err)
		}
	}
	return nil
}

func (r *Rows) columnName(i int) string {
	if i < len(r.columns) {
		return r.columns[i]
	}
	return "?"
}

// ConvertAssign writes a value decoded from D1's JSON into dest.
//
// It exists because JSON is a lossier wire format than SQLite's own.
// A local driver hands Go a value that still knows it was an INTEGER;
// D1 hands back a JSON number, and this is where the type comes back.
// Numbers arrive as [encoding/json.Number] rather than float64 —
// [decodeResults] asks for that — so an integer wider than 2^53
// reaches an *int64 destination with every bit intact, which it would
// not if it had been through a float.
//
// The conversions:
//
//	JSON null    → any pointer, set to its zero value; *[]byte to nil
//	JSON number  → *int…, *uint…, *float…, *string, *bool, *any
//	JSON string  → *string, *[]byte, *time.Time, the numeric kinds
//	               (parsed), *any
//	JSON bool    → *bool, the integer kinds (1/0), *any
//	JSON array   → *[]byte, one byte per element — D1's BLOB shape
//
// [database/sql.Scanner] destinations are handed the value after that
// normalisation, so sql.NullString and friends work unchanged.
// Pointer-to-pointer destinations (**int, *[]byte aside) allocate on
// a non-null value and are set to nil on a null one, which is how an
// optional column scans without a Null wrapper.
func ConvertAssign(dest, src any) error {
	if dest == nil {
		return errors.New("destination is nil")
	}

	// A Scanner decides for itself what it accepts; give it the
	// value normalised out of json.Number first so it sees int64 /
	// float64 the way database/sql would hand it one.
	if sc, ok := dest.(sql.Scanner); ok {
		v, err := normalize(src)
		if err != nil {
			return err
		}
		return sc.Scan(v)
	}

	switch d := dest.(type) {
	case *any:
		v, err := normalize(src)
		if err != nil {
			return err
		}
		*d = v
		return nil
	case *string:
		return assignString(d, src)
	case *[]byte:
		return assignBytes(d, src)
	case *bool:
		return assignBool(d, src)
	case *time.Time:
		return assignTime(d, src)
	case *json.RawMessage:
		raw, err := json.Marshal(src)
		if err != nil {
			return err
		}
		*d = raw
		return nil
	}

	return assignReflect(dest, src)
}

// normalize turns a json-decoded value into the Go value a
// database/sql driver would have produced: json.Number becomes int64
// where it is one and float64 otherwise, an array of numbers becomes
// []byte, everything else is already right.
func normalize(src any) (any, error) {
	switch v := src.(type) {
	case nil:
		return nil, nil
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		f, err := v.Float64()
		if err != nil {
			return nil, fmt.Errorf("%q is not a number: %w", v.String(), err)
		}
		return f, nil
	case []any:
		b, err := blobBytes(v)
		if err != nil {
			return nil, err
		}
		return b, nil
	default:
		return src, nil
	}
}

// blobBytes reads D1's BLOB shape — a JSON array with one number per
// byte — into a []byte. Anything outside 0..255 is not a blob, and
// saying so beats truncating it silently.
func blobBytes(arr []any) ([]byte, error) {
	out := make([]byte, len(arr))
	for i, e := range arr {
		n, ok := e.(json.Number)
		if !ok {
			return nil, fmt.Errorf("blob element %d is %T, want a number", i, e)
		}
		b, err := n.Int64()
		if err != nil || b < 0 || b > 255 {
			return nil, fmt.Errorf("blob element %d is %s, want 0..255", i, n.String())
		}
		out[i] = byte(b)
	}
	return out, nil
}

func assignString(d *string, src any) error {
	switch v := src.(type) {
	case nil:
		*d = ""
	case string:
		*d = v
	case json.Number:
		*d = v.String()
	case bool:
		*d = strconv.FormatBool(v)
	case []any:
		b, err := blobBytes(v)
		if err != nil {
			return err
		}
		*d = string(b)
	default:
		return fmt.Errorf("cannot scan %T into *string", src)
	}
	return nil
}

func assignBytes(d *[]byte, src any) error {
	switch v := src.(type) {
	case nil:
		*d = nil
	case string:
		*d = []byte(v)
	case []any:
		b, err := blobBytes(v)
		if err != nil {
			return err
		}
		*d = b
	case json.Number:
		*d = []byte(v.String())
	default:
		return fmt.Errorf("cannot scan %T into *[]byte", src)
	}
	return nil
}

func assignBool(d *bool, src any) error {
	switch v := src.(type) {
	case nil:
		*d = false
	case bool:
		*d = v
	case json.Number:
		// SQLite has no boolean type: a column declared BOOLEAN
		// holds 1 and 0, and that is what D1 sends back.
		n, err := v.Int64()
		if err != nil {
			return fmt.Errorf("cannot scan %s into *bool", v.String())
		}
		*d = n != 0
	case string:
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("cannot scan %q into *bool", v)
		}
		*d = b
	default:
		return fmt.Errorf("cannot scan %T into *bool", src)
	}
	return nil
}

// timeLayouts are the forms SQLite's own date/time functions write,
// plus RFC 3339 for the rows an application wrote itself. They are
// tried in order.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"15:04:05",
}

func assignTime(d *time.Time, src any) error {
	switch v := src.(type) {
	case nil:
		*d = time.Time{}
		return nil
	case string:
		for _, layout := range timeLayouts {
			if t, err := time.Parse(layout, v); err == nil {
				*d = t
				return nil
			}
		}
		return fmt.Errorf("cannot parse %q as a time", v)
	case json.Number:
		// A numeric timestamp column is Unix seconds — what
		// unixepoch() and strftime('%s', …) write.
		n, err := v.Int64()
		if err != nil {
			return fmt.Errorf("cannot scan %s into *time.Time", v.String())
		}
		*d = time.Unix(n, 0).UTC()
		return nil
	default:
		return fmt.Errorf("cannot scan %T into *time.Time", src)
	}
}

// assignReflect handles the numeric destinations and the
// pointer-to-pointer ones.
func assignReflect(dest, src any) error {
	dv := reflect.ValueOf(dest)
	if dv.Kind() != reflect.Ptr {
		return fmt.Errorf("destination %T is not a pointer", dest)
	}
	if dv.IsNil() {
		return fmt.Errorf("destination %T is a nil pointer", dest)
	}
	elem := dv.Elem()

	if src == nil {
		elem.Set(reflect.Zero(elem.Type()))
		return nil
	}

	// **T: allocate and recurse, so an optional column scans into a
	// pointer field without a Null wrapper.
	if elem.Kind() == reflect.Ptr {
		fresh := reflect.New(elem.Type().Elem())
		if err := ConvertAssign(fresh.Interface(), src); err != nil {
			return err
		}
		elem.Set(fresh)
		return nil
	}

	switch elem.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := srcInt64(src)
		if err != nil {
			return fmt.Errorf("cannot scan into *%s: %w", elem.Type(), err)
		}
		if elem.OverflowInt(n) {
			return fmt.Errorf("%d overflows %s", n, elem.Type())
		}
		elem.SetInt(n)
		return nil

	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := srcInt64(src)
		if err != nil {
			return fmt.Errorf("cannot scan into *%s: %w", elem.Type(), err)
		}
		if n < 0 {
			return fmt.Errorf("%d does not fit %s", n, elem.Type())
		}
		if elem.OverflowUint(uint64(n)) {
			return fmt.Errorf("%d overflows %s", n, elem.Type())
		}
		elem.SetUint(uint64(n))
		return nil

	case reflect.Float32, reflect.Float64:
		f, err := srcFloat64(src)
		if err != nil {
			return fmt.Errorf("cannot scan into *%s: %w", elem.Type(), err)
		}
		if elem.OverflowFloat(f) {
			return fmt.Errorf("%v overflows %s", f, elem.Type())
		}
		elem.SetFloat(f)
		return nil

	case reflect.String:
		var s string
		if err := assignString(&s, src); err != nil {
			return err
		}
		elem.SetString(s)
		return nil

	case reflect.Bool:
		var b bool
		if err := assignBool(&b, src); err != nil {
			return err
		}
		elem.SetBool(b)
		return nil
	}

	// A destination the conversions do not cover, but whose dynamic
	// type matches the source exactly, is simply assigned.
	sv := reflect.ValueOf(src)
	if sv.Type().AssignableTo(elem.Type()) {
		elem.Set(sv)
		return nil
	}
	return fmt.Errorf("cannot scan %T into *%s", src, elem.Type())
}

func srcInt64(src any) (int64, error) {
	switch v := src.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", v.String())
		}
		if f != float64(int64(f)) {
			return 0, fmt.Errorf("%s is not an integer", v.String())
		}
		return int64(f), nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not an integer", v)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("%T is not an integer", src)
	}
}

func srcFloat64(src any) (float64, error) {
	switch v := src.(type) {
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", v.String())
		}
		return f, nil
	case bool:
		if v {
			return 1, nil
		}
		return 0, nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("%q is not a number", v)
		}
		return f, nil
	default:
		return 0, fmt.Errorf("%T is not a number", src)
	}
}
