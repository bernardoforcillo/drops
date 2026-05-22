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
type Client struct {
	base   string
	apiKey string
	http   *http.Client
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

// Do issues a request to <base>+path with optional JSON body and
// decodes the response into out (if non-nil). The Qdrant convention is
// `{"result": ..., "status": "ok", "time": float}`; out is decoded
// against the result field.
//
// Callers usually don't need Do; the typed methods (CreateCollection,
// Upsert, Search, …) wrap it.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var buf io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("qdrant: encode body: %w", err)
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

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("qdrant: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		herr := &HTTPError{Status: resp.StatusCode, StatusText: resp.Status, Body: respBody}
		if resp.StatusCode == http.StatusNotFound && strings.Contains(string(respBody), "not found") {
			return fmt.Errorf("%w: %w", ErrCollectionMissing, herr)
		}
		return herr
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
	if err := json.Unmarshal(respBody, &env); err != nil {
		return fmt.Errorf("qdrant: decode envelope: %w", err)
	}
	if len(env.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("qdrant: decode result: %w", err)
	}
	return nil
}
