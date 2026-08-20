// Package tiered composes two cache.Cache backends into a single
// two-level (L1 + L2) read-through / write-through cache.
//
// The canonical shape is a fast in-process L1 (drops/cache/memory) in
// front of a shared, network L2 (drops/cache/redis or
// drops/cache/memcached):
//
//	l1 := memory.New(memory.Options{MaxEntries: 10_000})
//	l2, _ := redis.New(redis.Options{Addr: "redis:6379"})
//	c := tiered.New(tiered.Options{L1: l1, L2: l2, L1TTL: 30 * time.Second})
//
// Reads check L1 first; on a miss they fall through to L2 and, on an L2
// hit, backfill L1 so the next read is local. Writes and deletes fan out
// to both tiers. GetOrLoad adds read-through population from an origin
// function with singleflight stampede protection, so a burst of
// concurrent misses for the same key triggers exactly one load.
//
// The zero value is not usable; construct with New. Both tiers must be
// supplied. Tiered is itself a cache.Cache (and a cache.MultiCache), so
// it nests: an L2 can itself be another tiered cache.
//
// # Consistency
//
// L1 is allowed to be stale, and that is the trade the near tier buys.
// A read that hits L1 never consults L2, so a value some other process
// wrote to L2 stays invisible here until the local copy expires. L1TTL
// bounds that staleness; leaving it at 0 mirrors the L2 lifetime, which
// for a key with no expiry means "stale until something deletes it".
// Any deployment with more than one writer wants a small, non-zero
// L1TTL.
//
// Writes reach L2 first and stop there on failure, so a half-failed Set
// leaves both tiers on the old value rather than L1 ahead of L2.
// Deletes also reach L2 first, then clear L1 whatever L2 said: a failed
// L2 delete leaves the shared value in place, but never a local copy of
// a value the caller asked to drop. A read error from L2 fails the Get;
// a failed L1 backfill does not, since it costs only another round-trip
// next time. Because every write path puts L1 last and returns its
// failure rather than swallowing it, an error from Set, SetMulti or
// GetOrLoad does not mean the value failed to reach L2.
package tiered

import (
	"context"
	"errors"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
)

// Options configures a tiered cache.
type Options struct {
	// L1 is the fast, near tier (typically in-process memory). Required.
	L1 cache.Cache

	// L2 is the slow, shared tier (typically network Redis/Memcached),
	// and the source of truth for TTL. Required.
	L2 cache.Cache

	// L1TTL caps how long a value backfilled into L1 (on an L2 hit) or
	// written into L1 (on Set) may live, bounding how stale a local copy
	// can get relative to L2. 0 means "mirror the L2 TTL" — the value
	// lives in L1 for L2's remaining lifetime (which requires an extra
	// L2.TTL round-trip on backfill). A small non-zero L1TTL avoids that
	// round-trip and is the recommended production setting.
	L1TTL time.Duration

	// Hook fires after every tiered operation with kind = "cache.get",
	// "cache.set", "cache.del", "cache.exists", "cache.ttl",
	// "cache.ping", "cache.load". It observes the tiered call as a whole;
	// the underlying L1/L2 caches fire their own hooks independently.
	Hook drops.Hook

	// Clock is injectable for tests; defaults to time.Now.
	Clock func() time.Time
}

// Cache is the two-level cache. It satisfies cache.Cache and
// cache.MultiCache.
type Cache struct {
	l1    cache.Cache
	l2    cache.Cache
	l1ttl time.Duration
	hook  drops.Hook
	clock func() time.Time
	group singleflight
}

// New builds a tiered cache. It panics if either tier is nil — a
// misconfiguration that can only be a programming error at wiring time.
func New(opts Options) *Cache {
	if opts.L1 == nil || opts.L2 == nil {
		panic("cache/tiered: both L1 and L2 are required")
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Cache{
		l1:    opts.L1,
		l2:    opts.L2,
		l1ttl: opts.L1TTL,
		hook:  opts.Hook,
		clock: clock,
	}
}

// Compile-time interface conformance.
var (
	_ cache.Cache      = (*Cache)(nil)
	_ cache.MultiCache = (*Cache)(nil)
)

// Get returns the value for key, checking L1 then L2. On an L2 hit the
// entry is backfilled into L1. Returns cache.ErrNotFound when neither
// tier has it.
func (c *Cache) Get(ctx context.Context, key string) (_ []byte, err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.get", start, &err)

	v, err := c.l1.Get(ctx, key)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, cache.ErrNotFound) {
		return nil, err
	}
	// L1 miss → consult L2.
	v, err = c.l2.Get(ctx, key)
	if err != nil {
		return nil, err // includes ErrNotFound
	}
	c.backfill(ctx, key, v)
	return v, nil
}

