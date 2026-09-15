package d1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/d1"
)

// d1Server records the request bodies it was sent and replies with
// canned results, so a test can assert on the wire as well as on the
// values that come back.
type d1Server struct {
	t        *testing.T
	srv      *httptest.Server
	bodies   []string
	reply    func(n int) (status int, body string)
	requests int
}

func newServer(t *testing.T, reply func(n int) (int, string)) *d1Server {
	t.Helper()
	s := &d1Server{t: t, reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(raw))
		s.requests++
		status, body := s.reply(s.requests)
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *d1Server) driver(opts ...d1.Option) *d1.Driver {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.srv.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return d1.New(cf, "db-1", opts...)
}

// lastBody decodes the most recent request body.
func (s *d1Server) lastBody() (sql string, params []any) {
	s.t.Helper()
	var body struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}
	if err := json.Unmarshal([]byte(s.bodies[len(s.bodies)-1]), &body); err != nil {
		s.t.Fatalf("decode request body: %v", err)
	}
	return body.SQL, body.Params
}

// ok builds a /raw success envelope for one statement.
func ok(columns string, rows string, meta string) string {
	if meta == "" {
		meta = `{"changes":0,"last_row_id":0,"rows_read":0,"rows_written":0,"duration":0.1}`
	}
	return fmt.Sprintf(
		`{"result":[{"success":true,"meta":%s,"results":{"columns":%s,"rows":%s}}],"success":true,"errors":[],"messages":[]}`,
		meta, columns, rows)
}

func static(body string) func(int) (int, string) {
	return func(int) (int, string) { return http.StatusOK, body }
}

// Rows --------------------------------------------------------------

func TestQueryScansColumnsInOrder(t *testing.T) {
	s := newServer(t, static(ok(`["id","name","score"]`, `[[1,"ada",9.5],[2,"grace",8.25]]`, "")))
	rows, err := s.driver().Query(context.Background(), "SELECT id, name, score FROM users")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	if want := []string{"id", "name", "score"}; !equalStrings(cols, want) {
		t.Errorf("Columns = %v, want %v", cols, want)
	}

	type row struct {
		id    int64
		name  string
		score float64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.score); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []row{{1, "ada", 9.5}, {2, "grace", 8.25}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("rows = %+v, want %+v", got, want)
	}
}

// The reason the driver asks for /raw rather than /query: a JSON
// object has no column order, and a positional Scan needs one.
func TestDriverAsksForTheRawEndpoint(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		fmt.Fprint(w, ok(`["n"]`, `[[1]]`, ""))
	}))
	defer srv.Close()

	cf, _ := cloudflare.New("acct", cloudflare.WithAPIToken("t"), cloudflare.WithBaseURL(srv.URL))
	if _, err := d1.New(cf, "db-1").Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if want := "/accounts/acct/d1/database/db-1/raw"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

