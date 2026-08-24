package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Replicated wraps a primary driver and any number of read-only
// replicas. Exec / Begin always go to the primary; so does a Query
// whose statement writes, since a RETURNING clause is issued through
// Query and a write does not belong on a replica whatever the routing
// would otherwise say. A Query that reads is routed round-robin to a
// replica unless the caller is inside a read-your-writes window (see
// WithReadYourWrites), in which case reads stick to the primary so a
// follow-up SELECT after a write observes the new state.
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
// A write inside a transaction arms the window too, at commit — the
// commit is the write, and the reads that follow it must observe it.
// [Replicated.WithWriteDelay] adds the unconditional part of the
// window, and [DB.InReadTx] makes "this only reads" something the
// server enforces rather than something the routing assumes.
//
// Without replicas, Replicated is identical to wrapping primary
// directly — Query falls back to the primary.
type Replicated struct {
	primary  drops.Driver
	replicas []drops.Driver
	cursor   atomic.Uint64

	// LSN-based read-your-writes (see WithLSNTracking). Fields
	// are populated lazily so the original time-only path
	// stays allocation-free.
	lsnEnabled bool
	lsnTTL     time.Duration
	lsnCache   *sync.Map // drops.Driver → lsnEntry

	// writeDelay is the unconditional post-write pin (see
	// WithWriteDelay). Zero leaves routing entirely to the window and
	// the LSN.
	writeDelay time.Duration
}

// NewReplicated returns a Replicated driver. Pass zero replicas for
// a primary-only setup that still respects WithReadYourWrites.
func NewReplicated(primary drops.Driver, replicas ...drops.Driver) *Replicated {
	if primary == nil {
		panic("drops/pg: NewReplicated primary cannot be nil")
	}
	return &Replicated{primary: primary, replicas: replicas}
}

// WithWriteDelay sets the post-write delay: for d after a write, this
// session's reads go to the primary regardless of how far the replicas
// have replayed. It is Rails' DatabaseSelector delay, and it is the
// blunt half of read-your-writes.
//
// drops already had the sticky half under another name —
// [WithReadYourWrites] is the window, armed by a write and checked by
// every read on the same context. What it did not have is a floor
// underneath [Replicated.WithLSNTracking]: once LSN routing is on, a
// read is free to go to any replica that has replayed past the write,
// which is correct for the row just written and says nothing about
// anything else the write touched — a trigger, a materialised view
// refresh, a row in another table the application will read next. The
// delay buys the time to stop reasoning about that, at the price of
// primary load.
//
// The delay lives inside the caller's window and widens it if it is
// longer: a session with a 1s window under a 2s delay reads from the
// primary for 2s. A context whose window is zero — the documented way
// to clear it — stays cleared, because a session that opted out is not
// somebody the driver should opt back in.
//
// Without LSN tracking the whole window already pins to the primary,
// so a delay shorter than the window changes nothing there.
func (r *Replicated) WithWriteDelay(d time.Duration) *Replicated {
	if d < 0 {
		d = 0
	}
	r.writeDelay = d
	return r
}

// WriteDelay reports the configured post-write delay.
func (r *Replicated) WriteDelay() time.Duration { return r.writeDelay }

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
// same context stay sticky — and, when LSN tracking is enabled,
// captures pg_current_wal_lsn() so reads can route to a caught-up
// replica instead of unconditionally pinning to primary.
func (r *Replicated) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	res, err := r.primary.Exec(ctx, sql, args...)
	if s, ok := readYourWrites(ctx); ok {
		s.mark()
		if err == nil {
			r.captureWriteLSN(ctx, s)
		}
	}
	return res, err
}

