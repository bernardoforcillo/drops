// Package memory provides an in-process cache.Cache implementation.
//
// Properties:
//   - Map-backed, sync.RWMutex-guarded; safe for concurrent use.
//   - TTL is honoured on read AND swept lazily by an optional janitor
//     goroutine (interval set via Options.SweepEvery; 0 disables).
//   - Optional MaxEntries cap with a tiny FIFO eviction order (oldest
//     SetTime first). Not LRU — LRU adds an extra synchronisation
//     point per Get; if you need it, wrap an LRU pkg or vendor your
//     own. FIFO is a reasonable default for typical web caches.
//   - drops.Hook fires after every operation with kind = "cache.get",
//     "cache.set", "cache.del", "cache.exists", "cache.ttl", "cache.ping".
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
)

// Options tunes the in-memory cache.
type Options struct {
	// MaxEntries caps the live entry count. When inserting beyond it,
	// the oldest-inserted entry is evicted. 0 means unbounded.
	MaxEntries int

	// SweepEvery is the janitor interval that removes expired entries.
	// 0 disables the janitor; expired entries are still skipped on
	// read but the map will grow until something overwrites them.
	SweepEvery time.Duration

	// Hook fires after every operation, suitable for logging / metrics.
	Hook drops.Hook

	// Clock allows tests to inject a virtual clock; defaults to time.Now.
	Clock func() time.Time
}

// Cache is the in-memory implementation of cache.Cache.
type Cache struct {
	mu       sync.RWMutex
	entries  map[string]entry
	order    []string // insertion order, used for FIFO eviction
	opts     Options
	stopOnce sync.Once
	stop     chan struct{}
	closed   bool
}

type entry struct {
	value     []byte
	expiresAt time.Time // zero = no expiry
	insertedAt time.Time
}

// New returns a memory-backed cache. The janitor (if enabled) is
// started immediately; Close stops it.
func New(opts ...Options) *Cache {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	c := &Cache{
		entries: map[string]entry{},
		opts:    o,
		stop:    make(chan struct{}),
	}
	if o.SweepEvery > 0 {
		go c.janitor()
	}
	return c
}

// Compile-time interface conformance.
var _ cache.Cache = (*Cache)(nil)
var _ cache.MultiCache = (*Cache)(nil)

func (c *Cache) janitor() {
	t := time.NewTicker(c.opts.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			c.sweep()
		}
	}
}

func (c *Cache) sweep() {
	now := c.opts.Clock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if !e.expiresAt.IsZero() && now.After(e.expiresAt) {
			delete(c.entries, k)
			c.removeFromOrder(k)
		}
	}
}

// Get returns the value or cache.ErrNotFound.
func (c *Cache) Get(ctx context.Context, key string) (_ []byte, err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.get", start, &err)

	if err := c.guard(key); err != nil {
		return nil, err
	}
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil, cache.ErrNotFound
	}
	if c.expired(e) {
		// Lazy eviction.
		c.mu.Lock()
		if cur, ok := c.entries[key]; ok && c.expired(cur) {
			delete(c.entries, key)
			c.removeFromOrder(key)
		}
		c.mu.Unlock()
		return nil, cache.ErrNotFound
	}
	// Defensive copy so callers can't mutate stored bytes.
	out := make([]byte, len(e.value))
	copy(out, e.value)
	return out, nil
}

// Set stores value under key.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) (err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.set", start, &err)

	if err := c.guard(key); err != nil {
		return err
	}
	now := c.opts.Clock()
	stored := make([]byte, len(value))
	copy(stored, value)
	e := entry{value: stored, insertedAt: now}
	if ttl > 0 {
		e.expiresAt = now.Add(ttl)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	_, existed := c.entries[key]
	c.entries[key] = e
	if !existed {
		c.order = append(c.order, key)
	}
	c.evictIfNeeded()
	return nil
}

