package cloudflare_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare"
)

// newClient wires a Client to a test server, with retries off unless
// a test asks for them.
func newClient(t *testing.T, h http.HandlerFunc, opts ...cloudflare.Option) *cloudflare.Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	all := append([]cloudflare.Option{
		cloudflare.WithAPIToken("test-token"),
		cloudflare.WithBaseURL(srv.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	}, opts...)
	c, err := cloudflare.New("acct", all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func envelope(result string) string {
	return `{"result":` + result + `,"success":true,"errors":[],"messages":[]}`
}

func TestNewRequiresAccountAndToken(t *testing.T) {
	if _, err := cloudflare.New("", cloudflare.WithAPIToken("t")); !errors.Is(err, cloudflare.ErrNoAccountID) {
		t.Errorf("empty account: got %v, want ErrNoAccountID", err)
	}
	if _, err := cloudflare.New("acct"); !errors.Is(err, cloudflare.ErrNoCredentials) {
		t.Errorf("no token: got %v, want ErrNoCredentials", err)
	}
}

func TestDoSendsBearerTokenAndDecodesResult(t *testing.T) {
	var gotAuth, gotPath string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		fmt.Fprint(w, envelope(`{"name":"widgets"}`))
	})

	var out struct {
		Name string `json:"name"`
	}
	if err := c.Do(context.Background(), cloudflare.Request{
		Method: http.MethodGet,
		Path:   c.AccountPath("/d1/database/", "db-1"),
	}, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if want := "/accounts/acct/d1/database/db-1"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if out.Name != "widgets" {
		t.Errorf("result.name = %q, want widgets", out.Name)
	}
}

// The whole reason this package decodes the envelope rather than
// trusting the status: Cloudflare answers 200 with success:false, and
// a status-only check calls that a success.
func TestSuccessFalseOnHTTP200IsAnError(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"result":null,"success":false,"errors":[{"code":10001,"message":"Unauthorized to access requested resource"}],"messages":[]}`)
	})

	err := c.Do(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/x"}, nil)
	if err == nil {
		t.Fatal("expected an error for success:false")
	}
	var apiErr *cloudflare.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T", err)
	}
	if apiErr.Status != http.StatusOK {
		t.Errorf("Status = %d, want 200 — the failure was in the envelope", apiErr.Status)
	}
	if !apiErr.HasCode(cloudflare.CodeAuthorization) {
		t.Errorf("HasCode(10001) = false, errors = %v", apiErr.Errors)
	}
	if !errors.Is(err, cloudflare.ErrUnauthorized) {
		t.Errorf("errors.Is(ErrUnauthorized) = false for code 10001")
	}
}

// A code in the envelope classifies even when the status disagrees —
// that is the point of consulting it first.
func TestEnvelopeCodeClassifiesNotFoundOverStatus(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"result":null,"success":false,"errors":[{"code":7003,"message":"Could not route to /accounts/x"}],"messages":[]}`)
	})
	err := c.Do(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/x"}, nil)
	if !errors.Is(err, cloudflare.ErrNotFound) {
		t.Errorf("errors.Is(ErrNotFound) = false for code 7003: %v", err)
	}
}

func TestErrorChainIsSearched(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"result":null,"success":false,"errors":[{"code":1,"message":"outer","error_chain":[{"code":10000,"message":"inner"}]}],"messages":[]}`)
	})
	err := c.Do(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/x"}, nil)
	var apiErr *cloudflare.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T", err)
	}
	if !apiErr.HasCode(cloudflare.CodeAuthentication) {
		t.Error("HasCode did not look inside error_chain")
	}
}

func TestAccountPathEscapesParts(t *testing.T) {
	c := newClient(t, func(http.ResponseWriter, *http.Request) {})
	got := c.AccountPath("/storage/kv/namespaces/", "ns/../other", "values")
	if strings.Contains(got, "../") {
		t.Errorf("AccountPath let a part walk out of the path: %q", got)
	}
}

// Retries -----------------------------------------------------------

func TestRetriesOn429AndHonoursRetryAfter(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"result":null,"success":false,"errors":[],"messages":[]}`)
			return
		}
		fmt.Fprint(w, envelope(`"ok"`))
	}, cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{
		MaxAttempts:       3,
		RespectRetryAfter: true,
	}))

	var out string
	if err := c.Do(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/x"}, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("calls = %d, want 2 (one 429, one success)", got)
	}
	if out != "ok" {
		t.Errorf("result = %q", out)
	}
}

