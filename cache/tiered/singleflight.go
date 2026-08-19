package tiered

import (
	"fmt"
	"sync"
)

// singleflight deduplicates concurrent calls keyed by a string: while one
// call for a key is in flight, other calls for the same key block and
// share its result instead of each doing the work. It is a tiny, zero-dep
// analogue of golang.org/x/sync/singleflight — enough for cache stampede
// protection, no external dependency.
type singleflight struct {
	mu sync.Mutex
	m  map[string]*sfCall
}

type sfCall struct {
	wg  sync.WaitGroup
	val []byte
	err error
}

// Do runs fn for key, ensuring only one execution is in flight for a
// given key at a time. Duplicate callers wait for the original to
// complete and receive the same results. shared reports whether the
// result was shared with at least one other caller.
func (g *singleflight) Do(key string, fn func() ([]byte, error)) (val []byte, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*sfCall{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := new(sfCall)
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	// The teardown runs even when fn panics. Left to the happy path it
	// would be skipped, and the abandoned in-flight record then blocks
	// every later caller for that key forever — one panicking loader
	// wedging a hot key for the life of the process. Waiters are handed
	// the panic as an error so they don't read the zero value as a
	// successful empty result; the caller that ran fn still panics.
	defer func() {
		r := recover()
		if r != nil {
			c.err = fmt.Errorf("cache/tiered: load panicked: %v", r)
		}
		c.wg.Done()
		g.mu.Lock()
		delete(g.m, key)
		g.mu.Unlock()
		if r != nil {
			panic(r)
		}
	}()

	c.val, c.err = fn()
	return c.val, c.err, false
}
