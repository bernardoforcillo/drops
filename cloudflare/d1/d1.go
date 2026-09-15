package d1

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cloudflare"
)

// Sentinel errors.
var (
	// ErrNoDatabaseID is returned by New when the database ID is
	// empty.
	ErrNoDatabaseID = errors.New("drops/cloudflare/d1: database ID is empty")

	// ErrTxQuery is returned by Query inside a transaction. D1's
	// HTTP API buffers a transaction's statements until commit, so
	// there is nothing to read yet — see the package comment.
	ErrTxQuery = errors.New("drops/cloudflare/d1: cannot Query inside a transaction; D1 has no interactive transactions, so a transaction's statements do not run until Commit")

	// ErrPending is returned by a buffered statement's
	// [Result.RowsAffected] before the transaction commits.
	ErrPending = errors.New("drops/cloudflare/d1: statement has not run yet; its result is known after Commit")

	// ErrTxDone is returned by a transaction already committed or
	// rolled back.
	ErrTxDone = errors.New("drops/cloudflare/d1: transaction is already finished")

	// ErrNestedTx is returned by Begin on a transaction. D1 has no
	// SAVEPOINT over HTTP, so there is nothing to nest with.
	ErrNestedTx = errors.New("drops/cloudflare/d1: nested transactions are not supported")

	// ErrNoStatements is returned by a Batch with nothing in it.
	ErrNoStatements = errors.New("drops/cloudflare/d1: batch has no statements")

	// ErrUnsupportedParam is returned when a bound argument has a
	// type D1's JSON body cannot carry. See [BindParam].
	ErrUnsupportedParam = errors.New("drops/cloudflare/d1: unsupported parameter type")

	// ErrTooManyParams is returned when a statement carries more
	// bound parameters than D1 accepts. See [WithMaxBoundParams].
	ErrTooManyParams = errors.New("drops/cloudflare/d1: too many bound parameters")

	// ErrStatementTooLarge is returned when the rendered SQL exceeds
	// D1's per-statement size limit. See [WithMaxStatementBytes].
	ErrStatementTooLarge = errors.New("drops/cloudflare/d1: statement is too large")

	// ErrBatchTooLarge is returned when a batch carries more
	// statements than D1 will run in one request.
	ErrBatchTooLarge = errors.New("drops/cloudflare/d1: too many statements in one request")
)

// Driver runs statements against one D1 database, implementing
// [github.com/bernardoforcillo/drops.Driver].
//
// Safe for concurrent use by multiple goroutines. There is no pool to
// size and no connection to keep warm: every statement is an HTTPS
// request, and the http.Client on the underlying
// [github.com/bernardoforcillo/drops/cloudflare.Client] is what holds
// the keep-alive connections.
type Driver struct {
	tr         Transport
	maxParams  int
	maxBytes   int
	bridgeOpts []BridgeOption
}

// Option configures a [Driver].
type Option func(*Driver)

// WithBridgeOptions passes [BridgeOption] values through [NewBridge].
// Ignored by [New], which builds no bridge.
func WithBridgeOptions(opts ...BridgeOption) Option {
	return func(d *Driver) { d.bridgeOpts = append(d.bridgeOpts, opts...) }
}

// WithMaxStatementBytes sets the size a rendered statement may reach
// before the driver refuses it locally. Defaults to
// [Published].StatementSize; pass 0 to disable the check.
func WithMaxStatementBytes(n int) Option {
	return func(d *Driver) { d.maxBytes = n }
}

// WithMaxBoundParams sets the number of bound parameters a statement
// may carry before the driver refuses it locally, without a round
// trip. Defaults to [Published].BoundParams; pass 0 to disable the
// check.
//
// The check earns its place on the error message. Sending 250
// parameters to D1 gets back a service error that does not say what
// the limit is or which statement met it; refusing locally can say
// both, and can say what to do about it. The knob exists because the
// limit is Cloudflare's to raise, and a client that hard-coded it
// would keep refusing work D1 had started accepting.
func WithMaxBoundParams(n int) Option {
	return func(d *Driver) { d.maxParams = n }
}

