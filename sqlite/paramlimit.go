package sqlite

import (
	"errors"
	"fmt"
)

// The bound-parameter ceiling, and why drops refuses rather than splits.
//
// SQLite caps the parameters of one statement at
// SQLITE_MAX_VARIABLE_NUMBER, which has been 32766 since 3.32 and was
// 999 before it. It is a compile-time limit of the library the process
// is linked against rather than a wire format, so it can differ between
// two builds of the same program — which is exactly why a caller
// should not be discovering it from the server.
//
// Reaching it is ordinary rather than exotic. A bulk insert of a table
// with eight columns hits 32766 at 4096 rows, which is a modest batch
// by any standard, and so does an IN list built from a page of ids
// that grew. What SQLite answers with is "too many SQL variables",
// naming neither the statement nor the call that built it — and in a
// bulk write the whole batch fails with it, so the first guess is
// always that something is wrong with the data.
//
// # Why not split the statement
//
// Splitting a batch that is too large is what a caller usually wants,
// and it is not something drops can do FOR them without changing what
// the call means:
//
//   - One INSERT either happens or does not. Several do not: outside a
//     transaction, a failure on the third leaves the first two written,
//     and a caller relying on "these rows, or none" now has a
//     half-written batch.
//   - The Result is one statement's. Summing rows-affected over several
//     invents a number no statement returned, and last_insert_rowid()
//     answers for one of them.
//
// So the answer is a refusal that names the count, the ceiling and the
// way forward: chunk the batch at the call site, where the transaction
// boundary is a decision somebody makes.

// maxBoundParams is the number of parameters one statement may carry.
//
// It is SQLITE_MAX_VARIABLE_NUMBER's default since 3.32. A library
// built with a LOWER limit will still refuse a statement this admits,
// and the message will be SQLite's; drops cannot ask the library what
// its limit is through the driver interface, so this is the ceiling it
// can promise rather than the one in force. The direction is the safe
// one: every statement drops refuses would certainly have failed.
const maxBoundParams = 32766

// ErrTooManyParameters is returned before a statement carrying more
// than [maxBoundParams] bound parameters is sent.
//
// It is raised by drops rather than by the library, which is the whole
// point: the same failure arrives as "too many SQL variables", with no
// statement and no call site in it.
var ErrTooManyParameters = errors.New("drops/sqlite: statement carries more bound parameters than SQLite allows")

// checkParamCount refuses a statement whose parameter list cannot be
// sent. It is called from [DB.Exec] and [DB.Query] rather than from the
// builders, so every path is covered by one check: a bulk INSERT, an IN
// list that grew, and a statement a caller assembled themselves.
func checkParamCount(sql string, args []any) error {
	if len(args) <= maxBoundParams {
		return nil
	}
	return fmt.Errorf("%w: %d parameters, limit %d, in %s; send the rows in batches",
		ErrTooManyParameters, len(args), maxBoundParams, excerptSQL(sql))
}