// Query routes to a replica unless ctx is inside an active
// read-your-writes window. With LSN tracking enabled, reads inside
// a window route to whichever replica has replayed past the captured
// write LSN — only falling back to primary when none have caught up.
// Without LSN tracking, the time-based stickiness pins reads to
// primary for the configured duration.
//
// A statement that writes goes to the primary whatever the window says,
// and arms the window exactly as [Replicated.Exec] does. Query is not
// only how reads are issued: every write carrying a RETURNING clause
// travels this way — InsertBuilder.Scan, UpdateBuilder.Scan and
// DeleteBuilder.Scan all call it — and routing one of those by where a
// read belongs sends an INSERT to a replica.
func (r *Replicated) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	if isWriteStatement(sql) {
		rows, err := r.primary.Query(ctx, sql, args...)
		if s, ok := readYourWrites(ctx); ok {
			s.mark()
			if err == nil {
				r.captureWriteLSN(ctx, s)
			}
		}
		return rows, err
	}
	return r.routeRead(ctx).Query(ctx, sql, args...)
}

// isWriteStatement reports whether a statement issued through Query
// modifies data. It reads the leading keyword rather than parsing SQL:
// SELECT, VALUES, TABLE and SHOW are reads, WITH and EXPLAIN are reads
// only when no data-modifying keyword appears anywhere in them (a
// data-modifying CTE and EXPLAIN ANALYZE both really do write), and
// everything else arriving through Query is treated as a write.
//
// A keyword scan will misjudge the exotic cases — an INSERT spelled
// inside a string literal in a CTE reads as a write. That is the side
// to be wrong on: a read misrouted to the primary costs capacity, a
// write misrouted to a replica costs data.
func isWriteStatement(sql string) bool {
	s := trimSQLPrefix(sql)
	switch strings.ToUpper(leadingWord(s)) {
	case "SELECT", "VALUES", "TABLE", "SHOW":
		return false
	case "WITH", "EXPLAIN":
		return hasDataModifyingKeyword(s)
	case "":
		return false
	}
	return true
}

// trimSQLPrefix drops leading whitespace and leading comments so the
// first keyword can be read. Statements arrive tagged with a trailing
// SQLCommenter comment, but nothing stops a caller writing a leading
// one by hand.
func trimSQLPrefix(s string) string {
	for {
		s = strings.TrimLeft(s, " \t\r\n")
		switch {
		case strings.HasPrefix(s, "--"):
			if i := strings.IndexByte(s, '\n'); i >= 0 {
				s = s[i+1:]
				continue
			}
			return ""
		case strings.HasPrefix(s, "/*"):
			if i := strings.Index(s, "*/"); i >= 0 {
				s = s[i+2:]
				continue
			}
			return ""
		}
		return s
	}
}

func leadingWord(s string) string {
	i := 0
	for i < len(s) && (isWordByte(s[i])) {
		i++
	}
	return s[:i]
}

func isWordByte(b byte) bool {
	return b == '_' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// hasDataModifyingKeyword scans for INSERT / UPDATE / DELETE / MERGE as
// whole words, so a column named updated_at does not read as an UPDATE.
func hasDataModifyingKeyword(s string) bool {
	up := strings.ToUpper(s)
	for _, kw := range [...]string{"INSERT", "UPDATE", "DELETE", "MERGE"} {
		for i := 0; ; {
			j := strings.Index(up[i:], kw)
			if j < 0 {
				break
			}
			at := i + j
			before := at == 0 || !isWordByte(up[at-1])
			end := at + len(kw)
			after := end == len(up) || !isWordByte(up[end])
			if before && after {
				return true
			}
			i = end
		}
	}
	return false
}

// routeRead decides which driver serves a read on ctx. It is the whole
// routing policy in one place, so Query and BeginReadOnly cannot drift
// apart on where a read belongs.
func (r *Replicated) routeRead(ctx context.Context) drops.Driver {
	s, ok := readYourWrites(ctx)
	if !ok {
		return r.pickReplica()
	}
	sticky := r.stickyFor(s)
	if !s.activeFor(sticky) {
		return r.pickReplica()
	}
	// The write delay is the unconditional part of the window: inside
	// it the primary serves the read whatever the replicas say about
	// their replay position.
	if s.activeFor(r.writeDelay) {
		return r.primary
	}
	if r.lsnEnabled {
		if required := s.requiredLSN(); required > 0 {
			return r.pickLSNReplica(ctx, required)
		}
	}
	return r.primary
}

// stickyFor is how long this session's reads stay off the replicas
// after a write: the caller's window, widened to the driver's write
// delay when that is longer. A context whose window is zero has
// explicitly cleared itself and the driver's delay must not resurrect
// it.
func (r *Replicated) stickyFor(s *rywState) time.Duration {
	if s.duration <= 0 {
		return 0
	}
	if r.writeDelay > s.duration {
		return r.writeDelay
	}
	return s.duration
}

// Begin opens a transaction on the primary. Transactional reads
// share the same connection as the writes, so consistency is
// guaranteed without needing the read-your-writes window.
//
// The window still matters after the transaction ends: a commit is a
// write, and the reads that follow it on this context must observe it.
// The returned Tx therefore re-arms any read-your-writes window on ctx
// when it commits having executed at least one statement.
func (r *Replicated) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := r.primary.Begin(ctx)
	if err != nil || tx == nil {
		return tx, err
	}
	s, ok := readYourWrites(ctx)
	if !ok {
		return tx, nil
	}
	return &rywTx{Tx: tx, repl: r, state: s}, nil
}

