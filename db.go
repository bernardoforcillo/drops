package drops

import (
	"context"
	"time"
)

// rollbackTimeout caps how long a deferred Rollback may run. If the
// caller's context is cancelled or already past its deadline, the
// rollback path detaches into a fresh context so the cleanup still has
// a chance to succeed.
const rollbackTimeout = 5 * time.Second

// rollbackCtx returns a context suitable for a cleanup Rollback call.
// It inherits values from parent (for tracing / request IDs) but is not
// affected by parent's cancellation or deadline; it gets its own cap so
// the rollback can't hang forever.
func rollbackCtx(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), rollbackTimeout)
}

// InTx runs fn inside a transaction opened on d. The transaction is
// committed if fn returns nil and rolled back otherwise (including on
// panic, after which the panic is re-raised).
//
// Rollback uses a detached context with its own short timeout so a
// cancelled or expired caller-ctx doesn't prevent the cleanup from
// running.
func InTx(ctx context.Context, d Driver, fn func(Tx) error) (err error) {
	tx, err := d.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			rctx, cancel := rollbackCtx(ctx)
			_ = tx.Rollback(rctx)
			cancel()
			panic(p)
		}
		if err != nil {
			rctx, cancel := rollbackCtx(ctx)
			_ = tx.Rollback(rctx)
			cancel()
			return
		}
		err = tx.Commit(ctx)
	}()
	err = fn(tx)
	return err
}
