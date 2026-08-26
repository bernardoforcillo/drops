// Package dropstest provides a recording [drops.Driver] for tests: it
// captures every statement a builder renders, hands back canned rows,
// and injects errors on demand — with no database, no network and no
// dependency outside the standard library.
//
// The style is deliberately a recorder, not an expectation DSL. Nothing
// is declared up front and nothing is "verified" at the end; the driver
// keeps an ordered []Statement and the test asserts on it with ordinary
// Go comparisons:
//
//	drv := dropstest.New().Rows([]string{"id", "name"}, []any{int64(1), "Ada"})
//	db := pg.New(drv)
//
//	users, err := db.Select().From(Users).Where(pg.Eq(UsersCols.ID, 1)).Rows(ctx)
//	...
//	if got, want := drv.LastSQL(), `SELECT * FROM "users" WHERE "id" = $1`; got != want {
//	    t.Errorf("got = %v, want %v", got, want)
//	}
//
// The alternative most Go projects reach for is sqlmock, which matches
// statements with regular expressions. That is where its reputation for
// brittle tests comes from: the assertion is a pattern over SQL text, so
// a whitespace change in the renderer breaks a test that was never about
// whitespace, while a genuinely wrong statement can still slip through a
// loose pattern. A recorder inverts both failures — the comparison is
// exact, and it is written in the test rather than hidden in a matcher.
//
// That drops can ship this in a couple of hundred lines, in-tree and
// dependency-free, is a dividend of the interface design rather than a
// coincidence: [drops.Driver] has three methods. A test double for
// database/sql has to impersonate driver.Conn, driver.Stmt, driver.Tx,
// driver.Rows and the optional Context and NamedValue variants of half
// of those, which is why doing it well became somebody's library. Here
// the whole contract is Exec, Query and Begin, so the double is a file.
//
// A *Driver is safe for concurrent use; see [Driver.Statements].
//
// What it deliberately does not do is satisfy the optional capability
// interfaces the dialects declare beside [drops.Driver] — pg.Copier,
// pg.Listener, pg.PoolStatsProvider. Those are duck-typed extras that a
// real driver may or may not have, and the tests for them are testing
// the detection: whether drops takes the capability path or the
// fallback. A recorder that implemented all three would answer that
// question the same way for every test that used it, which is why
// drops/pg keeps small doubles of its own there (listenDriver,
// copyingDriver, statsDriver) and should go on doing so.
package dropstest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/bernardoforcillo/drops"
)

var (
	_ drops.Driver = (*Driver)(nil)
	_ drops.Tx     = (*Tx)(nil)
	_ drops.Rows   = (*cursor)(nil)
	_ drops.Result = result{}
)

// Op says which driver call produced a [Statement]. Recording the
// transaction boundaries in the same stream as the statements is what
// makes an InTx path testable: a test can assert not just that a
// statement ran but that it ran between OpBegin and OpCommit, which is
// the whole question for anything that relies on transaction-local
// state — SET LOCAL, advisory locks, savepoints.
//
// There is no zero Op on purpose. A Statement literal that forgets the
// field fails the comparison loudly instead of silently claiming to be
// an Exec.
type Op string

const (
	OpExec     Op = "exec"
	OpQuery    Op = "query"
	OpBegin    Op = "begin"
	OpCommit   Op = "commit"
	OpRollback Op = "rollback"
)

// Statement is one recorded driver call. SQL and Args carry the text and
// bound arguments exactly as the builder produced them — Args is a copy
// taken at record time, so a caller that reuses its argument slice
// cannot rewrite history.
//
// OpBegin, OpCommit and OpRollback carry no SQL: they are driver calls,
// not statements sent to a server, and inventing the text "BEGIN" for
// them would make them indistinguishable from a caller that really did
// Exec that string.
type Statement struct {
	Op   Op
	SQL  string
	Args []any
}

