package pg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bernardoforcillo/drops/cache"
)

// EntityCache wires a cache backend into an Entity so reads pass
// through the cache and writes invalidate the matching entries.
// Construct one via (*Entity[T]).WithCache; do not instantiate
// directly.
//
// Cache key conventions:
//
//	drops:<table>:pk:<id>          — Get by primary key
//	drops:<table>:q:<sql-hash>     — query results (best-effort, TTL)
//
// Primary-key entries are invalidated on Update / Save / Delete.
// Query entries rely on TTL alone — invalidation across an arbitrary
// WHERE/JOIN topology is intractable in the general case, and TTL is
// usually the right trade for read-heavy services.
//
// A built-in single-flight group dedupes concurrent identical
// PK-by-cache-miss reads, so a thundering herd of "give me user 42"
// resolves to one DB query.
type EntityCache struct {
	backend cache.Cache
	ttl     time.Duration
	sf      singleFlightGroup
}

// WithCache attaches c to the entity. ttl=0 means "no expiry" — the
// entries live until evicted by the backend (memory LRU) or until
// the cache backend's own policy kicks in. A non-zero ttl is
// recommended for query-result caching.
func (e *Entity[T]) WithCache(c cache.Cache, ttl time.Duration) *Entity[T] {
	e.cache = &EntityCache{backend: c, ttl: ttl}
	return e
}

// HasCache reports whether a cache is wired up.
func (e *Entity[T]) HasCache() bool { return e.cache != nil }

// pkKey builds the cache key for a primary-key lookup.
func (e *Entity[T]) pkKey(id any) string {
	return fmt.Sprintf("drops:%s:pk:%v", e.table.Name(), id)
}

// queryKey builds the cache key for a rendered SELECT (sql + args).
// SHA-256 of the concatenated form keeps the key short and stable.
func queryKey(table, sql string, args []any) string {
	h := sha256.New()
	h.Write([]byte(sql))
	for _, a := range args {
		fmt.Fprintf(h, "\x00%v", a)
	}
	return fmt.Sprintf("drops:%s:q:%s", table, hex.EncodeToString(h.Sum(nil))[:16])
}

// encodeGob serialises v into a byte slice via encoding/gob.
func encodeGob(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeGob deserialises b into dest via encoding/gob.
func decodeGob(b []byte, dest any) error {
	return gob.NewDecoder(bytes.NewReader(b)).Decode(dest)
}

// readPK looks up the cached row for id. Returns (true, nil) on hit,
// (false, nil) on miss, (false, err) on a backend error other than
// "not found".
func (c *EntityCache) readPK(ctx context.Context, key string, dest any) (bool, error) {
	raw, err := c.backend.Get(ctx, key)
	if err != nil {
		if errors.Is(err, cache.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := decodeGob(raw, dest); err != nil {
		// Stale or corrupted entry — drop it so the next miss can
		// repopulate cleanly.
		_, _ = c.backend.Delete(ctx, key)
		return false, nil
	}
	return true, nil
}

// writeKey serialises v and stores it under key with the configured
// TTL. Errors are returned to the caller — typical use is to ignore
// them (cache writes are best-effort).
func (c *EntityCache) writeKey(ctx context.Context, key string, v any) error {
	b, err := encodeGob(v)
	if err != nil {
		return err
	}
	return c.backend.Set(ctx, key, b, c.ttl)
}

// invalidatePK deletes the PK entry for id. Safe to call when no
// cache is attached (no-op).
func (e *Entity[T]) invalidatePK(ctx context.Context, id any) {
	if e.cache == nil {
		return
	}
	_, _ = e.cache.backend.Delete(ctx, e.pkKey(id))
}

// ----------------------------------------------------------------------
// Single-flight
// ----------------------------------------------------------------------

// singleFlightGroup dedupes concurrent identical lookups. Adapted
// from golang.org/x/sync/singleflight, kept minimal so drops takes
// no external dependency. Safe for concurrent use.
type singleFlightGroup struct {
	mu sync.Mutex
	m  map[string]*sfCall
}

type sfCall struct {
	wg  sync.WaitGroup
	val any
	err error
}

// do runs fn under key. Concurrent callers with the same key share
// the result of one fn invocation.
func (g *singleFlightGroup) do(key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*sfCall{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err
	}
	c := &sfCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.val, c.err = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()

	return c.val, c.err
}
