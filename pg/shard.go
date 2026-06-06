package pg

import (
	"context"
	"errors"
	"sync"

	"github.com/bernardoforcillo/drops"
)

// Sharded routes every query to one of N underlying drivers based
// on a shard key the caller stamps on ctx. This is the opposite
// end of the Replicated wrapper: where Replicated splits reads
// across replicas of a single dataset, Sharded splits the entire
// dataset across N independent primaries. Use it to scale beyond
// what a single primary's write throughput allows — typical
// shard axes are user id (social-feed services), chat id
// (messaging), or geographic region (mobility / dispatch).
//
//	shards := []drops.Driver{db1, db2, db3, db4}
//	sharded := pg.NewSharded(shards, func(key any) int {
//	    return int(key.(int64) % int64(len(shards)))
//	})
//	db := pg.New(sharded)
//
//	ctx = pg.WithShardKey(ctx, userID)
//	got, err := UserEntity.Get(db, ctx, userID)
//	// the shard for userID handles the query
//
// Missing shard key on ctx returns ErrShardKeyMissing — the bad
// code path fails closed (a "default to shard 0" policy would
// silently route writes to the wrong shard and leak data across
// customers). Cross-shard queries need explicit opt-in via
// ForEachShard.
//
// Transactions stay on a single shard — a tx that spans shards
// would need 2PC and drops doesn't model it. Begin uses the
// shard for ctx's shard key.
type Sharded struct {
	shards []drops.Driver
	pick   func(key any) int
}

// NewSharded wires the shards with a picker that maps a key to
// an index in [0, len(shards)). The picker is called for every
// query; keep it cheap. Drops does not enforce that the picker
// is deterministic, but non-determinism causes data corruption,
// so don't.
func NewSharded(shards []drops.Driver, pick func(key any) int) *Sharded {
	if len(shards) == 0 {
		panic("drops/pg: NewSharded requires at least one shard")
	}
	if pick == nil {
		panic("drops/pg: NewSharded requires a non-nil pick function")
	}
	return &Sharded{shards: shards, pick: pick}
}

// ErrShardKeyMissing is returned when a sharded driver receives
// a request without a shard key on ctx. Surfacing this rather
// than picking a default prevents silent cross-shard data leaks.
var ErrShardKeyMissing = errors.New("drops/pg: shard key missing; call pg.WithShardKey first")

type shardCtxKey int

const shardKeyKey shardCtxKey = 1

// WithShardKey annotates ctx with the value the picker uses to
// choose a shard. Typically the user id, customer id, or
// geographic region.
func WithShardKey(ctx context.Context, key any) context.Context {
	return context.WithValue(ctx, shardKeyKey, key)
}

// ShardKeyFrom returns the shard key stored on ctx, if any.
func ShardKeyFrom(ctx context.Context) (any, bool) {
	v := ctx.Value(shardKeyKey)
	return v, v != nil
}

// shardForCtx returns the shard chosen by the picker for ctx's
// shard key, plus an error when the key is missing.
func (s *Sharded) shardForCtx(ctx context.Context) (drops.Driver, error) {
	key, ok := ShardKeyFrom(ctx)
	if !ok {
		return nil, ErrShardKeyMissing
	}
	idx := s.pick(key)
	if idx < 0 || idx >= len(s.shards) {
		return nil, errors.New("drops/pg: shard picker returned out-of-range index")
	}
	return s.shards[idx], nil
}

// Exec routes to the ctx's shard.
func (s *Sharded) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	drv, err := s.shardForCtx(ctx)
	if err != nil {
		return nil, err
	}
	return drv.Exec(ctx, sql, args...)
}

// Query routes to the ctx's shard.
func (s *Sharded) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	drv, err := s.shardForCtx(ctx)
	if err != nil {
		return nil, err
	}
	return drv.Query(ctx, sql, args...)
}

// Begin opens a transaction on the ctx's shard. The tx stays
// on that shard for its lifetime — drops does not model cross-
// shard 2PC.
func (s *Sharded) Begin(ctx context.Context) (drops.Tx, error) {
	drv, err := s.shardForCtx(ctx)
	if err != nil {
		return nil, err
	}
	return drv.Begin(ctx)
}

// Shards returns a copy of the underlying drivers in declaration
// order. Used by ForEachShard / fan-out workflows.
func (s *Sharded) Shards() []drops.Driver {
	out := make([]drops.Driver, len(s.shards))
	copy(out, s.shards)
	return out
}

// Close closes every shard that exposes a Close method,
// returning the first error encountered.
func (s *Sharded) Close() error {
	type closer interface{ Close() error }
	var firstErr error
	for _, sh := range s.shards {
		if c, ok := sh.(closer); ok {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ForEachShard runs fn against every shard concurrently — the
// "scatter" half of scatter/gather. Use for cross-shard reads /
// admin tasks; results are aggregated by the caller. Errors are
// collected and returned per-shard.
//
// The supplied ctx is passed to fn unchanged; if fn needs the
// shard-bound *DB it should call pg.New(drv) inside.
func ForEachShard(s *Sharded, ctx context.Context, fn func(shardIdx int, drv drops.Driver) error) []error {
	errs := make([]error, len(s.shards))
	var wg sync.WaitGroup
	for i, drv := range s.shards {
		wg.Add(1)
		go func(idx int, d drops.Driver) {
			defer wg.Done()
			errs[idx] = fn(idx, d)
		}(i, drv)
	}
	wg.Wait()
	return errs
}

// HashShardKey is a convenience picker for string / []byte keys.
// Uses FNV-1a (the same hash as advisory locks) and modulo N.
//
//	sharded := pg.NewSharded(shards, pg.HashShardKey(len(shards)))
func HashShardKey(n int) func(key any) int {
	return func(key any) int {
		if n <= 0 {
			return 0
		}
		var s string
		switch v := key.(type) {
		case string:
			s = v
		case []byte:
			s = string(v)
		default:
			// Fall through to default fmt — covers ints / uuid
			// strings / structs implementing Stringer.
			s = stringOf(v)
		}
		h := int(uint64(lockKey(s)) % uint64(n))
		return h
	}
}

// stringOf renders any to a canonical string for hashing. Kept
// simple — relies on fmt for everything that isn't already a
// string.
func stringOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case int:
		return intStr(int64(x))
	case int64:
		return intStr(x)
	}
	return intStr(int64(0)) + "·" + interfaceStringFallback(v)
}

func intStr(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// interfaceStringFallback uses fmt's %v for any type without a
// dedicated case. Kept off the hot path so the typical
// string/int dispatch stays allocation-free.
func interfaceStringFallback(v any) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	return "" // unhashable types fall to bucket 0
}
