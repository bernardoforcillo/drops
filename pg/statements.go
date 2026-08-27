package pg

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bernardoforcillo/drops"
)

// What is running right now, and how to stop it.
//
// [Hook] answers what *ran*: every event reaches it after the
// operation finished. That is the right shape for tracing and metrics
// and the wrong one for the two questions asked during an incident —
// "is anything still writing?" and "can I make it stop?" — because by
// the time a hook fires there is nothing left to wait for or cancel.
//
// A StatementRegistry answers those. It wraps a [drops.Driver], so it
// sees a statement start, finish, and everything in between:
//
//	reg := pg.NewStatementRegistry()
//	db  := pg.New(reg.Wrap(stdlib.New(sqlDB)))
//
//	// A failover has been announced. Stop admitting work, let what
//	// is in flight finish, and cancel whatever is left after 5s.
//	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
//	defer cancel()
//	if err := reg.Quiesce(ctx); err != nil {
//	    reg.CancelAll()
//	}
//
// The same three calls are a graceful shutdown, which is the more
// common use: a process that exits while a transaction is open leaves
// the server holding locks until the TCP connection times out.
//
// # What it costs
//
// One map entry and one derived context per in-flight statement.
// Nothing is copied and no statement text is retained beyond what the
// caller already passed, so the overhead is a mutex acquisition on
// the way in and one on the way out.
//
// # What it does not do
//
// It does not cancel a statement on the *server* by itself. It
// cancels the context the driver was given, and every driver anyone
// uses turns that into a cancel request on the wire — which is what
// produces [ErrQueryCanceled] (SQLSTATE 57014). A driver that ignores
// its context will not be stopped by this, and there is nothing a
// wrapper can do about that.

// ErrQuiesced is returned to a caller that issues a statement while
// the registry is quiesced. It is deliberately not a
// [ErrConnectionFailure] or anything else a retry loop treats as
// transient: the database is fine, this process is on its way out,
// and the right response is to shed the request rather than to try
// again on the same node.
var ErrQuiesced = errors.New("drops/pg: statement registry is quiesced")

// StatementKind names what a registry entry is tracking.
type StatementKind string

const (
	// KindExec is a statement issued through Exec.
	KindExec StatementKind = "exec"
	// KindQuery is a statement issued through Query. It stays in
	// flight until its [drops.Rows] is closed or exhausted, because
	// that is when the server is actually done with it.
	KindQuery StatementKind = "query"
	// KindTx is an open transaction. It stays in flight until commit
	// or rollback, which makes it the entry that matters most during
	// a failover: an open transaction holds locks.
	KindTx StatementKind = "tx"
)

// InFlight describes one operation the registry is tracking.
type InFlight struct {
	// ID is unique for the lifetime of the registry.
	ID uint64

	// Kind is what the entry tracks.
	Kind StatementKind

	// SQL is the statement text. Empty for [KindTx].
	SQL string

	// Write reports whether the statement modifies data, decided by
	// the same keyword scan [Replicated] routes on — so it is
	// conservative in the same direction: something it cannot read
	// counts as a write. A transaction counts as a write from the
	// moment it opens, because nothing outside it can tell yet.
	Write bool

	// Started is when the operation was handed to the driver.
	Started time.Time
}

// Age returns how long the operation has been in flight, as of now.
func (s InFlight) Age() time.Duration { return time.Since(s.Started) }

// StatementRegistry tracks in-flight statements and transactions for
// a driver it wraps. The zero value is not usable; call
// [NewStatementRegistry].
type StatementRegistry struct {
	mu       sync.Mutex
	entries  map[uint64]*regEntry
	nextID   atomic.Uint64
	admit    bool
	idle     chan struct{} // closed and replaced whenever entries empties
	canceled atomic.Uint64
}

type regEntry struct {
	InFlight
	cancel context.CancelFunc
}

// NewStatementRegistry returns a registry that admits statements.
func NewStatementRegistry() *StatementRegistry {
	return &StatementRegistry{
		entries: make(map[uint64]*regEntry),
		admit:   true,
		idle:    make(chan struct{}),
	}
}