// String renders a statement the way a test failure wants to read it.
func (s Statement) String() string {
	if s.SQL == "" {
		return string(s.Op)
	}
	if len(s.Args) == 0 {
		return string(s.Op) + " " + s.SQL
	}
	return fmt.Sprintf("%s %s %v", s.Op, s.SQL, s.Args)
}

// resultSet is one canned answer to a Query: the columns the cursor
// reports and the rows it yields, in order.
type resultSet struct {
	cols []string
	data [][]any
}

// Answerer decides what a Query gets back by looking at the statement
// itself. It returns the columns and rows to answer with, or ok=false
// to say "not mine" and leave the answer to whatever comes next.
//
// See [Driver.Answer] for when a test wants one.
type Answerer func(sql string, args []any) (cols []string, rows [][]any, ok bool)

// Driver is a drops.Driver that records instead of executing. The zero
// value is not usable; call [New].
type Driver struct {
	mu        sync.Mutex
	stmts     []Statement
	queue     []resultSet
	fallback  *resultSet
	answerers []Answerer
	nextErr   error
	affected  int64
}

// New returns an empty recorder. Queries answer with an empty result set
// and Execs report one affected row until told otherwise.
func New() *Driver { return &Driver{affected: 1} }

// Rows queues one canned result set. The next Query pops it; further
// Rows calls queue further sets, answered in the order they were added,
// so a test that drives two queries describes both:
//
//	drv := dropstest.New().
//	    Rows([]string{"id"}, []any{int64(1)}, []any{int64(2)}).
//	    Rows([]string{"id", "userId"}, []any{int64(7), int64(1)})
//
// Once the queue is drained, Query falls back to the set given to
// [Driver.AlwaysRows], or — with none — to a result with no columns and
// no rows, which every scanner in drops treats as "nothing matched".
//
// The values are copied, so the caller may reuse its slices.
func (d *Driver) Rows(cols []string, rows ...[]any) *Driver {
	set := copySet(cols, rows)
	d.mu.Lock()
	d.queue = append(d.queue, set)
	d.mu.Unlock()
	return d
}

// AlwaysRows sets the result every Query gets once the queue built by
// [Driver.Rows] is empty. It exists for the loop: a benchmark running
// the same query b.N times, or a test whose subject issues an unknown
// number of queries, cannot queue one set per call and should not have
// to.
func (d *Driver) AlwaysRows(cols []string, rows ...[]any) *Driver {
	set := copySet(cols, rows)
	d.mu.Lock()
	d.fallback = &set
	d.mu.Unlock()
	return d
}

// Answer registers a function that answers a Query by reading the
// statement, rather than by position in the queue [Driver.Rows] builds.
//
// The queue is the right shape for a test that drives a known sequence
// of queries: it says what the first one gets, then the second. It is
// the wrong shape for anything introspection-driven, where the subject
// issues a fixed set of catalogue queries whose order is the subject's
// business and not the test's — pg.Push alone reads tables, columns,
// primary keys, uniques, checks, indexes, foreign keys and a row
// estimate, and a queue describing all of them positionally breaks the
// moment a reader is added, reordered, or skipped by a version check.
// Every such test in this tree had answered that by hand-rolling a
// driver of its own with a switch on the SQL in it, which is this
// method with the recording, the concurrency safety and the scanning
// left out.
//
//	drv := dropstest.New().Answer(func(sql string, _ []any) ([]string, [][]any, bool) {
//	    if strings.Contains(sql, "information_schema.tables") {
//	        return []string{"table_schema", "table_name"},
//	            [][]any{{"public", "users"}}, true
//	    }
//	    return nil, nil, false
//	})
//
// Answerers are consulted in the order they were registered, before the
// queue, and the first one to return ok=true supplies the answer. One
// that returns false leaves the queue untouched, so the two compose:
// answer the catalogue by statement and the query under test by
// position. With no answerer claiming it, a Query falls through to the
// queue, then to [Driver.AlwaysRows], then to an empty result — exactly
// as before.
//
// The function is called with the driver's lock released, so it may
// call back into the driver ([Driver.SQL] to count what has been asked
// so far, for instance). The values it returns are copied.
func (d *Driver) Answer(fn Answerer) *Driver {
	if fn == nil {
		return d
	}
	d.mu.Lock()
	d.answerers = append(d.answerers, fn)
	d.mu.Unlock()
	return d
}

