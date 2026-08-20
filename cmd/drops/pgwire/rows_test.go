package pgwire

import (
	"database/sql"
	"testing"
	"time"
)

func TestRowsScanTextFormat(t *testing.T) {
	rows := &Rows{
		fields: []string{"name", "n", "flag", "when", "maybe", "raw"},
		rows: [][][]byte{{
			[]byte("drops"),
			[]byte("42"),
			[]byte("t"),
			[]byte("2026-08-20 07:25:46.123456+00"),
			nil,
			[]byte(`\xdeadbeef`),
		}},
	}
	if !rows.Next() {
		t.Fatal("no row")
	}
	var (
		name  string
		n     int64
		flag  bool
		when  time.Time
		maybe sql.NullString
		raw   []byte
	)
	if err := rows.Scan(&name, &n, &flag, &when, &maybe, &raw); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if name != "drops" || n != 42 || !flag {
		t.Errorf("scalars = %q %d %v", name, n, flag)
	}
	if got := when.UTC().Format(time.RFC3339Nano); got != "2026-08-20T07:25:46.123456Z" {
		t.Errorf("timestamp = %s", got)
	}
	if maybe.Valid {
		t.Errorf("NULL scanned as valid: %+v", maybe)
	}
	if string(raw) != "\xde\xad\xbe\xef" {
		t.Errorf("bytea = %x", raw)
	}
	if rows.Next() {
		t.Error("a second row appeared from a one-row result")
	}
}

// A NULL into a destination that cannot hold one has to be an error,
// not a zero value: a migration that read a NULL version as "" would
// happily apply the migration again.
func TestRowsScanRefusesNullIntoValue(t *testing.T) {
	rows := &Rows{fields: []string{"v"}, rows: [][][]byte{{nil}}}
	rows.Next()
	var s string
	if err := rows.Scan(&s); err == nil {
		t.Fatal("Scan accepted NULL into a *string")
	}
}

func TestRowsScanChecksArity(t *testing.T) {
	rows := &Rows{fields: []string{"a", "b"}, rows: [][][]byte{{[]byte("1"), []byte("2")}}}
	rows.Next()
	var one string
	if err := rows.Scan(&one); err == nil {
		t.Fatal("Scan accepted one destination for a two-column row")
	}
}

func TestEncodeParam(t *testing.T) {
	cases := []struct {
		in   any
		want string
		null bool
	}{
		{in: "text", want: "text"},
		{in: int64(7), want: "7"},
		{in: 7, want: "7"},
		{in: true, want: "true"},
		{in: 1.5, want: "1.5"},
		{in: []byte{0xde, 0xad}, want: `\xdead`},
		{in: nil, null: true},
		{in: sql.NullString{}, null: true},
		{in: sql.NullString{String: "here", Valid: true}, want: "here"},
	}
	for _, c := range cases {
		got, err := encodeParam(c.in)
		if err != nil {
			t.Errorf("encodeParam(%v): %v", c.in, err)
			continue
		}
		if c.null {
			if got != nil {
				t.Errorf("encodeParam(%v) = %q, want NULL", c.in, got)
			}
			continue
		}
		if string(got) != c.want {
			t.Errorf("encodeParam(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := encodeParam(struct{ A int }{1}); err == nil {
		t.Error("encodeParam accepted a value it has no encoding for")
	}
}

func TestRowsFromTag(t *testing.T) {
	cases := map[string]int64{
		"INSERT 0 3":   3,
		"UPDATE 7":     7,
		"DELETE 0":     0,
		"SELECT 12":    12,
		"CREATE TABLE": 0,
		"":             0,
	}
	for tag, want := range cases {
		if got := rowsFromTag(tag); got != want {
			t.Errorf("rowsFromTag(%q) = %d, want %d", tag, got, want)
		}
	}
}

func TestBoilerplateNotices(t *testing.T) {
	quiet := &Error{Severity: "NOTICE", Code: "42P07", Message: `relation "x" already exists, skipping`}
	if !Boilerplate(quiet) {
		t.Error("an IF NOT EXISTS notice was treated as worth printing")
	}
	loud := &Error{Severity: "WARNING", Code: "01000", Message: "identifier will be truncated"}
	if Boilerplate(loud) {
		t.Error("a warning was suppressed as boilerplate")
	}
}
