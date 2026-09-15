package d1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare"
)

// Transport is how a [Driver] reaches D1. There are two, because
// there are two genuinely different places the calling code runs.
//
// [RESTTransport] goes to Cloudflare's public API over the internet,
// authenticated by an API token. It is what a migration runner, a CI
// job or a developer's laptop uses — anything outside Cloudflare's
// network, where a service binding does not exist.
//
// [BridgeTransport] goes to a Worker of your own over a service
// binding or a private hostname, and that Worker holds the D1
// binding. It is what a Container or a sidecar uses. It is also
// faster and cheaper: the request never leaves Cloudflare's network,
// and it needs no API token, because the binding is the
// authorisation.
//
// Both speak to the same [Driver]; only the constructor differs.
// Implement the interface yourself for a transport that is neither —
// a test double, a queue, a tunnel.
type Transport interface {
	// Send runs stmts and returns one result per statement, in
	// order. A list of more than one runs as a D1 batch.
	//
	// readOnly reports whether every statement is a read, which is
	// what decides whether a failed request may be repeated.
	Send(ctx context.Context, stmts []Statement, readOnly bool) ([]StatementResult, error)

	// Target names where the transport points, for error messages
	// and for [Driver.String].
	Target() string
}

// RESTTransport talks to Cloudflare's public D1 API.
type RESTTransport struct {
	cf       *cloudflare.Client
	database string
}

// NewRESTTransport returns a transport for the D1 database with the
// given ID — the UUID `wrangler d1 info <name>` prints, not the
// database's name.
func NewRESTTransport(cf *cloudflare.Client, databaseID string) *RESTTransport {
	return &RESTTransport{cf: cf, database: databaseID}
}

// Target implements [Transport].
func (t *RESTTransport) Target() string {
	return "d1 rest:" + t.database
}

// Client returns the underlying Cloudflare API client.
func (t *RESTTransport) Client() *cloudflare.Client { return t.cf }

// DatabaseID returns the database this transport addresses.
func (t *RESTTransport) DatabaseID() string { return t.database }

// Send implements [Transport].
//
// The REST API takes one sql field, so several statements travel as
// one script with their parameters flattened in order — which is how
// SQLite binds them anyway, positionally, left to right across the
// whole script.
func (t *RESTTransport) Send(ctx context.Context, stmts []Statement, readOnly bool) ([]StatementResult, error) {
	if t.database == "" {
		return nil, ErrNoDatabaseID
	}
	req := cloudflare.Request{
		Method: http.MethodPost,
		// /raw rather than /query: it answers with an ordered
		// column list and positional rows, which is what a
		// positional Scan needs. /query answers with objects.
		Path: t.cf.AccountPath("/d1/database/", t.database) + "/raw",
		Body: joinStatements(stmts),
	}
	if readOnly {
		req.Idempotent = cloudflare.Idempotently()
	}

	var raw json.RawMessage
	if err := t.cf.Do(ctx, req, &raw); err != nil {
		return nil, ClassifyError(err)
	}
	return decodeRESTResults(raw)
}

// restResult is one element of the /raw endpoint's result array.
type restResult struct {
	Success bool `json:"success"`
	Meta    Meta `json:"meta"`
	Results struct {
		Columns []string `json:"columns"`
		Rows    [][]any  `json:"rows"`
	} `json:"results"`
	Error string `json:"error,omitempty"`
}

// decodeRESTResults decodes the envelope's result array with
// UseNumber, so an INTEGER column past 2^53 keeps its low bits
// instead of being rounded through a float64.
func decodeRESTResults(raw json.RawMessage) ([]StatementResult, error) {
	if len(raw) == 0 {
		return nil, errors.New("drops/cloudflare/d1: D1 returned no result for the statement")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var results []restResult
	if err := dec.Decode(&results); err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: decode result: %w", err)
	}
	if len(results) == 0 {
		return nil, errors.New("drops/cloudflare/d1: D1 returned no result for the statement")
	}
	out := make([]StatementResult, len(results))
	for i, r := range results {
		if !r.Success {
			return nil, &Error{
				Sentinel: sentinelFromMessage(r.Error),
				Message:  r.Error,
				Err:      fmt.Errorf("drops/cloudflare/d1: statement %d failed", i+1),
			}
		}
		out[i] = StatementResult{Columns: r.Results.Columns, Rows: r.Results.Rows, Meta: r.Meta}
	}
	return out, nil
}