func copySet(cols []string, rows [][]any) resultSet {
	set := resultSet{cols: append([]string(nil), cols...)}
	for _, r := range rows {
		set.data = append(set.data, append([]any(nil), r...))
	}
	return set
}

// FailNext makes the next Exec or Query return err. The statement is
// still recorded before the error is returned: the SQL that failed is
// usually the thing the test wants to look at, and a driver that hid it
// would make "did it even get that far" unanswerable.
//
// A pending error is consumed by the call it fails, and it wins over a
// queued result set without consuming it — so a failure injected in the
// middle of a sequence does not shift the answers behind it. Calling
// FailNext again replaces an error that has not fired yet; pass nil to
// cancel one.
func (d *Driver) FailNext(err error) *Driver {
	d.mu.Lock()
	d.nextErr = err
	d.mu.Unlock()
	return d
}

// Affected sets the value [drops.Result.RowsAffected] reports for
// subsequent Execs. The default is 1, because the code paths that read
// it — optimistic locking, "did the UPDATE match anything" — treat 0 as
// a failed write, and a recorder whose default answer is "no rows
// touched" would fail those paths for reasons unrelated to the test.
func (d *Driver) Affected(n int64) *Driver {
	d.mu.Lock()
	d.affected = n
	d.mu.Unlock()
	return d
}

// Reset drops every recorded statement, queued result set and pending
// error, and restores the default affected count. It is what a
// benchmark calls in b.ResetTimer's neighbourhood and what a table test
// calls between subtests that share a driver.
func (d *Driver) Reset() *Driver {
	d.mu.Lock()
	d.stmts = nil
	d.queue = nil
	d.fallback = nil
	d.answerers = nil
	d.nextErr = nil
	d.affected = 1
	d.mu.Unlock()
	return d
}

// Statements returns every recorded call, in order, including the
// transaction boundaries and including statements issued through a Tx —
// a transaction records into the driver that opened it, so the ordering
// across the boundary survives.
//
// The slice and its statements are copies, and the method is safe to
// call while other goroutines are still using the driver: a query
// builder used from several goroutines is an ordinary shape, and a
// recorder that could only be read from the goroutine that wrote it
// would be unusable under -race in exactly the tests that need it most.
func (d *Driver) Statements() []Statement {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Statement, len(d.stmts))
	for i, s := range d.stmts {
		s.Args = append([]any(nil), s.Args...)
		out[i] = s
	}
	return out
}

// SQL returns the text of every recorded Exec and Query, in order. The
// transaction boundaries are skipped because they have none; use
// [Driver.Statements] when the boundaries are the subject.
func (d *Driver) SQL() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, s := range d.stmts {
		if s.Op == OpExec || s.Op == OpQuery {
			out = append(out, s.SQL)
		}
	}
	return out
}

// Last returns the most recent Exec or Query, or the zero Statement when
// none was recorded. Transaction boundaries are skipped for the reason a
// test cares about: after db.InTx(...) returns, the last recorded call is
// the commit, and "the last statement" is never what the caller meant by
// that.
func (d *Driver) Last() Statement {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i := len(d.stmts) - 1; i >= 0; i-- {
		if s := d.stmts[i]; s.Op == OpExec || s.Op == OpQuery {
			s.Args = append([]any(nil), s.Args...)
			return s
		}
	}
	return Statement{}
}

// LastSQL is the text of [Driver.Last], the single most common
// assertion this package exists to make pleasant.
func (d *Driver) LastSQL() string { return d.Last().SQL }

