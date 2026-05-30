package cache

import (
	"context"
	"errors"
	"time"
)

// Cache is the minimal contract every backend satisfies. Payloads are
// []byte so the interface stays codec-agnostic; callers serialise with
// whatever they like (encoding/json, msgpack, protobuf, raw bytes).
//
// Implementations are safe for concurrent use by multiple goroutines.
type Cache interface {
	// Get returns the value for key. ErrNotFound is returned (and is
	// the ONLY way to distinguish "missing" from "empty value") when
	// no entry exists.
	Get(ctx context.Context, key string) ([]byte, error)

	// Set stores value under key. ttl=0 means "no expiry" (the entry
	// lives until evicted or deleted).
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error

	// Delete removes the listed keys and returns the number actually
	// removed (entries that did not exist count as 0).
	Delete(ctx context.Context, keys ...string) (int, error)

	// Exists reports whether the key has a live entry.
	Exists(ctx context.Context, key string) (bool, error)

	// TTL returns the remaining lifetime. The reserved values are:
	//   -1 — key exists but has no expiry
	//    0 — key does not exist (also returns ErrNotFound)
	TTL(ctx context.Context, key string) (time.Duration, error)

	// Ping verifies the backend is reachable. Suitable as a readiness
	// probe shape.
	Ping(ctx context.Context) error

	// Close releases the underlying resources (connections, file
	// handles, goroutines). Idempotent.
	Close() error
}

// MultiCache extends Cache with bulk operations. Backends that can't
// implement them more efficiently than a loop over Get/Set should NOT
// satisfy this interface — callers that depend on the batch shape will
// then fall back to the loop and the round-trip cost is at least
// visible at the call site.
type MultiCache interface {
	Cache

	// GetMulti returns the entries that were found, keyed by their
	// cache key. Missing keys are simply absent from the result map.
	GetMulti(ctx context.Context, keys ...string) (map[string][]byte, error)

	// SetMulti stores the supplied items in a single round-trip. All
	// entries get the same TTL.
	SetMulti(ctx context.Context, items map[string][]byte, ttl time.Duration) error
}

// Item is a key/value pair with an optional per-item TTL; useful for
// helpers that take heterogeneous batches.
type Item struct {
	Key   string
	Value []byte
	TTL   time.Duration
}

// Sentinel errors. errors.Is works against the wrapped instances each
// backend may produce.
var (
	// ErrNotFound is returned by Get / TTL when the key is absent.
	ErrNotFound = errors.New("cache: key not found")

	// ErrClosed is returned by every method on a closed Cache.
	ErrClosed = errors.New("cache: closed")

	// ErrInvalidKey is returned when a key fails per-backend
	// validation (empty, too long, illegal bytes, etc.).
	ErrInvalidKey = errors.New("cache: invalid key")
)
