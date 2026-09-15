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
const ProtocolVersion = 1

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

// NewRequest builds a protocol request for stmts.
func NewRequest(stmts []Statement) Request {
	return Request{Protocol: ProtocolVersion, Statements: stmts}
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
	if resp.Protocol != 0 && resp.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("%w: handler speaks protocol %d, this client speaks %d",
			ErrProtocolMismatch, resp.Protocol, ProtocolVersion)
	}
	return &resp, nil
}
