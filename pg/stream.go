package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/bernardoforcillo/drops"
)

// Reading a result set that does not fit in memory.
//
// [CursorSpec] pages through a table for an API, and that is a
// different problem: a page is a separate query, the caller holds the
// cursor between requests, and each page re-executes the WHERE. This
// is the other one — a single query whose result is too large to
// materialise, read start to finish inside one process. Exports,
// migrations, a reconciliation pass, a sketch over every row.
//
// The obstacle is that most drivers buffer. database/sql hands back a
// *sql.Rows that streams, but the PostgreSQL protocol sends the whole
// result unless the client asks otherwise, and asking otherwise is a
// driver-specific setting drops cannot reach through the three-method
// [drops.Driver].
//
// PostgreSQL has a way that needs no driver support at all: declare a
// cursor and FETCH from it. It is plain SQL, so it works through any
// driver, and the server holds the result while the client takes it
// in batches.
//
//	err := pg.StreamQuery(ctx, db, pg.StreamOptions{Batch: 5000},
//	    "SELECT id, email FROM users WHERE created_at > $1", cutoff,
//	    func(rows drops.Rows) error {
//	        for rows.Next() { ... }
//	        return rows.Err()
//	    })
//
// # What holding a cursor costs
//
// A cursor lives inside a transaction, so streaming a large table
// holds one open for as long as it takes. An open transaction pins
// the oldest snapshot the database must preserve, which holds back
// vacuum on *every* table, not just this one. A full export of a
// large table this way is normal; leaving the transaction open while
// something else is decided is how a table's bloat doubles overnight.
//
// Return an error from fn to stop early — the cursor is closed and
// the transaction rolled back, which is the whole point of a cursor
// over a materialised result. [ErrStopStream] does that without
// surfacing as a failure.

// ErrStopStream ends a [StreamQuery] early without reporting an
// error, for the caller that has found what it was looking for.
var ErrStopStream = errors.New("drops/pg: stream stopped by the caller")

// StreamOptions configures a [StreamQuery].
type StreamOptions struct {
	// Batch is how many rows each FETCH takes. Default 1000.
	//
	// It trades round trips against the memory one batch occupies,
	// and the useful range is wide: a few hundred for wide rows, tens
	// of thousands for narrow ones. What it must not be is 1, which
	// turns a scan into one network round trip per row.
	Batch int

	// Hold keeps the cursor alive after the transaction commits
	// (WITH HOLD), so the stream does not pin a snapshot for its
	// whole life.
	//
	// The cost is paid up front and is not small: the server
	// materialises the entire result into a temporary file at commit
	// time, before the first row is read. Worth it for a long,
	// slow consumer — one that writes each batch to a remote API —
	// and not worth it for a fast local one.
	Hold bool
}

func (o StreamOptions) batch() int {
	if o.Batch <= 0 {
		return 1000
	}
	return o.Batch
}

// cursorSeq names cursors uniquely within a process.
var cursorSeq atomic.Uint64

// StreamQuery runs sql and hands fn each batch of rows in turn,
// holding at most one batch in memory.
//
// fn receives a [drops.Rows] over the batch and must consume it
// before returning; the rows are closed afterwards either way.
// Returning an error stops the stream and rolls back — return
// [ErrStopStream] to stop without reporting a failure.
//
// fn is not called at all for an empty result. A batch is never
// empty: the stream ends when a FETCH returns nothing.
func StreamQuery(ctx context.Context, db *DB, opts StreamOptions, sql string, args []any, fn func(drops.Rows) error) error {
	if fn == nil {
		return errors.New("drops/pg: StreamQuery needs a function to call")
	}
	if strings.TrimSpace(sql) == "" {
		return errors.New("drops/pg: StreamQuery needs a statement")
	}
	name := fmt.Sprintf("drops_cursor_%d", cursorSeq.Add(1))
	batch := opts.batch()

	run := func(tx *DB) error {
		hold := ""
		if opts.Hold {
			hold = " WITH HOLD"
		}
		// The cursor name is generated here and matches
		// [a-z_0-9]+ by construction, so quoting it is belt and
		// braces rather than the safety property. The caller's SQL
		// is substituted whole and its arguments stay bound —
		// DECLARE takes parameters, which is what keeps this from
		// being string-built SQL with a different name.
		declare := fmt.Sprintf("DECLARE %s NO SCROLL CURSOR%s FOR %s",
			quoteIdent(name), hold, sql)
		if _, err := tx.Exec(ctx, declare, args...); err != nil {
			return fmt.Errorf("drops/pg: declaring cursor: %w", err)
		}
		defer func() {
			// Best effort. A rollback closes the cursor anyway; this
			// matters for the WITH HOLD case, where it would
			// otherwise outlive the transaction.
			_, _ = tx.Exec(ctx, fmt.Sprintf("CLOSE %s", quoteIdent(name)))
		}()

		fetch := fmt.Sprintf("FETCH FORWARD %d FROM %s", batch, quoteIdent(name))
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, fetch)
			if err != nil {
				return fmt.Errorf("drops/pg: fetching from cursor: %w", err)
			}
			counted := &countingRows{Rows: rows}
			cbErr := fn(counted)
			closeErr := rows.Close()
			if cbErr != nil {
				return cbErr
			}
			if closeErr != nil {
				return closeErr
			}
			if counted.n < batch {
				// A short batch is the end of the result. This is
				// also why fn must consume the rows it is given: a
				// callback that returns early without iterating
				// would look like the end of the stream.
				return nil
			}
		}
	}

	err := db.InTx(ctx, run)
	if errors.Is(err, ErrStopStream) {
		return nil
	}
	return err
}

// StreamRows is [StreamQuery] one row at a time, for the caller that
// does not care about the batching and only wants bounded memory.
//
// The batching still happens — it is what bounds the memory — but fn
// sees rows rather than batches, which is what most callers actually
// want and what makes the "consume the batch" rule impossible to get
// wrong.
func StreamRows(ctx context.Context, db *DB, opts StreamOptions, sql string, args []any, fn func(drops.Rows) error) error {
	return StreamQuery(ctx, db, opts, sql, args, func(rows drops.Rows) error {
		for rows.Next() {
			if err := fn(singleRow{rows}); err != nil {
				return err
			}
		}
		return rows.Err()
	})
}

// countingRows counts how many rows the callback pulled, which is how
// the loop learns the batch was short.
type countingRows struct {
	drops.Rows
	n int
}

func (r *countingRows) Next() bool {
	if r.Rows.Next() {
		r.n++
		return true
	}
	return false
}

// singleRow presents the current row of a cursor as a [drops.Rows]
// positioned on it, so a per-row callback can Scan without being able
// to advance the underlying cursor.
type singleRow struct {
	drops.Rows
}

// Next reports false: the row the callback was handed is the only one
// it may read. Advancing is the loop's business, and a callback that
// could advance would silently skip rows.
func (singleRow) Next() bool { return false }

// Close is a no-op: the batch owns the cursor and closes it.
func (singleRow) Close() error { return nil }
