package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops"
)

// DefaultBaseURL is Cloudflare's REST API root.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// maxCapturedBody caps how much of a failed response body is kept on
// an [APIError]. Enough to read an edge proxy's HTML error page down
// to the useful line, not enough to put a megabyte in a log.
const maxCapturedBody = 4 << 10

// Client talks to Cloudflare's REST API for one account.
//
// Safe for concurrent use by multiple goroutines; the backends built
// on it share a single Client.
type Client struct {
	accountID string
	token     string
	base      string
	http      *http.Client
	hook      drops.Hook
	retry     RetryPolicy
	userAgent string
}

// Option configures a [Client].
type Option func(*Client)

// WithAPIToken sets the bearer token every request authenticates
// with. Required.
//
// Scope the token to the permission the backend needs — D1:Edit,
// Vectorize:Edit, Workers KV Storage:Edit — rather than reusing one
// token across all of them. The legacy global API key is deliberately
// not supported: it cannot be scoped at all.
func WithAPIToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithBaseURL overrides the API root. For tests against an
// httptest.Server, and for the rare deployment that fronts the API
// with a proxy.
func WithBaseURL(base string) Option {
	return func(c *Client) {
		if base != "" {
			c.base = strings.TrimRight(base, "/")
		}
	}
}

// WithHTTPClient supplies the http.Client used for every request —
// custom transport, connection pool, or instrumentation.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithTimeout sets the timeout on the default http.Client. Ignored
// when [WithHTTPClient] supplied one.
//
// The timeout covers a single attempt, not the whole retried call:
// three attempts against a hung endpoint can take three times this.
// Bound the call with the context if that matters.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithHook installs an observability hook, fired once per request
// after retries are exhausted or the request succeeds. Kind is
// "http", SQL is "<METHOD> <path>" — the shape drops/qdrant emits, so
// one drops.LoggerHook reads Cloudflare and Qdrant traffic alike.
func WithHook(h drops.Hook) Option {
	return func(c *Client) { c.hook = h }
}

// WithRetryPolicy replaces [DefaultRetryPolicy]. Pass the zero
// RetryPolicy to disable retries entirely.
func WithRetryPolicy(p RetryPolicy) Option {
	return func(c *Client) { c.retry = p }
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) {
		if ua != "" {
			c.userAgent = ua
		}
	}
}

// New returns a Client for the given Cloudflare account.
//
// The account ID is the 32-hex-character identifier in the dashboard
// URL, also printed by `wrangler whoami`.
func New(accountID string, opts ...Option) (*Client, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, ErrNoAccountID
	}
	c := &Client{
		accountID: accountID,
		base:      DefaultBaseURL,
		http:      &http.Client{Timeout: 30 * time.Second},
		retry:     DefaultRetryPolicy(),
		userAgent: "drops-cloudflare/1 (+https://github.com/bernardoforcillo/drops)",
	}
	for _, o := range opts {
		o(c)
	}
	if c.token == "" {
		return nil, ErrNoCredentials
	}
	if _, err := url.Parse(c.base); err != nil {
		return nil, fmt.Errorf("drops/cloudflare: invalid base URL %q: %w", c.base, err)
	}
	return c, nil
}

// AccountID returns the account every request is scoped to.
func (c *Client) AccountID() string { return c.accountID }

// BaseURL returns the configured API root.
func (c *Client) BaseURL() string { return c.base }

// HTTPClient returns the underlying http.Client.
func (c *Client) HTTPClient() *http.Client { return c.http }

// Hook returns the installed observability hook, or nil.
func (c *Client) Hook() drops.Hook { return c.hook }

// RetryPolicy returns the client's retry policy.
func (c *Client) RetryPolicy() RetryPolicy { return c.retry }

// WithHookFn returns a shallow copy of the client carrying hook — for
// layering a request-scoped hook over a shared, hook-free client.
func (c *Client) WithHookFn(hook drops.Hook) *Client {
	cp := *c
	cp.hook = hook
	return &cp
}

// AccountPath builds a path under this account:
// AccountPath("/d1/database/", id) is
// "/accounts/<account>/d1/database/<id>". Each part is path-escaped,
// so an identifier that arrives from configuration cannot walk out of
// the path it was meant to address.
func (c *Client) AccountPath(prefix string, parts ...string) string {
	var b strings.Builder
	b.WriteString("/accounts/")
	b.WriteString(url.PathEscape(c.accountID))
	b.WriteString(strings.TrimSuffix(prefix, "/"))
	for _, p := range parts {
		b.WriteString("/")
		b.WriteString(url.PathEscape(p))
	}
	return b.String()
}