// BridgeTransport talks to a Worker of yours that holds the D1
// binding, over the drops D1 wire protocol.
//
// This is the transport for code running inside Cloudflare — a
// Container reaching its Worker at http://d1.internal, a sidecar
// behind a service binding. No API token is involved: the binding is
// the authorisation, and the network the request travels is
// Cloudflare's own.
//
// The Worker side is worker/handler.js, which is a dependency-free ES
// module you mount inside your own Worker rather than deploy as-is —
// so your authentication, routing and logging stay yours and only the
// wire format comes from here. worker/fixtures.json is the
// conformance suite; run it from both sides and a protocol change
// cannot pass silently.
type BridgeTransport struct {
	base    string
	http    *http.Client
	hook    drops.Hook
	headers map[string]string
	retry   cloudflare.RetryPolicy
}

// BridgeOption configures a [BridgeTransport].
type BridgeOption func(*BridgeTransport)

// WithBridgeHTTPClient supplies the http.Client used for every
// request.
func WithBridgeHTTPClient(h *http.Client) BridgeOption {
	return func(t *BridgeTransport) {
		if h != nil {
			t.http = h
		}
	}
}

// WithBridgeTimeout sets the timeout on the default http.Client.
// Ignored when [WithBridgeHTTPClient] supplied one.
//
// D1 caps a query at thirty seconds, so a timeout below that will cut
// off queries D1 would have finished, and one far above it only
// delays noticing a hung bridge. The default is thirty-five seconds:
// D1's ceiling plus enough for the round trip.
func WithBridgeTimeout(d time.Duration) BridgeOption {
	return func(t *BridgeTransport) { t.http.Timeout = d }
}

// WithBridgeHeader adds a header to every request — a shared secret,
// a tenant, a trace parent. The bridge sends no credential of its
// own, so this is where one goes if your Worker wants one.
func WithBridgeHeader(name, value string) BridgeOption {
	return func(t *BridgeTransport) {
		if t.headers == nil {
			t.headers = map[string]string{}
		}
		t.headers[name] = value
	}
}

// WithBridgeHook installs an observability hook, fired once per
// request with Kind="query" or "exec" and the statement text.
func WithBridgeHook(h drops.Hook) BridgeOption {
	return func(t *BridgeTransport) { t.hook = h }
}

// WithBridgeRetry replaces the retry policy. The default retries
// read-only requests up to three times on 5xx, and never retries a
// request carrying a write.
func WithBridgeRetry(p cloudflare.RetryPolicy) BridgeOption {
	return func(t *BridgeTransport) { t.retry = p }
}

// NewBridgeTransport returns a transport pointing at a Worker.
//
//	drv := d1.NewBridge("http://d1.internal")
//
// baseURL is the origin, with or without a path prefix; the protocol
// endpoint is POSTed to baseURL itself, so mount the handler at
// whatever path the URL names.
func NewBridgeTransport(baseURL string, opts ...BridgeOption) (*BridgeTransport, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, errors.New("drops/cloudflare/d1: bridge base URL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: invalid bridge URL %q: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("drops/cloudflare/d1: bridge URL must include scheme and host, got %q", baseURL)
	}
	t := &BridgeTransport{
		base:  strings.TrimRight(baseURL, "/"),
		http:  &http.Client{Timeout: 35 * time.Second},
		retry: bridgeRetryPolicy(),
	}
	for _, o := range opts {
		o(t)
	}
	return t, nil
}

// bridgeRetryPolicy is the default: three attempts, exponential
// backoff with jitter, and no Retry-After handling because a Worker
// of yours is not rate-limiting you.
func bridgeRetryPolicy() cloudflare.RetryPolicy {
	return cloudflare.RetryPolicy{
		MaxAttempts: 3,
		Backoff:     cloudflare.ExponentialBackoff(100*time.Millisecond, 2*time.Second),
		RetryOn:     cloudflare.RetryableStatus,
	}
}

// Target implements [Transport].
func (t *BridgeTransport) Target() string { return "d1 bridge:" + t.base }