// Set writes value into both tiers. L2 gets the full ttl; L1 gets the
// smaller of ttl and L1TTL (when L1TTL is set) so a local copy can't
// outlive the shared one. An L2 write failure is returned without
// touching L1, so the tiers can't diverge on a failed write.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) (err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.set", start, &err)

	if err = c.l2.Set(ctx, key, value, ttl); err != nil {
		return err
	}
	return c.l1.Set(ctx, key, value, c.l1TTLFor(ttl))
}

// Delete removes the keys from both tiers and returns the count reported
// by L2 (the source of truth). L1 is always cleared even if L2 delete
// fails, so a stale local copy is never left behind.
//
// L2 goes first deliberately. Clearing L1 first opens a window as wide as
// the L2 round-trip in which a concurrent Get misses L1, still finds the
// doomed value in L2, and backfills it — resurrecting it in L1 for a full
// L1TTL (forever, when L1TTL is 0 and the L2 entry had no expiry). Once
// L2 no longer has the value there is nothing left for a reader to
// resurrect it from.
func (c *Cache) Delete(ctx context.Context, keys ...string) (n int, err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.del", start, &err)

	n, err = c.l2.Delete(ctx, keys...)
	_, l1err := c.l1.Delete(ctx, keys...)
	if err == nil && l1err != nil && !errors.Is(l1err, cache.ErrNotFound) {
		err = l1err
	}
	return n, err
}

// Exists reports whether key is live in either tier.
func (c *Cache) Exists(ctx context.Context, key string) (_ bool, err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.exists", start, &err)

	ok, err := c.l1.Exists(ctx, key)
	if err == nil && ok {
		return true, nil
	}
	if err != nil && !errors.Is(err, cache.ErrNotFound) {
		return false, err
	}
	return c.l2.Exists(ctx, key)
}

// TTL returns the remaining lifetime from L2, the source of truth.
func (c *Cache) TTL(ctx context.Context, key string) (_ time.Duration, err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.ttl", start, &err)
	return c.l2.TTL(ctx, key)
}

// Ping succeeds only when both tiers are reachable.
func (c *Cache) Ping(ctx context.Context) (err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.ping", start, &err)

	if err = c.l1.Ping(ctx); err != nil {
		return err
	}
	return c.l2.Ping(ctx)
}