// Request is one call to the Cloudflare API.
type Request struct {
	// Method is the HTTP method. Required.
	Method string

	// Path is the path below [Client.BaseURL], e.g.
	// "/accounts/abc/d1/database/xyz/raw". Build it with
	// [Client.AccountPath].
	Path string

	// Query is appended as the URL query string.
	Query url.Values

	// Body is JSON-encoded as the request body. Ignored when Raw is
	// set.
	Body any

	// Raw is sent as the body verbatim, with ContentType. For the
	// endpoints that are not JSON — Vectorize takes NDJSON, Workers
	// KV takes the value's own bytes.
	Raw []byte

	// Stream supplies the body as a reader instead of as bytes, for
	// a payload there is no reason to hold in memory — an R2 object,
	// a database dump on its way to one.
	//
	// It is called once per attempt and must return a fresh reader
	// each time, because a retry has to send the body again and a
	// reader that has been drained has nothing left to send. A
	// source that genuinely cannot be re-opened belongs on a request
	// marked not to be retried.
	//
	// Ignored when Raw or Body is set.
	Stream func() (io.ReadCloser, error)

	// ContentLength is the body's length in bytes, for a Stream that
	// knows it. Leave it zero and the request is sent chunked, which
	// some endpoints refuse.
	ContentLength int64

	// ContentType overrides the Content-Type header. Defaults to
	// application/json when Body is set.
	ContentType string

	// Accept overrides the Accept header. Defaults to
	// application/json.
	Accept string

	// Header carries any other headers the endpoint needs — R2's
	// cf-r2-jurisdiction and cf-r2-storage-class, a conditional
	// If-None-Match. Authorization, Accept, Content-Type and
	// User-Agent are set from the fields above and are not taken
	// from here.
	Header http.Header

	// Idempotent overrides the method-based decision about whether a
	// failed request may be tried again.
	//
	// It has to be an override rather than an inference because the
	// inference is wrong in both directions on this API. A Vectorize
	// query is a POST and is perfectly safe to repeat; a D1 POST
	// carrying an INSERT is not, and repeating it writes the row
	// twice. Leave it nil and POST is treated as unsafe; set it
	// where you know better.
	Idempotent *bool
}

// Idempotently marks a request safe to retry — for the reads that
// Cloudflare models as POSTs.
func Idempotently() *bool { yes := true; return &yes }

// safeToRetry reports whether a failed attempt may be repeated.
func (r Request) safeToRetry() bool {
	if r.Idempotent != nil {
		return *r.Idempotent
	}
	switch strings.ToUpper(r.Method) {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	default:
		return false
	}
}

// Response is a raw reply, for the endpoints that do not answer with
// a Cloudflare envelope.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Envelope is the shape every JSON Cloudflare endpoint answers with.
type Envelope struct {
	Result     json.RawMessage `json:"result"`
	ResultInfo json.RawMessage `json:"result_info,omitempty"`
	Success    bool            `json:"success"`
	Errors     []Error         `json:"errors"`
	Messages   []Error         `json:"messages"`
}

// Do issues req, checks the Cloudflare envelope, and decodes the
// envelope's result into out when out is non-nil.
//
// A reply is a success only when the HTTP status is 2xx *and* the
// envelope says so. Everything else — including a 200 carrying
// `"success": false` — comes back as an [APIError].
//
// Pass a *json.RawMessage as out to take the result undecoded, which
// is what a backend wants when the values inside need
// [encoding/json.Decoder.UseNumber] to survive the round trip.
func (c *Client) Do(ctx context.Context, req Request, out any) error {
	resp, err := c.DoRaw(ctx, req)
	if err != nil {
		return err
	}
	env, err := c.envelope(req, resp)
	if err != nil {
		return err
	}
	if out == nil || len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("drops/cloudflare: %s %s: decode result: %w", req.Method, req.Path, err)
	}
	return nil
}

// DoEnvelope issues req and returns the decoded envelope without
// touching the result — for endpoints whose result_info or messages
// matter as much as the result itself.
func (c *Client) DoEnvelope(ctx context.Context, req Request) (*Envelope, error) {
	resp, err := c.DoRaw(ctx, req)
	if err != nil {
		return nil, err
	}
	return c.envelope(req, resp)
}

// envelope turns a raw reply into a checked Envelope, or an APIError.
func (c *Client) envelope(req Request, resp *Response) (*Envelope, error) {
	var env Envelope
	decodeErr := json.Unmarshal(resp.Body, &env)

	if resp.Status < 200 || resp.Status >= 300 {
		return nil, newAPIError(req, resp, env.Errors, env.Messages)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("drops/cloudflare: %s %s: decode envelope: %w (body: %s)",
			req.Method, req.Path, decodeErr, truncate(string(resp.Body), 200))
	}
	if !env.Success {
		return nil, newAPIError(req, resp, env.Errors, env.Messages)
	}
	return &env, nil
}

func newAPIError(req Request, resp *Response, errs, msgs []Error) *APIError {
	body := resp.Body
	if len(body) > maxCapturedBody {
		body = body[:maxCapturedBody]
	}
	return &APIError{
		Status:   resp.Status,
		Method:   req.Method,
		Path:     req.Path,
		Errors:   errs,
		Messages: msgs,
		Body:     body,
		Sentinel: classify(resp.Status, errs),
	}
}