// Delete removes the listed keys, returning the count that existed.
func (c *Cache) Delete(ctx context.Context, keys ...string) (n int, err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.del", start, &err)

	if c.isClosed() {
		return 0, cache.ErrClosed
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, k := range keys {
		if _, ok := c.entries[k]; ok {
			delete(c.entries, k)
			c.removeFromOrder(k)
			n++
		}
	}
	return n, nil
}

// Exists reports presence (non-expired) for a key.
func (c *Cache) Exists(ctx context.Context, key string) (_ bool, err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.exists", start, &err)

	if err := c.guard(key); err != nil {
		return false, err
	}
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || c.expired(e) {
		return false, nil
	}
	return true, nil
}

// TTL returns the remaining lifetime, or -1 for "no expiry", or 0
// (with ErrNotFound) for absent keys.
func (c *Cache) TTL(ctx context.Context, key string) (_ time.Duration, err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.ttl", start, &err)

	if err := c.guard(key); err != nil {
		return 0, err
	}
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || c.expired(e) {
		return 0, cache.ErrNotFound
	}
	if e.expiresAt.IsZero() {
		return -1, nil
	}
	// Use the configured clock rather than time.Until so tests with a
	// virtual clock report deterministic TTLs.
	return e.expiresAt.Sub(c.opts.Clock()), nil
}

// Ping is a no-op for the in-memory cache (always healthy unless closed).
func (c *Cache) Ping(ctx context.Context) (err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.ping", start, &err)
	if c.isClosed() {
		return cache.ErrClosed
	}
	return nil
}

// GetMulti returns every key found.
func (c *Cache) GetMulti(ctx context.Context, keys ...string) (_ map[string][]byte, err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.mget", start, &err)

	if c.isClosed() {
		return nil, cache.ErrClosed
	}
	out := make(map[string][]byte, len(keys))
	c.mu.RLock()
	for _, k := range keys {
		e, ok := c.entries[k]
		if !ok || c.expired(e) {
			continue
		}
		v := make([]byte, len(e.value))
		copy(v, e.value)
		out[k] = v
	}
	c.mu.RUnlock()
	return out, nil
}

// SetMulti stores every item with the same TTL.
func (c *Cache) SetMulti(ctx context.Context, items map[string][]byte, ttl time.Duration) (err error) {
	start := c.opts.Clock()
	defer c.emit(ctx, "cache.mset", start, &err)

	if c.isClosed() {
		return cache.ErrClosed
	}
	for k := range items {
		if k == "" {
			return cache.ErrInvalidKey
		}
	}
	now := c.opts.Clock()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range items {
		stored := make([]byte, len(v))
		copy(stored, v)
		e := entry{value: stored, insertedAt: now}
		if ttl > 0 {
			e.expiresAt = now.Add(ttl)
		}
		_, existed := c.entries[k]
		c.entries[k] = e
		if !existed {
			c.order = append(c.order, k)
		}
	}
	c.evictIfNeeded()
	return nil
}

// Close stops the janitor goroutine and rejects subsequent calls.
func (c *Cache) Close() error {
	c.stopOnce.Do(func() {
		close(c.stop)
		c.mu.Lock()
		c.closed = true
		c.entries = nil
		c.order = nil
		c.mu.Unlock()
	})
	return nil
}

// Len returns the current entry count (for tests / metrics).
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// --- internal helpers -----------------------------------------------

func (c *Cache) guard(key string) error {
	if c.isClosed() {
		return cache.ErrClosed
	}
	if key == "" {
		return cache.ErrInvalidKey
	}
	return nil
}

func (c *Cache) isClosed() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.closed
}

func (c *Cache) expired(e entry) bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return c.opts.Clock().After(e.expiresAt)
}

func (c *Cache) removeFromOrder(key string) {
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

func (c *Cache) evictIfNeeded() {
	if c.opts.MaxEntries <= 0 {
		return
	}
	for len(c.entries) > c.opts.MaxEntries && len(c.order) > 0 {
		victim := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, victim)
	}
}

func (c *Cache) emit(ctx context.Context, kind string, start time.Time, errp *error) {
	drops.CallHook(c.opts.Hook, ctx, drops.QueryEvent{
		Kind:     kind,
		Duration: c.opts.Clock().Sub(start),
		Err:      *errp,
	})
}
