package pg

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Replicated wraps a primary driver and any number of read-only
// replicas. Exec / Begin always go to the primary; Query is routed
// round-robin to a replica unless the caller is inside a
// read-your-writes window (see WithReadYourWrites), in which case
// reads stick to the primary so a follow-up SELECT after a write
// observes the new state.
//
// It implements drops.Driver, so you wire it into a regular *pg.DB:
//
//	primary  := stdlib.New(primarySQL)
//	r1       := stdlib.New(replica1SQL)
//	r2       := stdlib.New(replica2SQL)
//	repl     := pg.NewReplicated(primary, r1, r2)
//	db       := pg.New(repl)
//
//	// Sticky read-your-writes: any Exec on ctx starts a 2s window;
//	// follow-up Queries stay on the primary for that window so
//	// the read observes the prior write.
//	ctx = pg.WithReadYourWrites(ctx, 2*time.Second)
//	_ = UserEntity.Update(db, ctx, &u)
//	got, _ := UserEntity.Get(db, ctx, u.ID)  // hits primary
//
// Without replicas, Replicated is identical to wrapping primary
// directly — Query falls back to the primary.
type Replicated struct {
	primary  drops.Driver
	replicas []drops.Driver
	cursor   atomic.Uint64
}

// NewReplicated returns a Replicated driver. Pass zero replicas for
// a primary-only setup that still respects WithReadYourWrites.
func NewReplicated(primary drops.Driver, replicas ...drops.Driver) *Replicated {
	if primary == nil {
		panic("drops/pg: NewReplicated primary cannot be nil")
	}
	return &Replicated{primary: primary, replicas: replicas}
}

// pickReplica returns the next replica via round-robin, or the
// primary when no replica is configured. Atomic counter avoids
// contention even under heavy fan-out.
func (r *Replicated) pickReplica() drops.Driver {
	if len(r.replicas) == 0 {
		return r.primary
	}
	idx := r.cursor.Add(1) - 1
	return r.replicas[idx%uint64(len(r.replicas))]
}

// Exec routes through the primary. The call also re-arms any
// read-your-writes window attached to ctx so subsequent reads on the
// same context stay sticky.
func (r *Replicated) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	if s, ok := readYourWrites(ctx); ok {
		s.mark()
	}
	return r.primary.Exec(ctx, sql, args...)
}

// Query routes to a replica unless ctx is inside an active
// read-your-writes window, in which case the primary serves the
// read for consistency.
func (r *Replicated) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	if s, ok := readYourWrites(ctx); ok && s.active() {
		return r.primary.Query(ctx, sql, args...)
	}
	return r.pickReplica().Query(ctx, sql, args...)
}

// Begin opens a transaction on the primary. Transactional reads
// share the same connection as the writes, so consistency is
// guaranteed without needing the read-your-writes window.
func (r *Replicated) Begin(ctx context.Context) (drops.Tx, error) {
	return r.primary.Begin(ctx)
}

// Primary returns the primary driver, useful for code paths that
// must opt out of replica routing.
func (r *Replicated) Primary() drops.Driver { return r.primary }

// Replicas returns the configured replicas in declaration order.
func (r *Replicated) Replicas() []drops.Driver {
	out := make([]drops.Driver, len(r.replicas))
	copy(out, r.replicas)
	return out
}

// Close closes every underlying driver that exposes Close, returning
// the first error encountered.
func (r *Replicated) Close() error {
	type closer interface{ Close() error }
	var firstErr error
	if c, ok := r.primary.(closer); ok {
		if err := c.Close(); err != nil {
			firstErr = err
		}
	}
	for _, rp := range r.replicas {
		if c, ok := rp.(closer); ok {
			if err := c.Close(); firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ----------------------------------------------------------------------
// Read-your-writes context window
// ----------------------------------------------------------------------

type rywContextKey int

const rywKey rywContextKey = 1

// rywState tracks the expiry of a per-context read-your-writes
// window. Reset by each write; checked by each read.
type rywState struct {
	duration time.Duration
	mu       sync.Mutex
	until    time.Time
}

func (s *rywState) mark() {
	s.mu.Lock()
	s.until = time.Now().Add(s.duration)
	s.mu.Unlock()
}

func (s *rywState) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.until.IsZero() && time.Now().Before(s.until)
}

// WithReadYourWrites annotates ctx with a sticky read-your-writes
// window. The window arms automatically on the first Exec /
// Replicated.Exec through this context and persists for d after
// every subsequent write. Reads done within the window go to the
// primary so callers always observe their own writes.
//
// Pass d=0 to clear the window from a derived context.
func WithReadYourWrites(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, rywKey, &rywState{duration: d})
}

// readYourWrites fetches the window state from ctx, if any.
func readYourWrites(ctx context.Context) (*rywState, bool) {
	s, ok := ctx.Value(rywKey).(*rywState)
	return s, ok && s != nil
}

// ErrNoReplicas is returned by NewReplicated when called with a nil
// primary. Kept as a package-level value so handlers and config
// loaders can branch cleanly.
var ErrNoReplicas = errors.New("drops/pg: no replicas configured")