// DoRaw issues req and returns the reply without interpreting it —
// for the endpoints that answer with the stored bytes rather than a
// JSON envelope, such as a Workers KV value read.
//
// Retries are applied here, so a caller of DoRaw gets the same
// rate-limit handling as one of Do. The hook fires once, after the
// last attempt.
func (c *Client) DoRaw(ctx context.Context, req Request) (resp *Response, err error) {
	start := time.Now()
	defer func() { c.emit(ctx, req, start, err) }()

	src, err := encodeBody(req)
	if err != nil {
		return nil, err
	}

	target := c.base + req.Path
	if len(req.Query) > 0 {
		target += "?" + req.Query.Encode()
	}

	attempts := c.retry.attempts()
	for attempt := 1; ; attempt++ {
		got, attemptErr := c.attempt(ctx, req, target, src)

		// A context that is done is the caller giving up, not the
		// network failing: it is never worth another attempt.
		if attemptErr != nil && isContextError(attemptErr) {
			return nil, attemptErr
		}
		status := 0
		if got != nil {
			status = got.Status
		}
		done := attempt >= attempts ||
			!req.safeToRetry() ||
			!c.retry.shouldRetry(status, attemptErr)
		if done {
			if attemptErr != nil {
				return nil, attemptErr
			}
			return got, nil
		}

		var header http.Header
		if got != nil {
			header = got.Header
		}
		wait, ok := c.retry.wait(attempt, header)
		if !ok {
			// Retry-After asked for longer than the cap: stop here
			// and say why, rather than parking the caller.
			if got != nil {
				apiErr := newAPIError(req, got, nil, nil)
				apiErr.Sentinel = ErrRateLimited
				return nil, apiErr
			}
			return nil, ErrRateLimited
		}
		if serr := sleep(ctx, wait); serr != nil {
			return nil, serr
		}
	}
}

// attempt performs one HTTP round trip and returns the drained
// reply. The [Response] carries the headers the retry policy needs,
// so the *http.Response does not escape this function and its body is
// closed here.
func (c *Client) attempt(ctx context.Context, req Request, target string, src bodySource) (*Response, error) {
	var reader io.Reader
	switch {
	case src.open != nil:
		rc, err := src.open()
		if err != nil {
			return nil, fmt.Errorf("drops/cloudflare: open request body: %w", err)
		}
		defer rc.Close()
		reader = rc
	case src.bytes != nil:
		reader = bytes.NewReader(src.bytes)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("drops/cloudflare: build request: %w", err)
	}
	if reader != nil {
		if src.bytes != nil {
			httpReq.ContentLength = int64(len(src.bytes))
			body := src.bytes
			httpReq.GetBody = func() (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(body)), nil
			}
		} else {
			httpReq.ContentLength = src.length
			httpReq.GetBody = src.open
		}
		if src.ctype != "" {
			httpReq.Header.Set("Content-Type", src.ctype)
		}
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			httpReq.Header.Add(k, v)
		}
	}
	accept := req.Accept
	if accept == "" {
		accept = "application/json"
	}
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("Authorization", "Bearer "+c.token)
	httpReq.Header.Set("User-Agent", c.userAgent)

	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
	if err != nil {
		// The headers still travel, so a read that failed part-way
		// through a 429 does not lose its Retry-After.
		return &Response{Status: httpResp.StatusCode, Header: httpResp.Header},
			fmt.Errorf("drops/cloudflare: %s %s: read response: %w", req.Method, req.Path, err)
	}
	return &Response{Status: httpResp.StatusCode, Header: httpResp.Header, Body: raw}, nil
}

// bodySource is a request body ready to be sent, once per attempt.
// Exactly one of bytes and open is set, or neither for a request
// with no body.
type bodySource struct {
	bytes  []byte
	open   func() (io.ReadCloser, error)
	length int64
	ctype  string
}

// encodeBody renders a Request's body and decides its content type.
func encodeBody(req Request) (bodySource, error) {
	ctype := req.ContentType
	switch {
	case req.Raw != nil:
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		return bodySource{bytes: req.Raw, ctype: ctype}, nil
	case req.Body != nil:
		raw, err := json.Marshal(req.Body)
		if err != nil {
			return bodySource{}, fmt.Errorf("drops/cloudflare: encode body: %w", err)
		}
		if ctype == "" {
			ctype = "application/json"
		}
		return bodySource{bytes: raw, ctype: ctype}, nil
	case req.Stream != nil:
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		return bodySource{open: req.Stream, length: req.ContentLength, ctype: ctype}, nil
	default:
		return bodySource{}, nil
	}
}

// emit fires the observability hook through drops.CallHook, so a
// panicking user hook cannot take the request goroutine with it.
func (c *Client) emit(ctx context.Context, req Request, start time.Time, err error) {
	drops.CallHook(c.hook, ctx, drops.QueryEvent{
		Kind:     "http",
		SQL:      req.Method + " " + req.Path,
		Duration: time.Since(start),
		Err:      err,
	})
}
