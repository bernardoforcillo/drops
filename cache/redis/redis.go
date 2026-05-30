package redis

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
)

// Options configures a Redis cache.
//
// Zero-valued numeric fields take sensible production defaults (see
// New); explicitly set them when the defaults don't fit.
type Options struct {
	// Addr is the host:port of the server. Default "127.0.0.1:6379".
	Addr string

	// Credentials is the primary auth path. If non-nil it overrides
	// Username / Password and is called once per new connection,
	// receiving the caller's context. Use it for short-lived tokens
	// (IAM / OIDC / Vault leases) that need refresh.
	Credentials CredentialsProvider

	// Password is the static-credentials shorthand. Used only when
	// Credentials is nil. Empty means no AUTH.
	Password string

	// Username pairs with Password as the static-credentials
	// shorthand (Redis 6+ ACL). Used only when Credentials is nil.
	// Empty falls back to the legacy single-arg AUTH form.
	Username string

	// TLS, if non-nil, enables TLS for new connections. Ignored when
	// Dialer is set (the custom Dialer is responsible for its own
	// transport encryption).
	TLS *tls.Config

	// DB is the database index to SELECT on connect (0..15 typically).
	DB int

	// ClientName is sent via CLIENT SETNAME on connect so the
	// connection shows up identifiable in CLIENT LIST / SLOWLOG /
	// MONITOR. Default "drops".
	ClientName string

	// MaxConns caps the simultaneous connections. Default 10.
	MaxConns int

	// MinIdleConns pre-dials this many connections at startup (and
	// keeps them warm best-effort). Smooths cold-start latency.
	// Default 0 (lazy). Must be ≤ MaxConns.
	MinIdleConns int

	// IdleTimeout closes connections that have been idle this long
	// before they're reused. 0 disables. Default 5 minutes.
	IdleTimeout time.Duration

	// MaxLifetime caps the absolute age of a connection regardless of
	// idle status. Important when AUTH tokens rotate or a load
	// balancer wants to drain connections. 0 disables. Default 0.
	MaxLifetime time.Duration

	// DialTimeout caps a single TCP+TLS+AUTH+SELECT+SETNAME dance.
	// Default 5s.
	DialTimeout time.Duration

	// ReadTimeout / WriteTimeout cap per-command I/O when the
	// caller's context has no deadline. Without these a misbehaving
	// server could hang the goroutine indefinitely. Default 3s each.
	// Set to a negative value to disable.
	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// MaxRetries is the number of retry attempts on a transient
	// transport error (broken pipe, EOF, network timeout, protocol
	// error). App-level errors (-ERR replies) are never retried.
	// Default 1.
	MaxRetries int

	// ShutdownTimeout caps how long Close waits for in-flight
	// operations to drain before forcing socket closure. Default 5s.
	ShutdownTimeout time.Duration

	// Dialer overrides the net.Dialer used for new connections. When
	// set, the TLS option is ignored.
	Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

	// Hook fires after every operation. Compose with drops.LoggerHook
	// for instant request logging.
	Hook drops.Hook

	// KeyPrefix is prepended to every key. Useful for multi-tenant
	// deployments sharing a Redis instance.
	KeyPrefix string
}

// PoolStats is a snapshot of pool / wire activity counters. All
// counters are monotonically increasing across the Cache's lifetime
// and are safe to read concurrently.
type PoolStats struct {
	// TotalConns is the number of open connections (idle + checked
	// out) at the moment of the snapshot.
	TotalConns int

	// Hits is the count of Get-from-pool calls that found a warm
	// connection ready to reuse.
	Hits int64

	// Misses is the count of Get-from-pool calls that had to dial a
	// new connection (or replaced a stale one).
	Misses int64

	// Timeouts is the count of Get-from-pool calls that returned
	// ctx.Err() while waiting for a slot.
	Timeouts int64

	// StaleClosed is the count of connections closed because they
	// exceeded IdleTimeout or MaxLifetime.
	StaleClosed int64

	// WaitCount is the count of Get-from-pool calls that had to
	// wait > 0 for a slot (i.e. the pool was at capacity).
	WaitCount int64

	// WaitDuration is the total time spent waiting for pool slots.
	WaitDuration time.Duration

	// Retries is the count of transient errors that triggered a
	// retry attempt.
	Retries int64
}

