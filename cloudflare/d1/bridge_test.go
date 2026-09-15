package d1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare/d1"
)

func TestNewBridgeValidatesItsURL(t *testing.T) {
	for _, bad := range []string{"", "   ", "d1.internal", "/just/a/path"} {
		if _, err := d1.NewBridge(bad); err == nil {
			t.Errorf("NewBridge(%q) should have failed", bad)
		}
	}
	if _, err := d1.NewBridge("http://d1.internal"); err != nil {
		t.Errorf("NewBridge on a valid URL: %v", err)
	}
}

func TestBridgeSendsTheProtocolEnvelope(t *testing.T) {
	var got d1.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		fmt.Fprint(w, `{"protocol":1,"results":[{"columns":["n"],"rows":[[1]],"meta":{}}]}`)
	}))
	defer srv.Close()

	drv, err := d1.NewBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	rows, err := drv.Query(context.Background(), "SELECT ?", 1)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	rows.Close()

	// A request declares the oldest version that can serve it
	// correctly, not the newest this package knows: an unsessioned
	// statement is served by a version 1 handler, so asking for more
	// would break deployments that need nothing newer.
	if got.Protocol != d1.MinProtocolVersion {
		t.Errorf("protocol = %d, want %d", got.Protocol, d1.MinProtocolVersion)
	}
	if got.Session != "" {
		t.Errorf("session = %q on an unsessioned request", got.Session)
	}
	if len(got.Statements) != 1 || got.Statements[0].SQL != "SELECT ?" {
		t.Errorf("statements = %+v", got.Statements)
	}
}

// Unlike the REST transport, the bridge keeps a batch's statements
// separate — the Worker hands them to db.batch(), which is where the
// atomicity comes from.
func TestBridgeBatchKeepsStatementsSeparate(t *testing.T) {
	var got d1.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		fmt.Fprint(w, `{"protocol":1,"results":[{"columns":[],"rows":[],"meta":{"changes":1}},{"columns":[],"rows":[],"meta":{"changes":1}}]}`)
	}))
	defer srv.Close()

	drv, _ := d1.NewBridge(srv.URL)
	if _, err := d1.NewBatch(drv).
		Add("INSERT INTO a VALUES (?)", 1).
		Add("INSERT INTO b VALUES (?)", 2).
		Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got.Statements) != 2 {
		t.Fatalf("%d statement(s) on the wire, want 2 — the bridge must not concatenate", len(got.Statements))
	}
	if len(got.Statements[0].Params) != 1 || len(got.Statements[1].Params) != 1 {
		t.Errorf("parameters were flattened: %+v", got.Statements)
	}
}

func TestBridgeHeadersAreSent(t *testing.T) {
	var secret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secret = r.Header.Get("X-Bridge-Secret")
		fmt.Fprint(w, `{"protocol":1,"results":[{"columns":[],"rows":[],"meta":{}}]}`)
	}))
	defer srv.Close()

	drv, err := d1.NewBridge(srv.URL, d1.WithBridgeOptions(d1.WithBridgeHeader("X-Bridge-Secret", "shh")))
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if _, err := drv.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if secret != "shh" {
		t.Errorf("header = %q, want shh", secret)
	}
}

