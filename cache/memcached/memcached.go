// Package memcached is a zero-dependency cache.Cache backend for
// Memcached. It speaks the classic ASCII protocol (get / set / delete,
// with native multi-key get) over a small bounded connection pool, and
// uses the meta-get command (mg, Memcached 1.6+) to report remaining TTL.
//
// Like drops/cache/redis it imports only the standard library: the wire
// protocol and pool are implemented here rather than pulled from a
// third-party client.
//
//	c, _ := memcached.New(memcached.Options{Addr: "127.0.0.1:11211"})
//	defer c.Close()
//	_ = c.Set(ctx, "k", []byte("v"), time.Minute)
//	v, _ := c.Get(ctx, "k")
package memcached

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
)

// Options configures a Memcached cache. Zero-valued fields take the
// defaults documented below.
type Options struct {
	// Addr is the host:port of the server. Default "127.0.0.1:11211".
	Addr string

	// MaxConns caps the simultaneous connections, idle and checked out
	// alike. Callers beyond the cap wait for one to come free, or for
	// their context to end. Default 10.
	MaxConns int

	// DialTimeout caps a single TCP dial. Default 5s.
	DialTimeout time.Duration

	// ReadTimeout / WriteTimeout cap per-command I/O when the caller's
	// context has no deadline. Default 3s each. Negative disables.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// KeyPrefix is prepended to every key. Useful for namespacing a
	// shared server.
	KeyPrefix string

	// Hook fires after every operation with kind = "cache.get",
	// "cache.set", "cache.del", "cache.exists", "cache.ttl",
	// "cache.ping", "cache.mget", "cache.mset".
	Hook drops.Hook

	// Dialer overrides how new connections are made (for tests / custom
	// transports). Default net.Dialer.
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Cache is the Memcached implementation of cache.Cache. It also satisfies
// cache.MultiCache (native multi-key get; SetMulti is a per-item loop, as
// Memcached has no atomic multi-set).
type Cache struct {
	opts Options

	// sem holds one slot per allowed connection. Taking a slot is what
	// bounds the sockets open at once, idle or checked out alike.
	sem chan struct{}

	mu     sync.Mutex
	idle   []*conn
	closed bool
}

type conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
}

// New constructs a Memcached cache. It does not dial eagerly; the first
// operation opens a connection. An error is returned only for an invalid
// configuration (there is none today, so it is always nil — the signature
// mirrors redis.New for symmetry).
func New(opts Options) (*Cache, error) {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:11211"
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = 10
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 5 * time.Second
	}
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 3 * time.Second
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 3 * time.Second
	}
	return &Cache{opts: opts, sem: make(chan struct{}, opts.MaxConns)}, nil
}

// Compile-time interface conformance.
var (
	_ cache.Cache      = (*Cache)(nil)
	_ cache.MultiCache = (*Cache)(nil)
)

// Get returns the value for key, or cache.ErrNotFound.
func (c *Cache) Get(ctx context.Context, key string) (_ []byte, err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.get", start, &err)

	if err = c.guard(key); err != nil {
		return nil, err
	}
	m, err := c.getMulti(ctx, []string{key})
	if err != nil {
		return nil, err
	}
	v, ok := m[c.prefixed(key)]
	if !ok {
		return nil, cache.ErrNotFound
	}
	return v, nil
}

// Set stores value under key. ttl=0 means no expiry.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) (err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.set", start, &err)

	if err = c.guard(key); err != nil {
		return err
	}
	return c.withConn(ctx, func(cn *conn) error {
		header := fmt.Sprintf("set %s 0 %d %d\r\n", c.prefixed(key), exptime(ttl), len(value))
		if _, err := cn.w.WriteString(header); err != nil {
			return err
		}
		if _, err := cn.w.Write(value); err != nil {
			return err
		}
		if _, err := cn.w.WriteString("\r\n"); err != nil {
			return err
		}
		if err := cn.w.Flush(); err != nil {
			return err
		}
		line, err := cn.readLine()
		if err != nil {
			return err
		}
		if line != "STORED" {
			return fmt.Errorf("memcached: set failed: %s", line)
		}
		return nil
	})
}