// A POST may have already been applied when the transport failed, so
// repeating it is not the client's call to make.
func TestPostIsNotRetriedByDefault(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"result":null,"success":false,"errors":[],"messages":[]}`)
	}, cloudflare.WithRetryPolicy(cloudflare.DefaultRetryPolicy()))

	_ = c.Do(context.Background(), cloudflare.Request{Method: http.MethodPost, Path: "/x", Body: map[string]string{"a": "b"}}, nil)
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1 — an unmarked POST must not be repeated", got)
	}
}

func TestPostMarkedIdempotentIsRetried(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"result":null,"success":false,"errors":[],"messages":[]}`)
			return
		}
		fmt.Fprint(w, envelope(`"ok"`))
	}, cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{MaxAttempts: 3}))

	var out string
	if err := c.Do(context.Background(), cloudflare.Request{
		Method:     http.MethodPost,
		Path:       "/x",
		Body:       map[string]string{"q": "read"},
		Idempotent: cloudflare.Idempotently(),
	}, &out); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

// The body has to be readable on every attempt, not just the first.
func TestRetriedRequestResendsItsBody(t *testing.T) {
	var bodies []string
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		bodies = append(bodies, string(buf))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"result":null,"success":false,"errors":[],"messages":[]}`)
			return
		}
		fmt.Fprint(w, envelope(`"ok"`))
	}, cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{MaxAttempts: 2}))

	if err := c.Do(context.Background(), cloudflare.Request{
		Method: http.MethodPut, Path: "/x", Body: map[string]int{"n": 7},
	}, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(bodies) != 2 || bodies[0] != bodies[1] || bodies[0] == "" {
		t.Errorf("bodies = %q, want the same non-empty body twice", bodies)
	}
}

// 4xx that is not 429 will fail identically next time.
func TestNoRetryOnPlain4xx(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"result":null,"success":false,"errors":[{"code":1003,"message":"bad"}],"messages":[]}`)
	}, cloudflare.WithRetryPolicy(cloudflare.DefaultRetryPolicy()))

	_ = c.Do(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/x"}, nil)
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// A Retry-After longer than the cap must fail instead of parking the
// caller for as long as the server asked.
func TestRetryAfterBeyondCapFailsFast(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"result":null,"success":false,"errors":[],"messages":[]}`)
	}, cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{
		MaxAttempts: 3, RespectRetryAfter: true, MaxRetryAfter: time.Second,
	}))

	start := time.Now()
	err := c.Do(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/x"}, nil)
	if !errors.Is(err, cloudflare.ErrRateLimited) {
		t.Errorf("errors.Is(ErrRateLimited) = false: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v — the cap did not apply", elapsed)
	}
}

func TestCancelledContextIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}, cloudflare.WithRetryPolicy(cloudflare.DefaultRetryPolicy()))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := c.Do(ctx, cloudflare.Request{Method: http.MethodGet, Path: "/x"}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("calls = %d, want 0", got)
	}
}

// The hook fires once per call, not once per attempt: a caller
// counting requests should see calls, not retries.
func TestHookFiresOncePerCallNotPerAttempt(t *testing.T) {
	var events []drops.QueryEvent
	var calls atomic.Int32
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"result":null,"success":false,"errors":[],"messages":[]}`)
			return
		}
		fmt.Fprint(w, envelope(`"ok"`))
	},
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{MaxAttempts: 3}),
		cloudflare.WithHook(func(_ context.Context, e drops.QueryEvent) { events = append(events, e) }),
	)

	if err := c.Do(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/x"}, nil); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("hook fired %d times, want 1", len(events))
	}
	if events[0].Kind != "http" || events[0].SQL != "GET /x" {
		t.Errorf("event = %+v, want Kind=http SQL=\"GET /x\"", events[0])
	}
}

func TestDoRawReturnsBodyUninterpreted(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("not an envelope"))
	})
	resp, err := c.DoRaw(context.Background(), cloudflare.Request{Method: http.MethodGet, Path: "/v"})
	if err != nil {
		t.Fatalf("DoRaw: %v", err)
	}
	if string(resp.Body) != "not an envelope" {
		t.Errorf("body = %q", resp.Body)
	}
	if resp.Status != http.StatusOK {
		t.Errorf("status = %d", resp.Status)
	}
}

func TestExponentialBackoffStaysWithinCap(t *testing.T) {
	b := cloudflare.ExponentialBackoff(10*time.Millisecond, 100*time.Millisecond)
	for attempt := 1; attempt <= 10; attempt++ {
		if d := b(attempt); d < 0 || d > 100*time.Millisecond {
			t.Fatalf("attempt %d: backoff %v out of [0, 100ms]", attempt, d)
		}
	}
}

func TestRetryAfterParsesBothForms(t *testing.T) {
	secs := http.Header{"Retry-After": []string{"12"}}
	if d, ok := cloudflare.RetryAfter(secs); !ok || d != 12*time.Second {
		t.Errorf("seconds form: %v %v", d, ok)
	}
	date := http.Header{
		"Retry-After": []string{time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)},
	}
	if d, ok := cloudflare.RetryAfter(date); !ok || d != 0 {
		t.Errorf("past date should clamp to 0, got %v %v", d, ok)
	}
	if _, ok := cloudflare.RetryAfter(http.Header{}); ok {
		t.Error("absent header reported present")
	}
}

