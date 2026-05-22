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

	// Duration is the elapsed time of the operation.
	Duration time.Duration

	// Err is the error returned by the operation, or nil on success.
	Err error
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
// nil hooks are skipped.
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
			if h != nil {
				h(ctx, e)
			}
		}
	}
}