// record appends a statement and hands back any error injected for it.
// Recording and consuming the error happen under one lock so two
// goroutines cannot both collect the same FailNext.
func (d *Driver) record(op Op, sql string, args []any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, Statement{Op: op, SQL: sql, Args: append([]any(nil), args...)})
	err := d.nextErr
	d.nextErr = nil
	return err
}

// Exec records the statement and reports the configured affected count.
func (d *Driver) Exec(ctx context.Context, sqlStr string, args ...any) (drops.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.record(OpExec, sqlStr, args); err != nil {
		return nil, err
	}
	d.mu.Lock()
	n := d.affected
	d.mu.Unlock()
	return result{affected: n}, nil
}

// Query records the statement and answers with the next queued result
// set. It returns an explicit nil cursor on failure, for the reason
// stdlib's adapter does: a non-nil interface holding a nil cursor turns
// every caller's `if rows != nil { rows.Close() }` into a panic.
func (d *Driver) Query(ctx context.Context, sqlStr string, args ...any) (drops.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := d.record(OpQuery, sqlStr, args); err != nil {
		return nil, err
	}
	d.mu.Lock()
	answerers := d.answerers
	d.mu.Unlock()
	for _, fn := range answerers {
		if cols, rows, ok := fn(sqlStr, args); ok {
			set := copySet(cols, rows)
			return &cursor{cols: set.cols, data: set.data}, nil
		}
	}
	d.mu.Lock()
	var set resultSet
	switch {
	case len(d.queue) > 0:
		set, d.queue = d.queue[0], d.queue[1:]
	case d.fallback != nil:
		set = *d.fallback
	}
	d.mu.Unlock()
	return &cursor{cols: set.cols, data: set.data}, nil
}

// Begin records the boundary and returns a transaction that writes into
// this same recorder, so an InTx path is one ordered stream of calls
// rather than two records to be correlated afterwards.
//
// Nesting is allowed and simply records another begin: the recorder
// models no savepoints and rejects nothing, which is the honest position
// for a double whose job is to show what was sent.
func (d *Driver) Begin(ctx context.Context) (drops.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.stmts = append(d.stmts, Statement{Op: OpBegin})
	d.mu.Unlock()
	return &Tx{drv: d}, nil
}

// Tx is the transaction [Driver.Begin] hands back. Its Exec and Query
// record into the driver that opened it; Commit and Rollback record the
// boundary that closes it.
type Tx struct {
	drv  *Driver
	mu   sync.Mutex
	done bool
}

// ErrTxDone is returned by a second Commit or Rollback on the same
// transaction, and by an Exec or Query issued after either. Real drivers
// reject all three, and a recorder that accepted them would let a
// use-after-commit bug record a tidy statement list and pass.
var ErrTxDone = errors.New("drops/dropstest: transaction has already been committed or rolled back")

// finish closes the transaction and reports whether this call is the
// one that did it — false means it was already closed, which is what
// makes `if !t.finish() { return ErrTxDone }` the second Commit's
// refusal rather than the first one's.
func (t *Tx) finish() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	return true
}

func (t *Tx) open() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.done
}

func (t *Tx) Exec(ctx context.Context, sqlStr string, args ...any) (drops.Result, error) {
	if !t.open() {
		return nil, ErrTxDone
	}
	return t.drv.Exec(ctx, sqlStr, args...)
}

func (t *Tx) Query(ctx context.Context, sqlStr string, args ...any) (drops.Rows, error) {
	if !t.open() {
		return nil, ErrTxDone
	}
	return t.drv.Query(ctx, sqlStr, args...)
}

func (t *Tx) Begin(ctx context.Context) (drops.Tx, error) {
	if !t.open() {
		return nil, ErrTxDone
	}
	return t.drv.Begin(ctx)
}

// Commit records the boundary. It does not consult FailNext: an injected
// error is aimed at a statement, and a commit that failed because the
// statement before it was told to fail would be a second, invisible
// effect of a one-line call.
func (t *Tx) Commit(_ context.Context) error { return t.close(OpCommit) }

