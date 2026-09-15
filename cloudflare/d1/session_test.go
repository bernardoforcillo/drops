package d1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/d1"
)

// sessionServer records every request body it receives and answers
// with the bookmarks given, one per request.
type sessionServer struct {
	*httptest.Server

	mu   sync.Mutex
	got  []d1.Request
	next []string
}

func newSessionServer(t *testing.T, bookmarks ...string) *sessionServer {
	t.Helper()
	s := &sessionServer{next: bookmarks}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req d1.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("request is not protocol JSON: %v", err)
		}
		s.mu.Lock()
		s.got = append(s.got, req)
		var bookmark string
		if len(s.next) > 0 {
			bookmark, s.next = s.next[0], s.next[1:]
		}
		s.mu.Unlock()

		reply := map[string]any{
			"protocol": 2,
			"results": []any{map[string]any{
				"columns": []string{"n"},
				"rows":    [][]any{{1}},
				"meta":    map[string]any{"served_by_primary": true},
			}},
		}
		if bookmark != "" {
			reply["bookmark"] = bookmark
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sessionServer) requests() []d1.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]d1.Request(nil), s.got...)
}

func newSessionBridge(t *testing.T, srv *sessionServer) *d1.Driver {
	t.Helper()
	drv, err := d1.NewBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	return drv
}

// The whole point of a session: the first request declares where it
// may be served from, and every one after it carries the bookmark the
// previous one returned, so no read can go backwards.
func TestSessionCarriesConstraintThenBookmark(t *testing.T) {
	srv := newSessionServer(t, "bookmark-one", "bookmark-two")
	drv := newSessionBridge(t, srv)

	sess, err := drv.Session(d1.FirstPrimary)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got := sess.Bookmark(); got != "" {
		t.Errorf("a fresh session already has bookmark %q", got)
	}

	for i := 0; i < 2; i++ {
		rows, qErr := sess.Query(context.Background(), "SELECT 1")
		if qErr != nil {
			t.Fatalf("Query %d: %v", i, qErr)
		}
		rows.Close()
	}

	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if reqs[0].Session != string(d1.FirstPrimary) {
		t.Errorf("first request session = %q, want %q", reqs[0].Session, d1.FirstPrimary)
	}
	if reqs[1].Session != "bookmark-one" {
		t.Errorf("second request session = %q, want the first reply's bookmark", reqs[1].Session)
	}
	if got := sess.Bookmark(); got != "bookmark-two" {
		t.Errorf("session bookmark = %q, want bookmark-two", got)
	}
	for i, r := range reqs {
		if r.Protocol != 2 {
			t.Errorf("request %d asks for protocol %d, want 2 — a session needs a handler that understands one", i, r.Protocol)
		}
	}
}

// A runtime that reports no bookmark — wrangler dev does not — must
// leave the session where it was rather than resetting it to its
// constraint, which would silently drop the guarantee mid-session.
func TestSessionKeepsBookmarkWhenReplyOmitsOne(t *testing.T) {
	srv := newSessionServer(t, "bookmark-one", "")
	drv := newSessionBridge(t, srv)

	sess, err := drv.Session(d1.FirstUnconstrained)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	for i := 0; i < 3; i++ {
		rows, qErr := sess.Query(context.Background(), "SELECT 1")
		if qErr != nil {
			t.Fatalf("Query %d: %v", i, qErr)
		}
		rows.Close()
	}
	if got := sess.Bookmark(); got != "bookmark-one" {
		t.Errorf("bookmark = %q, want it held at bookmark-one", got)
	}
	reqs := srv.requests()
	if reqs[2].Session != "bookmark-one" {
		t.Errorf("third request session = %q, want bookmark-one", reqs[2].Session)
	}
}

// A failed request did not move the database, so it must not move the
// session: a retry has to be as consistent as the attempt that failed.
func TestSessionBookmarkSurvivesAFailedRequest(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			fmt.Fprint(w, `{"protocol":2,"results":[{"columns":[],"rows":[],"meta":{}}],"bookmark":"good"}`)
			return
		}
		fmt.Fprint(w, `{"protocol":2,"error":{"message":"UNIQUE constraint failed: users.email","index":0}}`)
	}))
	defer srv.Close()

	drv, err := d1.NewBridge(srv.URL)
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	sess, err := drv.Session(d1.FirstPrimary)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err = sess.Exec(context.Background(), "INSERT INTO users VALUES (1)"); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	if _, err = sess.Exec(context.Background(), "INSERT INTO users VALUES (1)"); !errors.Is(err, d1.ErrUniqueViolation) {
		t.Fatalf("second Exec err = %v, want a unique violation", err)
	}
	if got := sess.Bookmark(); got != "good" {
		t.Errorf("bookmark = %q after a failure, want it unchanged at %q", got, "good")
	}
}

