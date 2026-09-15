// Package d1 runs drops against Cloudflare D1 over its HTTP API.
//
// D1 is SQLite, so the whole of
// [github.com/bernardoforcillo/drops/sqlite] applies unchanged —
// schema declarations, the query builders, entities, relations,
// migrations, the tenant axis. This package supplies the missing
// half: a [github.com/bernardoforcillo/drops.Driver] that sends those
// statements to D1 instead of to a local file.
//
//	cf, _ := cloudflare.New(accountID, cloudflare.WithAPIToken(token))
//	drv := d1.New(cf, databaseID)
//	db := sqlite.New(drv)
//
//	users, err := sqlite.All[User](Users.Select(), db, ctx)
//
// # Transactions are the thing to read before shipping this
//
// D1's HTTP API has no interactive transactions. There is no BEGIN to
// send: the API is one request, one unit of work, and consecutive
// requests are not guaranteed to reach the same connection, so a
// BEGIN in one and a COMMIT in the next would be a transaction
// straddling two sessions. D1 rejects the attempt rather than
// pretending.
//
// What it offers instead is the batch: several statements in one
// request, which D1 documents as running in an implicit transaction.
// [Driver.Begin] is built on that, and the shape it can honestly
// offer is a deferred one:
//
//   - Exec inside the transaction is buffered, not sent. The
//     [drops.Result] it returns is a promise — [ErrPending] until
//     the commit fills it in, real afterwards.
//   - Query inside the transaction fails with [ErrTxQuery]. The rows
//     do not exist yet, and reading around the buffer — running the
//     SELECT immediately, outside the batch — would silently break
//     read-your-writes for every caller that reads back what it just
//     wrote.
//   - Commit sends the buffer as one request. Rollback discards it,
//     which costs nothing because nothing was sent.
//
// So [github.com/bernardoforcillo/drops.InTx] and
// [github.com/bernardoforcillo/drops/sqlite.DB.InTx] work for
// write-only units of work — the common one, an insert plus the rows
// that hang off it — and refuse the read-modify-write ones rather
// than getting them subtly wrong. Do the read first, outside the
// transaction, and write the guard into the statement:
// a conditional UPDATE … WHERE version = ? tells you it lost by
// reporting zero rows affected, which is the same answer a
// transaction would have given and needs no session to hold it.
//
// [Batch] is the same mechanism without the drops.Tx clothing, and is
// the clearer spelling when the unit of work is already a list of
// statements.
//
// # Read replication, and the session that makes it safe
//
// D1 can place read replicas around the world. A replica is allowed
// to be behind the primary, so without a session two consecutive
// reads may be served by two instances and the second may see less
// than the first — and a read after a write may not see the write.
//
// [Session] is D1's answer, and this package's. Every request in a
// session returns a bookmark, the next request carries it, and D1
// refuses to serve that request from an instance that has not caught
// up to it. A Session is itself a
// [github.com/bernardoforcillo/drops.Driver], so the dialect runs
// inside one unchanged:
//
//	sess, err := drv.Session(d1.FirstUnconstrained)
//	db := sqlite.New(sess)
//
// Scope one to the unit the consistency is wanted over — an HTTP
// request, a job, a page render. They are free: there is no
// connection and nothing to close. To carry read-your-writes past the
// end of one, hand [Session.Bookmark] to [Driver.Resume] at the start
// of the next.
//
// Two things to know before designing around it. Sessions are a
// Worker binding feature, so they need [NewBridge] and a Worker
// holding the binding — [New], over the REST API, answers
// [ErrSessionsUnsupported], because Cloudflare does not offer
// sessions there. And a session gives sequential consistency, not
// transactions: everything above about [Driver.Begin] still applies
// inside one.
//
// # What the rows come back as
//
// D1 answers in JSON, which loses the type information SQLite had.
// The driver asks for the /raw response shape — columns and rows as
// arrays rather than objects — so column order survives, and decodes
// values with [encoding/json.Decoder.UseNumber] so an INTEGER
// primary key past 2^53 is not rounded on the way through a float64.
// [Rows.Scan] converts into the usual destinations, including
// [database/sql.Scanner] and [time.Time]; scan.go says what converts
// into what.
//
// # Errors
//
// D1 passes SQLite's own message through, so a violated constraint
// arrives as "UNIQUE constraint failed: users.email" inside a
// Cloudflare envelope error. [ClassifyError] turns that back into the
// same [github.com/bernardoforcillo/drops/sqlite] sentinels the local
// dialect produces, which is what lets an entity's Create return a
// [github.com/bernardoforcillo/drops.FieldError] here exactly as it
// does against a file:
//
//	if fe, ok := drops.AsFieldError(err); ok && fe.Rule == drops.RuleUnique {
//	    form.AddError(fe.Field, "is already taken")
//	}
//
// # The limits worth knowing before the schema is written
//
// D1 is SQLite with a service around it, and the service has edges
// the file does not. [Published] carries them as a [Limits] value so
// a test can assert against a number rather than read one in a
// comment. The ones that change a schema decision are the 10 GB
// ceiling on a database, the 1 MB ceiling on one query's response —
// a D1 result set is not streamed — and the hundred-parameter
// ceiling on one statement, which an `IN (?, ?, …)` over a slice
// meets sooner than anyone expects. The absent features are listed
// at the foot of limits.go: no ATTACH, no PRAGMA, no extensions, no
// user-defined functions.
package d1