// Rollback records the boundary. Note that drops rolls back on a
// detached context with its own deadline, so a cancelled ctx must not
// stop it being recorded — the rollback that runs after a cancellation
// is precisely the one a test is checking for.
func (t *Tx) Rollback(_ context.Context) error { return t.close(OpRollback) }

func (t *Tx) close(op Op) error {
	if !t.finish() {
		return ErrTxDone
	}
	t.drv.mu.Lock()
	t.drv.stmts = append(t.drv.stmts, Statement{Op: op})
	t.drv.mu.Unlock()
	return nil
}

// result is the outcome of a recorded Exec.
type result struct{ affected int64 }

func (r result) RowsAffected() (int64, error) { return r.affected, nil }

// cursor walks one canned result set. Each Query gets its own, so two
// goroutines querying the same driver never share a position.
type cursor struct {
	cols   []string
	data   [][]any
	pos    int
	closed bool
	err    error
}

func (c *cursor) Next() bool {
	if c.closed || c.err != nil || c.pos >= len(c.data) {
		return false
	}
	c.pos++
	return true
}

// Scan copies the current row into dest. The conversion is deliberately
// closer to a real driver than a plain assignment: a canned int64 lands
// in an int field, a nil lands in a *string as nil rather than as a
// panic, and a destination implementing sql.Scanner is asked to scan the
// value — those are the paths a reflection scanner gets wrong, so a
// double that skipped them would test the easy half of the scanner.
func (c *cursor) Scan(dest ...any) error {
	if c.closed {
		return fmt.Errorf("drops/dropstest: Scan on a closed cursor")
	}
	if c.pos == 0 || c.pos > len(c.data) {
		return fmt.Errorf("drops/dropstest: Scan called without a preceding Next")
	}
	row := c.data[c.pos-1]
	if len(dest) != len(row) {
		c.err = fmt.Errorf("drops/dropstest: row %d has %d values but Scan was given %d destinations for columns %v",
			c.pos-1, len(row), len(dest), c.cols)
		return c.err
	}
	for i, d := range dest {
		if err := assign(d, row[i]); err != nil {
			c.err = fmt.Errorf("drops/dropstest: column %d (%s): %w", i, c.columnName(i), err)
			return c.err
		}
	}
	return nil
}

func (c *cursor) columnName(i int) string {
	if i < len(c.cols) {
		return c.cols[i]
	}
	return "?"
}

func (c *cursor) Columns() ([]string, error) { return c.cols, nil }
func (c *cursor) Close() error               { c.closed = true; return nil }
func (c *cursor) Err() error                 { return c.err }

// assign writes src into the pointer dest, following the conversions a
// database driver would perform.
func assign(dest, src any) error {
	if sc, ok := dest.(sql.Scanner); ok {
		return sc.Scan(src)
	}
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return fmt.Errorf("destination %T is not a non-nil pointer", dest)
	}
	elem := rv.Elem()
	if src == nil {
		elem.Set(reflect.Zero(elem.Type()))
		return nil
	}
	sv := reflect.ValueOf(src)
	st, dt := sv.Type(), elem.Type()
	switch {
	case st.AssignableTo(dt):
		elem.Set(sv)
	case dt.Kind() == reflect.Ptr && st.AssignableTo(dt.Elem()):
		// A nullable column read into *T: the canned value is the
		// unwrapped T, because writing []any{ptr(&"x")} in a test is
		// noise nobody would tolerate twice.
		p := reflect.New(dt.Elem())
		p.Elem().Set(sv)
		elem.Set(p)
	case dt.Kind() == reflect.String && isInteger(st.Kind()):
		// Go converts an integer to a string as a rune, so int64(65)
		// would arrive as "A". No driver does that, and a silent "A" is
		// far worse than a refusal.
		return fmt.Errorf("cannot scan %T into *%s", src, dt)
	case st.ConvertibleTo(dt):
		elem.Set(sv.Convert(dt))
	default:
		return fmt.Errorf("cannot scan %T into *%s", src, dt)
	}
	return nil
}

func isInteger(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}
