package d1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare/d1"
)

// The conformance suite. worker/fixtures.json is the shared
// description of the wire protocol; this file is the Go half of
// running it, and the Worker's own test runner is the other. A change
// to one half that the other has not made fails here.

type fixtures struct {
	Protocol  int `json:"protocol"`
	Encode    []encodeCase
	Decode    []decodeCase
	Requests  []requestCase
	Responses []responseCase
}

type encodeCase struct {
	Name   string `json:"name"`
	Go     any    `json:"go"`
	GoKind string `json:"goKind"`
	Wire   any    `json:"wire"`
}

type decodeCase struct {
	Name string `json:"name"`
	Wire any    `json:"wire"`
	As   string `json:"as"`
	Want any    `json:"want"`
}

type requestCase struct {
	Name string          `json:"name"`
	Call callSpec        `json:"call"`
	Body json.RawMessage `json:"body"`
}

type callSpec struct {
	Op         string     `json:"op"`
	SQL        string     `json:"sql"`
	Args       []any      `json:"args"`
	Statements []callSpec `json:"statements"`
}

type responseCase struct {
	Name   string          `json:"name"`
	Body   json.RawMessage `json:"body"`
	Expect expectation     `json:"expect"`
}

type expectation struct {
	Kind         string   `json:"kind"`
	Columns      []string `json:"columns"`
	RowCount     int      `json:"rowCount"`
	RowsAffected int64    `json:"rowsAffected"`
	LastInsertID int64    `json:"lastInsertID"`
	RowsRead     int64    `json:"rowsRead"`
	Sentinel     string   `json:"sentinel"`
	Table        string   `json:"table"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	raw, err := os.ReadFile("worker/fixtures.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var f fixtures
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&f); err != nil {
		t.Fatalf("decode fixtures: %v", err)
	}
	if f.Protocol != d1.ProtocolVersion {
		t.Fatalf("fixtures are protocol %d, package is %d", f.Protocol, d1.ProtocolVersion)
	}
	return f
}

// Encoding ------------------------------------------------------------

func TestConformanceEncode(t *testing.T) {
	f := loadFixtures(t)
	if len(f.Encode) == 0 {
		t.Fatal("no encode fixtures")
	}
	for _, tc := range f.Encode {
		t.Run(tc.Name, func(t *testing.T) {
			in, err := goValue(tc.Go, tc.GoKind)
			if err != nil {
				t.Fatalf("fixture value: %v", err)
			}
			got, err := d1.BindParam(in)
			if err != nil {
				t.Fatalf("BindParam(%#v): %v", in, err)
			}
			if !sameJSON(t, got, tc.Wire) {
				t.Errorf("BindParam(%#v) = %#v, fixture says %#v", in, got, tc.Wire)
			}
		})
	}
}

// goValue turns a fixture's JSON into the Go value the case is about.
func goValue(v any, kind string) (any, error) {
	switch kind {
	case "bytes":
		if v == nil {
			return []byte(nil), nil
		}
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("bytes fixture is %T, want a string", v)
		}
		return []byte(s), nil
	case "time":
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("time fixture is %T, want a string", v)
		}
		return time.Parse(time.RFC3339, s)
	}
	if n, ok := v.(json.Number); ok {
		if i, err := n.Int64(); err == nil {
			return i, nil
		}
		return n.Float64()
	}
	return v, nil
}

// sameJSON compares two values by their JSON rendering, which is the
// only comparison that means anything for a wire format.
func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal %#v: %v", a, err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal %#v: %v", b, err)
	}
	return string(ja) == string(jb)
}

// Decoding -------------------------------------------------------------

func TestConformanceDecode(t *testing.T) {
	f := loadFixtures(t)
	if len(f.Decode) == 0 {
		t.Fatal("no decode fixtures")
	}
	for _, tc := range f.Decode {
		t.Run(tc.Name, func(t *testing.T) {
			// Round-trip the wire value through a real response so
			// the test exercises the same decode path a query does.
			body := fmt.Sprintf(`{"protocol":1,"results":[{"columns":["c"],"rows":[[%s]],"meta":{}}]}`,
				mustJSON(t, tc.Wire))
			rows := bridgeRows(t, body, "SELECT c FROM t")
			defer rows.Close()
			if !rows.Next() {
				t.Fatal("no row")
			}
			switch tc.As {
			case "int64":
				var got int64
				scan(t, rows, &got)
				want := mustInt64(t, tc.Want)
				if got != want {
					t.Errorf("got %d, fixture says %d", got, want)
				}
			case "float64":
				var got float64
				scan(t, rows, &got)
				want := mustFloat64(t, tc.Want)
				if got != want {
					t.Errorf("got %v, fixture says %v", got, want)
				}
			case "bool":
				var got bool
				scan(t, rows, &got)
				if got != tc.Want {
					t.Errorf("got %v, fixture says %v", got, tc.Want)
				}
			case "string":
				var got string
				scan(t, rows, &got)
				if got != tc.Want {
					t.Errorf("got %q, fixture says %q", got, tc.Want)
				}
			case "bytes":
				var got []byte
				scan(t, rows, &got)
				if string(got) != fmt.Sprint(tc.Want) {
					t.Errorf("got %q, fixture says %q", got, tc.Want)
				}
			case "time":
				var got time.Time
				scan(t, rows, &got)
				want, err := time.Parse(time.RFC3339, fmt.Sprint(tc.Want))
				if err != nil {
					t.Fatalf("fixture time: %v", err)
				}
				if !got.Equal(want) {
					t.Errorf("got %v, fixture says %v", got, want)
				}
			default:
				t.Fatalf("fixture asks for destination type %q, which the suite does not know", tc.As)
			}
		})
	}
}

// Requests --------------------------------------------------------------

// The exact bytes the driver puts on the wire are part of the
// contract, so the fixtures pin them rather than describing them.
func TestConformanceRequestBodies(t *testing.T) {
	f := loadFixtures(t)
	for _, tc := range f.Requests {
		t.Run(tc.Name, func(t *testing.T) {
			var sent string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw := make([]byte, r.ContentLength)
				_, _ = r.Body.Read(raw)
				sent = string(raw)
				fmt.Fprint(w, `{"protocol":1,"results":[{"columns":[],"rows":[],"meta":{}},{"columns":[],"rows":[],"meta":{}}]}`)
			}))
			defer srv.Close()

			drv, err := d1.NewBridge(srv.URL)
			if err != nil {
				t.Fatalf("NewBridge: %v", err)
			}
			ctx := context.Background()
			switch tc.Call.Op {
			case "query":
				rows, qErr := drv.Query(ctx, tc.Call.SQL, tc.Call.Args...)
				if qErr != nil {
					t.Fatalf("Query: %v", qErr)
				}
				rows.Close()
			case "exec":
				if _, eErr := drv.Exec(ctx, tc.Call.SQL, tc.Call.Args...); eErr != nil {
					t.Fatalf("Exec: %v", eErr)
				}
			case "batch":
				b := d1.NewBatch(drv)
				for _, st := range tc.Call.Statements {
					b.Add(st.SQL, st.Args...)
				}
				if _, bErr := b.Run(ctx); bErr != nil {
					t.Fatalf("Batch: %v", bErr)
				}
			default:
				t.Fatalf("unknown op %q", tc.Call.Op)
			}

			if !equalJSONBytes(t, []byte(sent), tc.Body) {
				t.Errorf("request body\n got: %s\nwant: %s", sent, tc.Body)
			}
		})
	}
}

// Responses --------------------------------------------------------------

func TestConformanceResponses(t *testing.T) {
	f := loadFixtures(t)
	for _, tc := range f.Responses {
		t.Run(tc.Name, func(t *testing.T) {
			switch tc.Expect.Kind {
			case "rows":
				rows := bridgeRows(t, string(tc.Body), "SELECT *")
				defer rows.Close()
				cols, err := rows.Columns()
				if err != nil {
					t.Fatalf("Columns: %v", err)
				}
				if !reflect.DeepEqual(cols, tc.Expect.Columns) {
					t.Errorf("columns = %v, fixture says %v", cols, tc.Expect.Columns)
				}
				n := 0
				for rows.Next() {
					n++
				}
				if n != tc.Expect.RowCount {
					t.Errorf("%d rows, fixture says %d", n, tc.Expect.RowCount)
				}

			case "result":
				res, err := bridgeExec(t, string(tc.Body), "DELETE FROM t")
				if err != nil {
					t.Fatalf("Exec: %v", err)
				}
				n, err := res.RowsAffected()
				if err != nil || n != tc.Expect.RowsAffected {
					t.Errorf("RowsAffected = %d, %v; fixture says %d", n, err, tc.Expect.RowsAffected)
				}
				r := res.(*d1.Result)
				if got, _ := r.LastInsertID(); got != tc.Expect.LastInsertID {
					t.Errorf("LastInsertID = %d, fixture says %d", got, tc.Expect.LastInsertID)
				}
				if r.Meta().RowsRead != tc.Expect.RowsRead {
					t.Errorf("RowsRead = %d, fixture says %d", r.Meta().RowsRead, tc.Expect.RowsRead)
				}

			case "error":
				_, err := bridgeExec(t, string(tc.Body), "INSERT INTO t VALUES (1)")
				if err == nil {
					t.Fatal("expected an error")
				}
				want := sentinelByName(t, tc.Expect.Sentinel)
				if want != nil && !errors.Is(err, want) {
					t.Errorf("err = %v, fixture says sentinel %q", err, tc.Expect.Sentinel)
				}
				if tc.Expect.Sentinel == "protocol" {
					return
				}
				table, cols := d1.ConstraintColumns(err)
				if table != tc.Expect.Table {
					t.Errorf("table = %q, fixture says %q", table, tc.Expect.Table)
				}
				if len(cols) != len(tc.Expect.Columns) {
					t.Errorf("columns = %v, fixture says %v", cols, tc.Expect.Columns)
				}

			default:
				t.Fatalf("unknown expectation kind %q", tc.Expect.Kind)
			}
		})
	}
}

func sentinelByName(t *testing.T, name string) error {
	t.Helper()
	switch name {
	case "unique":
		return d1.ErrUniqueViolation
	case "foreign_key":
		return d1.ErrForeignKeyViolation
	case "check":
		return d1.ErrCheckViolation
	case "not_null":
		return d1.ErrNotNullViolation
	case "undefined_table":
		return d1.ErrUndefinedTable
	case "undefined_column":
		return d1.ErrUndefinedColumn
	case "protocol":
		// Not a sentinel: a version mismatch is reported by message,
		// because there is nothing to branch on beyond "do not talk
		// to this handler".
		return nil
	default:
		t.Fatalf("fixture names sentinel %q, which the suite does not know", name)
		return nil
	}
}

// Helpers ---------------------------------------------------------------

// bridgeServer stands a server up that replies with body, and returns
// a bridge driver pointed at it.
func bridgeServer(t *testing.T, body string) *d1.Driver {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	drv, err := d1.NewBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	return drv
}

func bridgeRows(t *testing.T, body, sql string) interface {
	Next() bool
	Scan(...any) error
	Columns() ([]string, error)
	Close() error
	Err() error
} {
	t.Helper()
	rows, err := bridgeServer(t, body).Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return rows
}

func bridgeExec(t *testing.T, body, sql string) (res interface{ RowsAffected() (int64, error) }, err error) {
	t.Helper()
	return bridgeServer(t, body).Exec(context.Background(), sql)
}

func scan(t *testing.T, rows interface{ Scan(...any) error }, dest any) {
	t.Helper()
	if err := rows.Scan(dest); err != nil {
		t.Fatalf("Scan: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func mustInt64(t *testing.T, v any) int64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("fixture value %#v is not a number", v)
	}
	i, err := n.Int64()
	if err != nil {
		t.Fatalf("fixture value %s is not an integer: %v", n, err)
	}
	return i
}

func mustFloat64(t *testing.T, v any) float64 {
	t.Helper()
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("fixture value %#v is not a number", v)
	}
	f, err := n.Float64()
	if err != nil {
		t.Fatalf("fixture value %s is not a number: %v", n, err)
	}
	return f
}

func equalJSONBytes(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("got is not JSON: %v (%s)", err, a)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("fixture is not JSON: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}
