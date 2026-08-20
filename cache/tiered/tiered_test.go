package tiered_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/memory"
	"github.com/bernardoforcillo/drops/cache/tiered"
)

func newTiered(l1ttl time.Duration) (*tiered.Cache, *memory.Cache, *memory.Cache) {
	l1 := memory.New()
	l2 := memory.New()
	c := tiered.New(tiered.Options{L1: l1, L2: l2, L1TTL: l1ttl})
	return c, l1, l2
}

func TestReadThroughBackfillsL1(t *testing.T) {
	ctx := context.Background()
	c, l1, l2 := newTiered(time.Minute)

	// Seed only L2.
	if err := l2.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := l1.Len(); n != 0 {
		t.Fatalf("L1 should be empty, has %d", n)
	}

	v, err := c.Get(ctx, "k")
	if err != nil || string(v) != "v" {
		t.Fatalf("Get: %q %v", v, err)
	}
	// L2 hit must have backfilled L1.
	if got, err := l1.Get(ctx, "k"); err != nil || string(got) != "v" {
		t.Errorf("L1 not backfilled: %q %v", got, err)
	}
}

func TestWriteThroughFansOut(t *testing.T) {
	ctx := context.Background()
	c, l1, l2 := newTiered(0)

	if err := c.Set(ctx, "k", []byte("v"), time.Minute); err != nil {
		t.Fatal(err)
	}
	for name, tier := range map[string]cache.Cache{"L1": l1, "L2": l2} {
		if v, err := tier.Get(ctx, "k"); err != nil || string(v) != "v" {
			t.Errorf("%s missing value: %q %v", name, v, err)
		}
	}
}

func TestL1TTLIsCapped(t *testing.T) {
	ctx := context.Background()
	c, l1, l2 := newTiered(10 * time.Second)

	if err := c.Set(ctx, "k", []byte("v"), time.Hour); err != nil {
		t.Fatal(err)
	}
	l1ttl, _ := l1.TTL(ctx, "k")
	if l1ttl > 10*time.Second || l1ttl <= 0 {
		t.Errorf("L1 TTL should be capped at 10s, got %v", l1ttl)
	}
	l2ttl, _ := l2.TTL(ctx, "k")
	if l2ttl < 59*time.Minute {
		t.Errorf("L2 TTL should keep the full hour, got %v", l2ttl)
	}
}

func TestDeleteClearsBothTiers(t *testing.T) {
	ctx := context.Background()
	c, l1, l2 := newTiered(0)
	_ = c.Set(ctx, "k", []byte("v"), time.Minute)

	if _, err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := l1.Exists(ctx, "k"); ok {
		t.Error("L1 still has key")
	}
	if ok, _ := l2.Exists(ctx, "k"); ok {
		t.Error("L2 still has key")
	}
}

func TestGetMissReturnsNotFound(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTiered(0)
	if _, err := c.Get(ctx, "absent"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestGetOrLoadSingleflight(t *testing.T) {
	ctx := context.Background()
	c, l1, l2 := newTiered(time.Minute)

	var loads int64
	load := func(context.Context) ([]byte, error) {
		atomic.AddInt64(&loads, 1)
		time.Sleep(20 * time.Millisecond) // widen the stampede window
		return []byte("loaded"), nil
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			v, err := c.GetOrLoad(ctx, "hot", time.Minute, load)
			if err != nil || string(v) != "loaded" {
				t.Errorf("GetOrLoad: %q %v", v, err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&loads); got != 1 {
		t.Errorf("stampede not collapsed: %d loads for %d concurrent callers", got, n)
	}
	// Both tiers should now hold the loaded value.
	if v, err := l1.Get(ctx, "hot"); err != nil || string(v) != "loaded" {
		t.Errorf("L1 not populated: %q %v", v, err)
	}
	if v, err := l2.Get(ctx, "hot"); err != nil || string(v) != "loaded" {
		t.Errorf("L2 not populated: %q %v", v, err)
	}

	// A second call is a pure cache hit — no further load.
	if _, err := c.GetOrLoad(ctx, "hot", time.Minute, load); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&loads); got != 1 {
		t.Errorf("cached call still triggered load: %d", got)
	}
}

func TestGetOrLoadPropagatesError(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTiered(0)
	sentinel := errors.New("origin down")
	_, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("want sentinel, got %v", err)
	}
	// Nothing should have been cached.
	if _, err := c.Get(ctx, "k"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("failed load should not cache: %v", err)
	}
}

func TestGetMultiMergesTiers(t *testing.T) {
	ctx := context.Background()
	c, l1, l2 := newTiered(time.Minute)

	_ = l1.Set(ctx, "a", []byte("1"), time.Minute)
	_ = l2.Set(ctx, "b", []byte("2"), time.Minute)
	// "c" is absent everywhere.

	got, err := c.GetMulti(ctx, "a", "b", "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got["a"]) != "1" || string(got["b"]) != "2" {
		t.Errorf("merged result wrong: %v", got)
	}
	// "b" came from L2 and should be backfilled into L1.
	if v, err := l1.Get(ctx, "b"); err != nil || string(v) != "2" {
		t.Errorf("L2 hit not backfilled to L1: %q %v", v, err)
	}
}

func TestNilTierPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil tier")
		}
	}()
	tiered.New(tiered.Options{L1: memory.New()})
}