// Close closes both tiers, returning the first error.
func (c *Cache) Close() error {
	e1 := c.l1.Close()
	e2 := c.l2.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

// GetMulti fetches keys from L1, then fills the gaps from L2 in a single
// batched call, backfilling every L2 hit into L1. Missing keys are simply
// absent from the result.
func (c *Cache) GetMulti(ctx context.Context, keys ...string) (_ map[string][]byte, err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.mget", start, &err)

	out := make(map[string][]byte, len(keys))
	var missing []string
	if mc, ok := c.l1.(cache.MultiCache); ok {
		hit, err := mc.GetMulti(ctx, keys...)
		if err != nil {
			return nil, err
		}
		for k, v := range hit {
			out[k] = v
		}
	} else {
		for _, k := range keys {
			if v, err := c.l1.Get(ctx, k); err == nil {
				out[k] = v
			}
		}
	}
	for _, k := range keys {
		if _, ok := out[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return out, nil
	}
	l2hit, err := c.getMultiL2(ctx, missing)
	if err != nil {
		return nil, err
	}
	for k, v := range l2hit {
		out[k] = v
		c.backfill(ctx, k, v)
	}
	return out, nil
}

// SetMulti writes every item to L2 then L1, with the same TTL rules as
// Set. An L2 failure is returned before L1 is touched.
func (c *Cache) SetMulti(ctx context.Context, items map[string][]byte, ttl time.Duration) (err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.mset", start, &err)

	if err = c.setMulti(ctx, c.l2, items, ttl); err != nil {
		return err
	}
	return c.setMulti(ctx, c.l1, items, c.l1TTLFor(ttl))
}

// Loader produces the authoritative value for a key on a full cache miss.
type Loader func(ctx context.Context) ([]byte, error)

// GetOrLoad returns key's cached value, or — on a full miss — calls load
// exactly once (even under concurrent callers for the same key, via
// singleflight), stores the result in both tiers with ttl, and returns
// it. This is the read-through / stampede-protection entry point: a
// thundering herd of requests for a cold key results in a single origin
// load, not one per request.
//
// load is not called for keys already present in L1 or L2. A load error
// is propagated and nothing is cached.
func (c *Cache) GetOrLoad(ctx context.Context, key string, ttl time.Duration, load Loader) (_ []byte, err error) {
	start := c.clock()
	defer c.emit(ctx, "cache.load", start, &err)

	// Fast path: present in some tier.
	if v, gerr := c.get(ctx, key); gerr == nil {
		return v, nil
	} else if !errors.Is(gerr, cache.ErrNotFound) {
		return nil, gerr
	}

	// Dedup concurrent loads for the same key.
	v, err, _ := c.group.Do(key, func() ([]byte, error) {
		// Re-check under the flight in case another caller populated it
		// while we were queuing.
		if v, gerr := c.get(ctx, key); gerr == nil {
			return v, nil
		}
		loaded, lerr := load(ctx)
		if lerr != nil {
			return nil, lerr
		}
		if serr := c.set(ctx, key, loaded, ttl); serr != nil {
			return nil, serr
		}
		return loaded, nil
	})
	if err != nil {
		return nil, err
	}
	// Every caller of a shared flight would otherwise hold the loader's
	// one backing array, and every other read path in the package hands
	// out a private copy. Copying for the joiners alone is not enough:
	// the caller that ran the load returns while they are still copying,
	// so if it mutates in place they read a half-scribbled value. Nobody
	// gets the original.
	return append([]byte(nil), v...), nil
}

// --- internal helpers -------------------------------------------------

// get / set are the hook-free primitives GetOrLoad composes so it doesn't
// double-emit the per-op events.
func (c *Cache) get(ctx context.Context, key string) ([]byte, error) {
	v, err := c.l1.Get(ctx, key)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, cache.ErrNotFound) {
		return nil, err
	}
	v, err = c.l2.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	c.backfill(ctx, key, v)
	return v, nil
}

func (c *Cache) set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := c.l2.Set(ctx, key, value, ttl); err != nil {
		return err
	}
	return c.l1.Set(ctx, key, value, c.l1TTLFor(ttl))
}

// backfill writes an L2 hit into L1, best-effort. A failed backfill is
// swallowed: it only costs the next read another L2 round-trip, it must
// never fail the current Get.
func (c *Cache) backfill(ctx context.Context, key string, value []byte) {
	ttl := c.l1ttl
	if ttl == 0 {
		// Mirror L2's remaining lifetime.
		if rem, err := c.l2.TTL(ctx, key); err == nil && rem > 0 {
			ttl = rem
		}
	}
	_ = c.l1.Set(ctx, key, value, ttl)
}

// l1TTLFor caps a write TTL by L1TTL when the latter is set and smaller.
func (c *Cache) l1TTLFor(ttl time.Duration) time.Duration {
	if c.l1ttl <= 0 {
		return ttl
	}
	if ttl <= 0 || c.l1ttl < ttl {
		return c.l1ttl
	}
	return ttl
}

func (c *Cache) getMultiL2(ctx context.Context, keys []string) (map[string][]byte, error) {
	if mc, ok := c.l2.(cache.MultiCache); ok {
		return mc.GetMulti(ctx, keys...)
	}
	out := make(map[string][]byte, len(keys))
	for _, k := range keys {
		v, err := c.l2.Get(ctx, k)
		if err == nil {
			out[k] = v
			continue
		}
		if !errors.Is(err, cache.ErrNotFound) {
			return nil, err
		}
	}
	return out, nil
}

func (c *Cache) setMulti(ctx context.Context, tier cache.Cache, items map[string][]byte, ttl time.Duration) error {
	if mc, ok := tier.(cache.MultiCache); ok {
		return mc.SetMulti(ctx, items, ttl)
	}
	for k, v := range items {
		if err := tier.Set(ctx, k, v, ttl); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) emit(ctx context.Context, kind string, start time.Time, errp *error) {
	drops.CallHook(c.hook, ctx, drops.QueryEvent{
		Kind:     kind,
		Duration: c.clock().Sub(start),
		Err:      *errp,
	})
}