// Delete removes the listed keys, returning the count that existed.
func (c *Cache) Delete(ctx context.Context, keys ...string) (n int, err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.del", start, &err)

	if err = c.notClosed(); err != nil {
		return 0, err
	}
	for _, k := range keys {
		if err := validateKey(k); err != nil {
			return n, err
		}
	}
	err = c.withConn(ctx, func(cn *conn) error {
		for _, k := range keys {
			if _, err := cn.w.WriteString("delete " + c.prefixed(k) + "\r\n"); err != nil {
				return err
			}
			if err := cn.w.Flush(); err != nil {
				return err
			}
			line, err := cn.readLine()
			if err != nil {
				return err
			}
			switch line {
			case "DELETED":
				n++
			case "NOT_FOUND":
				// not counted
			default:
				return fmt.Errorf("memcached: delete failed: %s", line)
			}
		}
		return nil
	})
	return n, err
}

// Exists reports whether the key has a live entry.
func (c *Cache) Exists(ctx context.Context, key string) (_ bool, err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.exists", start, &err)

	if err = c.guard(key); err != nil {
		return false, err
	}
	m, err := c.getMulti(ctx, []string{key})
	if err != nil {
		return false, err
	}
	_, ok := m[c.prefixed(key)]
	return ok, nil
}

// TTL returns the remaining lifetime via the meta-get command (Memcached
// 1.6+): -1 for "no expiry", or cache.ErrNotFound for an absent key.
func (c *Cache) TTL(ctx context.Context, key string) (_ time.Duration, err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.ttl", start, &err)

	if err = c.guard(key); err != nil {
		return 0, err
	}
	var out time.Duration
	err = c.withConn(ctx, func(cn *conn) error {
		if _, err := cn.w.WriteString("mg " + c.prefixed(key) + " t\r\n"); err != nil {
			return err
		}
		if err := cn.w.Flush(); err != nil {
			return err
		}
		line, err := cn.readLine()
		if err != nil {
			return err
		}
		switch {
		case line == "EN" || strings.HasPrefix(line, "EN "):
			return cache.ErrNotFound
		case strings.HasPrefix(line, "HD"):
			secs, ok := metaFlagInt(line, 't')
			if !ok {
				return fmt.Errorf("memcached: mg missing t flag: %s", line)
			}
			if secs < 0 {
				out = -1 // never expires
				return nil
			}
			out = time.Duration(secs) * time.Second
			return nil
		default:
			return fmt.Errorf("memcached: mg failed: %s", line)
		}
	})
	if errors.Is(err, cache.ErrNotFound) {
		return 0, cache.ErrNotFound
	}
	return out, err
}

// Ping verifies the server is reachable via the version command.
func (c *Cache) Ping(ctx context.Context) (err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.ping", start, &err)

	if err = c.notClosed(); err != nil {
		return err
	}
	return c.withConn(ctx, func(cn *conn) error {
		if _, err := cn.w.WriteString("version\r\n"); err != nil {
			return err
		}
		if err := cn.w.Flush(); err != nil {
			return err
		}
		line, err := cn.readLine()
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, "VERSION") {
			return fmt.Errorf("memcached: unexpected version reply: %s", line)
		}
		return nil
	})
}

// GetMulti fetches many keys in a single get command. Missing keys are
// absent from the result. Keys in the returned map carry the KeyPrefix
// (they are the wire keys), consistent with how a batched get echoes them.
func (c *Cache) GetMulti(ctx context.Context, keys ...string) (_ map[string][]byte, err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.mget", start, &err)

	if err = c.notClosed(); err != nil {
		return nil, err
	}
	for _, k := range keys {
		if err := validateKey(k); err != nil {
			return nil, err
		}
	}
	return c.getMulti(ctx, keys)
}

