package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/cache"
	"github.com/bernardoforcillo/drops/cache/memory"
)

func TestGetSetDelete(t *testing.T) {
	c := memory.New()
	defer c.Close()
	ctx := context.Background()

	if _, err := c.Get(ctx, "missing"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get(missing): %v, want ErrNotFound", err)
	}
	if err := c.Set(ctx, "k", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "v" {
		t.Errorf("Get = %q, want v", got)
	}
	ok, err := c.Exists(ctx, "k")
	if err != nil || !ok {
		t.Errorf("Exists = %v, %v", ok, err)
	}
	n, err := c.Delete(ctx, "k", "missing")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("Delete returned %d, want 1", n)
	}
	if _, err := c.Get(ctx, "k"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("Get after Delete: %v", err)
	}
}

func TestTTLAndExpiry(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := &mockClock{t: now}
	c := memory.New(memory.Options{Clock: clock.now})
	defer c.Close()
	ctx := context.Background()

	if err := c.Set(ctx, "k", []byte("v"), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	d, err := c.TTL(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if d <= 0 || d > 50*time.Millisecond {
		t.Errorf("TTL = %s, want (0, 50ms]", d)
	}
	// Advance past the expiry; Get should now report ErrNotFound and
	// TTL should return 0 + ErrNotFound.
	clock.advance(60 * time.Millisecond)
	if _, err := c.Get(ctx, "k"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("expired Get: %v", err)
	}
	if _, err := c.TTL(ctx, "k"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("expired TTL: %v", err)
	}
}

func TestNoExpiryReturnsMinusOne(t *testing.T) {
	c := memory.New()
	defer c.Close()
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 0)
	d, err := c.TTL(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if d != -1 {
		t.Errorf("TTL for non-expiring = %s, want -1", d)
	}
}

func TestEvictionRespectsMaxEntries(t *testing.T) {
	c := memory.New(memory.Options{MaxEntries: 3})
	defer c.Close()
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c", "d"} {
		_ = c.Set(ctx, k, []byte("v"), 0)
	}
	if l := c.Len(); l != 3 {
		t.Errorf("Len = %d, want 3", l)
	}
	// "a" was inserted first and should have been evicted.
	if _, err := c.Get(ctx, "a"); !errors.Is(err, cache.ErrNotFound) {
		t.Errorf("a should have been evicted, got %v", err)
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, err := c.Get(ctx, k); err != nil {
			t.Errorf("Get(%q): %v", k, err)
		}
	}
}

func TestJanitorSweepsExpired(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := &mockClock{t: now}
	c := memory.New(memory.Options{
		SweepEvery: 10 * time.Millisecond,
		Clock:      clock.now,
	})
	defer c.Close()
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 1*time.Millisecond)
	// Wait long enough for the janitor to wake; meanwhile advance the
	// virtual clock so the expiry has passed.
	clock.advance(50 * time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	if l := c.Len(); l != 0 {
		t.Errorf("janitor didn't clear expired entries; Len = %d", l)
	}
}

func TestGetMultiAndSetMulti(t *testing.T) {
	c := memory.New()
	defer c.Close()
	ctx := context.Background()
	if err := c.SetMulti(ctx, map[string][]byte{
		"a": []byte("1"),
		"b": []byte("2"),
		"c": []byte("3"),
	}, 0); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetMulti(ctx, "a", "b", "missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || string(got["a"]) != "1" || string(got["b"]) != "2" {
		t.Errorf("GetMulti = %v", got)
	}
}

func TestDefensiveCopyOnGet(t *testing.T) {
	c := memory.New()
	defer c.Close()
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("original"), 0)
	got, _ := c.Get(ctx, "k")
	got[0] = 'X' // mutate caller copy
	again, _ := c.Get(ctx, "k")
	if string(again) != "original" {
		t.Errorf("Get returned a shared slice; mutation leaked: %q", again)
	}
}

func TestCloseRejectsSubsequentCalls(t *testing.T) {
	c := memory.New()
	c.Close()
	if _, err := c.Get(context.Background(), "k"); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Get after Close: %v", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, cache.ErrClosed) {
		t.Errorf("Ping after Close: %v", err)
	}
}

func TestEmptyKeyRejected(t *testing.T) {
	c := memory.New()
	defer c.Close()
	if err := c.Set(context.Background(), "", []byte("v"), 0); !errors.Is(err, cache.ErrInvalidKey) {
		t.Errorf("Set empty key: %v", err)
	}
}

func TestHookFiresPerOperation(t *testing.T) {
	var (
		mu     sync.Mutex
		events []drops.QueryEvent
	)
	c := memory.New(memory.Options{
		Hook: func(_ context.Context, e drops.QueryEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
	})
	defer c.Close()
	ctx := context.Background()
	_ = c.Set(ctx, "k", []byte("v"), 0)
	_, _ = c.Get(ctx, "k")
	_, _ = c.Delete(ctx, "k")

	mu.Lock()
	defer mu.Unlock()
	kinds := make([]string, len(events))
	for i, e := range events {
		kinds[i] = e.Kind
	}
	want := []string{"cache.set", "cache.get", "cache.del"}
	if len(kinds) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(kinds), len(want), kinds)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("event[%d] kind = %q, want %q", i, kinds[i], want[i])
		}
	}
}

func TestConcurrentAccessIsRaceFree(t *testing.T) {
	c := memory.New(memory.Options{MaxEntries: 100})
	defer c.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := string(rune('a'+(id%26))) + "x"
				_ = c.Set(ctx, k, []byte("v"), 0)
				_, _ = c.Get(ctx, k)
				_, _ = c.Delete(ctx, k)
			}
		}(w)
	}
	wg.Wait()
}

// --- helpers --------------------------------------------------------

type mockClock struct {
	mu sync.Mutex
	t  time.Time
}

func (m *mockClock) now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.t
}

func (m *mockClock) advance(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.t = m.t.Add(d)
}