// Wrap returns a driver that registers everything issued through it.
//
// Compose it closest to the real driver, under [Replicated] and under
// [RetryCachedPlans]: those two decide *where* and *whether* a
// statement is sent, and the registry should record what was actually
// sent rather than what was asked for.
func (r *StatementRegistry) Wrap(drv drops.Driver) drops.Driver {
	if drv == nil {
		panic("drops/pg: StatementRegistry.Wrap driver cannot be nil")
	}
	return &registryDriver{reg: r, drv: drv}
}

// InFlightCount returns how many operations are currently tracked.
func (r *StatementRegistry) InFlightCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// Snapshot returns the operations currently in flight, oldest first
// is *not* guaranteed — sort by Started if the order matters. The
// returned slice is a copy and safe to keep.
//
// It is what a /debug handler or a pre-shutdown log line prints, and
// the difference between "the deploy hung" and "the deploy is waiting
// on this 40-second UPDATE".
func (r *StatementRegistry) Snapshot() []InFlight {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]InFlight, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.InFlight)
	}
	return out
}

// Quiesce stops admitting new statements and waits for the in-flight
// ones to finish.
//
// It returns nil once the registry is idle, or ctx.Err() if ctx ends
// first — in which case the registry stays quiesced and the caller
// decides whether to wait longer or reach for [CancelAll].
//
// New statements are refused with [ErrQuiesced] from the moment this
// is called, including if it returns early. That is the point: a
// drain that lets new work in behind it never finishes. Call
// [Resume] to admit statements again.
func (r *StatementRegistry) Quiesce(ctx context.Context) error {
	r.mu.Lock()
	r.admit = false
	if len(r.entries) == 0 {
		r.mu.Unlock()
		return nil
	}
	idle := r.idle
	r.mu.Unlock()

	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Resume admits statements again after a [Quiesce]. A registry that
// was never quiesced is unaffected.
func (r *StatementRegistry) Resume() {
	r.mu.Lock()
	r.admit = true
	r.mu.Unlock()
}

// Quiesced reports whether the registry is currently refusing
// statements.
func (r *StatementRegistry) Quiesced() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.admit
}

// CancelAll cancels every in-flight operation and returns how many it
// signalled.
//
// Cancellation is asynchronous: this marks the operations and
// returns, and each one ends when its driver notices. Follow it with
// a [Quiesce] on a short context to wait for that to happen.
//
// A cancelled statement fails with [ErrQueryCanceled] on the caller's
// side. A cancelled *transaction* ends: cancelling the context it was
// begun on is enough for the driver to roll it back, so no committed
// work is lost, and a later tx.Rollback finds it already gone and
// reports success rather than an error nobody can act on. A caller
// mid-transaction still sees its next statement fail, which is the
// intended outcome and not a gentle one — prefer [Quiesce] and use
// this when it has run out of time.
func (r *StatementRegistry) CancelAll() int {
	return r.cancelWhere(func(*regEntry) bool { return true })
}

// CancelWrites cancels only the operations that modify data — writes
// and open transactions — and returns how many it signalled.
//
// It is the gentler failover move: a read that is still running
// against a primary about to be demoted will finish and return
// correct data, while a write against it cannot commit anywhere
// useful.
func (r *StatementRegistry) CancelWrites() int {
	return r.cancelWhere(func(e *regEntry) bool { return e.Write })
}

// CancelOlderThan cancels operations that have been in flight longer
// than d, and returns how many it signalled. It is the watchdog for
// the statement that has no timeout because nobody set one.
func (r *StatementRegistry) CancelOlderThan(d time.Duration) int {
	cutoff := time.Now().Add(-d)
	return r.cancelWhere(func(e *regEntry) bool { return e.Started.Before(cutoff) })
}

// Canceled returns the total number of operations this registry has
// signalled for cancellation over its lifetime. A counter that climbs
// outside a failover is a timeout that is firing routinely.
func (r *StatementRegistry) Canceled() uint64 { return r.canceled.Load() }

