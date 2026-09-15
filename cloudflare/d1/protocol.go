package d1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ProtocolVersion is the version of the drops D1 wire protocol this
// package speaks — the JSON a [BridgeTransport] sends to a Worker and
// the JSON it expects back.
//
// The protocol is defined here rather than left to each deployment
// because it is a contract between two halves of one feature written
// in two languages, and a contract owned by neither half drifts. The
// drift is the dangerous kind: a change to column ordering or to how
// a BLOB is encoded does not fail to compile, it returns the wrong
// rows. Pinning the version in the request is what lets a handler
// refuse a client it does not understand instead of guessing.
//
// worker/handler.js is the reference implementation, and
// worker/fixtures.json is the conformance suite both sides run
// against.
const ProtocolVersion = 2

// MinProtocolVersion is the oldest version this package still speaks.
//
// A request carries the *floor* it needs rather than the newest
// version this package knows: a plain statement is served correctly by
// a version 1 handler and says so, and only a request that needs
// something version 1 does not have — today, a session — asks for 2.
// That is why the number in a request can go down as well as up.
//
// The alternative, stamping every request with the newest version,
// would make a library upgrade break every deployed Worker that had
// not been redeployed with it, including the ones using no new
// feature at all. This way the break lands only where the feature
// does, and it lands as [ErrProtocolMismatch] rather than as a
// silently unsessioned query.
const MinProtocolVersion = 1

// ErrProtocolMismatch is returned when a handler answers with a
// protocol version this client does not speak. It is never retried:
// the handler will answer the same way next time, and guessing at a
// version it did not claim is how a wire format silently starts
// returning the wrong rows.
var ErrProtocolMismatch = errors.New("drops/cloudflare/d1: wire protocol version mismatch")

// Statement is one SQL statement and its bound parameters.
//
// Parameters are the JSON values [BindParam] produces: null, numbers,
// strings, and an array of byte values for a BLOB. They bind
// positionally to the statement's "?" markers.
type Statement struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

// StatementResult is one statement's outcome.
//
// Columns and Rows are positional and parallel: Rows[i][j] is the
// value of Columns[j] in row i. That is the whole reason the protocol
// does not carry rows as objects — an object has no order, so a
// positional Scan would have to guess which key a destination meant,
// and a join projecting two columns of the same name would lose one.
type StatementResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Meta    Meta     `json:"meta"`
}

// Request is the body a client sends.
//
// Statements is always a list, even for one statement, so a handler
// has one code path. A list of one runs on its own; a list of more
// than one runs as a D1 batch, which is the implicit transaction D1
// offers in place of BEGIN/COMMIT.
type Request struct {
	Protocol   int         `json:"protocol"`
	Statements []Statement `json:"statements"`

	// Session is what D1's withSession() takes: a constraint
	// ("first-primary", "first-unconstrained") for a session's first
	// request, or the bookmark an earlier request in the same
	// session returned. Empty means no session, which is the
	// unsessioned behaviour version 1 had.
	//
	// Setting it raises the request's protocol floor to 2, because a
	// version 1 handler would ignore the field rather than refuse
	// it — and an ignored session is a read served from a replica
	// that the caller asked to be served from the primary, which is
	// precisely the failure a wire version exists to prevent.
	Session string `json:"session,omitempty"`
}

// Response is the body a handler returns.
//
// Exactly one of Results and Error is set. A handler answers HTTP 200
// for any request that reached D1 — including one whose SQL failed,
// which is reported in Error — and a non-2xx status only for failures
// before that point: a malformed body, a missing binding, a rejected
// credential. The split matters because the two need different
// handling: a SQL failure is the caller's to fix and must never be
// retried, while a 5xx may be worth another attempt.
type Response struct {
	Protocol int               `json:"protocol"`
	Results  []StatementResult `json:"results,omitempty"`
	Error    *WireError        `json:"error,omitempty"`

	// Bookmark is where the session stands after this request, as
	// D1's session.getBookmark() reports it. Present only for a
	// request that carried [Request.Session]; empty otherwise.
	Bookmark string `json:"bookmark,omitempty"`
}

// WireError is a statement failure as the handler reports it.
type WireError struct {
	// Message is SQLite's own text, passed through unaltered:
	// "UNIQUE constraint failed: users.email". It is the only
	// classification on the wire, so a handler that rewords it
	// breaks [ClassifyError] on the far side.
	Message string `json:"message"`

	// Code is D1's or SQLite's symbolic code where the runtime
	// exposes one — "SQLITE_CONSTRAINT_UNIQUE", "D1_ERROR". Advisory:
	// the classification is done from Message, because Code is not
	// reliably present.
	Code string `json:"code,omitempty"`

	// Index is the 0-based position of the statement that failed
	// within the request, or -1 when the failure was not
	// attributable to one.
	Index int `json:"index"`
}

// Error implements error.
func (e *WireError) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Code + ": " + e.Message
}

// NewRequest builds an unsessioned protocol request for stmts.
func NewRequest(stmts []Statement) Request {
	return Request{Protocol: MinProtocolVersion, Statements: stmts}
}

// NewSessionRequest builds a request that runs inside a D1 session.
//
// token is a constraint for the session's first request and a
// bookmark for every one after it — the same value D1's withSession()
// takes. An empty token yields an unsessioned request, identical to
// [NewRequest].
func NewSessionRequest(stmts []Statement, token string) Request {
	if token == "" {
		return NewRequest(stmts)
	}
	return Request{Protocol: 2, Statements: stmts, Session: token}
}

// DecodeResponse parses a handler's reply.
//
// Numbers are decoded with [encoding/json.Decoder.UseNumber] so an
// INTEGER primary key past 2^53 keeps its low bits — the one thing
// JSON silently destroys on the way from SQLite to Go, and the reason
// this function exists rather than a bare json.Unmarshal.
func DecodeResponse(raw []byte) (*Response, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("drops/cloudflare/d1: decode response: %w", err)
	}
	// A handler answers with the version it served the request at,
	// which is the version the request asked for. Zero is a handler
	// that omitted the field; it is read as version 1, the only
	// version that existed before the field was load-bearing.
	if resp.Protocol != 0 && (resp.Protocol < MinProtocolVersion || resp.Protocol > ProtocolVersion) {
		return nil, fmt.Errorf("%w: handler answered at protocol %d, this client speaks %d to %d",
			ErrProtocolMismatch, resp.Protocol, MinProtocolVersion, ProtocolVersion)
	}
	return &resp, nil
}
