package cloudflarekv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cloudflare"
)

// Workers KV limits.
const (
	// MinTTL is the shortest expiry KV accepts. A shorter one is
	// [ErrTTLTooShort] unless [WithRoundUp] was used.
	MinTTL = 60 * time.Second

	// MaxKeyBytes is the longest a key may be.
	MaxKeyBytes = 512

	// MaxValueBytes is the largest a value may be.
	MaxValueBytes = 25 << 20 // 25 MiB

	// MaxMetadataBytes is the budget for a key's metadata, which
	// this package spends a few dozen bytes of on the expiry marker.
	MaxMetadataBytes = 1024

	// MaxBulkKeys is the most keys one bulk request may name.
	MaxBulkKeys = 100
)

// Sentinel errors.
var (
	// ErrNoNamespace is returned by New when the namespace ID is
	// empty.
	ErrNoNamespace = errors.New("drops/cache/cloudflarekv: namespace ID is empty")

	// ErrTTLTooShort is returned by Set for a positive TTL below
	// [MinTTL].
	//
	// It is an error rather than a rounding because the two are not
	// interchangeable. An entry asked to live five seconds and given
	// sixty is served stale for fifty-five, and a cache that does
	// that quietly is worse than one that refuses: the bug shows up
	// far from here, as a value that should have been invalidated
	// and was not. [WithRoundUp] is how a caller says the staleness
	// is acceptable.
	ErrTTLTooShort = fmt.Errorf("drops/cache/cloudflarekv: TTL is below Workers KV's %s minimum", MinTTL)

	// ErrKeyTooLong is returned for a key above [MaxKeyBytes].
	ErrKeyTooLong = errors.New("drops/cache/cloudflarekv: key is too long")

	// ErrValueTooLarge is returned for a value above
	// [MaxValueBytes].
	ErrValueTooLarge = errors.New("drops/cache/cloudflarekv: value is too large")
)

// Metadata is what this package writes alongside each value.
//
// It exists for one reason: KV's value endpoint does not report when
// a key expires, so [Cache.TTL] would have nothing to answer with.
// Recording the absolute expiry at write time is what makes the
// question answerable — and it is why a key written by something else
// reads back as "no expiry" rather than as a wrong number.
type Metadata struct {
	// ExpiresAt is the Unix second the entry expires, or 0 for an
	// entry with no expiry.
	ExpiresAt int64 `json:"drops_exp,omitempty"`
}

// Cache is a [github.com/bernardoforcillo/drops/cache.Cache] over one
// Workers KV namespace.
//
// Safe for concurrent use by multiple goroutines.
type Cache struct {
	cf        *cloudflare.Client
	namespace string
	prefix    string
	roundUp   bool
	hook      drops.Hook

	mu     sync.RWMutex
	closed bool
}

var (
	_ cache.Cache      = (*Cache)(nil)
	_ cache.MultiCache = (*Cache)(nil)
)

// Option configures a [Cache].
type Option func(*Cache)

// WithKeyPrefix prepends a prefix to every key, so one namespace can
// hold several caches without them colliding.
func WithKeyPrefix(p string) Option {
	return func(c *Cache) { c.prefix = p }
}

// WithRoundUp rounds a TTL below [MinTTL] up to it instead of
// returning [ErrTTLTooShort].
//
// It is opt-in because it trades correctness for convenience: the
// entry then lives longer than the caller asked, and anything relying
// on the shorter life is quietly wrong. Use it when the TTL is a cost
// control rather than a correctness bound.
func WithRoundUp() Option {
	return func(c *Cache) { c.roundUp = true }
}

// WithHook installs an observability hook, fired once per operation
// with Kind set to the operation name.
func WithHook(h drops.Hook) Option {
	return func(c *Cache) { c.hook = h }
}

// New returns a Cache over the Workers KV namespace with the given ID
// — the identifier `wrangler kv namespace list` prints, not the
// binding name.
func New(cf *cloudflare.Client, namespaceID string, opts ...Option) (*Cache, error) {
	if strings.TrimSpace(namespaceID) == "" {
		return nil, ErrNoNamespace
	}
	c := &Cache{cf: cf, namespace: namespaceID}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Namespace returns the KV namespace this cache reads and writes.
func (c *Cache) Namespace() string { return c.namespace }

// Get returns the value stored under key.
//
// [github.com/bernardoforcillo/drops/cache.ErrNotFound] is the only
// way to tell a missing entry from an empty one — KV stores a
// zero-length value perfectly well.
func (c *Cache) Get(ctx context.Context, key string) (out []byte, err error) {
	defer c.emit(ctx, "get", key, time.Now(), &err)
	if err = c.usable(key); err != nil {
		return nil, err
	}
	return c.getRaw(ctx, key)
}

// getRaw reads one value through the single-key endpoint, which
// answers with the stored bytes rather than a JSON rendering of them.
func (c *Cache) getRaw(ctx context.Context, key string) ([]byte, error) {
	resp, err := c.cf.DoRaw(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   c.valuePath(key),
		Accept: "*/*",
	})
	if err != nil {
		return nil, err
	}
	switch {
	case resp.Status == http.StatusNotFound:
		return nil, cache.ErrNotFound
	case resp.Status < 200 || resp.Status >= 300:
		return nil, c.statusError("get", key, resp)
	}
	return resp.Body, nil
}