func (r *StatementRegistry) cancelWhere(pred func(*regEntry) bool) int {
	r.mu.Lock()
	// Collect under the lock, call outside it: a cancel func runs
	// user-supplied context cancellation, and the deregistration it
	// triggers takes this same mutex.
	victims := make([]context.CancelFunc, 0, len(r.entries))
	for _, e := range r.entries {
		if pred(e) {
			victims = append(victims, e.cancel)
		}
	}
	r.mu.Unlock()

	for _, c := range victims {
		c()
	}
	r.canceled.Add(uint64(len(victims)))
	return len(victims)
}

// register admits an operation and returns its entry plus the context
// the driver call should use. A quiesced registry returns
// ErrQuiesced and nothing is started.
func (r *StatementRegistry) register(ctx context.Context, kind StatementKind, sql string) (*regEntry, context.Context, error) {
	write := kind == KindTx || isWriteStatement(sql)

	r.mu.Lock()
	if !r.admit {
		r.mu.Unlock()
		return nil, nil, ErrQuiesced
	}
	callCtx, cancel := context.WithCancel(ctx)
	e := &regEntry{
		InFlight: InFlight{
			ID:      r.nextID.Add(1),
			Kind:    kind,
			SQL:     sql,
			Write:   write,
			Started: time.Now(),
		},
		cancel: cancel,
	}
	r.entries[e.ID] = e
	r.mu.Unlock()
	return e, callCtx, nil
}

// deregister removes an entry and releases its context. It is safe to
// call more than once for the same entry, which matters because a
// [drops.Rows] can be closed twice.
func (r *StatementRegistry) deregister(e *regEntry) {
	if e == nil {
		return
	}
	r.mu.Lock()
	if _, ok := r.entries[e.ID]; !ok {
		r.mu.Unlock()
		return
	}
	delete(r.entries, e.ID)
	if len(r.entries) == 0 {
		// Wake every waiter in Quiesce, then arm a fresh channel for
		// the next drain. Closing is the only broadcast a channel
		// offers, so the channel has to be replaced rather than
		// reused.
		close(r.idle)
		r.idle = make(chan struct{})
	}
	r.mu.Unlock()
	e.cancel()
}

// --- Driver wrapper -------------------------------------------------

type registryDriver struct {
	reg *StatementRegistry
	drv drops.Driver
}

func (d *registryDriver) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	e, callCtx, err := d.reg.register(ctx, KindExec, sql)
	if err != nil {
		return nil, err
	}
	defer d.reg.deregister(e)
	return d.drv.Exec(callCtx, sql, args...)
}

func (d *registryDriver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	e, callCtx, err := d.reg.register(ctx, KindQuery, sql)
	if err != nil {
		return nil, err
	}
	rows, err := d.drv.Query(callCtx, sql, args...)
	if err != nil {
		d.reg.deregister(e)
		return nil, err
	}
	// The statement is not finished when Query returns — the rows are
	// still being read off the connection, and cancelling callCtx now
	// would invalidate them under the caller. The entry lives until
	// the cursor is closed.
	return &registryRows{Rows: rows, reg: d.reg, entry: e}, nil
}

func (d *registryDriver) Begin(ctx context.Context) (drops.Tx, error) {
	e, callCtx, err := d.reg.register(ctx, KindTx, "")
	if err != nil {
		return nil, err
	}
	tx, err := d.drv.Begin(callCtx)
	if err != nil {
		d.reg.deregister(e)
		return nil, err
	}
	return &registryTx{Tx: tx, reg: d.reg, entry: e, ctx: callCtx}, nil
}

// Unwrap exposes the wrapped driver so the duck-typed capability
// probes ([Copier], [Listener], [PoolStatsProvider]) can still find
// it through the wrapper.
func (d *registryDriver) Unwrap() drops.Driver { return d.drv }

