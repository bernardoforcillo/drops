package pgwire

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Conn is a single connection to a PostgreSQL server. It implements
// drops.Driver, so pg.New(conn) yields a *pg.DB with the whole query,
// introspection and migration surface available.
//
// One connection, not a pool: the CLI runs one statement at a time and
// a migration's transaction has to stay on the socket that opened it.
// The mutex serialises callers rather than making concurrency work —
// two goroutines interleaving on one socket would desynchronise the
// protocol, and a lock is the cheapest way to make that impossible.
type Conn struct {
	mu      sync.Mutex
	conn    net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	scratch []byte

	// params holds the ParameterStatus values the server reported at
	// startup (server_version, TimeZone, ...).
	params map[string]string
	pid    int32
	secret int32

	// txStatus is the last ReadyForQuery indicator: 'I' idle, 'T' in a
	// transaction, 'E' in a failed transaction.
	txStatus byte

	// savepoints counts nested transactions so each gets its own name.
	savepoints int

	// Notice, when set, is called for every NoticeResponse the server
	// sends. A migration that says "table does not exist, skipping" is
	// worth showing; dropping it on the floor is not.
	Notice func(*Error)

	// dead records the failure that desynchronised the protocol. Once
	// set, every later call returns it rather than writing more bytes
	// onto a socket whose state is unknown.
	dead error
}

// Connect dials the server named by dsn and completes startup and
// authentication. dsn may be a postgres:// URL or a keyword string;
// see parseDSN.
func Connect(ctx context.Context, dsn string) (*Conn, error) {
	cfg, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	network, address := cfg.address()
	d := net.Dialer{}
	raw, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", address, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	c := &Conn{
		conn:   raw,
		params: map[string]string{},
	}
	c.r = bufio.NewReader(raw)
	c.w = bufio.NewWriter(raw)

	if err := c.negotiateTLS(ctx, cfg); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := c.startup(cfg); err != nil {
		_ = c.conn.Close()
		return nil, err
	}
	// The dial deadline must not outlive the dial: a migration that
	// takes longer than the connect timeout is not a connect failure.
	_ = c.conn.SetDeadline(time.Time{})
	return c, nil
}

// negotiateTLS performs the SSLRequest handshake when sslmode asks for
// it. The modes are libpq's: disable never tries, prefer tries and
// carries on in plaintext if refused, require insists on encryption
// but not on identity, and verify-ca / verify-full insist on both —
// this client verifies the certificate chain and the host name for
// either of the two, which is stricter than libpq's verify-ca.
func (c *Conn) negotiateTLS(ctx context.Context, cfg *config) error {
	switch cfg.SSLMode {
	case "disable":
		return nil
	case "prefer", "require", "verify-ca", "verify-full":
	default:
		return fmt.Errorf("sslmode=%q is not one of disable, prefer, require, verify-ca, verify-full", cfg.SSLMode)
	}
	if strings.HasPrefix(cfg.Host, "/") {
		// A Unix socket is not reachable off the machine; libpq skips
		// TLS on one too.
		return nil
	}

	var w writeBuf
	w.startUntyped()
	w.int32(sslRequestCode)
	w.finish()
	if err := c.send(&w); err != nil {
		return err
	}
	reply, err := c.r.ReadByte()
	if err != nil {
		return fmt.Errorf("read TLS negotiation reply: %w", err)
	}
	if reply != 'S' {
		if cfg.SSLMode == "prefer" {
			return nil
		}
		return fmt.Errorf("the server refused TLS and sslmode=%s requires it", cfg.SSLMode)
	}
	tlsCfg := &tls.Config{ServerName: cfg.Host}
	if cfg.SSLMode == "prefer" || cfg.SSLMode == "require" {
		// libpq's rule: these modes encrypt but do not authenticate.
		tlsCfg.InsecureSkipVerify = true
	}
	tlsConn := tls.Client(c.conn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	c.conn = tlsConn
	c.r = bufio.NewReader(tlsConn)
	c.w = bufio.NewWriter(tlsConn)
	return nil
}

// startup sends the StartupMessage and runs the authentication
// exchange to ReadyForQuery.
func (c *Conn) startup(cfg *config) error {
	var w writeBuf
	w.startUntyped()
	w.int32(protocolVersion3)
	w.str("user")
	w.str(cfg.User)
	w.str("database")
	w.str(cfg.Database)
	w.str("client_encoding")
	w.str("UTF8")
	for k, v := range cfg.Options {
		w.str(k)
		w.str(v)
	}
	w.byte(0)
	w.finish()
	if err := c.send(&w); err != nil {
		return err
	}

	for {
		typ, body, err := c.recv()
		if err != nil {
			return err
		}
		switch typ {
		case msgAuthentication:
			if err := c.authenticate(cfg, body); err != nil {
				return err
			}
		case msgParameterStatus:
			k, v := body.str(), body.str()
			c.params[k] = v
		case msgBackendKeyData:
			c.pid, c.secret = body.int32(), body.int32()
		case msgNoticeResponse:
			c.notice(parseErrorResponse(body))
		case msgErrorResponse:
			return parseErrorResponse(body)
		case msgReadyForQuery:
			c.txStatus = body.byte()
			return nil
		default:
			return c.fatal(fmt.Errorf("unexpected message %q during startup", typ))
		}
	}
}

// Close terminates the session politely and closes the socket.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	if c.dead == nil {
		var w writeBuf
		w.start(msgTerminate)
		w.finish()
		_ = c.send(&w)
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

// Param returns a ParameterStatus value reported at startup, such as
// "server_version".
func (c *Conn) Param(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.params[name]
}

func (c *Conn) notice(e *Error) {
	if c.Notice != nil {
		c.Notice(e)
	}
}

// fatal marks the connection unusable. A protocol error leaves the
// stream at an unknown offset, so every later call has to fail rather
// than guess where the next message starts.
func (c *Conn) fatal(err error) error {
	if c.dead == nil {
		c.dead = err
	}
	return err
}

// ----------------------------------------------------------------------
// drops.Driver
// ----------------------------------------------------------------------

// Result is the outcome of a statement, carrying the row count
// PostgreSQL reports in its CommandComplete tag.
type Result struct {
	Tag  string
	Rows int64
}

// RowsAffected returns the number of rows the statement touched, or 0
// for statements whose tag carries no count (DDL).
func (r Result) RowsAffected() (int64, error) { return r.Rows, nil }

// Exec runs a statement and discards any rows it returns.
func (c *Conn) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	res, err := c.run(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	return Result{Tag: res.tag, Rows: res.affected}, nil
}

// Query runs a statement and returns its rows. Every value is read in
// the text format and decoded on Scan; see Rows.Scan.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	res, err := c.run(ctx, sql, args)
	if err != nil {
		return nil, err
	}
	return &Rows{fields: res.fields, rows: res.rows}, nil
}