// A streamed body is for the payloads there is no reason to hold in
// memory — an R2 object, a database dump on its way to one.
func TestStreamBodyIsSentWithItsLength(t *testing.T) {
	const payload = "CREATE TABLE t (a);\n"
	var got string
	var length int64 = -1
	var ctype string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got, length, ctype = string(raw), r.ContentLength, r.Header.Get("Content-Type")
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{}}`)
	})

	err := c.Do(context.Background(), cloudflare.Request{
		Method:        http.MethodPut,
		Path:          "/objects/dump.sql",
		Stream:        func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(payload)), nil },
		ContentLength: int64(len(payload)),
		ContentType:   "application/sql",
	}, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got != payload {
		t.Errorf("body = %q", got)
	}
	if length != int64(len(payload)) {
		t.Errorf("Content-Length = %d, want %d — a chunked upload is refused by some endpoints", length, len(payload))
	}
	if ctype != "application/sql" {
		t.Errorf("Content-Type = %q", ctype)
	}
}

// A retry has to send the body again, and a reader that has been
// drained has nothing left to send — so Stream is a factory rather
// than a reader, and it is called once per attempt.
func TestStreamIsReopenedForEachAttempt(t *testing.T) {
	var opens atomic.Int64
	var bodies []string
	var attempts atomic.Int64
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{}}`)
	}, cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{
		MaxAttempts: 3,
		Backoff:     func(int) time.Duration { return time.Millisecond },
		RetryOn:     cloudflare.RetryableStatus,
	}))

	err := c.Do(context.Background(), cloudflare.Request{
		// PUT is retryable by default, which is the case a stream
		// has to survive.
		Method: http.MethodPut,
		Path:   "/objects/k",
		Stream: func() (io.ReadCloser, error) {
			opens.Add(1)
			return io.NopCloser(strings.NewReader("payload")), nil
		},
		ContentLength: 7,
	}, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if opens.Load() != 3 {
		t.Errorf("the stream was opened %d times, want one per attempt", opens.Load())
	}
	for i, b := range bodies {
		if b != "payload" {
			t.Errorf("attempt %d sent %q, want the whole body again", i+1, b)
		}
	}
}

func TestStreamOpenFailureIsReported(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		t.Error("a request was made despite the body failing to open")
		fmt.Fprint(w, `{"success":true}`)
	})
	want := errors.New("no such file")
	err := c.Do(context.Background(), cloudflare.Request{
		Method: http.MethodPut,
		Path:   "/objects/k",
		Stream: func() (io.ReadCloser, error) { return nil, want },
	}, nil)
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want it to wrap the opener's", err)
	}
}

// R2 needs cf-r2-jurisdiction and cf-r2-storage-class on requests the
// shared client builds, so Request carries arbitrary headers.
func TestRequestHeadersTravel(t *testing.T) {
	var got http.Header
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{}}`)
	})
	err := c.Do(context.Background(), cloudflare.Request{
		Method: http.MethodGet,
		Path:   "/buckets",
		Header: http.Header{
			"Cf-R2-Jurisdiction": []string{"eu"},
			"If-None-Match":      []string{`"abc"`},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if got.Get("Cf-R2-Jurisdiction") != "eu" || got.Get("If-None-Match") != `"abc"` {
		t.Errorf("headers = %v", got)
	}
}

// The four headers the client owns come from the Request's own
// fields. A copy in Header would not override them — it would add a
// second value, and two Content-Type headers on one request is not a
// thing an endpoint has to accept.
func TestReservedHeadersAreNotDuplicated(t *testing.T) {
	var got http.Header
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `{"success":true,"errors":[],"messages":[],"result":{}}`)
	})
	err := c.Do(context.Background(), cloudflare.Request{
		Method:      http.MethodPost,
		Path:        "/x",
		Raw:         []byte("{}"),
		ContentType: "application/json",
		Accept:      "application/json",
		Header: http.Header{
			"content-type":  []string{"text/plain"},
			"Authorization": []string{"Bearer stolen"},
			"Accept":        []string{"text/csv"},
			"User-Agent":    []string{"not-drops"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	for _, h := range []string{"Content-Type", "Authorization", "Accept", "User-Agent"} {
		if n := len(got.Values(h)); n != 1 {
			t.Errorf("%s has %d values (%v), want exactly 1", h, n, got.Values(h))
		}
	}
	if got.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want the Request field's", got.Get("Content-Type"))
	}
	if got.Get("Authorization") != "Bearer test-token" {
		t.Errorf("Authorization = %q, want the client's own token", got.Get("Authorization"))
	}
}