// SetMulti stores each item with the same TTL. Memcached has no atomic
// multi-set, so this pipelines individual set commands on one connection.
func (c *Cache) SetMulti(ctx context.Context, items map[string][]byte, ttl time.Duration) (err error) {
	start := time.Now()
	defer c.emit(ctx, "cache.mset", start, &err)

	if err = c.notClosed(); err != nil {
		return err
	}
	for k := range items {
		if err := validateKey(k); err != nil {
			return err
		}
	}
	return c.withConn(ctx, func(cn *conn) error {
		for k, v := range items {
			header := fmt.Sprintf("set %s 0 %d %d\r\n", c.prefixed(k), exptime(ttl), len(v))
			if _, err := cn.w.WriteString(header); err != nil {
				return err
			}
			if _, err := cn.w.Write(v); err != nil {
				return err
			}
			if _, err := cn.w.WriteString("\r\n"); err != nil {
				return err
			}
			if err := cn.w.Flush(); err != nil {
				return err
			}
			line, err := cn.readLine()
			if err != nil {
				return err
			}
			if line != "STORED" {
				return fmt.Errorf("memcached: set %q failed: %s", k, line)
			}
		}
		return nil
	})
}

// Close closes every pooled connection and rejects further use.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	for _, cn := range c.idle {
		_ = cn.nc.Close()
	}
	c.idle = nil
	return nil
}

// --- get pipeline -----------------------------------------------------

