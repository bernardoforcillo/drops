package pg_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// taggedShard records its index on every call.
type taggedShard struct {
	idx    int
	calls  atomic.Int32
	reads  atomic.Int32
	begins atomic.Int32
}

func (t *taggedShard) Exec(context.Context, string, ...any) (drops.Result, error) {
	t.calls.Add(1)
	return nil, nil
}
func (t *taggedShard) Query(context.Context, string, ...any) (drops.Rows, error) {
	t.reads.Add(1)
	return &fakeRows{}, nil
}
func (t *taggedShard) Begin(context.Context) (drops.Tx, error) {
	t.begins.Add(1)
	return nil, nil
}

func newShards(n int) []*taggedShard {
	out := make([]*taggedShard, n)
	for i := range out {
		out[i] = &taggedShard{idx: i}
	}
	return out
}

func toDrivers(shards []*taggedShard) []drops.Driver {
	out := make([]drops.Driver, len(shards))
	for i, s := range shards {
		out[i] = s
	}
	return out
}

func TestShardedRoutesByKey(t *testing.T) {
	shards := newShards(4)
	sharded := pg.NewSharded(toDrivers(shards), func(key any) int {
		return int(key.(int64) % 4)
	})
	db := pg.New(sharded)

	// userID=7 → shard 7 % 4 = 3
	ctx := pg.WithShardKey(context.Background(), int64(7))
	_, _ = db.Exec(ctx, "INSERT ...")
	if shards[3].calls.Load() != 1 {
		t.Errorf("expected shard 3 to handle the write, got distribution %v",
			[]int32{shards[0].calls.Load(), shards[1].calls.Load(), shards[2].calls.Load(), shards[3].calls.Load()})
	}

	// userID=10 → shard 2
	ctx10 := pg.WithShardKey(context.Background(), int64(10))
	_, _ = db.Query(ctx10, "SELECT ...")
	if shards[2].reads.Load() != 1 {
		t.Errorf("expected shard 2 to handle the read")
	}
}

func TestShardedReturnsErrShardKeyMissing(t *testing.T) {
	shards := newShards(2)
	sharded := pg.NewSharded(toDrivers(shards), func(any) int { return 0 })
	db := pg.New(sharded)
	_, err := db.Exec(context.Background(), "INSERT ...")
	if !errors.Is(err, pg.ErrShardKeyMissing) {
		t.Errorf("expected ErrShardKeyMissing, got %v", err)
	}
	for _, s := range shards {
		if s.calls.Load() > 0 {
			t.Error("no shard should have been hit when key is missing")
		}
	}
}

func TestShardedBeginUsesShardKey(t *testing.T) {
	shards := newShards(2)
	sharded := pg.NewSharded(toDrivers(shards), func(key any) int {
		return int(key.(int64) % 2)
	})
	db := pg.New(sharded)
	ctx := pg.WithShardKey(context.Background(), int64(3)) // → shard 1
	_, _, _ = db.Begin(ctx)
	if shards[1].begins.Load() != 1 {
		t.Errorf("Begin should land on shard 1")
	}
}

func TestForEachShardFansOut(t *testing.T) {
	shards := newShards(3)
	sharded := pg.NewSharded(toDrivers(shards), func(any) int { return 0 })
	counts := make([]atomic.Int32, len(shards))
	errs := pg.ForEachShard(sharded, context.Background(), func(idx int, _ drops.Driver) error {
		counts[idx].Add(1)
		return nil
	})
	for _, e := range errs {
		if e != nil {
			t.Errorf("unexpected error: %v", e)
		}
	}
	for i := range counts {
		if counts[i].Load() != 1 {
			t.Errorf("shard %d not invoked", i)
		}
	}
}

func TestForEachShardCollectsErrors(t *testing.T) {
	shards := newShards(3)
	sharded := pg.NewSharded(toDrivers(shards), func(any) int { return 0 })
	boom := errors.New("boom")
	errs := pg.ForEachShard(sharded, context.Background(), func(idx int, _ drops.Driver) error {
		if idx == 1 {
			return boom
		}
		return nil
	})
	if !errors.Is(errs[1], boom) {
		t.Errorf("shard 1 error not propagated: %+v", errs)
	}
	if errs[0] != nil || errs[2] != nil {
		t.Errorf("other shards should not have errors: %+v", errs)
	}
}

func TestNewShardedPanicsOnEmpty(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewSharded(nil, ...) should panic")
		}
	}()
	_ = pg.NewSharded(nil, func(any) int { return 0 })
}

func TestHashShardKeyDistributes(t *testing.T) {
	pick := pg.HashShardKey(8)
	seen := map[int]int{}
	for i := int64(0); i < 1000; i++ {
		idx := pick(i)
		if idx < 0 || idx >= 8 {
			t.Fatalf("hash returned out-of-range %d", idx)
		}
		seen[idx]++
	}
	// All 8 buckets should see at least some traffic over 1000 keys.
	if len(seen) < 6 {
		t.Errorf("hash distribution is suspicious — only %d buckets used: %v", len(seen), seen)
	}
}

func TestShardedClose(t *testing.T) {
	shards := []drops.Driver{&closableTagged{}, &closableTagged{}}
	sharded := pg.NewSharded(shards, func(any) int { return 0 })
	if err := sharded.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	for i, s := range shards {
		if !s.(*closableTagged).closed {
			t.Errorf("shard %d not closed", i)
		}
	}
}

type closableTagged struct {
	closed bool
}

func (c *closableTagged) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}
func (c *closableTagged) Query(context.Context, string, ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (c *closableTagged) Begin(context.Context) (drops.Tx, error) { return nil, nil }
func (c *closableTagged) Close() error                            { c.closed = true; return nil }