// Set stores value under key.
//
// ttl=0 means no expiry. A positive ttl below [MinTTL] is
// [ErrTTLTooShort] unless [WithRoundUp] was used — see that option
// for why refusing is the default.
func (c *Cache) Set(ctx context.Context, key string, value []byte, ttl time.Duration) (err error) {
	defer c.emit(ctx, "set", key, time.Now(), &err)
	if err = c.usable(key); err != nil {
		return err
	}
	if len(value) > MaxValueBytes {
		return fmt.Errorf("%w: %d bytes, limit is %d", ErrValueTooLarge, len(value), MaxValueBytes)
	}
	seconds, expiresAt, err := c.expiry(ttl)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(Metadata{ExpiresAt: expiresAt})
	if err != nil {
		return err
	}
	// KV's write endpoint takes the value and the metadata as a
	// multipart body; there is no way to send metadata alongside a
	// raw body.
	body, contentType, err := multipartValue(value, meta)
	if err != nil {
		return err
	}

	req := cloudflare.Request{
		Method:      http.MethodPut,
		Path:        c.valuePath(key),
		Raw:         body,
		ContentType: contentType,
	}
	if seconds > 0 {
		req.Query = url.Values{"expiration_ttl": []string{strconv.FormatInt(seconds, 10)}}
	}
	resp, err := c.cf.DoRaw(ctx, req)
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return c.statusError("set", key, resp)
	}
	return nil
}

// Delete removes the listed keys and returns how many KV reported
// removing.
//
// KV's bulk delete does not say which keys existed, so the count is
// the number of keys accepted rather than the number that were
// there. That is the honest reading of what the service returns; a
// caller needing the distinction has to Exists first, and race.
func (c *Cache) Delete(ctx context.Context, keys ...string) (n int, err error) {
	defer c.emit(ctx, "delete", strings.Join(keys, ","), time.Now(), &err)
	if len(keys) == 0 {
		return 0, nil
	}
	if err = c.usable(keys...); err != nil {
		return 0, err
	}

	deleted := 0
	for _, chunk := range chunkStrings(c.prefixed(keys), MaxBulkKeys) {
		resp, dErr := c.cf.DoRaw(ctx, cloudflare.Request{
			Method:      http.MethodDelete,
			Path:        c.cf.AccountPath("/storage/kv/namespaces/", c.namespace) + "/bulk",
			Body:        chunk,
			ContentType: "application/json",
		})
		if dErr != nil {
			return deleted, dErr
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return deleted, c.statusError("delete", strings.Join(chunk, ","), resp)
		}
		deleted += len(chunk)
	}
	return deleted, nil
}

// Exists reports whether key has a live entry.
func (c *Cache) Exists(ctx context.Context, key string) (ok bool, err error) {
	defer c.emit(ctx, "exists", key, time.Now(), &err)
	if err = c.usable(key); err != nil {
		return false, err
	}
	_, found, err := c.metadata(ctx, key)
	if err != nil {
		return false, err
	}
	return found, nil
}

// TTL returns the remaining lifetime of key.
//
// The reserved values are the cache package's: -1 for an entry with
// no expiry, 0 with
// [github.com/bernardoforcillo/drops/cache.ErrNotFound] for one that
// is not there.
//
// An entry written by something other than this package reads as -1,
// because the answer comes from the marker in [Metadata] and there is
// none. KV does not report expiries any other way.
func (c *Cache) TTL(ctx context.Context, key string) (d time.Duration, err error) {
	defer c.emit(ctx, "ttl", key, time.Now(), &err)
	if err = c.usable(key); err != nil {
		return 0, err
	}
	meta, found, err := c.metadata(ctx, key)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, cache.ErrNotFound
	}
	if meta.ExpiresAt == 0 {
		return -1, nil
	}
	remaining := time.Until(time.Unix(meta.ExpiresAt, 0))
	if remaining < 0 {
		// The marker says it is gone but KV has not swept it yet.
		// The marker is the one this package wrote, so it is the
		// authority on what the caller asked for.
		return 0, cache.ErrNotFound
	}
	return remaining, nil
}

