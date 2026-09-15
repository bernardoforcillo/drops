package d1

import "time"

// Meta is what D1 reports about a statement it ran, alongside the
// rows.
//
// It is worth surfacing rather than discarding because two of its
// numbers are the ones D1 bills and throttles on. RowsRead is the
// count of rows the engine examined, not the count it returned: a
// query that answers with one row after scanning a million reads a
// million, and that is the number that shows up on the bill and in
// the "too many rows read" error. It is the cheapest index-coverage
// check there is — run the query, read RowsRead, compare it to the
// rows you expected to match.
type Meta struct {
	// ServedBy names the D1 instance that answered.
	ServedBy string `json:"served_by"`

	// ServedByRegion is the region it answered from, when D1 reports
	// one.
	ServedByRegion string `json:"served_by_region"`

	// ServedByColo is the three-letter airport code of the colo that
	// ran the statement. Empty where the runtime reports none —
	// wrangler dev does not.
	ServedByColo string `json:"served_by_colo"`

	// ServedByPrimary reports whether the answer came from the
	// primary rather than a read replica. False on a replica means
	// the read may be behind the primary — which is the trade
	// Sessions API read replication offers, and the flag to check
	// when a read-your-writes bug is suspected.
	ServedByPrimary bool `json:"served_by_primary"`

	// Duration is how long D1 spent on the statement, in
	// milliseconds. It excludes the network time to reach
	// Cloudflare, so it is smaller than what the hook measures.
	Duration float64 `json:"duration"`

	// Changes is the number of rows the statement inserted, updated
	// or deleted — SQLite's changes(). It is what
	// [Result.RowsAffected] answers.
	Changes int64 `json:"changes"`

	// LastRowID is SQLite's last_insert_rowid() after the statement.
	LastRowID int64 `json:"last_row_id"`

	// ChangedDB reports whether the statement wrote anything.
	ChangedDB bool `json:"changed_db"`

	// SizeAfter is the database's size in bytes after the
	// statement. Compare it against [Limits.DatabaseSize].
	SizeAfter int64 `json:"size_after"`

	// RowsRead and RowsWritten are the billed row counts. See the
	// type comment: RowsRead counts rows examined, not returned.
	RowsRead    int64 `json:"rows_read"`
	RowsWritten int64 `json:"rows_written"`
}

// Elapsed returns [Meta.Duration] as a time.Duration.
func (m Meta) Elapsed() time.Duration {
	return time.Duration(m.Duration * float64(time.Millisecond))
}

// Result is the [github.com/bernardoforcillo/drops.Result] D1 returns
// for a statement, carrying the full [Meta] alongside the row count.
//
// Reach it from a plain drops.Result with a type assertion:
//
//	res, _ := db.Exec(ctx, "DELETE FROM sessions WHERE expires_at < ?", cutoff)
//	if r, ok := res.(*d1.Result); ok {
//	    log.Printf("scanned %d rows to delete %d", r.Meta().RowsRead, r.Meta().Changes)
//	}
type Result struct {
	meta Meta

	// pending is set on a result handed back from inside a
	// transaction, before the batch has run. See [Tx].
	pending *pendingResult
}

// pendingResult is the shared state between a buffered statement's
// Result and the commit that fills it in.
type pendingResult struct {
	done bool
	meta Meta
	err  error
}

// RowsAffected implements [github.com/bernardoforcillo/drops.Result].
//
// Inside a transaction, before the commit has sent the batch, it
// returns [ErrPending]: the statement has not run, so there is no
// honest number to give. After the commit it returns the real one.
func (r *Result) RowsAffected() (int64, error) {
	if r.pending != nil {
		if !r.pending.done {
			return 0, ErrPending
		}
		if r.pending.err != nil {
			return 0, r.pending.err
		}
		return r.pending.meta.Changes, nil
	}
	return r.meta.Changes, nil
}

// LastInsertID returns SQLite's last_insert_rowid() after the
// statement — the rowid of the row an INSERT just wrote, for a table
// that has one.
//
// It carries the same [ErrPending] caveat as [Result.RowsAffected]
// inside a transaction, and the same caveat SQLite itself carries: on
// a WITHOUT ROWID table, or after a statement that inserted nothing,
// the number means nothing.
func (r *Result) LastInsertID() (int64, error) {
	if r.pending != nil {
		if !r.pending.done {
			return 0, ErrPending
		}
		if r.pending.err != nil {
			return 0, r.pending.err
		}
		return r.pending.meta.LastRowID, nil
	}
	return r.meta.LastRowID, nil
}

// Meta returns what D1 reported about the statement. Inside a
// transaction before commit it is the zero Meta.
func (r *Result) Meta() Meta {
	if r.pending != nil {
		return r.pending.meta
	}
	return r.meta
}

// Pending reports whether the statement is still buffered inside an
// uncommitted transaction.
func (r *Result) Pending() bool { return r.pending != nil && !r.pending.done }
