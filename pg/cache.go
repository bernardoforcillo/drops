package pg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
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
// The two namespaces are not equally safe to fill, and the difference
// is the tenant. A query entry is keyed by the statement the ctx
// resolved to, so the tenant and the subject are hashed into it and two
// tenants asking one question get two entries. A primary-key entry has
// room for the id and nothing else. So an entity whose rows are scoped
// — by a tenant axis, an authorisation guard, or a context filter on
// its table — never writes a PK entry and never reads one; it keeps the
// query cache and loses the PK cache. See (*Entity).hasRowScope for
// what goes wrong when a scoped row reaches that namespace.
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
//
// The values are joined with a separator that cannot appear in a
// rendered key value, so a composite key ("ab", "c") cannot collide
// with ("a", "bc") — the kind of collision that would serve one row
// in place of another.
func (e *Entity[T]) pkKey(values []any) string {
	var sb strings.Builder
	sb.WriteString("drops:")
	sb.WriteString(e.table.Name())
	sb.WriteString(":pk:")
	for i, v := range values {
		if i > 0 {
			sb.WriteByte(0x1f) // unit separator
		}
		fmt.Fprintf(&sb, "%v", v)
	}
	return sb.String()
}

// queryKey builds the cache key for a rendered SELECT (sql + args).
// SHA-256 of the encoded form keeps the key one fixed-width token
// whatever the statement's size.
//
// Since P0-1 this key is a tenant-isolation boundary and not only a
// lookup shortcut, and it is built like one. The tenant predicate is
// resolved by the executor rather than by the caller, so the *sql* of
// two tenants asking the same question is byte-identical: the only
// thing separating their cached result sets is that their tenant
// values are bound as arguments and hashed in here. What that asks of
// the encoding, and of the digest, follows — and none of it is
// optional.
//
// Arguments are encoded with their dynamic type. A bare %v erases it,
// so WithTenant(ctx, "7") and WithTenant(ctx, int64(7)) produced the
// same key — and an application that reads the tenant off an HTTP
// header on one path and out of a database column on another has both
// spellings of the same customer id in the same process. One of them
// was served the other's rows.
//
// Pointer arguments are followed to the value they point at instead of
// being rendered as an address. An address is not stable across runs
// (which would only cost hit rate), but it is also *reused*: once the
// pointee is collected the allocator may hand the same address to a
// different tenant's value, and the second one inherits the first
// one's cache entry.
//
// Each rendered value is length-prefixed so it cannot spell out its
// neighbours: without it an argument containing the separator byte can
// reproduce the encoding of a different argument list.
//
// The whole digest goes into the key, where the first 64 bits used to.
// At 64 bits a collision is expected around 2^32 entries, which is a
// population a busy cache reaches, and a collision here does not
// corrupt an entry — it answers one tenant's query with another
// tenant's rows. Forty-eight more bytes per key is not a price worth
// arguing about against that.
func queryKey(table, sql string, args []any) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\x00%s", len(sql), sql)
	for _, a := range args {
		hashArg(h, a)
	}
	return fmt.Sprintf("drops:%s:q:%s", table, hex.EncodeToString(h.Sum(nil)))
}

// hashArg writes one bound argument into h as
// "\x00<dynamic type>\x00<len>\x00<value>". See queryKey for why the
// type and the length are in there and why pointers are dereferenced.
func hashArg(h io.Writer, a any) {
	fmt.Fprintf(h, "\x00%T", a)
	v := reflect.ValueOf(a)
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			break
		}
		v = v.Elem()
	}
	if !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		// A nil argument has no value to render, and its type is
		// already in the digest.
		fmt.Fprint(h, "\x00nil")
		return
	}
	rendered := fmt.Sprintf("%v", v.Interface())
	fmt.Fprintf(h, "\x00%d\x00%s", len(rendered), rendered)
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
//
// Unlike the writes, this is not gated on (*Entity).hasRowScope. A
// scoped entity is not supposed to have an entry under that key at
// all, and deleting one that is not there costs a round trip and
// cannot leak anything; deleting one that *is* there — left by an
// older build, or by a sibling entity over the same table that is not
// scoped — is exactly what should happen. Gates belong on the paths
// that put rows in, never on the path that takes them out.
func (e *Entity[T]) invalidatePK(ctx context.Context, values []any) {
	if e.cache == nil {
		return
	}
	_, _ = e.cache.backend.Delete(ctx, e.pkKey(values))
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