// BaseURL returns the Worker the transport points at.
func (t *BridgeTransport) BaseURL() string { return t.base }

// Send implements [Transport].
func (t *BridgeTransport) Send(ctx context.Context, stmts []Statement, readOnly bool) (out []StatementResult, err error) {
	start := time.Now()
	kind := "exec"
	if readOnly {
		kind = "query"
	}
	defer func() {
		drops.CallHook(t.hook, ctx, drops.QueryEvent{
			Kind:     kind,
			SQL:      statementText(stmts),
			Duration: time.Since(start),
			Err:      err,
		})
	}()

	body, err := json.Marshal(NewRequest(stmts))
	if err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: encode request: %w", err)
	}

	attempts := t.retry.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	for attempt := 1; ; attempt++ {
		resp, status, attemptErr := t.roundTrip(ctx, body)

		if attemptErr != nil && (errors.Is(attemptErr, context.Canceled) || errors.Is(attemptErr, context.DeadlineExceeded)) {
			return nil, attemptErr
		}
		// Two failures the bridge will reproduce exactly, so
		// repeating them only wastes the caller's time: a statement
		// SQLite refused, and a handler speaking a protocol this
		// client does not. Both arrive as errors alongside a 200,
		// which is why the status alone cannot decide this.
		if attemptErr != nil && !transientFailure(attemptErr) {
			return nil, attemptErr
		}
		// A statement that failed at SQLite will fail identically
		// next time, and a request carrying a write must never be
		// repeated at all: the client cannot tell a request that
		// never arrived from a reply that never came back.
		retryable := attemptErr != nil || t.retry.RetryOn == nil || t.retry.RetryOn(status, attemptErr)
		if attempt >= attempts || !readOnly || !retryable {
			if attemptErr != nil {
				return nil, attemptErr
			}
			return resp, nil
		}
		if t.retry.Backoff != nil {
			timer := time.NewTimer(t.retry.Backoff(attempt))
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
}

// transientFailure reports whether a failure is worth another
// attempt at all, before the retry policy gets a say.
func transientFailure(err error) bool {
	var sqlErr *Error
	if errors.As(err, &sqlErr) {
		return false
	}
	return !errors.Is(err, ErrProtocolMismatch)
}

// roundTrip performs one request. status is 0 when the failure was at
// the transport level.
func (t *BridgeTransport) roundTrip(ctx context.Context, body []byte) ([]StatementResult, int, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("drops/cloudflare/d1: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	for k, v := range t.headers {
		httpReq.Header.Set(k, v)
	}

	httpResp, err := t.http.Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, httpResp.StatusCode, fmt.Errorf("drops/cloudflare/d1: read bridge response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// A non-2xx is a bridge-level failure — a malformed body, a
		// missing binding, a rejected credential — not a SQL one.
		// It may carry a protocol error body, and says more when it
		// does.
		if resp, decodeErr := DecodeResponse(raw); decodeErr == nil && resp.Error != nil {
			return nil, httpResp.StatusCode, fmt.Errorf("drops/cloudflare/d1: bridge returned %d: %w",
				httpResp.StatusCode, resp.Error)
		}
		return nil, httpResp.StatusCode, fmt.Errorf("drops/cloudflare/d1: bridge returned %d: %s",
			httpResp.StatusCode, truncate(string(raw), 200))
	}

	resp, err := DecodeResponse(raw)
	if err != nil {
		return nil, httpResp.StatusCode, err
	}
	if resp.Error != nil {
		return nil, httpResp.StatusCode, &Error{
			Sentinel: sentinelFromMessage(resp.Error.Message),
			Message:  resp.Error.Message,
			Err:      resp.Error,
		}
	}
	if len(resp.Results) == 0 {
		return nil, httpResp.StatusCode, errors.New("drops/cloudflare/d1: bridge returned no result for the statement")
	}
	return resp.Results, httpResp.StatusCode, nil
}

// statementText renders the statements for a hook event — the one
// statement's text, or a count and the first for a batch.
func statementText(stmts []Statement) string {
	switch len(stmts) {
	case 0:
		return ""
	case 1:
		return stmts[0].SQL
	default:
		return fmt.Sprintf("batch of %d: %s; …", len(stmts), stmts[0].SQL)
	}
}

// truncate caps s at n bytes for an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