// Cache is the Redis-backed cache.Cache implementation.
type Cache struct {
	opts Options

	// pool holds MaxConns slots. A nil entry means "the slot is
	// free; dial a fresh connection if you need it." A non-nil entry
	// is a warm idle connection ready to reuse. A channel of size
	// MaxConns gives free pool wait + ctx cancellation.
	pool chan *conn

	closeOnce sync.Once
	closing   atomic.Bool
	closeCh   chan struct{}
	inFlight  sync.WaitGroup

	statsTotalConns   atomic.Int64
	statsHits         atomic.Int64
	statsMisses       atomic.Int64
	statsTimeouts     atomic.Int64
	statsStaleClosed  atomic.Int64
	statsWaitCount    atomic.Int64
	statsWaitDurNanos atomic.Int64
	statsRetries      atomic.Int64
}

// conn is a buffered RESP2 connection plus metadata for pool
// management.
type conn struct {
	nc        net.Conn
	r         *bufio.Reader
	w         *bufio.Writer
	createdAt time.Time
	lastUsed  time.Time
}

// New creates a Cache. The first connection is established lazily on
// the first call (Ping forces it for eager warm-up). If MinIdleConns
// is set, a background goroutine pre-dials those connections.
func New(opts Options) *Cache {
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:6379"
	}
	if opts.MaxConns <= 0 {
		opts.MaxConns = 10
	}
	if opts.MinIdleConns < 0 {
		opts.MinIdleConns = 0
	}
	if opts.MinIdleConns > opts.MaxConns {
		opts.MinIdleConns = opts.MaxConns
	}
	if opts.IdleTimeout == 0 {
		opts.IdleTimeout = 5 * time.Minute
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 5 * time.Second
	}
	// Per-op deadlines: 3s defaults. A negative value disables.
	if opts.ReadTimeout == 0 {
		opts.ReadTimeout = 3 * time.Second
	}
	if opts.WriteTimeout == 0 {
		opts.WriteTimeout = 3 * time.Second
	}
	if opts.MaxRetries < 0 {
		opts.MaxRetries = 0
	} else if opts.MaxRetries == 0 {
		opts.MaxRetries = 1
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 5 * time.Second
	}
	if opts.ClientName == "" {
		opts.ClientName = "drops"
	}
	if opts.Dialer == nil {
		base := &net.Dialer{Timeout: opts.DialTimeout}
		if opts.TLS != nil {
			tlsCfg := opts.TLS
			opts.Dialer = func(ctx context.Context, network, addr string) (net.Conn, error) {
				td := &tls.Dialer{NetDialer: base, Config: tlsCfg}
				return td.DialContext(ctx, network, addr)
			}
		} else {
			opts.Dialer = base.DialContext
		}
	}
	if opts.Credentials == nil && (opts.Username != "" || opts.Password != "") {
		opts.Credentials = StaticCredentials(opts.Username, opts.Password)
	}

	c := &Cache{
		opts:    opts,
		pool:    make(chan *conn, opts.MaxConns),
		closeCh: make(chan struct{}),
	}
	// Seed the pool with empty slots — one per allowed connection.
	for i := 0; i < opts.MaxConns; i++ {
		c.pool <- nil
	}
	if opts.MinIdleConns > 0 {
		go c.warmUp()
	}
	return c
}

// Compile-time interface conformance.
var _ cache.Cache = (*Cache)(nil)
var _ cache.MultiCache = (*Cache)(nil)

// Stats returns a snapshot of pool / wire counters. Safe to call
// concurrently from a metrics emitter.
func (c *Cache) Stats() PoolStats {
	return PoolStats{
		TotalConns:   int(c.statsTotalConns.Load()),
		Hits:         c.statsHits.Load(),
		Misses:       c.statsMisses.Load(),
		Timeouts:     c.statsTimeouts.Load(),
		StaleClosed:  c.statsStaleClosed.Load(),
		WaitCount:    c.statsWaitCount.Load(),
		WaitDuration: time.Duration(c.statsWaitDurNanos.Load()),
		Retries:      c.statsRetries.Load(),
	}
}

