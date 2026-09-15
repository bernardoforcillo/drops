package clickhouse

import (
	"errors"
	"fmt"
	"time"
)

// The statement-size ceiling, and why this file is not paramlimit.go.
//
// drops/pg, drops/mysql and drops/sqlite each refuse a statement
// carrying more bound parameters than the backend accepts — 65535 for
// the first two, because the wire protocol counts them in an int16, and
// 32766 for the third. ClickHouse has no such number, and porting the
// check would have meant measuring a quantity that does not exist.
//
// The reason is the driver. clickhouse-go binds "?" by SUBSTITUTION:
// bindPositional walks the query, renders each argument as a SQL
// literal — quoted and escaped — and returns a string. Nothing travels
// as a parameter, so nothing counts them, and a statement with a
// million of them is to the server one long query.
//
// # What the ceiling is instead
//
// max_query_size, which is a server setting rather than a protocol
// constant: 262144 bytes by default, changeable per session and per
// user. It applies to the text the server receives, which is the text
// AFTER substitution — so the quantity that matters is not what drops
// rendered but what the driver will render from it.
//
// Reaching it is ordinary rather than exotic, and for the same reasons
// the parameter ceilings are reached elsewhere: a bulk insert whose
// batch grew, an IN list built from a page of ids. Eight short columns
// at 32 bytes a row cross 256 KiB at about a thousand rows, which is a
// small batch by any standard.
//
// # Why the failure is worth intercepting
//
// ClickHouse answers with a SYNTAX ERROR:
//
//	Code: 62. DB::Exception: Syntax error: failed at position 262144:
//	Max query size exceeded
//
// Which is the misleading part. The data is fine, the SQL is
// well-formed, and the position is not where anything is wrong — it is
// simply where the server stopped reading. A caller debugging that
// message goes looking for a quoting bug in row 4,000-something, which
// is not there.
//
// # Why the estimate is a lower bound, deliberately
//
// drops cannot compute the rendered size exactly: the rendering is
// clickhouse-go's, and it depends on the connection's timezone for
// times, on the element types for arrays and maps, and on how many
// characters in each string need escaping. So this estimates from
// below — the smallest text each argument could possibly render to.
//
// That direction is the one that makes a refusal safe. An estimate that
// could overshoot would refuse statements the server would have
// accepted, which is worse than the failure it prevents; one that can
// only undershoot refuses nothing that would have worked. The price is
// that a statement just over the line is still sent and still fails,
// with ClickHouse's message. Everything comfortably over is caught
// here, named, and told what to do.

// maxQuerySize is the number of bytes the server will read of one
// statement.
//
// It is max_query_size's default. A server or a user profile configured
// with a LOWER value will still refuse a statement this admits, and the
// message will be ClickHouse's; drops has no way to ask the connection
// what its setting is, so this is the ceiling it can promise rather
// than the one in force. The direction is the safe one, as with the
// estimate itself: every statement drops refuses here would have
// failed at the default too.
const maxQuerySize = 262144

// ErrQueryTooLarge is returned before a statement that cannot fit in
// one query is sent.
//
// It is raised by drops rather than by the server, which is the whole
// point: the same failure arrives from ClickHouse as a syntax error at
// a position where nothing is wrong.
var ErrQueryTooLarge = errors.New("drops/clickhouse: statement is larger than ClickHouse will read in one query")

// checkQuerySize refuses a statement whose rendered text cannot be
// read. It is called from [DB.Exec] and [DB.Query] rather than from the
// builders, so one check covers every path: a bulk INSERT, an IN list
// that grew, and a statement a caller assembled themselves.
func checkQuerySize(sql string, args []any) error {
	size := renderedSizeAtLeast(sql, args)
	if size <= maxQuerySize {
		return nil
	}
	return fmt.Errorf("%w: at least %d bytes once the arguments are substituted, limit %d, in %s; "+
		"send the rows in batches, or narrow the list",
		ErrQueryTooLarge, size, maxQuerySize, excerptSQL(sql))
}

// renderedSizeAtLeast is the smallest text the driver could produce
// from sql and args.
//
// Each argument replaces one byte of the query — its "?" — with at
// least renderedAtLeast bytes. Placeholders are not counted from the
// query text: a "?" inside a string literal is not one, and miscounting
// in either direction is avoided by taking the count from the arguments
// the caller actually passed, which is the number the driver will
// substitute.
func renderedSizeAtLeast(sql string, args []any) int {
	size := len(sql) - len(args)
	for _, a := range args {
		size += renderedAtLeast(a)
	}
	return size
}

// renderedAtLeast is the shortest SQL literal an argument could render
// to.
//
// Every case is a floor rather than a guess. A string renders inside
// quotes and grows by one byte for each character that needs escaping,
// so len+2 is exactly its minimum. A number's decimal text is at least
// one digit. A time renders as a quoted datetime, which is never
// shorter than "'2006-01-02 15:04:05'". Anything drops does not
// recognise contributes 1, which is smaller than any literal ClickHouse
// has — an unknown type must not be able to push the estimate above
// what the driver will produce.
func renderedAtLeast(a any) int {
	switch v := a.(type) {
	case nil:
		return len("NULL")
	case string:
		return len(v) + 2
	case []byte:
		return len(v) + 2
	case time.Time:
		// The shortest a time can render to, and it is much shorter
		// than a datetime literal looks: clickhouse-go writes a
		// Local-zone time as toDateTime('<unix seconds>'), and the
		// Unix epoch itself as the bare toDateTime(0). Everything else
		// it emits — a zoned toDateTime('2006-01-02 15:04:05', 'UTC'),
		// a toDateTime64 with a scale — is longer.
		//
		// Estimating the datetime literal here instead would have been
		// the natural guess and would have broken the guarantee: 21
		// bytes for a value that renders to 13 makes the estimate an
		// over-estimate, and an over-estimate refuses statements the
		// server would have accepted.
		return len("toDateTime(0)")
	case bool:
		return 1
	case []string:
		// An array renders as [a, b, c]: the brackets, each element
		// quoted, and a separator between them.
		size := 2
		for i, s := range v {
			if i > 0 {
				size++
			}
			size += len(s) + 2
		}
		return size
	default:
		return 1
	}
}