// A statement SQLite refused comes back 200 with an error body, and
// must never be repeated: it will fail identically.
func TestBridgeStatementFailureIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"protocol":1,"error":{"message":"UNIQUE constraint failed: users.email","code":"SQLITE_CONSTRAINT_UNIQUE","index":0}}`)
	}))
	defer srv.Close()

	drv, _ := d1.NewBridge(srv.URL)
	_, err := drv.Query(context.Background(), "SELECT * FROM users")
	if !errors.Is(err, d1.ErrUniqueViolation) {
		t.Fatalf("err = %v, want ErrUniqueViolation", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("%d call(s), want 1 — a SQL failure is not transient", got)
	}
}

// A 5xx from the bridge may be transient, so a read is worth
// repeating and a write is not.
func TestBridgeRetriesReadsButNotWrites(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   func(*d1.Driver) error
		calls int32
	}{
		{"read", func(d *d1.Driver) error {
			rows, err := d.Query(context.Background(), "SELECT 1")
			if rows != nil {
				rows.Close()
			}
			return err
		}, 2},
		{"write", func(d *d1.Driver) error {
			_, err := d.Exec(context.Background(), "INSERT INTO t VALUES (1)")
			return err
		}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(http.StatusBadGateway)
					fmt.Fprint(w, `{"protocol":1,"error":{"message":"upstream","index":-1}}`)
					return
				}
				fmt.Fprint(w, `{"protocol":1,"results":[{"columns":[],"rows":[],"meta":{}}]}`)
			}))
			defer srv.Close()

			drv, _ := d1.NewBridge(srv.URL)
			_ = tc.run(drv)
			if got := calls.Load(); got != tc.calls {
				t.Errorf("%d call(s), want %d", got, tc.calls)
			}
		})
	}
}

// A handler speaking a version this client does not is refused rather
// than parsed hopefully.
func TestBridgeRefusesAForeignProtocolVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"protocol":99,"results":[]}`)
	}))
	defer srv.Close()

	drv, _ := d1.NewBridge(srv.URL)
	_, err := drv.Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("expected a protocol-version error")
	}
	if !contains(err.Error(), "protocol 99") {
		t.Errorf("err = %v, want it to name the version mismatch", err)
	}
}

func TestBridgeSurfacesANonJSONReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, "<html>the worker is down</html>")
	}))
	defer srv.Close()

	drv, _ := d1.NewBridge(srv.URL)
	_, err := drv.Exec(context.Background(), "SELECT 1")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "502") || !contains(err.Error(), "the worker is down") {
		t.Errorf("err = %v, want it to carry the status and the body", err)
	}
}

func TestBridgeHookReportsTheStatement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"protocol":1,"results":[{"columns":[],"rows":[],"meta":{}}]}`)
	}))
	defer srv.Close()

	var events []drops.QueryEvent
	drv, err := d1.NewBridge(srv.URL, d1.WithBridgeOptions(
		d1.WithBridgeHook(func(_ context.Context, e drops.QueryEvent) { events = append(events, e) }),
	))
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	if _, err := drv.Exec(context.Background(), "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(events))
	}
	if events[0].Kind != "exec" || events[0].SQL != "INSERT INTO t VALUES (1)" {
		t.Errorf("event = %+v", events[0])
	}
}

// The limits are checked before anything is sent, so the error can
// say what to do instead.
func TestBridgeRefusesAnOversizeStatementLocally(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, `{"protocol":1,"results":[{"columns":[],"rows":[],"meta":{}}]}`)
	}))
	defer srv.Close()

	drv, _ := d1.NewBridge(srv.URL)
	big := "SELECT '" + repeat("x", d1.Published.StatementSize) + "'"
	_, err := drv.Query(context.Background(), big)
	if !errors.Is(err, d1.ErrStatementTooLarge) {
		t.Fatalf("err = %v, want ErrStatementTooLarge", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("%d call(s), want 0", got)
	}
}

// The too-many-parameters message has to say what to do instead, or
// it is no better than D1's own.
func TestTooManyParamsSuggestsJSONEach(t *testing.T) {
	drv, _ := d1.NewBridge("http://d1.internal")
	args := make([]any, d1.Published.BoundParams+1)
	for i := range args {
		args[i] = i
	}
	_, err := drv.Query(context.Background(), "SELECT 1", args...)
	if !errors.Is(err, d1.ErrTooManyParams) {
		t.Fatalf("err = %v, want ErrTooManyParams", err)
	}
	if !contains(err.Error(), "json_each") {
		t.Errorf("err = %v, want it to name the json_each workaround", err)
	}
}

func TestDriverStringNamesTheTarget(t *testing.T) {
	drv, _ := d1.NewBridge("http://d1.internal")
	if got := drv.String(); !contains(got, "d1.internal") {
		t.Errorf("String() = %q, want it to name the bridge", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

func repeat(s string, n int) string {
	out := make([]byte, 0, n*len(s))
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