// warmUp pre-dials MinIdleConns connections.
func (c *Cache) warmUp() {
	for i := 0; i < c.opts.MinIdleConns; i++ {
		select {
		case <-c.closeCh:
			return
		case slot := <-c.pool:
			if slot != nil {
				// Already populated by an earlier checkout-return
				// cycle — leave it.
				c.pool <- slot
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), c.opts.DialTimeout)
			cn, err := c.dial(ctx)
			cancel()
			if err != nil {
				c.pool <- nil
				continue
			}
			c.statsTotalConns.Add(1)
			c.pool <- cn
		}
	}
}

// --- dial ----------------------------------------------------------

func (c *Cache) dial(ctx context.Context) (*conn, error) {
	nc, err := c.opts.Dialer(ctx, "tcp", c.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("redis: dial %s: %w", c.opts.Addr, err)
	}
	cn := &conn{
		nc:        nc,
		r:         bufio.NewReader(nc),
		w:         bufio.NewWriter(nc),
		createdAt: time.Now(),
	}
	if c.opts.Credentials != nil {
		creds, err := c.opts.Credentials(ctx)
		if err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redis: credentials: %w", err)
		}
		if creds.Password != "" {
			var args []any
			if creds.Username != "" {
				args = []any{"AUTH", creds.Username, creds.Password}
			} else {
				args = []any{"AUTH", creds.Password}
			}
			if _, err := c.cmd(ctx, cn, args); err != nil {
				_ = nc.Close()
				return nil, err
			}
		}
	}
	if c.opts.DB != 0 {
		if _, err := c.cmd(ctx, cn, []any{"SELECT", c.opts.DB}); err != nil {
			_ = nc.Close()
			return nil, err
		}
	}
	if c.opts.ClientName != "" {
		// CLIENT SETNAME may fail on very old Redis; treat as
		// non-fatal so we don't break compatibility for an
		// observability nicety.
		_, _ = c.cmd(ctx, cn, []any{"CLIENT", "SETNAME", c.opts.ClientName})
	}
	cn.lastUsed = time.Now()
	return cn, nil
}

// cmd writes args, applies deadlines, and reads one reply. It is the
// only place that touches the wire — both dial and exec funnel here.
func (c *Cache) cmd(ctx context.Context, cn *conn, args []any) (reply, error) {
	dl := c.deadline(ctx)
	if !dl.IsZero() {
		_ = cn.nc.SetDeadline(dl)
		defer cn.nc.SetDeadline(time.Time{})
	}
	if err := writeCommand(cn.w, args...); err != nil {
		return reply{}, err
	}
	r, err := readReply(cn.r)
	if err != nil {
		return reply{}, err
	}
	if r.kind == '-' {
		return r, fmt.Errorf("redis: %s", r.str)
	}
	return r, nil
}

// deadline picks the larger of the caller's ctx deadline and the
// configured per-op timeouts. The returned zero value means "no
// deadline" (caller has none AND options disabled the defaults).
func (c *Cache) deadline(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	// Use the larger of read/write timeout as the round-trip cap.
	rw := c.opts.ReadTimeout
	if c.opts.WriteTimeout > rw {
		rw = c.opts.WriteTimeout
	}
	if rw <= 0 {
		return time.Time{}
	}
	return time.Now().Add(rw)
}

// --- pool ----------------------------------------------------------

// get checks out a connection. The channel does the wait + cancel
// dance; this function just classifies the result and dials as needed.
func (c *Cache) get(ctx context.Context) (*conn, error) {
	if c.closing.Load() {
		return nil, cache.ErrClosed
	}
	waitStart := time.Now()
	select {
	case slot := <-c.pool:
		if waited := time.Since(waitStart); waited > 0 {
			// Only count waits that actually blocked — fast-path hits
			// take ~ns and just inflate the histograms.
			if waited > 100*time.Microsecond {
				c.statsWaitCount.Add(1)
				c.statsWaitDurNanos.Add(waited.Nanoseconds())
			}
		}
		if slot != nil && !c.isStale(slot) {
			c.statsHits.Add(1)
			return slot, nil
		}
		if slot != nil {
			_ = slot.nc.Close()
			c.statsStaleClosed.Add(1)
			c.statsTotalConns.Add(-1)
		}
		c.statsMisses.Add(1)
		cn, err := c.dial(ctx)
		if err != nil {
			// Restore the empty slot so other waiters can try.
			c.pool <- nil
			return nil, err
		}
		c.statsTotalConns.Add(1)
		return cn, nil

	case <-ctx.Done():
		return nil, ctx.Err()

	case <-c.closeCh:
		return nil, cache.ErrClosed
	}
}