// hookedL2 wraps a far tier so a test can run beforeDelete at the exact
// instant the tiered cache reaches into L2.
type hookedL2 struct {
	cache.Cache
	beforeDelete func()
}

func (h *hookedL2) Delete(ctx context.Context, keys ...string) (int, error) {
	if h.beforeDelete != nil {
		h.beforeDelete()
	}
	return h.Cache.Delete(ctx, keys...)
}

func TestDeleteLeavesNoStaleL1Copy(t *testing.T) {
	ctx := context.Background()
	l1, l2 := memory.New(), memory.New()
	far := &hookedL2{Cache: l2}
	c := tiered.New(tiered.Options{L1: l1, L2: far, L1TTL: time.Minute})

	if err := c.Set(ctx, "k", []byte("stale"), 0); err != nil {
		t.Fatal(err)
	}
	// Evict the near copy so the interleaved reader has to consult L2,
	// which is what makes it able to resurrect the value.
	if _, err := l1.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}

	// A reader that lands inside the invalidation: it misses L1, finds
	// L2 still holding the pre-delete value, and backfills it.
	far.beforeDelete = func() {
		if v, err := c.Get(ctx, "k"); err != nil || string(v) != "stale" {
			t.Errorf("interleaved read: %q %v", v, err)
		}
	}

	if _, err := c.Delete(ctx, "k"); err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Get(ctx, "k"); !errors.Is(err, cache.ErrNotFound) {
		t.Error("a read interleaved with Delete resurrected the value in L1")
	}
	if _, err := c.Get(ctx, "k"); !errors.Is(err, cache.ErrNotFound) {
		t.Error("deleted key still readable")
	}
}

func TestGetOrLoadSurvivesAPanickingLoad(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTiered(0)

	func() {
		defer func() { _ = recover() }()
		_, _ = c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
			panic("origin exploded")
		})
	}()

	done := make(chan error, 1)
	go func() {
		v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
			return []byte("v"), nil
		})
		if err == nil && string(v) != "v" {
			err = errors.New("wrong value: " + string(v))
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a panicking load wedged the key: every later caller blocks forever")
	}
}

func TestGetOrLoadCopiesBeforeTheLoadedArrayEscapes(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTiered(time.Minute)

	// Big enough that a caller scribbling over the array while another is
	// copying it leaves a visible tear rather than a lucky miss.
	const size = 1 << 20
	joined := make(chan struct{})
	waiterDone := make(chan struct{})

	go func() {
		v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
			close(joined)
			time.Sleep(50 * time.Millisecond)
			return bytes.Repeat([]byte{'a'}, size), nil
		})
		if err != nil {
			t.Error(err)
			return
		}
		// The caller that ran the load starts using its value at once —
		// decoding in place, appending, whatever it does with the bytes.
		for i := 0; i < 200; i++ {
			for j := range v {
				v[j] = 'b'
			}
			select {
			case <-waiterDone:
				return
			default:
			}
		}
	}()

	<-joined
	v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
		t.Error("second caller ran its own load")
		return nil, nil
	})
	close(waiterDone)
	if err != nil {
		t.Fatal(err)
	}
	if i := bytes.IndexByte(v, 'b'); i >= 0 {
		t.Errorf("value torn at byte %d: the loader's array escaped to the caller that ran it, "+
			"which mutated it while this one was still copying", i)
	}
}

func TestGetOrLoadGivesEachCallerItsOwnValue(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTiered(time.Minute)

	loading := make(chan struct{})
	release := make(chan struct{})
	type result struct {
		v   []byte
		err error
	}
	res := make(chan result, 2)

	go func() {
		v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
			close(loading)
			<-release
			return []byte("original"), nil
		})
		res <- result{v, err}
	}()
	<-loading
	go func() {
		v, err := c.GetOrLoad(ctx, "k", time.Minute, func(context.Context) ([]byte, error) {
			t.Error("second caller ran its own load")
			return nil, nil
		})
		res <- result{v, err}
	}()
	time.Sleep(50 * time.Millisecond) // let the second caller join the flight
	close(release)

	a, b := <-res, <-res
	if a.err != nil || b.err != nil {
		t.Fatalf("GetOrLoad: %v / %v", a.err, b.err)
	}
	a.v[0] = 'X'
	if string(b.v) != "original" {
		t.Errorf("callers share one backing array; a mutation leaked: %q", b.v)
	}
}
