package memory_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cache/memory"
)

// The janitor is the only goroutine in the cache backends that a
// caller starts without asking for it: New spawns it whenever
// SweepEvery is set, and the only thing that can stop it is Close.
// A cache built per tenant, or per test, therefore leaks a goroutine
// and its ticker for the life of the process if Close does not
// actually reach it — the kind of leak that shows up as a slow climb
// in a runtime metric and is never traced back to a cache.
func TestJanitorStopsOnClose(t *testing.T) {
	var ticks atomic.Int64
	c := memory.New(memory.Options{
		SweepEvery: time.Millisecond,
		// The janitor reads the clock once per sweep and nothing else
		// in this test touches the cache, so the clock is the sweep
		// counter.
		Clock: func() time.Time {
			ticks.Add(1)
			return time.Now()
		},
	})

	deadline := time.Now().Add(time.Second)
	for ticks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if ticks.Load() == 0 {
		t.Fatal("the janitor never swept; the test proves nothing")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A sweep already under way when Close lands is allowed to finish
	// — it reads the clock before it takes the lock, and finds an
	// emptied map when it gets there. What must not happen is another
	// one after that, which is what a janitor still on its ticker
	// would do fifty more times in the window below.
	time.Sleep(30 * time.Millisecond)
	after := ticks.Load()
	time.Sleep(50 * time.Millisecond)
	if got := ticks.Load(); got != after {
		t.Errorf("the janitor swept %d more times after Close, want 0", got-after)
	}
}

// Close is documented as rejecting subsequent calls, and a cache with
// both an explicit Close and a deferred one is ordinary. Closing the
// janitor's stop channel twice would panic, so the guard has to be on
// the close itself and not only on the state it sets.
func TestCloseIsIdempotent(t *testing.T) {
	c := memory.New(memory.Options{SweepEvery: time.Millisecond})
	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := c.Get(context.Background(), "k"); err == nil {
		t.Error("Get after Close should fail")
	}
}

// With no sweep interval there is no goroutine at all, which is the
// documented default — worth pinning because "Close stops the janitor"
// must not become "Close panics when there was not one".
func TestCloseWithoutJanitor(t *testing.T) {
	c := memory.New()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