// put returns the connection to the pool, discarding it on transport
// error or during shutdown.
func (c *Cache) put(cn *conn, err error) {
	if cn == nil {
		return
	}
	if err != nil && !isAppLevelError(err) {
		_ = cn.nc.Close()
		c.statsTotalConns.Add(-1)
		c.pool <- nil
		return
	}
	if c.closing.Load() {
		_ = cn.nc.Close()
		c.statsTotalConns.Add(-1)
		c.pool <- nil
		return
	}
	cn.lastUsed = time.Now()
	c.pool <- cn
}

func (c *Cache) isStale(cn *conn) bool {
	now := time.Now()
	if c.opts.IdleTimeout > 0 && now.Sub(cn.lastUsed) > c.opts.IdleTimeout {
		return true
	}
	if c.opts.MaxLifetime > 0 && now.Sub(cn.createdAt) > c.opts.MaxLifetime {
		return true
	}
	return false
}

// isAppLevelError reports whether err is a Redis-replied -ERR rather
// than an I/O or protocol failure. App-level errors leave the
// connection usable.
func isAppLevelError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, cache.ErrNotFound) {
		return true
	}
	return strings.HasPrefix(err.Error(), "redis: ") &&
		!strings.Contains(err.Error(), "dial ")
}

// isRetryable reports whether err is the kind of transient transport
// failure that has a chance of succeeding on a fresh connection.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, cache.ErrNotFound) {
		return false
	}
	if isAppLevelError(err) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, ErrProtocol) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return false
}

// --- Cache implementation ------------------------------------------

func (c *Cache) Get(ctx context.Context, key string) ([]byte, error) {
	if err := c.guard(key); err != nil {
		return nil, err
	}
	var out []byte
	err := c.exec(ctx, "cache.get", func(cn *conn) error {
		r, err := c.cmd(ctx, cn, []any{"GET", c.k(key)})
		if err != nil {
			return err
		}
		if r.isNil() {
			return cache.ErrNotFound
		}
		out = make([]byte, len(r.bulk))
		copy(out, r.bulk)
		return nil
	})
	return out, err
}

func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := c.guard(key); err != nil {
		return err
	}
	return c.exec(ctx, "cache.set", func(cn *conn) error {
		args := []any{"SET", c.k(key), value}
		if ttl > 0 {
			args = append(args, "PX", strconv.FormatInt(ttl.Milliseconds(), 10))
		}
		_, err := c.cmd(ctx, cn, args)
		return err
	})
}

func (c *Cache) Delete(ctx context.Context, keys ...string) (int, error) {
	if c.isClosed() {
		return 0, cache.ErrClosed
	}
	if len(keys) == 0 {
		return 0, nil
	}
	var n int
	err := c.exec(ctx, "cache.del", func(cn *conn) error {
		args := make([]any, 0, len(keys)+1)
		args = append(args, "DEL")
		for _, k := range keys {
			args = append(args, c.k(k))
		}
		r, err := c.cmd(ctx, cn, args)
		if err != nil {
			return err
		}
		n = int(r.int64)
		return nil
	})
	return n, err
}

func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	if err := c.guard(key); err != nil {
		return false, err
	}
	var ok bool
	err := c.exec(ctx, "cache.exists", func(cn *conn) error {
		r, err := c.cmd(ctx, cn, []any{"EXISTS", c.k(key)})
		if err != nil {
			return err
		}
		ok = r.int64 > 0
		return nil
	})
	return ok, err
}

func (c *Cache) TTL(ctx context.Context, key string) (time.Duration, error) {
	if err := c.guard(key); err != nil {
		return 0, err
	}
	var d time.Duration
	err := c.exec(ctx, "cache.ttl", func(cn *conn) error {
		r, err := c.cmd(ctx, cn, []any{"PTTL", c.k(key)})
		if err != nil {
			return err
		}
		switch {
		case r.int64 == -2:
			return cache.ErrNotFound
		case r.int64 == -1:
			d = -1
		default:
			d = time.Duration(r.int64) * time.Millisecond
		}
		return nil
	})
	return d, err
}

func (c *Cache) Ping(ctx context.Context) error {
	return c.exec(ctx, "cache.ping", func(cn *conn) error {
		_, err := c.cmd(ctx, cn, []any{"PING"})
		return err
	})
}

