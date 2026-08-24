package drops

import (
	"context"
	"sync/atomic"
	"time"
)

// Queue time and query time are two measurements, and almost every Go
// database instrumentation reports their sum. Waiting for a free
// connection and waiting for the server have opposite remedies — the
// first says raise the pool ceiling or shed concurrency, the second
// says fix the query or the index — so one number tells an operator
// which question to ask only by accident.
//
// drops cannot take the first measurement on its own. [Driver] is three
// methods and connection acquisition happens inside whatever the caller
// plugged in; by the time Exec returns, the wait is over and nobody
// wrote it down. What drops can do is carry a slot on the context and
// let a driver that does know fill it in.
//
//	dialect:  ctx, waited := drops.NewConnWait(ctx)  // arm the slot
//	driver:   drops.ReportConnWait(ctx, waitedFor)   // fill it in
//	dialect:  e.WaitDuration, e.WaitKnown = waited() // onto the event
//
// A driver that says nothing leaves [QueryEvent.WaitKnown] false and
// the hook stream carries no queue time at all, which is the point: a
// gauge reading "0s queued" when nobody was counting is worse than a
// gauge that is absent, because only the second one can be noticed.
//
// pg.QueueTimed is the turnkey implementation for any pool that can
// check out a single connection.

type connWaitKey struct{}

// connWait is the slot itself. The duration is stored biased by one so
// a single atomic distinguishes "reported zero" from "not reported",
// and so the report is one compare-and-swap rather than two stores a
// reader could catch half-done.
type connWait struct{ ns atomic.Int64 }

func (w *connWait) report(d time.Duration) {
	if d < 0 {
		d = 0
	}
	// First report wins. An operation may issue more than one
	// acquisition — pg's LSN capture runs a second statement on the
	// same context after the write — and the one belonging to the
	// statement being timed is the one that came first.
	w.ns.CompareAndSwap(0, int64(d)+1)
}

func (w *connWait) read() (time.Duration, bool) {
	v := w.ns.Load()
	if v == 0 {
		return 0, false
	}
	return time.Duration(v - 1), true
}

// NewConnWait returns a copy of ctx carrying a slot for one
// connection-acquisition measurement, plus the function that reads the
// slot back. It is meant for dialects, which arm the slot around a
// single driver call and copy the result onto the [QueryEvent].
//
// The read function is safe to call at any time and never blocks; it
// returns ok=false until some driver has reported.
func NewConnWait(ctx context.Context) (context.Context, func() (time.Duration, bool)) {
	w := &connWait{}
	return context.WithValue(ctx, connWaitKey{}, w), w.read
}

// ReportConnWait records d as the time the operation running on ctx
// spent waiting for a connection from the pool. It is meant for
// drivers, and it is the only way the queue half of a statement's
// latency can reach drops — see the commentary at the top of this file.
//
// Report the wait even when acquisition ultimately failed: a pool that
// stays exhausted until the caller's deadline expires is precisely the
// condition the measurement exists to expose.
//
// Calling it on a context that carries no slot is a no-op, so a driver
// can report unconditionally without knowing whether anyone is
// listening.
func ReportConnWait(ctx context.Context, d time.Duration) {
	if w, ok := ctx.Value(connWaitKey{}).(*connWait); ok && w != nil {
		w.report(d)
	}
}