// Ping verifies the namespace is reachable and the token can read it.
func (c *Cache) Ping(ctx context.Context) (err error) {
	defer c.emit(ctx, "ping", "", time.Now(), &err)
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return cache.ErrClosed
	}
	// Listing one key touches the namespace without reading a value
	// and without needing one to exist.
	resp, err := c.cf.DoRaw(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   c.cf.AccountPath("/storage/kv/namespaces/", c.namespace) + "/keys",
		Query:  url.Values{"limit": []string{"10"}},
	})
	if err != nil {
		return err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return c.statusError("ping", "", resp)
	}
	return nil
}

// Close marks the cache closed. There is no connection to release —
// every operation is an HTTPS request — so this only makes subsequent
// calls return
// [github.com/bernardoforcillo/drops/cache.ErrClosed], which is what
// the Cache contract asks for. Idempotent.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

// GetMulti implements
// [github.com/bernardoforcillo/drops/cache.MultiCache].
//
// KV's bulk read is a real batch rather than a loop, which is why
// this package satisfies MultiCache at all: a backend that could only
// loop should not, so the round-trip cost stays visible at the call
// site.
//
// It has one wrinkle worth knowing about, because it would otherwise
// be a silent data-corruption bug. The bulk endpoint answers in JSON,
// so every value comes back decoded as text — and a value that is not
// valid UTF-8 (msgpack, protobuf, a compressed blob) loses the
// offending bytes to U+FFFD on the way. This method therefore checks
// each value for the replacement character and re-fetches those keys
// through the single-value endpoint, which is byte-exact. The common
// case stays one round trip; the binary case is correct rather than
// fast.
func (c *Cache) GetMulti(ctx context.Context, keys ...string) (out map[string][]byte, err error) {
	defer c.emit(ctx, "get_multi", strings.Join(keys, ","), time.Now(), &err)
	if len(keys) == 0 {
		return map[string][]byte{}, nil
	}
	if err = c.usable(keys...); err != nil {
		return nil, err
	}

	found := make(map[string][]byte, len(keys))
	var suspect []string
	for _, chunk := range chunkStrings(c.prefixed(keys), MaxBulkKeys) {
		var body struct {
			Values map[string]*string `json:"values"`
		}
		if err = c.cf.Do(ctx, cloudflare.Request{
			Method:     http.MethodPost,
			Path:       c.cf.AccountPath("/storage/kv/namespaces/", c.namespace) + "/bulk/get",
			Body:       map[string]any{"keys": chunk, "type": "text"},
			Idempotent: cloudflare.Idempotently(),
		}, &body); err != nil {
			return nil, err
		}
		for k, v := range body.Values {
			// A null value is a key that was not there. The Cache
			// contract says absent keys are simply not in the
			// result.
			if v == nil {
				continue
			}
			plain := c.unprefixed(k)
			if strings.ContainsRune(*v, utf8.RuneError) {
				suspect = append(suspect, plain)
				continue
			}
			found[plain] = []byte(*v)
		}
	}

	// The text decode may have eaten bytes in these. Ask for them
	// again, one at a time, where the answer is the stored bytes.
	for _, k := range suspect {
		raw, gErr := c.getRaw(ctx, k)
		if errors.Is(gErr, cache.ErrNotFound) {
			continue
		}
		if gErr != nil {
			return nil, fmt.Errorf("drops/cache/cloudflarekv: re-reading %q after a lossy bulk decode: %w", k, gErr)
		}
		found[k] = raw
	}
	return found, nil
}