// New returns a Driver for the D1 database with the given ID,
// reaching it over Cloudflare's public REST API.
//
// The ID is the UUID `wrangler d1 info <name>` prints, not the
// database's name.
//
//	drv := d1.New(cf, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
//	db := sqlite.New(drv)
//
// Use it from outside Cloudflare's network — a migration runner, a CI
// job, a laptop. From inside it, where a service binding exists,
// [NewBridge] is faster, cheaper and needs no API token.
func New(cf *cloudflare.Client, databaseID string, opts ...Option) *Driver {
	return NewWithTransport(NewRESTTransport(cf, databaseID), opts...)
}

// NewBridge returns a Driver that reaches D1 through a Worker of
// yours rather than through Cloudflare's public API.
//
//	drv, err := d1.NewBridge("http://d1.internal")
//	db := sqlite.New(drv)
//
// The Worker holds the D1 binding and speaks the wire protocol in
// protocol.go; worker/handler.js is the reference implementation to
// mount inside it. See [BridgeTransport].
func NewBridge(baseURL string, opts ...Option) (*Driver, error) {
	var bridgeOpts []BridgeOption
	d := &Driver{maxParams: Published.BoundParams, maxBytes: Published.StatementSize}
	for _, o := range opts {
		o(d)
	}
	bridgeOpts = append(bridgeOpts, d.bridgeOpts...)
	tr, err := NewBridgeTransport(baseURL, bridgeOpts...)
	if err != nil {
		return nil, err
	}
	d.tr = tr
	return d, nil
}

// NewWithTransport returns a Driver over any [Transport] — the
// constructor for a transport that is neither of the two built in.
func NewWithTransport(tr Transport, opts ...Option) *Driver {
	d := &Driver{tr: tr, maxParams: Published.BoundParams, maxBytes: Published.StatementSize}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Transport returns how this driver reaches D1.
func (d *Driver) Transport() Transport { return d.tr }

// String names the target, for logs and error messages.
func (d *Driver) String() string {
	if d.tr == nil {
		return "d1 driver: no transport"
	}
	return d.tr.Target()
}

// Close implements the closer
// [github.com/bernardoforcillo/drops/sqlite.DB.Close] looks for. It is
// a no-op: there is no connection to release, and the http.Client is
// the caller's to close if they made it.
func (d *Driver) Close() error { return nil }

// Ping verifies the database is reachable and the token can read it.
func (d *Driver) Ping(ctx context.Context) error {
	_, err := d.Exec(ctx, "SELECT 1")
	return err
}

// Exec runs a statement that returns no rows.
//
// The returned [drops.Result] is a *[Result], so the full [Meta] —
// RowsRead, LastRowID, the size of the database afterwards — is one
// type assertion away.
func (d *Driver) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	params, err := BindParams(args)
	if err != nil {
		return nil, err
	}
	out, err := d.send(ctx, []Statement{{SQL: sql, Params: params}})
	if err != nil {
		return nil, err
	}
	return &Result{meta: out[len(out)-1].Meta}, nil
}

// Query runs a statement that returns rows.
//
// The whole result set arrives with the response, so the returned
// [Rows] never blocks and never fails mid-iteration. A statement
// whose answer would exceed [Limits.QueryResponseSize] fails here
// rather than part-way through.
func (d *Driver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	params, err := BindParams(args)
	if err != nil {
		return nil, err
	}
	out, err := d.send(ctx, []Statement{{SQL: sql, Params: params}})
	if err != nil {
		return nil, err
	}
	last := out[len(out)-1]
	return &Rows{columns: last.Columns, rows: last.Rows, meta: last.Meta}, nil
}

// Begin opens a deferred transaction.
//
// Read the package comment before using it: D1 has no interactive
// transactions, so the returned [Tx] buffers writes and sends them as
// one batch at Commit. Query inside it returns [ErrTxQuery], and a
// buffered statement's row count is [ErrPending] until the commit.
func (d *Driver) Begin(ctx context.Context) (drops.Tx, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &Tx{on: d}, nil
}

