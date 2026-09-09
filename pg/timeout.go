package pg

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The server-side statement timeout, and why a ctx deadline is not one.
//
// A ctx deadline protects the CALLER. When it elapses the driver stops
// waiting and sends a cancel request on a second connection, and the
// server acts on it when it next checks — which is at an interruption
// point, not immediately. Until then the statement goes on holding
// whatever it holds: a seq scan of a large table goes on reading, a
// lock stays taken, the work goes on being paid for. Cancellation is
// asynchronous and best-effort by construction, and a request whose
// client has already given up is exactly the one nobody is watching.
//
// statement_timeout protects the DATABASE. The server aborts the
// statement itself, at a bound it enforces, and answers with SQLSTATE
// 57014 — which drops classifies as [ErrQueryCanceled], so the caller
// gets a typed refusal rather than a connection that eventually
// returns. The two are complementary: the deadline says how long this
// request is worth waiting for, and the timeout says how long the
// server will spend on it regardless.
//
// # Why it is transaction-scoped
//
// SET statement_timeout without LOCAL changes the SESSION, and a
// session is a pooled connection: the setting outlives the request that
// made it and governs whatever the next borrower runs. That is the
// hazard [DiscardAll]'s doc comment already names — "a connection that
// a request left with a stray SET statement_timeout carries that into
// whoever gets it next" — and it is worse here than for most settings,
// because the symptom is a query that fails on a timeout nobody set, in
// a request that has nothing to do with the one that set it.
//
// SET LOCAL is scoped to the transaction and undone at COMMIT or
// ROLLBACK, which is the same boundary the connection returns to the
// pool across. So the timeout is established inside a transaction, and
// asking for one outside a transaction is refused rather than quietly
// promoted to a session setting.

// ErrTimeoutOutsideTx is returned by [DB.InTxWithTimeout] when it is
// called on a *DB that is already bound to a transaction, or when the
// duration is not positive.
//
// It is not a limitation to work around: the transaction IS the scope.
// A timeout established outside one either leaks into the pool or has
// no boundary to be undone at.
var ErrTimeoutOutsideTx = errors.New("drops/pg: a statement timeout needs a transaction to be scoped to")

// InTxWithTimeout runs fn inside a transaction whose every statement is
// bounded by the server, not merely by the caller's patience.
//
//	err := db.InTxWithTimeout(ctx, 2*time.Second, func(tx *pg.DB) error {
//	    return tx.Update(Accounts).Set(...).Where(...).Exec(ctx)
//	})
//
// A statement that runs longer is aborted by PostgreSQL with SQLSTATE
// 57014, which reaches the caller as [ErrQueryCanceled] — a typed
// refusal, not a connection that eventually comes back. The bound
// covers each statement individually, which is what statement_timeout
// means: a transaction of five statements at two seconds each may take
// ten, and putting a ceiling on the whole is what the ctx deadline is
// for.
//
// It delegates to [DB.InTx], so it inherits the hook events, the
// panic-safe rollback and any [RetryPolicy] — and a retried attempt
// re-establishes the timeout, because the SET happens inside the
// retried body rather than once around it. The SET goes through
// [DB.Exec], so an attached Hook sees it in the same stream as the
// statements it governs.
//
// The timeout is undone at the transaction boundary and cannot reach
// the next borrower of the connection. See the file comment for why
// that is the whole design rather than a detail of it.
func (db *DB) InTxWithTimeout(ctx context.Context, d time.Duration, fn func(*DB) error) error {
	if d <= 0 {
		return fmt.Errorf("%w: %v is not a positive duration", ErrTimeoutOutsideTx, d)
	}
	return db.InTx(ctx, func(tx *DB) error {
		if err := tx.setStatementTimeout(ctx, d); err != nil {
			return err
		}
		// The timeout is established and the caller may already be
		// gone. Checking here rather than letting fn's first statement
		// discover it keeps the refusal at the boundary, exactly as
		// InTxAs does.
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn(tx)
	})
}

// setStatementTimeout issues the SET LOCAL. It is unexported because a
// timeout with no transaction around it is the thing this file exists
// to prevent, and an exported setter is an invitation to establish one.
//
// The duration is rendered as an integer number of milliseconds rather
// than bound: SET takes a value in its own grammar and PostgreSQL has
// nowhere to bind a parameter in it. Milliseconds are statement_timeout's
// own unit, so the rendering carries no interpretation of its own — and
// a sub-millisecond duration rounds UP to 1ms rather than down to 0,
// because 0 is statement_timeout's spelling of "no limit" and rounding
// into it would turn the tightest bound a caller can ask for into none
// at all.
func (db *DB) setStatementTimeout(ctx context.Context, d time.Duration) error {
	ms := d.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	_, err := db.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", ms))
	return err
}
