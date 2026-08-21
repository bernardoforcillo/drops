package drops

import (
	"context"
	"time"
)

// QueryEvent carries observability information about a single driver
// operation. It is passed to a Hook after the operation completes (or
// fails) so the hook can log, trace, emit metrics, etc. without altering
// control flow.
type QueryEvent struct {
	// Kind names the operation: "exec", "query", "begin", "commit",
	// "rollback", "ping".
	Kind string

	// SQL is the rendered statement text. Empty for begin/commit/
	// rollback/ping.
	SQL string

	// Args are the bound parameters, in $1, $2, ... order. They may
	// contain secrets — redact before logging in untrusted contexts.
	Args []any

	// Duration is the elapsed time of the operation, end to end: the
	// wait for a connection plus the database's own work.
	Duration time.Duration

	// WaitDuration is the part of Duration spent queueing for a
	// connection from the pool, before the database ever saw the
	// statement. Only a Driver can measure it — drops sits on the far
	// side of the pool — so it is set only when the driver reported it
	// through [ReportConnWait].
	WaitDuration time.Duration

	// WaitKnown reports whether WaitDuration was measured. It exists to
	// separate "the pool handed a connection over immediately" from
	// "nobody was counting": both leave WaitDuration at zero and only
	// the first is a fact. Emit queue-time metrics only when it is true.
	WaitKnown bool

	// Err is the error returned by the operation, or nil on success.
	Err error
}

// QueryDuration returns the time the database itself spent on the
// operation — Duration less the queue time — and whether that split is
// actually known. When it is not, the total is returned with ok=false:
// the caller may still want the number, but must not label it query
// time, because everything the pool spent finding a connection is
// still inside it.
func (e QueryEvent) QueryDuration() (d time.Duration, ok bool) {
	if !e.WaitKnown {
		return e.Duration, false
	}
	if e.WaitDuration > e.Duration {
		// Shouldn't happen, but a driver reporting a wait longer than
		// the operation must not turn into a negative latency sample.
		return 0, true
	}
	return e.Duration - e.WaitDuration, true
}

// Hook observes driver operations performed via a DB. It is purely
// observational — the operation has already happened by the time the
// hook is invoked, and the hook's return value (if any) is discarded.
//
// Hooks must be safe for concurrent use: a single DB may issue queries
// from multiple goroutines, and each will invoke the hook independently.
//
// To compose multiple hooks, use ChainHooks.
type Hook func(ctx context.Context, e QueryEvent)

// ChainHooks returns a Hook that invokes each given hook in order.
// nil hooks are skipped. A panic inside any hook is recovered so the
// remaining hooks still run and the caller of CallHook is never
// crashed by a buggy observer.
func ChainHooks(hooks ...Hook) Hook {
	// Common-case fast paths so a chain of zero or one doesn't allocate
	// an unnecessary closure.
	switch len(hooks) {
	case 0:
		return nil
	case 1:
		return hooks[0]
	}
	cp := append([]Hook(nil), hooks...)
	return func(ctx context.Context, e QueryEvent) {
		for _, h := range cp {
			CallHook(h, ctx, e)
		}
	}
}

// CallHook invokes h with (ctx, e), tolerating both a nil hook and a
// hook that panics. It is the safe entrypoint every dialect uses to
// emit events: a buggy user-supplied Hook (nil dereference,
// out-of-bounds index in a logger format string, etc.) must NOT take
// down the caller's request goroutine.
//
// Adapters typically call this from their internal `emit` helper:
//
//	func (c *Cache) emit(ctx context.Context, e drops.QueryEvent) {
//	    drops.CallHook(c.hook, ctx, e)
//	}
func CallHook(h Hook, ctx context.Context, e QueryEvent) {
	if h == nil {
		return
	}
	defer func() { _ = recover() }()
	h(ctx, e)
}