// send issues one D1 request carrying stmts and returns one result
// per statement.
func (d *Driver) send(ctx context.Context, stmts []Statement) ([]StatementResult, error) {
	if len(stmts) == 0 {
		return nil, ErrNoStatements
	}
	if d.tr == nil {
		return nil, errors.New("drops/cloudflare/d1: driver has no transport")
	}
	if err := d.checkLimits(stmts); err != nil {
		return nil, err
	}
	return d.tr.Send(ctx, stmts, readOnly(stmts))
}

// checkLimits refuses a request D1 would reject, locally and without
// a round trip.
//
// The check earns its place on the error message. D1's answer to 250
// bound parameters does not say what the limit is, which statement
// met it, or what to do instead; this one says all three. The knobs
// exist because the limits are Cloudflare's to raise, and a client
// that hard-coded them would keep refusing work D1 had started
// accepting.
func (d *Driver) checkLimits(stmts []Statement) error {
	var params, size int
	for _, s := range stmts {
		params += len(s.Params)
		size += len(s.SQL)
	}
	if d.maxParams > 0 && params > d.maxParams {
		return fmt.Errorf(
			"%w: %d bound parameters exceeds D1's limit of %d — pass the values as one JSON parameter and match with `IN (SELECT value FROM json_each(?))`, or chunk the statement (d1.WithMaxBoundParams changes or disables this check)",
			ErrTooManyParams, params, d.maxParams)
	}
	if d.maxBytes > 0 && size > d.maxBytes {
		return fmt.Errorf(
			"%w: %d bytes of SQL exceeds D1's limit of %d — split the batch, or move the literal values into bound parameters (d1.WithMaxStatementBytes changes or disables this check)",
			ErrStatementTooLarge, size, d.maxBytes)
	}
	if len(stmts) > Published.BatchStatements {
		return fmt.Errorf(
			"%w: %d statements exceeds D1's limit of %d per request — split the batch, which costs its atomicity, or reduce it",
			ErrBatchTooLarge, len(stmts), Published.BatchStatements)
	}
	return nil
}

// joinStatements renders a request body. D1's HTTP API takes one sql
// field, so several statements travel as one script with their
// parameters flattened in order — which is exactly how SQLite binds
// them, positionally, left to right across the whole script.
func joinStatements(stmts []Statement) Statement {
	if len(stmts) == 1 {
		return stmts[0]
	}
	var sb strings.Builder
	params := make([]any, 0, len(stmts)*2)
	for i, s := range stmts {
		if i > 0 {
			sb.WriteString(";\n")
		}
		sb.WriteString(strings.TrimRight(strings.TrimSpace(s.SQL), ";"))
		params = append(params, s.Params...)
	}
	return Statement{SQL: sb.String(), Params: params}
}

// readOnlyPrefixes are the statement keywords that cannot write. A
// statement starting with anything else is treated as a write, which
// is the safe direction to be wrong in: the cost of a missed retry is
// one failed request, the cost of a wrong one is a duplicated row.
var readOnlyPrefixes = []string{"SELECT", "WITH", "PRAGMA", "EXPLAIN"}

// readOnly reports whether every statement is a read.
//
// WITH is on the list and is the one that needs a word: a CTE can
// carry an INSERT in SQLite, so `WITH x AS (…) INSERT …` would be
// mis-read as safe. The check looks at the whole statement for a
// writing keyword rather than at the first word alone, so it is not.
func readOnly(stmts []Statement) bool {
	for _, s := range stmts {
		u := strings.ToUpper(s.SQL)
		trimmed := strings.TrimLeft(u, " \t\r\n(")
		var prefixed bool
		for _, p := range readOnlyPrefixes {
			if strings.HasPrefix(trimmed, p) {
				prefixed = true
				break
			}
		}
		if !prefixed {
			return false
		}
		for _, w := range writingKeywords {
			if hasKeyword(u, w) {
				return false
			}
		}
	}
	return true
}

