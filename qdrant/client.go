package qdrant

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
)

// Errors --------------------------------------------------------------

// HTTPError is returned for non-2xx responses. The body is captured so
// callers can extract Qdrant's error payload when needed.
type HTTPError struct {
	Status     int
	StatusText string
	Body       []byte
}

func (e *HTTPError) Error() string {
	if len(e.Body) == 0 {
		return fmt.Sprintf("qdrant: %d %s", e.Status, e.StatusText)
	}
	return fmt.Sprintf("qdrant: %d %s: %s", e.Status, e.StatusText, e.Body)
}

// ErrCollectionMissing is returned when a 404 references a missing
// collection. Use errors.Is to branch on it.
var ErrCollectionMissing = errors.New("qdrant: collection not found")

// Client wraps Qdrant's HTTP API.
//
// Safe for concurrent use by multiple goroutines. The optional Hook
// fires after every HTTP request with method + path + duration + error
// so the same observability story (drops.LoggerHook, OTel adapter,
// metrics emitter) works against Qdrant just like it does against pg
// and clickhouse.
type Client struct {
	base   string
	apiKey string
	http   *http.Client
	hook   drops.Hook
}

// ClientOption configures a Client.
type ClientOption func(*Client)

// WithAPIKey sets the Authorization header sent with every request.
// Qdrant Cloud expects the api-key header; the package wires both
// `api-key` and `Authorization: Bearer` so either deployment style
// works without further config.
func WithAPIKey(key string) ClientOption { return func(c *Client) { c.apiKey = key } }

// WithHTTPClient overrides the default http.Client (useful for custom
// transports, timeouts, instrumentation).
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		if h != nil {
			c.http = h
		}
	}
}

// WithTimeout sets the request timeout on the underlying http.Client.
// Ignored if WithHTTPClient was used.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.http.Timeout = d }
}

// WithHook installs an observability hook that fires after every HTTP
// request the client makes. The same drops.Hook contract used by
// pg.DB and clickhouse.DB — compose with drops.ChainHooks and pair
// with drops.LoggerHook for instant request logging.
func WithHook(h drops.Hook) ClientOption {
	return func(c *Client) { c.hook = h }
}

// NewClient returns a Client for the supplied Qdrant base URL — e.g.
// "http://localhost:6333" or "https://my-cluster.eu.cloud.qdrant.io".
// Trailing slashes are trimmed.
func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("qdrant: baseURL is empty")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("qdrant: invalid baseURL %q: %w", baseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("qdrant: baseURL must include scheme + host, got %q", baseURL)
	}
	c := &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// BaseURL returns the configured base URL.
func (c *Client) BaseURL() string { return c.base }

// HTTPClient returns the underlying http.Client. Useful for tests that
// want to swap out a transport.
func (c *Client) HTTPClient() *http.Client { return c.http }

// Hook returns the currently attached observability hook, or nil.
func (c *Client) Hook() drops.Hook { return c.hook }

// WithHookFn returns a shallow copy of the client with hook installed
// (nil clears it). Useful when an existing client needs a request-
// scoped hook layered on top of a shared, hook-free client.
func (c *Client) WithHookFn(hook drops.Hook) *Client {
	cp := *c
	cp.hook = hook
	return &cp
}

// Ping issues a request to /healthz to confirm the Qdrant instance is
// reachable and willing to serve traffic. Suitable as a Kubernetes
// readiness probe shape.
func (c *Client) Ping(ctx context.Context) error {
	// /healthz returns 200 with body "healthz check passed" on success.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return err
	}
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.emit(ctx, "ping", "/healthz", start, err)
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		herr := &HTTPError{Status: resp.StatusCode, StatusText: resp.Status, Body: body}
		c.emit(ctx, "ping", "/healthz", start, herr)
		return herr
	}
	c.emit(ctx, "ping", "/healthz", start, nil)
	return nil
}

// emit fires the observability hook, if installed. Routed through
// drops.CallHook so a panicking user-supplied hook can't crash the
// request goroutine.
//
// Kind values: "http" for ordinary API calls (Do), "ping" for Ping.
// SQL is repurposed for the HTTP method + path (`"GET /collections"`);
// the contract is small enough for both SQL and HTTP backends.
func (c *Client) emit(ctx context.Context, kind, methodPath string, start time.Time, err error) {
	drops.CallHook(c.hook, ctx, drops.QueryEvent{
		Kind:     kind,
		SQL:      methodPath,
		Duration: time.Since(start),
		Err:      err,
	})
}

// Do issues a request to <base>+path with optional JSON body and
// decodes the response into out (if non-nil). The Qdrant convention is
// `{"result": ..., "status": "ok", "time": float}`; out is decoded
// against the result field.
//
// Every Do call fires the observability hook (if installed) exactly
// once after the request completes, with Kind="http", SQL set to
// "<METHOD> <path>", the elapsed duration, and any error.
//
// Callers usually don't need Do; the typed methods (CreateCollection,
// Upsert, Search, …) wrap it.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) (err error) {
	start := time.Now()
	defer func() { c.emit(ctx, "http", method+" "+path, start, err) }()

	var buf io.Reader
	if body != nil {
		raw, mErr := json.Marshal(body)
		if mErr != nil {
			err = fmt.Errorf("qdrant: encode body: %w", mErr)
			return err
		}
		buf = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, buf)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("api-key", c.apiKey)
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		err = fmt.Errorf("qdrant: read response: %w", readErr)
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		herr := &HTTPError{Status: resp.StatusCode, StatusText: resp.Status, Body: respBody}
		if resp.StatusCode == http.StatusNotFound && strings.Contains(string(respBody), "not found") {
			err = fmt.Errorf("%w: %w", ErrCollectionMissing, herr)
			return err
		}
		err = herr
		return err
	}

	if out == nil {
		return nil
	}
	// Decode into the `result` field of the envelope.
	var env struct {
		Result json.RawMessage `json:"result"`
		Status any             `json:"status"`
		Time   float64         `json:"time"`
	}
	if uErr := json.Unmarshal(respBody, &env); uErr != nil {
		err = fmt.Errorf("qdrant: decode envelope: %w", uErr)
		return err
	}
	if len(env.Result) == 0 {
		return nil
	}
	if uErr := json.Unmarshal(env.Result, out); uErr != nil {
		err = fmt.Errorf("qdrant: decode result: %w", uErr)
		return err
	}
	return nil
}