// SetMulti implements
// [github.com/bernardoforcillo/drops/cache.MultiCache].
func (c *Cache) SetMulti(ctx context.Context, items map[string][]byte, ttl time.Duration) (err error) {
	defer c.emit(ctx, "set_multi", "", time.Now(), &err)
	if len(items) == 0 {
		return nil
	}
	keys := make([]string, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	if err = c.usable(keys...); err != nil {
		return err
	}
	seconds, expiresAt, err := c.expiry(ttl)
	if err != nil {
		return err
	}

	type bulkEntry struct {
		Key           string   `json:"key"`
		Value         string   `json:"value"`
		ExpirationTTL int64    `json:"expiration_ttl,omitempty"`
		Metadata      Metadata `json:"metadata,omitempty"`
		Base64        bool     `json:"base64"`
	}
	entries := make([]bulkEntry, 0, len(items))
	for k, v := range items {
		if len(v) > MaxValueBytes {
			return fmt.Errorf("%w: %q is %d bytes, limit is %d", ErrValueTooLarge, k, len(v), MaxValueBytes)
		}
		entries = append(entries, bulkEntry{
			Key: c.prefix + k,
			// base64 so a value that is not valid UTF-8 survives
			// the JSON body. KV's bulk write has no other way to
			// carry arbitrary bytes.
			Value:         encodeBase64(v),
			ExpirationTTL: seconds,
			Metadata:      Metadata{ExpiresAt: expiresAt},
			Base64:        true,
		})
	}

	for _, chunk := range chunkEntries(entries, MaxBulkKeys) {
		resp, sErr := c.cf.DoRaw(ctx, cloudflare.Request{
			Method:      http.MethodPut,
			Path:        c.cf.AccountPath("/storage/kv/namespaces/", c.namespace) + "/bulk",
			Body:        chunk,
			ContentType: "application/json",
		})
		if sErr != nil {
			return sErr
		}
		if resp.Status < 200 || resp.Status >= 300 {
			return c.statusError("set_multi", "", resp)
		}
	}
	return nil
}

// Helpers -------------------------------------------------------------

// metadata reads a key's stored metadata, reporting whether the key
// exists at all.
func (c *Cache) metadata(ctx context.Context, key string) (Metadata, bool, error) {
	resp, err := c.cf.DoRaw(ctx, cloudflare.Request{Method: http.MethodGet, Path: c.metadataPath(key)})
	if err != nil {
		return Metadata{}, false, err
	}
	if resp.Status == http.StatusNotFound {
		return Metadata{}, false, nil
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return Metadata{}, false, c.statusError("metadata", key, resp)
	}
	var env struct {
		Result  Metadata `json:"result"`
		Success bool     `json:"success"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		// A key with no metadata at all still exists; it just has
		// nothing to say about its expiry.
		return Metadata{}, true, nil
	}
	return env.Result, true, nil
}

// expiry converts a TTL into the seconds KV wants and the absolute
// expiry the metadata marker records.
func (c *Cache) expiry(ttl time.Duration) (seconds, expiresAt int64, err error) {
	if ttl <= 0 {
		return 0, 0, nil
	}
	if ttl < MinTTL {
		if !c.roundUp {
			return 0, 0, fmt.Errorf("%w: asked for %s (cloudflarekv.WithRoundUp accepts the staleness)", ErrTTLTooShort, ttl)
		}
		ttl = MinTTL
	}
	seconds = int64(ttl.Seconds())
	return seconds, time.Now().Add(ttl).Unix(), nil
}

// usable checks the cache is open and the keys are legal.
func (c *Cache) usable(keys ...string) error {
	c.mu.RLock()
	closed := c.closed
	c.mu.RUnlock()
	if closed {
		return cache.ErrClosed
	}
	for _, k := range keys {
		if k == "" {
			return fmt.Errorf("%w: key is empty", cache.ErrInvalidKey)
		}
		if n := len(c.prefix) + len(k); n > MaxKeyBytes {
			return fmt.Errorf("%w: %d bytes with the prefix, limit is %d", ErrKeyTooLong, n, MaxKeyBytes)
		}
	}
	return nil
}

func (c *Cache) valuePath(key string) string {
	return c.cf.AccountPath("/storage/kv/namespaces/", c.namespace) + "/values/" + url.PathEscape(c.prefix+key)
}

func (c *Cache) metadataPath(key string) string {
	return c.cf.AccountPath("/storage/kv/namespaces/", c.namespace) + "/metadata/" + url.PathEscape(c.prefix+key)
}

func (c *Cache) prefixed(keys []string) []string {
	if c.prefix == "" {
		return keys
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = c.prefix + k
	}
	return out
}

func (c *Cache) unprefixed(key string) string {
	return strings.TrimPrefix(key, c.prefix)
}

func (c *Cache) statusError(op, key string, resp *cloudflare.Response) error {
	var env struct {
		Errors []cloudflare.Error `json:"errors"`
	}
	_ = json.Unmarshal(resp.Body, &env)
	apiErr := &cloudflare.APIError{
		Status: resp.Status,
		Method: op,
		Path:   key,
		Errors: env.Errors,
		Body:   resp.Body,
	}
	if resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden {
		apiErr.Sentinel = cloudflare.ErrUnauthorized
	}
	return apiErr
}

func (c *Cache) emit(ctx context.Context, op, key string, start time.Time, err *error) {
	var failed error
	if err != nil {
		failed = *err
	}
	drops.CallHook(c.hook, ctx, drops.QueryEvent{
		Kind:     op,
		SQL:      key,
		Duration: time.Since(start),
		Err:      failed,
	})
}

func chunkStrings(in []string, n int) [][]string {
	if len(in) <= n {
		return [][]string{in}
	}
	var out [][]string
	for i := 0; i < len(in); i += n {
		end := i + n
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[i:end])
	}
	return out
}

func chunkEntries[T any](in []T, n int) [][]T {
	if len(in) <= n {
		return [][]T{in}
	}
	var out [][]T
	for i := 0; i < len(in); i += n {
		end := i + n
		if end > len(in) {
			end = len(in)
		}
		out = append(out, in[i:end])
	}
	return out
}