// A JSON float64 cannot hold a SQLite rowid past 2^53. UseNumber is
// what keeps the low bits, and this is the test that would fail
// without it.
func TestLargeIntegerSurvivesTheJSONRoundTrip(t *testing.T) {
	const big = int64(9007199254740993) // 2^53 + 1
	s := newServer(t, static(ok(`["id"]`, fmt.Sprintf(`[[%d]]`, big), "")))
	rows, err := s.driver().Query(context.Background(), "SELECT id FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var got int64
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if got != big {
		t.Errorf("id = %d, want %d — the value went through a float64", got, big)
	}
}

func TestScanNullIntoPointerAndValue(t *testing.T) {
	s := newServer(t, static(ok(`["a","b"]`, `[[null,null]]`, "")))
	rows, err := s.driver().Query(context.Background(), "SELECT a, b FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var plain string
	ptr := new(int)
	if err := rows.Scan(&plain, &ptr); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if plain != "" {
		t.Errorf("null into *string = %q, want \"\"", plain)
	}
	if ptr != nil {
		t.Errorf("null into **int = %v, want nil", ptr)
	}
}

func TestScanBlobAndBool(t *testing.T) {
	s := newServer(t, static(ok(`["blob","flag"]`, `[[[104,105],1]]`, "")))
	rows, err := s.driver().Query(context.Background(), "SELECT blob, flag FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var b []byte
	var flag bool
	if err := rows.Scan(&b, &flag); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if string(b) != "hi" {
		t.Errorf("blob = %q, want \"hi\"", b)
	}
	if !flag {
		t.Error("flag = false, want true — SQLite writes a boolean as 1")
	}
}

func TestScanTimeFromTextAndUnixSeconds(t *testing.T) {
	s := newServer(t, static(ok(`["iso","sqlite","unix"]`,
		`[["2024-03-01T10:20:30Z","2024-03-01 10:20:30",1709288430]]`, "")))
	rows, err := s.driver().Query(context.Background(), "SELECT iso, sqlite, unix FROM t")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no row")
	}
	var iso, sqliteForm, unix time.Time
	if err := rows.Scan(&iso, &sqliteForm, &unix); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	want := time.Date(2024, 3, 1, 10, 20, 30, 0, time.UTC)
	if !iso.Equal(want) {
		t.Errorf("RFC3339 = %v, want %v", iso, want)
	}
	if !sqliteForm.Equal(want) {
		t.Errorf("SQLite datetime() form = %v, want %v", sqliteForm, want)
	}
	if unix.Unix() != 1709288430 {
		t.Errorf("unix seconds = %v", unix)
	}
}

func TestScanRejectsWrongDestinationCount(t *testing.T) {
	s := newServer(t, static(ok(`["a","b"]`, `[[1,2]]`, "")))
	rows, _ := s.driver().Query(context.Background(), "SELECT a, b FROM t")
	defer rows.Close()
	rows.Next()
	var one int
	if err := rows.Scan(&one); err == nil {
		t.Error("Scan with 1 destination for 2 columns should fail")
	}
}

func TestScanBeforeNextIsAnError(t *testing.T) {
	s := newServer(t, static(ok(`["a"]`, `[[1]]`, "")))
	rows, _ := s.driver().Query(context.Background(), "SELECT a FROM t")
	defer rows.Close()
	var n int
	if err := rows.Scan(&n); err == nil {
		t.Error("Scan before Next should fail")
	}
}

func TestEmptyResultSetIteratesZeroTimes(t *testing.T) {
	s := newServer(t, static(ok(`["a"]`, `[]`, "")))
	rows, err := s.driver().Query(context.Background(), "SELECT a FROM t WHERE 0")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Error("Next returned true on an empty result set")
	}
	cols, err := rows.Columns()
	if err != nil || len(cols) != 1 {
		t.Errorf("Columns = %v, %v", cols, err)
	}
}

// Params -------------------------------------------------------------

func TestBindParamConversions(t *testing.T) {
	when := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"nil", nil, nil},
		{"true", true, 1},
		{"false", false, 0},
		{"string", "x", "x"},
		{"int64", int64(7), int64(7)},
		{"float", 1.5, 1.5},
		{"time", when, "2024-01-02T03:04:05Z"},
		{"nil bytes", []byte(nil), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := d1.BindParam(tc.in)
			if err != nil {
				t.Fatalf("BindParam(%v): %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("BindParam(%v) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// A nil []byte is SQL NULL; an empty one is a zero-length blob. They
// are different values and must not collapse into each other.
func TestEmptyBlobIsNotNull(t *testing.T) {
	got, err := d1.BindParam([]byte{})
	if err != nil {
		t.Fatalf("BindParam: %v", err)
	}
	arr, isArray := got.([]any)
	if !isArray || len(arr) != 0 {
		t.Errorf("empty []byte = %#v, want an empty array (a zero-length blob)", got)
	}
}

func TestBindParamRefusesAGuess(t *testing.T) {
	type payload struct{ A int }
	_, err := d1.BindParam(payload{A: 1})
	if !errors.Is(err, d1.ErrUnsupportedParam) {
		t.Errorf("err = %v, want ErrUnsupportedParam — a struct must not be silently JSON-encoded", err)
	}
}

func TestParamsReachTheWire(t *testing.T) {
	s := newServer(t, static(ok(`["n"]`, `[[1]]`, "")))
	if _, err := s.driver().Exec(context.Background(),
		"INSERT INTO t (a, b, c) VALUES (?, ?, ?)", "x", 42, true); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	sql, params := s.lastBody()
	if !strings.Contains(sql, "INSERT INTO t") {
		t.Errorf("sql = %q", sql)
	}
	if len(params) != 3 || params[0] != "x" || params[1] != float64(42) || params[2] != float64(1) {
		t.Errorf("params = %#v, want [x 42 1]", params)
	}
}

func TestTooManyParamsIsRefusedLocally(t *testing.T) {
	s := newServer(t, static(ok(`["n"]`, `[[1]]`, "")))
	args := make([]any, 101)
	for i := range args {
		args[i] = i
	}
	_, err := s.driver().Exec(context.Background(),
		"SELECT * FROM t WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", 101), ",")+")", args...)
	if !errors.Is(err, d1.ErrTooManyParams) {
		t.Fatalf("err = %v, want ErrTooManyParams", err)
	}
	if s.requests != 0 {
		t.Errorf("made %d request(s) — the check should not need a round trip", s.requests)
	}
}

func TestMaxBoundParamsCanBeDisabled(t *testing.T) {
	s := newServer(t, static(ok(`["n"]`, `[[1]]`, "")))
	args := make([]any, 200)
	for i := range args {
		args[i] = i
	}
	if _, err := s.driver(d1.WithMaxBoundParams(0)).Exec(context.Background(), "SELECT 1", args...); err != nil {
		t.Fatalf("Exec with the check disabled: %v", err)
	}
}

// Result -------------------------------------------------------------

func TestResultCarriesMeta(t *testing.T) {
	meta := `{"changes":3,"last_row_id":17,"rows_read":900,"rows_written":3,"duration":2.5,"served_by_primary":true,"size_after":4096}`
	s := newServer(t, static(ok(`[]`, `[]`, meta)))
	res, err := s.driver().Exec(context.Background(), "DELETE FROM t WHERE x < ?", 5)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n != 3 {
		t.Errorf("RowsAffected = %d, %v, want 3", n, err)
	}
	r, isD1 := res.(*d1.Result)
	if !isD1 {
		t.Fatalf("Result is %T, want *d1.Result", res)
	}
	if got, err := r.LastInsertID(); err != nil || got != 17 {
		t.Errorf("LastInsertID = %d, %v, want 17", got, err)
	}
	if r.Meta().RowsRead != 900 {
		t.Errorf("RowsRead = %d, want 900 — the number the bill is computed from", r.Meta().RowsRead)
	}
	if r.Meta().Elapsed() != 2500*time.Microsecond {
		t.Errorf("Elapsed = %v, want 2.5ms", r.Meta().Elapsed())
	}
}

// Errors ---------------------------------------------------------------

func TestUniqueViolationIsClassified(t *testing.T) {
	s := newServer(t, func(int) (int, string) {
		return http.StatusOK, `{"result":null,"success":false,"errors":[{"code":7500,"message":"UNIQUE constraint failed: users.email"}],"messages":[]}`
	})
	_, err := s.driver().Exec(context.Background(), "INSERT INTO users (email) VALUES (?)", "a@b.c")
	if !errors.Is(err, d1.ErrUniqueViolation) {
		t.Fatalf("err = %v, want ErrUniqueViolation", err)
	}
	table, cols := d1.ConstraintColumns(err)
	if table != "users" || len(cols) != 1 || cols[0] != "email" {
		t.Errorf("ConstraintColumns = %q, %v, want users [email]", table, cols)
	}
	// SQLite's own words have to survive, because a dialect package
	// reads this error's text to classify it a second time.
	if !strings.Contains(err.Error(), "UNIQUE constraint failed: users.email") {
		t.Errorf("error text lost SQLite's message: %q", err.Error())
	}
}

func TestConstraintClassifications(t *testing.T) {
	cases := []struct {
		message string
		want    error
	}{
		{"FOREIGN KEY constraint failed", d1.ErrForeignKeyViolation},
		{"CHECK constraint failed: positive_total", d1.ErrCheckViolation},
		{"NOT NULL constraint failed: users.name", d1.ErrNotNullViolation},
		{"no such table: ghosts", d1.ErrUndefinedTable},
		{"no such column: nope", d1.ErrUndefinedColumn},
	}
	for _, tc := range cases {
		t.Run(tc.message, func(t *testing.T) {
			s := newServer(t, func(int) (int, string) {
				return http.StatusOK, fmt.Sprintf(
					`{"result":null,"success":false,"errors":[{"code":7500,"message":%q}],"messages":[]}`, tc.message)
			})
			_, err := s.driver().Exec(context.Background(), "SELECT 1")
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// A foreign-key failure names no columns, and inventing some would be
// worse than admitting it.
func TestForeignKeyFailureHasNoColumnDetail(t *testing.T) {
	s := newServer(t, func(int) (int, string) {
		return http.StatusOK, `{"result":null,"success":false,"errors":[{"code":7500,"message":"FOREIGN KEY constraint failed"}],"messages":[]}`
	})
	_, err := s.driver().Exec(context.Background(), "INSERT INTO t VALUES (1)")
	if d := d1.ConstraintDetail(err); d != "" {
		t.Errorf("ConstraintDetail = %q, want empty", d)
	}
}

// A Cloudflare-level failure is not a SQLite one and must keep its own
// classification.
func TestAuthFailurePassesThroughUnclassified(t *testing.T) {
	s := newServer(t, func(int) (int, string) {
		return http.StatusForbidden, `{"result":null,"success":false,"errors":[{"code":10000,"message":"Authentication error"}],"messages":[]}`
	})
	_, err := s.driver().Exec(context.Background(), "SELECT 1")
	if !errors.Is(err, cloudflare.ErrUnauthorized) {
		t.Errorf("err = %v, want cloudflare.ErrUnauthorized", err)
	}
	if errors.Is(err, d1.ErrUniqueViolation) {
		t.Error("an auth failure was classified as a constraint violation")
	}
}

// Idempotency ----------------------------------------------------------

// A SELECT may be repeated; an INSERT may not, because the client
// cannot tell a request that never arrived from a reply that never
// came back.
func TestOnlyReadsAreRetried(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sql   string
		tries int
	}{
		{"select is retried", "SELECT * FROM t", 2},
		{"select of created_at is still a read", "SELECT created_at FROM t", 2},
		{"insert is not retried", "INSERT INTO t VALUES (1)", 1},
		{"cte carrying an insert is not retried", "WITH x AS (SELECT 1) INSERT INTO t SELECT * FROM x", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				n++
				if n == 1 {
					w.WriteHeader(http.StatusBadGateway)
					fmt.Fprint(w, `{"result":null,"success":false,"errors":[],"messages":[]}`)
					return
				}
				fmt.Fprint(w, ok(`["n"]`, `[[1]]`, ""))
			}))
			defer srv.Close()

			cf, _ := cloudflare.New("acct",
				cloudflare.WithAPIToken("t"),
				cloudflare.WithBaseURL(srv.URL),
				cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{MaxAttempts: 2}))
			_, _ = d1.New(cf, "db").Query(context.Background(), tc.sql)
			if n != tc.tries {
				t.Errorf("%d request(s), want %d", n, tc.tries)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ drops.Driver = (*d1.Driver)(nil)