// Resume is what carries read-your-writes past the end of a request.
func TestResumeStartsFromTheGivenBookmark(t *testing.T) {
	srv := newSessionServer(t, "later")
	drv := newSessionBridge(t, srv)

	sess, err := drv.Resume("earlier")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := sess.Bookmark(); got != "earlier" {
		t.Errorf("Bookmark = %q before any request, want earlier", got)
	}
	rows, err := sess.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	rows.Close()

	if got := srv.requests()[0].Session; got != "earlier" {
		t.Errorf("request session = %q, want earlier", got)
	}
	if got := sess.Bookmark(); got != "later" {
		t.Errorf("Bookmark = %q after the request, want later", got)
	}
}

func TestResumeRefusesAnEmptyBookmark(t *testing.T) {
	srv := newSessionServer(t)
	drv := newSessionBridge(t, srv)
	if _, err := drv.Resume(""); !errors.Is(err, d1.ErrNoBookmark) {
		t.Errorf("Resume(\"\") err = %v, want ErrNoBookmark", err)
	}
}

func TestSessionRefusesAnUnknownConstraint(t *testing.T) {
	srv := newSessionServer(t)
	drv := newSessionBridge(t, srv)
	if _, err := drv.Session("first-replica"); !errors.Is(err, d1.ErrUnknownConstraint) {
		t.Errorf("err = %v, want ErrUnknownConstraint", err)
	}
}

func TestSessionDefaultsToUnconstrained(t *testing.T) {
	srv := newSessionServer(t)
	drv := newSessionBridge(t, srv)
	sess, err := drv.Session("")
	if err != nil {
		t.Fatalf("Session(\"\"): %v", err)
	}
	if sess.Constraint() != d1.FirstUnconstrained {
		t.Errorf("constraint = %q, want %q", sess.Constraint(), d1.FirstUnconstrained)
	}
}

// Cloudflare does not offer the Sessions API over the REST API, and
// the refusal is at construction so a misrouted deployment fails at
// boot rather than serving a stale read months later.
func TestRESTTransportRefusesSessions(t *testing.T) {
	cf, err := cloudflare.New("acct", cloudflare.WithAPIToken("tok"))
	if err != nil {
		t.Fatalf("cloudflare.New: %v", err)
	}
	drv := d1.New(cf, "db-id")

	if _, err = drv.Session(d1.FirstPrimary); !errors.Is(err, d1.ErrSessionsUnsupported) {
		t.Errorf("Session err = %v, want ErrSessionsUnsupported", err)
	}
	if _, err = drv.Resume("bookmark"); !errors.Is(err, d1.ErrSessionsUnsupported) {
		t.Errorf("Resume err = %v, want ErrSessionsUnsupported", err)
	}
	// The message has to name the way through, not just the refusal.
	if _, err = drv.Session(d1.FirstPrimary); err != nil {
		for _, want := range []string{"NewBridge", "Worker"} {
			if !contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}
}

// A Session is a drops.Driver, so the dialect runs inside one.
func TestSessionIsADriver(t *testing.T) {
	srv := newSessionServer(t, "b1")
	drv := newSessionBridge(t, srv)
	sess, err := drv.Session(d1.FirstUnconstrained)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	var d drops.Driver = sess
	rows, err := d.Query(context.Background(), "SELECT 1")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	rows.Close()
	if sess.Driver() != drv {
		t.Error("Driver() does not return the driver the session was opened on")
	}
}

// A transaction opened on a session commits inside it, so its batch
// advances the bookmark like any other request.
func TestSessionTransactionCommitsSessioned(t *testing.T) {
	srv := newSessionServer(t, "after-commit")
	drv := newSessionBridge(t, srv)
	sess, err := drv.Session(d1.FirstPrimary)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	err = drops.InTx(context.Background(), sess, func(tx drops.Tx) error {
		_, execErr := tx.Exec(context.Background(), "INSERT INTO t VALUES (1)")
		return execErr
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}

	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests, want 1 — the transaction ships as one batch", len(reqs))
	}
	if reqs[0].Session != string(d1.FirstPrimary) {
		t.Errorf("commit session = %q, want the session's constraint", reqs[0].Session)
	}
	if got := sess.Bookmark(); got != "after-commit" {
		t.Errorf("bookmark = %q, want after-commit", got)
	}
}

func TestClosedSessionRefusesStatements(t *testing.T) {
	srv := newSessionServer(t, "b1")
	drv := newSessionBridge(t, srv)
	sess, err := drv.Session(d1.FirstUnconstrained)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err = sess.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err = sess.Exec(context.Background(), "SELECT 1"); !errors.Is(err, d1.ErrSessionDone) {
		t.Errorf("err = %v, want ErrSessionDone", err)
	}
}

// The local limit checks belong to the driver, and a session must not
// be a way around them.
func TestSessionAppliesTheDriversLimits(t *testing.T) {
	srv := newSessionServer(t, "b1")
	drv, err := d1.NewBridge(srv.URL, d1.WithMaxBoundParams(2))
	if err != nil {
		t.Fatalf("NewBridge: %v", err)
	}
	sess, err := drv.Session(d1.FirstUnconstrained)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err = sess.Exec(context.Background(), "INSERT INTO t VALUES (?,?,?)", 1, 2, 3); !errors.Is(err, d1.ErrTooManyParams) {
		t.Errorf("err = %v, want ErrTooManyParams", err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("%d requests reached the server; the check should be local", n)
	}
}
