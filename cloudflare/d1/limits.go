package d1

import "time"

// Limits are D1's service limits, as Cloudflare publishes them.
//
// They are values rather than prose because two of them change what a
// schema or a query has to look like, and a number a test can assert
// against is worth more than a number in a comment. Cloudflare does
// revise these; treat the struct as this package's record of them at
// the time it was written, not as an authority, and check the current
// figures before designing around one.
type Limits struct {
	// BoundParams is the maximum number of bound parameters in one
	// statement. It is the limit most likely to be met by accident:
	// `WHERE id IN (?, ?, …)` over a slice hits it at a hundred
	// ids, and the fix is to chunk the slice or to join against a
	// temporary table rather than to widen the statement.
	//
	// [WithMaxBoundParams] is the pre-flight check for it.
	BoundParams int

	// QueryResponseSize caps the bytes one statement may answer
	// with. A D1 result set is not streamed — it arrives whole,
	// inside an HTTP response — so a SELECT over a large table
	// fails at the service rather than arriving a page at a time.
	// Page with LIMIT, or with a keyset cursor.
	QueryResponseSize int64

	// QueryDuration caps how long one statement may run.
	QueryDuration time.Duration

	// DatabaseSize is the ceiling on one database on a paid plan.
	// D1 is a database per tenant as readily as a table per tenant,
	// and this is the number that decides which.
	DatabaseSize int64

	// ValueSize caps a single string, BLOB or row.
	ValueSize int64

	// StatementSize caps the bytes of SQL in one statement. It is
	// met by generated SQL far sooner than by hand-written SQL: a
	// VALUES list built from a slice reaches it at a few thousand
	// rows.
	StatementSize int

	// BatchStatements caps how many statements one request may
	// carry. It is the D1 queries-per-invocation ceiling, which a
	// batch spends one of per statement.
	BatchStatements int
}

// Published are D1's limits as documented for the Workers Paid plan.
//
// The free plan is smaller in the two storage figures — a 500 MB
// database and 5 GB per account — and identical in the rest.
var Published = Limits{
	BoundParams:       100,
	QueryResponseSize: 1 << 20,          // 1 MB
	QueryDuration:     30 * time.Second, //
	DatabaseSize:      10 << 30,         // 10 GB
	ValueSize:         2_000_000,        // 2 MB
	StatementSize:     100_000,          // 100 KB
	BatchStatements:   1000,
}

// What D1 does not have, and what to do instead. These are not
// limits that a bigger plan lifts; they are absent features, and
// each one changes how a drops schema is written for D1.
//
//   - No ATTACH, so no cross-database query. A join across tenants
//     that a single SQLite file would serve needs one database and a
//     tenant column — which is the axis
//     [github.com/bernardoforcillo/drops/sqlite]'s tenant support
//     already models.
//
//   - No PRAGMA. Foreign keys are always enforced (so the local
//     dialect's "did you forget PRAGMA foreign_keys=ON?" failure
//     mode cannot happen here), and journal_mode, synchronous and
//     the rest are D1's to set.
//
//   - No interactive transactions. See the package comment; [Batch]
//     and [Driver.Begin] are what there is instead.
//
//   - No extensions, so no FTS5 and no R*Tree unless D1 ships them.
//     A schema that leans on either does not port.
//
//   - No user-defined functions, so a CHECK or an index over one
//     does not port either.
const _ = 0