// BindParams converts bound arguments into the JSON values D1's
// params array can carry. See [BindParam].
func BindParams(args []any) ([]any, error) {
	if len(args) == 0 {
		return nil, nil
	}
	out := make([]any, len(args))
	for i, a := range args {
		v, err := BindParam(a)
		if err != nil {
			return nil, fmt.Errorf("drops/cloudflare/d1: parameter %d: %w", i+1, err)
		}
		out[i] = v
	}
	return out, nil
}

// BindParam converts one bound argument into a JSON value.
//
// D1's params array carries null, numbers, strings and — for a BLOB —
// an array with one number per byte, which is the same shape D1
// answers a BLOB column in. The conversions:
//
//	nil                → null
//	bool               → 1 / 0, because SQLite has no boolean
//	int… / uint… /
//	float… /
//	json.Number        → number
//	string             → string
//	[]byte             → array of byte values (a BLOB)
//	time.Time          → RFC 3339 with nanoseconds, in UTC
//	driver.Valuer      → the above, applied to Value()
//
// Anything else is [ErrUnsupportedParam] rather than a guess. A
// struct silently JSON-encoded into a TEXT column is the kind of
// thing that is discovered months later; if you want JSON in a
// column, encode it at the call site — which is what
// [github.com/bernardoforcillo/drops/sqlite]'s JSON columns already
// expect, since SQLite stores JSON as TEXT.
func BindParam(v any) (any, error) {
	switch a := v.(type) {
	case nil:
		return nil, nil
	case bool:
		if a {
			return 1, nil
		}
		return 0, nil
	case string:
		return a, nil
	case []byte:
		// Distinguish a nil slice (SQL NULL) from an empty one (a
		// zero-length blob): they are different values in SQLite.
		if a == nil {
			return nil, nil
		}
		out := make([]any, len(a))
		for i, b := range a {
			out[i] = int(b)
		}
		return out, nil
	case time.Time:
		return a.UTC().Format(time.RFC3339Nano), nil
	case json.Number:
		return a, nil
	case int:
		return a, nil
	case int8:
		return a, nil
	case int16:
		return a, nil
	case int32:
		return a, nil
	case int64:
		return a, nil
	case uint:
		return a, nil
	case uint8:
		return a, nil
	case uint16:
		return a, nil
	case uint32:
		return a, nil
	case uint64:
		return a, nil
	case float32:
		return a, nil
	case float64:
		return a, nil
	case driver.Valuer:
		got, err := a.Value()
		if err != nil {
			return nil, err
		}
		if _, isValuer := got.(driver.Valuer); isValuer {
			return nil, fmt.Errorf("%w: %T.Value() returned another Valuer", ErrUnsupportedParam, v)
		}
		return BindParam(got)
	default:
		return nil, fmt.Errorf("%w: %T", ErrUnsupportedParam, v)
	}
}

// writingKeywords are the ones whose presence anywhere in a statement
// makes it a write — including inside a CTE, which is the case the
// prefix check alone would miss.
var writingKeywords = []string{"INSERT", "UPDATE", "DELETE", "REPLACE", "CREATE", "DROP", "ALTER", "VACUUM", "ATTACH"}

// hasKeyword reports whether upper contains word as a whole
// identifier rather than as part of a longer one — so a SELECT of
// created_at is not read as a CREATE.
//
// It still says "write" for a keyword inside a string literal
// ('deleted'), and that is the direction to be wrong in: the cost is
// a retry not taken, where the opposite costs a row written twice.
func hasKeyword(upper, word string) bool {
	for i := 0; ; {
		j := strings.Index(upper[i:], word)
		if j < 0 {
			return false
		}
		start := i + j
		end := start + len(word)
		if !identByte(upper, start-1) && !identByte(upper, end) {
			return true
		}
		i = start + 1
	}
}

// identByte reports whether the byte at i is part of an identifier.
// Out-of-range indices are not, which makes the boundary checks above
// read without a length guard at each end.
func identByte(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}