func (c *Cache) GetMulti(ctx context.Context, keys ...string) (map[string][]byte, error) {
	if c.isClosed() {
		return nil, cache.ErrClosed
	}
	out := make(map[string][]byte, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	err := c.exec(ctx, "cache.mget", func(cn *conn) error {
		args := make([]any, 0, len(keys)+1)
		args = append(args, "MGET")
		for _, k := range keys {
			args = append(args, c.k(k))
		}
		r, err := c.cmd(ctx, cn, args)
		if err != nil {
			return err
		}
		if r.kind != '*' {
			return fmt.Errorf("%w: MGET expected array, got 0x%02x", ErrProtocol, r.kind)
		}
		for i, el := range r.array {
			if el.isNil() {
				continue
			}
			v := make([]byte, len(el.bulk))
			copy(v, el.bulk)
			out[keys[i]] = v
		}
		return nil
	})
	return out, err
}

func (c *Cache) SetMulti(ctx context.Context, items map[string][]byte, ttl time.Duration) error {
	if c.isClosed() {
		return cache.ErrClosed
	}
	if len(items) == 0 {
		return nil
	}
	if ttl == 0 {
		return c.exec(ctx, "cache.mset", func(cn *conn) error {
			args := make([]any, 0, 1+2*len(items))
			args = append(args, "MSET")
			for k, v := range items {
				args = append(args, c.k(k), v)
			}
			_, err := c.cmd(ctx, cn, args)
			return err
		})
	}
	return c.exec(ctx, "cache.mset", func(cn *conn) error {
		for k, v := range items {
			args := []any{"SET", c.k(k), v, "PX",
				strconv.FormatInt(ttl.Milliseconds(), 10)}
			if _, err := c.cmd(ctx, cn, args); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close releases pooled connections, waiting up to ShutdownTimeout
// for in-flight operations to finish first.
func (c *Cache) Close() error {
	c.closeOnce.Do(func() {
		c.closing.Store(true)
		close(c.closeCh)

		// Wait up to ShutdownTimeout for in-flight ops to drain.
		done := make(chan struct{})
		go func() {
			c.inFlight.Wait()
			close(done)
		}()
		t := time.NewTimer(c.opts.ShutdownTimeout)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
		}

		// Drain remaining idle conns. Connections still checked out
		// will be closed by put() when they're returned (closing is
		// already true so put discards them).
		for i := 0; i < c.opts.MaxConns; i++ {
			select {
			case cn := <-c.pool:
				if cn != nil {
					_ = cn.nc.Close()
					c.statsTotalConns.Add(-1)
				}
			default:
				return
			}
		}
	})
	return nil
}

// --- exec with retry-once ------------------------------------------

func (c *Cache) exec(ctx context.Context, kind string, fn func(*conn) error) (err error) {
	start := time.Now()
	defer func() {
		// Any user-visible ctx failure counts as one Timeout,
		// regardless of which layer (pool wait, dial, wire I/O)
		// caught it.
		if err != nil && (errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded)) {
			c.statsTimeouts.Add(1)
		}
		drops.CallHook(c.opts.Hook, ctx, drops.QueryEvent{
			Kind:     kind,
			Duration: time.Since(start),
			Err:      err,
		})
	}()
	c.inFlight.Add(1)
	defer c.inFlight.Done()

	attempts := c.opts.MaxRetries + 1
	for attempt := 0; attempt < attempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		var cn *conn
		cn, err = c.get(ctx)
		if err != nil {
			return err
		}
		err = fn(cn)
		c.put(cn, err)

		// Success or a non-retryable error → return.
		if err == nil || !isRetryable(err) {
			return err
		}
		// Last attempt → return the error.
		if attempt == attempts-1 {
			return err
		}
		// Retry on a fresh connection.
		c.statsRetries.Add(1)
	}
	return nil // unreachable
}

// --- helpers --------------------------------------------------------

func (c *Cache) k(key string) string {
	if c.opts.KeyPrefix == "" {
		return key
	}
	return c.opts.KeyPrefix + key
}

func (c *Cache) guard(key string) error {
	if c.isClosed() {
		return cache.ErrClosed
	}
	if key == "" {
		return cache.ErrInvalidKey
	}
	return nil
}

func (c *Cache) isClosed() bool { return c.closing.Load() }