// registryRows keeps its entry registered until the cursor is done.
type registryRows struct {
	drops.Rows
	reg   *StatementRegistry
	entry *regEntry
	once  sync.Once
}

func (r *registryRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(func() { r.reg.deregister(r.entry) })
	return err
}

// Next deregisters when the cursor is exhausted, so a caller that
// reads to the end without closing — legal with database/sql, which
// closes the rows itself at that point — does not leave the entry
// behind forever.
func (r *registryRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.once.Do(func() { r.reg.deregister(r.entry) })
	return false
}

// registryTx keeps the transaction registered until it ends, and
// registers the statements issued inside it.
//
// Statements inside a transaction get their own entries so a snapshot
// shows what the transaction is doing, not merely that one is open.
// They are cancelled independently, and cancelling one aborts the
// transaction — which is the server's rule, not this wrapper's.
type registryTx struct {
	drops.Tx
	reg   *StatementRegistry
	entry *regEntry
	ctx   context.Context
	once  sync.Once
}

func (t *registryTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	e, callCtx, err := t.reg.register(t.txContext(ctx), KindExec, sql)
	if err != nil {
		return nil, err
	}
	defer t.reg.deregister(e)
	return t.Tx.Exec(callCtx, sql, args...)
}

func (t *registryTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	e, callCtx, err := t.reg.register(t.txContext(ctx), KindQuery, sql)
	if err != nil {
		return nil, err
	}
	rows, err := t.Tx.Query(callCtx, sql, args...)
	if err != nil {
		t.reg.deregister(e)
		return nil, err
	}
	return &registryRows{Rows: rows, reg: t.reg, entry: e}, nil
}

// Begin inside a transaction is a savepoint for the drivers that
// support it. It is tracked as a nested transaction.
func (t *registryTx) Begin(ctx context.Context) (drops.Tx, error) {
	e, callCtx, err := t.reg.register(t.txContext(ctx), KindTx, "")
	if err != nil {
		return nil, err
	}
	tx, err := t.Tx.Begin(callCtx)
	if err != nil {
		t.reg.deregister(e)
		return nil, err
	}
	return &registryTx{Tx: tx, reg: t.reg, entry: e, ctx: callCtx}, nil
}

func (t *registryTx) Commit(ctx context.Context) error {
	err := t.Tx.Commit(t.txContext(ctx))
	t.once.Do(func() { t.reg.deregister(t.entry) })
	return err
}

func (t *registryTx) Rollback(ctx context.Context) error {
	// Rollback runs on the caller's context rather than the
	// transaction's. The transaction's is what [CancelAll] cancels,
	// and a rollback issued on a cancelled context would be refused
	// before reaching the server.
	// Read before deregistering: deregistering releases the entry's
	// context, so afterwards every transaction looks cancelled.
	cancelled := t.ctx.Err() != nil

	err := t.Tx.Rollback(ctx)
	t.once.Do(func() { t.reg.deregister(t.entry) })

	if err != nil && cancelled {
		// The registry cancelled this transaction, and cancelling
		// the context a transaction was begun on ends it:
		// database/sql rolls it back itself, and pgx tears down the
		// connection. So the transaction is over, and this Rollback
		// is finding it already gone rather than failing to end it.
		//
		// Reporting that as an error is what the integration suite
		// caught: every caller with a `defer tx.Rollback()` would log
		// a failure for the one outcome that is entirely correct.
		// A caller that wants to know its transaction was cancelled
		// learns it from the statement that failed, not from here.
		return nil
	}
	return err
}

// txContext keeps a statement inside the transaction's cancellation
// scope even when the caller passes an unrelated context, so
// cancelling the transaction reaches the statement running in it.
// A caller's own cancellation still applies: the statement's context
// is derived from the caller's in [StatementRegistry.register], and
// this only decides which one it is derived from when the caller's
// carries no deadline of its own.
func (t *registryTx) txContext(ctx context.Context) context.Context {
	if ctx == nil {
		return t.ctx
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx
	}
	if ctx.Done() != nil {
		return ctx
	}
	return t.ctx
}