// Begin opens a transaction. A Begin on an already-open transaction
// takes a savepoint instead, so a nested InTx behaves the way its
// caller expects rather than erroring on the second BEGIN.
func (c *Conn) Begin(ctx context.Context) (drops.Tx, error) {
	c.mu.Lock()
	inTx := c.txStatus == 'T' || c.txStatus == 'E'
	c.mu.Unlock()
	if inTx {
		c.mu.Lock()
		c.savepoints++
		name := "drops_sp_" + strconv.Itoa(c.savepoints)
		c.mu.Unlock()
		if _, err := c.Exec(ctx, "SAVEPOINT "+quoteIdent(name)); err != nil {
			return nil, err
		}
		return &tx{c: c, savepoint: name}, nil
	}
	if _, err := c.Exec(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}

// tx is a transaction bound to the connection that opened it. It is
// itself a drops.Driver, so builders work identically inside and out.
type tx struct {
	c         *Conn
	savepoint string
	done      bool
}

func (t *tx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return t.c.Exec(ctx, sql, args...)
}

func (t *tx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return t.c.Query(ctx, sql, args...)
}

func (t *tx) Begin(ctx context.Context) (drops.Tx, error) { return t.c.Begin(ctx) }

func (t *tx) Commit(ctx context.Context) error {
	if t.done {
		return errors.New("drops: transaction already finished")
	}
	t.done = true
	if t.savepoint != "" {
		_, err := t.c.Exec(ctx, "RELEASE SAVEPOINT "+quoteIdent(t.savepoint))
		return err
	}
	_, err := t.c.Exec(ctx, "COMMIT")
	return err
}

func (t *tx) Rollback(ctx context.Context) error {
	if t.done {
		return errors.New("drops: transaction already finished")
	}
	t.done = true
	if t.savepoint != "" {
		_, err := t.c.Exec(ctx, "ROLLBACK TO SAVEPOINT "+quoteIdent(t.savepoint))
		return err
	}
	_, err := t.c.Exec(ctx, "ROLLBACK")
	return err
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// ----------------------------------------------------------------------
// Query execution
// ----------------------------------------------------------------------

// queryResult is everything one statement produced.
type queryResult struct {
	fields   []string
	rows     [][][]byte
	tag      string
	affected int64
}

// run executes sql, choosing the protocol by whether there are
// parameters to bind.
//
// The simple protocol is used for a statement with no arguments
// because it is the only one that accepts several statements in one
// string — which a migration file, applied whole, is. The extended
// protocol is used when there are arguments, because it is the only
// one that has any.
func (c *Conn) run(ctx context.Context, sql string, args []any) (*queryResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead != nil {
		return nil, c.dead
	}
	if c.conn == nil {
		return nil, errors.New("drops: connection is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A cancelled context has to reach a blocking socket read, and the
	// only lever this client has on one is the deadline. A deadline
	// that has already passed makes the read return at once, which is
	// how Ctrl-C during a migration gets the connection torn down —
	// and a torn-down connection is a transaction the server rolls
	// back rather than one nobody is waiting on.
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetDeadline(deadline)
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	if ctx.Done() != nil {
		done := make(chan struct{})
		defer close(done)
		conn := c.conn
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.SetDeadline(time.Now())
			case <-done:
			}
		}()
	}

	if len(args) == 0 {
		if err := c.sendSimpleQuery(sql); err != nil {
			return nil, cancelled(ctx, err)
		}
	} else if err := c.sendExtendedQuery(sql, args); err != nil {
		return nil, cancelled(ctx, err)
	}
	res, err := c.readUntilReady()
	return res, cancelled(ctx, err)
}

// cancelled reports a cancellation as one.
//
// The only lever this client has on a blocking socket read is the
// deadline, so a cancelled query fails as "i/o timeout" — which sends
// the operator who just pressed Ctrl-C to look at their network. The
// context is the authority on why the read was cut short, and the
// caller can still reach the socket error underneath.
func cancelled(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%w: %v", ctxErr, err)
	}
	return err
}

func (c *Conn) sendSimpleQuery(sql string) error {
	var w writeBuf
	w.start(msgQuery)
	w.str(sql)
	w.finish()
	return c.send(&w)
}

func (c *Conn) sendExtendedQuery(sql string, args []any) error {
	params := make([][]byte, len(args))
	for i, a := range args {
		enc, err := encodeParam(a)
		if err != nil {
			return fmt.Errorf("parameter $%d: %w", i+1, err)
		}
		params[i] = enc
	}

	var w writeBuf
	w.start(msgParse)
	w.str("") // unnamed statement
	w.str(sql)
	w.int16(0) // let the server infer every parameter type
	w.finish()

	w.start(msgBind)
	w.str("") // unnamed portal
	w.str("") // unnamed statement
	w.int16(0)
	w.int16(int16(len(params)))
	for _, p := range params {
		if p == nil {
			w.int32(-1)
			continue
		}
		w.int32(int32(len(p)))
		w.bytes(p)
	}
	w.int16(0) // every result column in the text format
	w.finish()

	w.start(msgDescribe)
	w.byte('P')
	w.str("")
	w.finish()

	w.start(msgExecute)
	w.str("")
	w.int32(0) // no row limit
	w.finish()

	w.start(msgSync)
	w.finish()
	return c.send(&w)
}

// readUntilReady drains the response to ReadyForQuery, which the
// server always sends — including after an error. Reading to it is
// what leaves the connection usable for the next statement.
func (c *Conn) readUntilReady() (*queryResult, error) {
	res := &queryResult{}
	var firstErr error
	for {
		typ, body, err := c.recv()
		if err != nil {
			return nil, err
		}
		switch typ {
		case msgRowDescription:
			n := int(body.int16())
			res.fields = make([]string, 0, n)
			for i := 0; i < n; i++ {
				res.fields = append(res.fields, body.str())
				body.int32() // table OID
				body.int16() // column attribute number
				body.int32() // type OID
				body.int16() // type length
				body.int32() // type modifier
				body.int16() // format code
			}
		case msgDataRow:
			n := int(body.int16())
			row := make([][]byte, n)
			for i := 0; i < n; i++ {
				size := int(body.int32())
				if size < 0 {
					row[i] = nil
					continue
				}
				row[i] = body.next(size)
			}
			res.rows = append(res.rows, row)
		case msgCommandComplete:
			res.tag = body.str()
			res.affected = rowsFromTag(res.tag)
		case msgErrorResponse:
			if e := parseErrorResponse(body); firstErr == nil {
				firstErr = e
			}
		case msgNoticeResponse:
			c.notice(parseErrorResponse(body))
		case msgParameterStatus:
			k, v := body.str(), body.str()
			c.params[k] = v
		case msgNotification:
			body.rest() // no LISTEN support; drain and move on
		case msgReadyForQuery:
			c.txStatus = body.byte()
			if body.err != nil {
				return nil, c.fatal(body.err)
			}
			if firstErr != nil {
				return nil, firstErr
			}
			return res, nil
		case msgParseComplete, msgBindComplete, msgCloseComplete,
			msgNoData, msgEmptyQuery, msgParameterDesc, msgPortalSuspended:
			body.rest()
		case msgCopyInResponse, msgCopyOutResponse, msgCopyBothResponse:
			return nil, c.fatal(fmt.Errorf("COPY is not supported by this client"))
		default:
			return nil, c.fatal(fmt.Errorf("unexpected message %q from the server", typ))
		}
		if body.err != nil {
			return nil, c.fatal(body.err)
		}
	}
}

// rowsFromTag reads the count off a CommandComplete tag: "INSERT 0 3"
// and "UPDATE 3" both mean three rows; "CREATE TABLE" means none.
func rowsFromTag(tag string) int64 {
	i := strings.LastIndexByte(tag, ' ')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(tag[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