// rywTx arms the read-your-writes window when the transaction it wraps
// commits. Without it a write done through InTx — which is how most
// writes worth reading back are done — leaves the window unarmed, and
// the very next read on the same context goes to a replica that may
// not have replayed the commit yet.
//
// Rollback deliberately does not arm anything: there is nothing to read
// back.
type rywTx struct {
	drops.Tx
	repl  *Replicated
	state *rywState
	wrote atomic.Bool
}

func (t *rywTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	res, err := t.Tx.Exec(ctx, sql, args...)
	if err == nil {
		t.wrote.Store(true)
	}
	return res, err
}

// Query counts too when the statement writes: a RETURNING clause is how
// a row's generated key comes back, so the transaction whose only write
// was an INSERT ... RETURNING is the one most likely to be read back
// immediately.
func (t *rywTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	rows, err := t.Tx.Query(ctx, sql, args...)
	if err == nil && isWriteStatement(sql) {
		t.wrote.Store(true)
	}
	return rows, err
}

func (t *rywTx) Commit(ctx context.Context) error {
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if t.wrote.Load() {
		t.state.mark()
		// After the commit, so the captured position is at or past the
		// commit's own WAL record.
		t.repl.captureWriteLSN(ctx, t.state)
	}
	return nil
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

// rywState tracks when this context last wrote. Reset by each write;
// checked by each read. When LSN tracking is enabled it also carries
// the primary's WAL position captured at write time so reads can route
// to a caught-up replica instead of pinning to primary.
//
// The state stores the moment of the write rather than an expiry,
// because two durations are checked against it — the caller's window
// and the driver's write delay — and only one of them is known here.
type rywState struct {
	duration  time.Duration
	mu        sync.Mutex
	lastWrite time.Time
	writeLSN  uint64
}

func (s *rywState) mark() {
	s.mu.Lock()
	s.lastWrite = time.Now()
	s.mu.Unlock()
}

// activeFor reports whether the last write on this context was less
// than d ago. A context that has never written is never active, and
// d<=0 is never active either — that is how a cleared window stays
// cleared.
func (s *rywState) activeFor(d time.Duration) bool {
	if d <= 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.lastWrite.IsZero() && time.Since(s.lastWrite) < d
}

func (s *rywState) requiredLSN() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLSN
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

// ----------------------------------------------------------------------
// Read-only enforcement
// ----------------------------------------------------------------------

// Routing a read to a replica is a decision about where a statement
// goes; it is not a guarantee about what the statement does. Query
// keeps a statement it recognises as a write off the replicas, but it
// recognises it by its leading keyword, and a keyword is a guess about
// intent rather than a rule about behaviour — a function called in a
// SELECT may write. Against a real streaming replica a write that slips
// through ends in a runtime error from the server — the right outcome,
// arriving at the worst moment. Against a setup where the "replica" is
// another handle on the primary, or a logical replica that happens to
// be writable, it ends in a write that lands in the wrong database and
// is never replicated anywhere.
//
// The mechanism that makes it a rule rather than a convention is a
// read-only transaction, because the server is the only party that can
// enforce it: PostgreSQL refuses INSERT, UPDATE, DELETE and most DDL
// inside one with SQLSTATE 25006, whatever the connection is pointed
// at. [DB.InReadTx] opens one and runs the caller's reads inside it.
//
// [drops.Driver] cannot ask for it — Begin takes a context and nothing
// else, and there is no room in three methods for transaction options.
// The smallest extension that would is one optional method, which is
// [ReadOnlyBeginner] below; a database/sql adapter implements it with
// sql.TxOptions{ReadOnly: true}, a pgx one with pgx.TxOptions{AccessMode:
// pgx.ReadOnly}. Until a driver does, drops opens an ordinary
// transaction and immediately issues SET TRANSACTION READ ONLY, which
// PostgreSQL accepts before the first statement of a transaction and
// which produces the identical server-side state. That costs one round
// trip and is the reason this works today on every driver rather than
// waiting for one.

// ErrNotReadOnly reports that a transaction meant to be read-only could
// not be made read-only. It is returned rather than degrading to a
// writable transaction: a caller who asked for the guarantee and
// silently did not get it is worse off than one who got an error.
var ErrNotReadOnly = errors.New("drops/pg: could not open a read-only transaction")

// ReadOnlyBeginner is the optional extension a driver implements when
// it can open a transaction the server will refuse writes in. It is the
// one method [drops.Driver] would need for read-routing to be
// enforceable without an extra round trip.
type ReadOnlyBeginner interface {
	BeginReadOnly(ctx context.Context) (drops.Tx, error)
}

// BeginReadOnly opens a read-only transaction on the driver a read
// belongs on — a replica, or the primary when the read-your-writes
// window says so. Replicated implements [ReadOnlyBeginner] so that a
// DB built on it gets replica routing and write refusal from one call.
func (r *Replicated) BeginReadOnly(ctx context.Context) (drops.Tx, error) {
	return beginReadOnly(ctx, r.routeRead(ctx))
}

// beginReadOnly opens a transaction on drv that the server will refuse
// writes in, preferring the driver's own support and falling back to
// SET TRANSACTION READ ONLY.
func beginReadOnly(ctx context.Context, drv drops.Driver) (drops.Tx, error) {
	if b, ok := drv.(ReadOnlyBeginner); ok {
		return b.BeginReadOnly(ctx)
	}
	tx, err := drv.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Must precede every other statement in the transaction, which it
	// does: the caller has not been handed the Tx yet.
	if _, err := tx.Exec(ctx, "SET TRANSACTION READ ONLY"); err != nil {
		_ = tx.Rollback(ctx)
		return nil, fmt.Errorf("%w: %w", ErrNotReadOnly, err)
	}
	return tx, nil
}

// readOnlyDriver turns Begin into beginReadOnly and otherwise gets out
// of the way, so InReadTx can reuse InTx wholesale — the retry policy,
// the hook events, the rollback on panic — instead of growing a second
// copy of it that will drift.
type readOnlyDriver struct{ drops.Driver }

func (d readOnlyDriver) Begin(ctx context.Context) (drops.Tx, error) {
	return beginReadOnly(ctx, d.Driver)
}

// InReadTx runs fn inside a read-only transaction, routed the way a
// read is routed: to a replica when the driver is a [Replicated] and
// the read-your-writes window allows it, to the primary otherwise.
//
//	err := db.InReadTx(ctx, func(rdb *pg.DB) error {
//	    return UserEntity.Query(rdb, ctx).Scan(&users)
//	})
//
// The read-only part is enforced by the server, not by drops: a write
// attempted inside fn comes back as SQLSTATE 25006, on a replica and on
// the primary alike. That makes "this handler only reads" a fact a test
// can assert rather than a comment above the function.
//
// Every statement in fn sees one snapshot of one database — which is
// the other reason to reach for it, since consecutive reads routed
// independently can land on two replicas with different replay
// positions and disagree.
func (db *DB) InReadTx(ctx context.Context, fn func(*DB) error) error {
	ro := *db
	ro.drv = readOnlyDriver{db.drv}
	return ro.InTx(ctx, fn)
}
