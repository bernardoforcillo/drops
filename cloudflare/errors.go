package cloudflare

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Sentinel errors for the failures a caller branches on.
var (
	// ErrNoAccountID is returned by New when the account ID is empty.
	ErrNoAccountID = errors.New("drops/cloudflare: account ID is empty")

	// ErrNoCredentials is returned by New when no API token was
	// supplied.
	ErrNoCredentials = errors.New("drops/cloudflare: no API token; use WithAPIToken")

	// ErrUnauthorized is returned when Cloudflare rejects the
	// credential — HTTP 401/403, or one of the authentication error
	// codes in the envelope. A token that was minted for the wrong
	// account, or without the permission the endpoint needs, lands
	// here rather than on a 404.
	ErrUnauthorized = errors.New("drops/cloudflare: unauthorized")

	// ErrNotFound is returned when the addressed resource does not
	// exist — a database ID, an index name, a namespace ID, a key.
	ErrNotFound = errors.New("drops/cloudflare: not found")

	// ErrRateLimited is returned when the request was throttled and
	// the retry policy gave up. [RetryAfter] reads the wait
	// Cloudflare asked for.
	ErrRateLimited = errors.New("drops/cloudflare: rate limited")
)

// Cloudflare envelope error codes this package classifies. They are
// stable across the whole API, which is what makes branching on them
// better than branching on the HTTP status: a token without the right
// permission answers 200 + 10000 as readily as it answers 403.
const (
	// CodeAuthentication — the token is missing, malformed, or not
	// valid for this account.
	CodeAuthentication = 10000

	// CodeAuthorization — the token is valid but lacks the
	// permission the endpoint requires.
	CodeAuthorization = 10001

	// CodeNoRoute — no route for the requested URI. Almost always a
	// wrong account ID, database ID or index name in the path rather
	// than a client bug.
	CodeNoRoute = 7003

	// CodeNotFound — the addressed resource does not exist.
	CodeNotFound = 7000
)

// APIError is a failed Cloudflare API call.
//
// It is returned for a non-2xx response and, just as importantly, for
// a 2xx response whose envelope says `"success": false` — which is
// how several endpoints report a bad request. Callers that only check
// the status code miss those, so this package does not give them the
// chance: any reply that is not a successful envelope becomes an
// APIError.
type APIError struct {
	// Status is the HTTP status code, e.g. 400. Set even when the
	// failure came from the envelope rather than the status, in
	// which case it is 200.
	Status int

	// Method and Path are the request that failed, for an error
	// message that says what was being done.
	Method string
	Path   string

	// Errors are the envelope's errors[] entries, in order. Usually
	// one; Cloudflare occasionally returns several.
	Errors []Error

	// Messages are the envelope's messages[] entries — advisory
	// text, not failures.
	Messages []Error

	// Body is the raw response body, captured for the cases where
	// the reply was not a decodable envelope at all (an HTML error
	// page from an edge proxy, say). Truncated to the first 4 KiB.
	Body []byte

	// Sentinel is the package-level Err* value classifying the
	// failure, or nil when it is not one of them. Reachable with
	// errors.Is.
	Sentinel error
}

// Error is one entry from a Cloudflare envelope's errors[] or
// messages[] array.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`

	// ErrorChain carries the nested causes some endpoints attach.
	ErrorChain []Error `json:"error_chain,omitempty"`
}

func (e Error) String() string {
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("%d: %s", e.Code, e.Message)
}

// Error implements error.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "drops/cloudflare: %s %s: %d", e.Method, e.Path, e.Status)
	if len(e.Errors) == 0 {
		if len(e.Body) > 0 {
			fmt.Fprintf(&b, ": %s", truncate(string(e.Body), 200))
		}
		return b.String()
	}
	parts := make([]string, 0, len(e.Errors))
	for _, one := range e.Errors {
		parts = append(parts, one.String())
	}
	fmt.Fprintf(&b, ": %s", strings.Join(parts, "; "))
	return b.String()
}

// Is matches the package sentinel so errors.Is(err,
// cloudflare.ErrUnauthorized) answers for the classified failures.
func (e *APIError) Is(target error) bool {
	return e.Sentinel != nil && target == e.Sentinel
}

// HasCode reports whether any of the envelope's errors carries code,
// including inside an error_chain. It is the branch a caller actually
// wants:
//
//	if apiErr.HasCode(cloudflare.CodeAuthorization) {
//	    return fmt.Errorf("token needs D1:Edit on this account")
//	}
func (e *APIError) HasCode(code int) bool { return hasCode(e.Errors, code) }

// FirstMessage returns the message of the first envelope error, or
// the empty string when there is none. Useful for backends that
// classify on Cloudflare's wording — D1 passes SQLite's own
// "UNIQUE constraint failed: …" through here.
func (e *APIError) FirstMessage() string {
	if len(e.Errors) == 0 {
		return ""
	}
	return e.Errors[0].Message
}

func hasCode(errs []Error, code int) bool {
	for _, one := range errs {
		if one.Code == code || hasCode(one.ErrorChain, code) {
			return true
		}
	}
	return false
}

// classify picks the sentinel for a failed reply. The envelope code
// is consulted before the HTTP status because it is the more specific
// of the two: Cloudflare answers 200 + 10000 for a token that is
// valid but unscoped, and a status-only reading calls that a success.
func classify(status int, errs []Error) error {
	switch {
	case hasCode(errs, CodeAuthentication), hasCode(errs, CodeAuthorization):
		return ErrUnauthorized
	case hasCode(errs, CodeNoRoute), hasCode(errs, CodeNotFound):
		return ErrNotFound
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusTooManyRequests:
		return ErrRateLimited
	}
	return nil
}

// NewAPIError builds an [APIError] from a reply a backend read for
// itself rather than through [Client.Do].
//
// It exists for the endpoints that do not answer with a Cloudflare
// envelope — an R2 object read, a Workers KV value — where a failure
// still has to arrive as the same type, matching the same sentinels,
// as one from any other endpoint. A caller branching on
// [ErrNotFound] should not have to know which kind of endpoint it
// asked.
func NewAPIError(method, path string, resp *Response, errs []Error) *APIError {
	return newAPIError(Request{Method: method, Path: path}, resp, errs, nil)
}

// truncate caps s at n bytes, appending an ellipsis when it cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
