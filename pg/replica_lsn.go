package pg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bernardoforcillo/drops"
)

// LSN-based read-your-writes — the upgrade over the time-based
// stickiness baked into Replicated.
//
// Time-based RYW (the original WithReadYourWrites) sends every read
// to the primary for a fixed window after a write. That's correct
// but overpays — the primary serves reads that the replica might
// already have replayed long ago. With LSN tracking, drops captures
// the primary's WAL position at write time and routes follow-up
// reads to the first replica that has replayed past that point,
// only falling back to primary when no replica has caught up yet.
//
//	repl := pg.NewReplicated(primary, r1, r2).WithLSNTracking(50*time.Millisecond)
//	db   := pg.New(repl)
//
//	ctx = pg.WithReadYourWrites(ctx, 2*time.Second)
//	_ = UserEntity.Update(db, ctx, &u)  // captures pg_current_wal_lsn()
//	got, _ := UserEntity.Get(db, ctx, u.ID)
//	// → routes to whichever replica has replay_lsn >= captured, else primary
//
// The TTL passed to WithLSNTracking is the maximum age of a cached
// replica LSN reading. Lower values catch up faster (more lag-aware
// routing) at the cost of more pg_last_wal_replay_lsn round-trips.

// WithLSNTracking enables LSN-based replica routing on r. TTL is the
// max age of cached per-replica LSN samples; a query asks a replica
// for a fresh value when its cache entry is older than TTL.
//
// Without LSN tracking, the read-your-writes window falls back to
// the time-based stickiness inherited from Replicated.
func (r *Replicated) WithLSNTracking(ttl time.Duration) *Replicated {
	if ttl <= 0 {
		ttl = 100 * time.Millisecond
	}
	r.lsnTTL = ttl
	if r.lsnCache == nil {
		r.lsnCache = &sync.Map{}
	}
	r.lsnEnabled = true
	return r
}

// LSNTracking reports whether LSN routing is active.
func (r *Replicated) LSNTracking() bool { return r.lsnEnabled }

// captureWriteLSN records the primary's current WAL position into
// the RYW context state, so follow-up reads know which point to
// catch up to.
func (r *Replicated) captureWriteLSN(ctx context.Context, s *rywState) {
	if !r.lsnEnabled {
		return
	}
	lsn, err := queryLSN(ctx, r.primary, "SELECT pg_current_wal_lsn()::text")
	if err != nil {
		// LSN unavailable (not running PG, replica only, ...) —
		// silently keep the time-based fallback.
		return
	}
	s.mu.Lock()
	s.writeLSN = lsn
	s.mu.Unlock()
}

// pickLSNReplica returns the first replica whose replay LSN has
// caught up to required. Returns the primary when no replica has
// caught up — preserves the read-your-writes invariant at the
// cost of extra primary load.
func (r *Replicated) pickLSNReplica(ctx context.Context, required uint64) drops.Driver {
	if len(r.replicas) == 0 {
		return r.primary
	}
	start := int(r.cursor.Add(1) - 1)
	for i := 0; i < len(r.replicas); i++ {
		idx := (start + i) % len(r.replicas)
		rep := r.replicas[idx]
		lsn, err := r.replicaLSN(ctx, rep)
		if err == nil && lsn >= required {
			return rep
		}
	}
	return r.primary
}

// replicaLSN returns rep's last replayed LSN, served from cache
// when the entry is fresher than the configured TTL.
func (r *Replicated) replicaLSN(ctx context.Context, rep drops.Driver) (uint64, error) {
	if r.lsnCache == nil {
		return 0, errors.New("drops/pg: LSN cache not initialised")
	}
	if cached, ok := r.lsnCache.Load(rep); ok {
		e := cached.(lsnEntry)
		if time.Since(e.at) < r.lsnTTL {
			return e.lsn, nil
		}
	}
	lsn, err := queryLSN(ctx, rep, "SELECT pg_last_wal_replay_lsn()::text")
	if err != nil {
		return 0, err
	}
	r.lsnCache.Store(rep, lsnEntry{lsn: lsn, at: time.Now()})
	return lsn, nil
}

// queryLSN runs sql against drv and parses the single text result as
// a PG LSN ("X/Y" hex pair → uint64 byte position).
func queryLSN(ctx context.Context, drv drops.Driver, sql string) (uint64, error) {
	rows, err := drv.Query(ctx, sql)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, errors.New("drops/pg: LSN query returned no row")
	}
	var s string
	if err := rows.Scan(&s); err != nil {
		return 0, err
	}
	return ParseLSN(s)
}

// ParseLSN converts PG's "X/Y" hex-pair LSN text format into the
// underlying byte position. Returns an error on malformed input.
// Exposed so tests and tooling can build LSN values without going
// through the driver.
func ParseLSN(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("drops/pg: empty LSN")
	}
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("drops/pg: malformed LSN %q", s)
	}
	hi, err := strconv.ParseUint(parts[0], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("drops/pg: LSN high half: %w", err)
	}
	lo, err := strconv.ParseUint(parts[1], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("drops/pg: LSN low half: %w", err)
	}
	return hi<<32 | lo, nil
}

// FormatLSN renders an LSN back into PG's "X/Y" text shape — the
// inverse of ParseLSN.
func FormatLSN(lsn uint64) string {
	hi := lsn >> 32
	lo := lsn & 0xFFFFFFFF
	return fmt.Sprintf("%X/%X", hi, lo)
}

// lsnEntry is one cached per-replica LSN sample.
type lsnEntry struct {
	lsn uint64
	at  time.Time
}
