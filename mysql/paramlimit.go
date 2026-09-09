package mysql

import (
	"errors"
	"fmt"
)

// The bound-parameter ceiling, and why drops refuses rather than splits.
//
// MySQL's binary protocol carries a prepared statement's parameter
// count in a 16-bit field, so a statement may carry at most 65535 of
// them. The limit is the WIRE's, not the server's configuration: it
// cannot be raised, and every driver that speaks the protocol has it.
//
// Reaching it is ordinary rather than exotic. A bulk insert of a table
// with eight columns hits it at 8192 rows, which is a modest batch by
// any standard, and so does an IN list built from a page of ids that
// grew. What the server answers with is a message about the prepared
// statement, arriving from the driver, naming neither the statement nor
// the call that built it — and in a bulk write the whole batch fails
// with it, so the first guess is always that something is wrong with
// the data.
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
//     invents a number no statement returned, and LAST_INSERT_ID()
//     answers for one of them.
//
// So the answer is a refusal that names the count, the ceiling and the
// way forward: chunk the batch at the call site, where the transaction
// boundary is a decision somebody makes.

// maxBoundParams is the number of parameters one statement may carry.
// It is a property of the protocol rather than of a setting: the count
// is a 16-bit field, so 65535 is the largest that can be expressed.
const maxBoundParams = 65535

// ErrTooManyParameters is returned before a statement carrying more
// than [maxBoundParams] bound parameters is sent.
//
// It is raised by drops rather than by the server, which is the whole
// point: the same failure arrives from the driver with no statement and
// no call site in it.
var ErrTooManyParameters = errors.New("drops/mysql: statement carries more bound parameters than the protocol allows")

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