// getMulti runs one `get k1 k2 ...` and parses the VALUE/END stream. The
// returned map is keyed by wire key (prefix included).
func (c *Cache) getMulti(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(keys) == 0 {
		return out, nil
	}
	err := c.withConn(ctx, func(cn *conn) error {
		var b strings.Builder
		b.WriteString("get")
		for _, k := range keys {
			b.WriteByte(' ')
			b.WriteString(c.prefixed(k))
		}
		b.WriteString("\r\n")
		if _, err := cn.w.WriteString(b.String()); err != nil {
			return err
		}
		if err := cn.w.Flush(); err != nil {
			return err
		}
		for {
			line, err := cn.readLine()
			if err != nil {
				return err
			}
			if line == "END" {
				return nil
			}
			if !strings.HasPrefix(line, "VALUE ") {
				return fmt.Errorf("memcached: unexpected get reply: %s", line)
			}
			// VALUE <key> <flags> <bytes>
			parts := strings.Fields(line)
			if len(parts) < 4 {
				return fmt.Errorf("memcached: malformed VALUE: %s", line)
			}
			n, err := strconv.Atoi(parts[3])
			if err != nil {
				return fmt.Errorf("memcached: bad byte count: %s", line)
			}
			data := make([]byte, n+2) // include trailing \r\n
			if err := cn.readFull(data); err != nil {
				return err
			}
			out[parts[1]] = data[:n]
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- connection pool --------------------------------------------------

func (c *Cache) withConn(ctx context.Context, fn func(*conn) error) error {
	cn, err := c.acquire(ctx)
	if err != nil {
		return err
	}
	c.applyDeadline(ctx, cn)
	if err := fn(cn); err != nil {
		// A protocol/transport error taints the connection — drop it.
		_ = cn.nc.Close()
		c.release(nil)
		return err
	}
	_ = cn.nc.SetDeadline(time.Time{})
	c.release(cn)
	return nil
}

// acquire checks out a connection, waiting for a free slot when every
// allowed connection is busy. Waiting is the point: dialling regardless
// lets a burst of goroutines open a socket each and run the process out
// of file descriptors, which is what MaxConns exists to prevent.
func (c *Cache) acquire(ctx context.Context) (*conn, error) {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.sem
		return nil, cache.ErrClosed
	}
	if n := len(c.idle); n > 0 {
		cn := c.idle[n-1]
		c.idle = c.idle[:n-1]
		c.mu.Unlock()
		return cn, nil
	}
	c.mu.Unlock()

	cn, err := c.dial(ctx)
	if err != nil {
		<-c.sem
		return nil, err
	}
	return cn, nil
}

// release returns a healthy connection to the pool, or accounts for a
// dropped one when cn is nil, and frees the slot either way.
func (c *Cache) release(cn *conn) {
	defer func() { <-c.sem }()
	if cn == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = cn.nc.Close()
		return
	}
	c.idle = append(c.idle, cn)
}

func (c *Cache) dial(ctx context.Context) (*conn, error) {
	dialer := c.opts.Dialer
	if dialer == nil {
		d := &net.Dialer{Timeout: c.opts.DialTimeout}
		dialer = d.DialContext
	}
	dctx := ctx
	if c.opts.DialTimeout > 0 {
		var cancel context.CancelFunc
		dctx, cancel = context.WithTimeout(ctx, c.opts.DialTimeout)
		defer cancel()
	}
	nc, err := dialer(dctx, "tcp", c.opts.Addr)
	if err != nil {
		return nil, err
	}
	return &conn{nc: nc, r: bufio.NewReader(nc), w: bufio.NewWriter(nc)}, nil
}

func (c *Cache) applyDeadline(ctx context.Context, cn *conn) {
	if dl, ok := ctx.Deadline(); ok {
		_ = cn.nc.SetDeadline(dl)
		return
	}
	// Fall back to the configured per-op timeouts.
	to := c.opts.ReadTimeout
	if c.opts.WriteTimeout > to {
		to = c.opts.WriteTimeout
	}
	if to > 0 {
		_ = cn.nc.SetDeadline(time.Now().Add(to))
	}
}

// --- conn I/O helpers -------------------------------------------------

// readLine reads one CRLF-terminated line, returning it without the CRLF.
func (cn *conn) readLine() (string, error) {
	line, err := cn.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readFull reads exactly len(p) bytes.
func (cn *conn) readFull(p []byte) error {
	got := 0
	for got < len(p) {
		n, err := cn.r.Read(p[got:])
		if n > 0 {
			got += n
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ----------------------------------------------------------

func (c *Cache) guard(key string) error {
	if err := c.notClosed(); err != nil {
		return err
	}
	return validateKey(key)
}

func (c *Cache) notClosed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return cache.ErrClosed
	}
	return nil
}

func (c *Cache) prefixed(key string) string {
	if c.opts.KeyPrefix == "" {
		return key
	}
	return c.opts.KeyPrefix + key
}

func (c *Cache) emit(ctx context.Context, kind string, start time.Time, errp *error) {
	drops.CallHook(c.opts.Hook, ctx, drops.QueryEvent{
		Kind:     kind,
		Duration: time.Since(start),
		Err:      *errp,
	})
}

// validateKey enforces Memcached's key rules: non-empty, ≤ 250 bytes, no
// spaces or control characters (which would corrupt the text protocol).
// The prefix is validated implicitly by callers building the full key.
func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: empty", cache.ErrInvalidKey)
	}
	if len(key) > 250 {
		return fmt.Errorf("%w: longer than 250 bytes", cache.ErrInvalidKey)
	}
	for i := 0; i < len(key); i++ {
		if key[i] <= ' ' || key[i] == 0x7f {
			return fmt.Errorf("%w: contains space or control byte", cache.ErrInvalidKey)
		}
	}
	return nil
}

// maxRelativeExptime is Memcached's cutoff between the two readings of
// the exptime field: 30 days in seconds.
const maxRelativeExptime = 60 * 60 * 24 * 30

// exptime converts a TTL to Memcached's exptime field. 0 (or negative)
// means "no expiry"; a positive sub-second TTL rounds up to 1 second so
// it does not accidentally become "never expire".
//
// Anything above 30 days is sent as an absolute unix timestamp, which is
// how the server reads such a value anyway. Sent as a relative count it
// would be read as a timestamp somewhere in 1970 and the entry would
// expire on arrival — a silent, total cache miss for exactly the
// long-lived entries the caller asked to keep.
func exptime(ttl time.Duration) int {
	if ttl <= 0 {
		return 0
	}
	secs := int64(ttl / time.Second)
	switch {
	case secs == 0:
		return 1
	case secs > maxRelativeExptime:
		return int(time.Now().Add(ttl).Unix())
	}
	return int(secs)
}

// metaFlagInt extracts the integer value of a single-letter meta flag from
// a meta response line such as "HD t120".
func metaFlagInt(line string, flag byte) (int, bool) {
	for _, tok := range strings.Fields(line) {
		if len(tok) >= 1 && tok[0] == flag {
			v, err := strconv.Atoi(tok[1:])
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}
