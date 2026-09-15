package d1

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bernardoforcillo/drops"
)

// Session errors.
var (
	// ErrSessionsUnsupported is returned by [Driver.Session] and
	// [Driver.Resume] when the driver's transport cannot carry one.
	//
	// It is the answer for [RESTTransport], because Cloudflare does
	// not offer sessions on the REST API — not because this package
	// declined to implement them. The Sessions API is a Worker
	// binding feature, so a session needs [NewBridge] and a Worker
	// holding the binding.
	//
	// The refusal is at construction rather than at the first query,
	// so a deployment that expects read-your-writes and is pointed
	// at the REST transport by a changed environment variable fails
	// at boot instead of serving a stale read months later.
	ErrSessionsUnsupported = errors.New("drops/cloudflare/d1: this transport cannot carry a D1 session")

	// ErrUnknownConstraint is returned for a starting constraint D1
	// does not define.
	ErrUnknownConstraint = errors.New("drops/cloudflare/d1: unknown session constraint")

	// ErrSessionDone is returned by a session used after [Session.Close].
	ErrSessionDone = errors.New("drops/cloudflare/d1: session is closed")
)

// Constraint is where a session's first request is allowed to be
// served from.
//
// It applies to the first request only. Every request after it
// travels with the bookmark the previous one returned, which is what
// makes the session sequentially consistent regardless of which
// instance answers.
type Constraint string

// The constraints D1 defines.
const (
	// FirstUnconstrained lets the first request be served by any
	// instance, replica or primary. It is the fast default and the
	// right one for a session that only reads.
	//
	// What it does not promise is that the first read sees a write
	// made in some *other* session — a write from the previous HTTP
	// request, for instance. Carry that session's bookmark into this
	// one with [Driver.Resume] if it must.
	FirstUnconstrained Constraint = "first-unconstrained"

	// FirstPrimary sends the first request to the primary, so it
	// sees every write committed before it. Use it when the session
	// starts by reading something it is about to decide on, and
	// there is no bookmark to resume from.
	FirstPrimary Constraint = "first-primary"
)

// Valid reports whether c is a constraint D1 defines.
func (c Constraint) Valid() bool {
	switch c {
	case FirstUnconstrained, FirstPrimary:
		return true
	default:
		return false
	}
}

// SessionTransport is a [Transport] that can carry a D1 session.
//
// It is a second interface rather than two more methods on
// [Transport] because the ability is genuinely not universal:
// Cloudflare's REST API has no sessions, so [RESTTransport] cannot
// implement this however much it would like to, and a transport that
// answered "yes" and then dropped the session would break the only
// guarantee a session exists to give.
type SessionTransport interface {
	Transport

	// SendInSession runs stmts inside a session.
	//
	// token is what D1's withSession() takes: a [Constraint] for the
	// session's first request, and the bookmark returned by the
	// previous request for every one after it.
	//
	// The returned bookmark is where the session stands afterwards.
	// An empty one means the runtime reported none — wrangler dev
	// does not — and leaves the caller's token unchanged rather than
	// resetting the session to its constraint.
	SendInSession(ctx context.Context, stmts []Statement, readOnly bool, token string) ([]StatementResult, string, error)
}

// Session is a sequentially consistent series of statements against a
// D1 database that has read replication enabled.
//
// It exists because a replica is allowed to be behind. Without a
// session, two consecutive reads may be served by two instances and
// the second may see *less* than the first; a write followed by a
// read may not see the write. D1's answer is the bookmark: every
// request in a session returns one, the next request carries it, and
// D1 refuses to serve that request from an instance that has not
// caught up to it. The reads stay local and cheap, and they stop
// going backwards.
//
// A Session is itself a [github.com/bernardoforcillo/drops.Driver],
// so the whole of drops/sqlite runs inside one:
//
//	sess, err := drv.Session(d1.FirstUnconstrained)
//	db := sqlite.New(sess)
//	users, err := sqlite.All[User](Users.Select(), db, ctx)
//
// The unit to scope one to is the unit the consistency is wanted
// over: an HTTP request, a job, a page render. Sessions are cheap —
// there is no connection and nothing to close — so making one per
// request is the intended use, not a cost to avoid.
//
// To extend the guarantee past the end of one, hand the bookmark on:
// [Session.Bookmark] after the last statement, [Driver.Resume] at the
// start of the next. That is what carries read-your-writes across a
// redirect, or from the request that wrote to the one that reads.
//
// # Concurrency
//
// A Session is safe to use from several goroutines, in the sense that
// the bookmark is not corrupted by concurrent use. It is not made
// *ordered* by that: two statements issued concurrently reach D1 in
// whatever order they reach it, and each carries whichever bookmark
// the session held when it was issued. Sequential consistency is a
// property of a sequence, so a session used concurrently gets the
// guarantee its requests' actual order earns, which is the same thing
// concurrent statements get on any database.
type Session struct {
	drv        *Driver
	tr         SessionTransport
	constraint Constraint

	mu       sync.Mutex
	bookmark string
	closed   bool
}

var _ drops.Driver = (*Session)(nil)

// Session opens a session on d, starting under the given constraint.
//
// The empty constraint means [FirstUnconstrained], which is what
// D1's withSession() means when called with no argument.
//
// It returns [ErrSessionsUnsupported] when the driver's transport
// cannot carry a session, which is the case for the REST transport:
// Cloudflare offers the Sessions API through a Worker binding only.
// Use [NewBridge].
func (d *Driver) Session(c Constraint) (*Session, error) {
	if c == "" {
		c = FirstUnconstrained
	}
	if !c.Valid() {
		return nil, fmt.Errorf("%w: %q — use d1.FirstPrimary or d1.FirstUnconstrained", ErrUnknownConstraint, c)
	}
	tr, err := d.sessionTransport()
	if err != nil {
		return nil, err
	}
	return &Session{drv: d, tr: tr, constraint: c}, nil
}

// Resume opens a session that continues from a bookmark an earlier
// session reported — so the new session's first read is served by an
// instance that has caught up to where the old one left off.
//
// This is how read-your-writes survives the end of a request. The
// request that wrote calls [Session.Bookmark] and stores the string
// (a cookie, a header, a queue message); the request that reads
// passes it here.
//
// An empty bookmark is [ErrNoBookmark] rather than a silently
// unconstrained session: the caller asked for a guarantee, and a
// session that quietly does not provide it is worse than one that
// says so.
func (d *Driver) Resume(bookmark string) (*Session, error) {
	if bookmark == "" {
		return nil, ErrNoBookmark
	}
	tr, err := d.sessionTransport()
	if err != nil {
		return nil, err
	}
	return &Session{drv: d, tr: tr, constraint: FirstUnconstrained, bookmark: bookmark}, nil
}

// ErrNoBookmark is returned by [Driver.Resume] with an empty
// bookmark.
var ErrNoBookmark = errors.New("drops/cloudflare/d1: cannot resume a session from an empty bookmark")

// sessionTransport returns the driver's transport as a
// [SessionTransport], or explains why it is not one.
func (d *Driver) sessionTransport() (SessionTransport, error) {
	if d.tr == nil {
		return nil, errors.New("drops/cloudflare/d1: driver has no transport")
	}
	tr, ok := d.tr.(SessionTransport)
	if !ok {
		return nil, fmt.Errorf("%w: %s — the Sessions API is a Worker binding feature, so a session needs d1.NewBridge and a Worker holding the D1 binding",
			ErrSessionsUnsupported, d.tr.Target())
	}
	return tr, nil
}

// Bookmark returns where the session stands: the bookmark the last
// request returned, or the bookmark it was resumed from before any
// request has run.
//
// It is empty for a session that has not run a statement yet and was
// not resumed from one, and for a runtime that reports no bookmark —
// wrangler dev does not. An empty bookmark is not an error to store;
// it is [ErrNoBookmark] to resume from, which is the point at which
// the absence matters.
func (s *Session) Bookmark() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bookmark
}

// Constraint returns the constraint the session started under.
func (s *Session) Constraint() Constraint { return s.constraint }

// Driver returns the driver the session runs on.
func (s *Session) Driver() *Driver { return s.drv }

// String names the session's target, for logs and error messages.
func (s *Session) String() string {
	return fmt.Sprintf("d1 session(%s) on %s", s.constraint, s.tr.Target())
}

// Close ends the session. It releases nothing — there is no
// connection — and exists so a Session can stand in for a Driver
// wherever one is closed, and so that a session handed on by mistake
// after its request finished fails loudly instead of running with a
// stale bookmark.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// Ping verifies the database is reachable inside the session.
func (s *Session) Ping(ctx context.Context) error {
	_, err := s.Exec(ctx, "SELECT 1")
	return err
}

// Exec runs a statement that returns no rows, inside the session.
func (s *Session) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	params, err := BindParams(args)
	if err != nil {
		return nil, err
	}
	out, err := s.send(ctx, []Statement{{SQL: sql, Params: params}})
	if err != nil {
		return nil, err
	}
	return &Result{meta: out[len(out)-1].Meta}, nil
}

// Query runs a statement that returns rows, inside the session.
//
// [Rows.Meta] reports which instance answered:
// [Meta.ServedByPrimary] false is a replica, which the bookmark has
// already established is caught up far enough to answer.
func (s *Session) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	params, err := BindParams(args)
	if err != nil {
		return nil, err
	}
	out, err := s.send(ctx, []Statement{{SQL: sql, Params: params}})
	if err != nil {
		return nil, err
	}
	last := out[len(out)-1]
	return &Rows{columns: last.Columns, rows: last.Rows, meta: last.Meta}, nil
}

// Begin opens a deferred transaction whose commit runs inside the
// session, advancing its bookmark like any other request.
//
// Every restriction in the package comment still applies: the
// statements are buffered until Commit, [Tx.Query] returns
// [ErrTxQuery], and a buffered result's row count is [ErrPending]
// until then. A session makes the reads consistent; it does not give
// D1 the interactive transactions it does not have.
func (s *Session) Begin(ctx context.Context) (drops.Tx, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return &Tx{on: s}, nil
}

// Batch starts a batch that runs inside the session.
func (s *Session) Batch() *Batch { return &Batch{on: s} }

// send runs stmts inside the session and advances the bookmark.
//
// The bookmark is read and written under the lock, but the request
// itself is made outside it: holding a mutex across an HTTPS round
// trip would serialise a session that a caller deliberately made
// concurrent, and the ordering that would buy is not one this type
// can promise anyway (see the type comment).
func (s *Session) send(ctx context.Context, stmts []Statement) ([]StatementResult, error) {
	if len(stmts) == 0 {
		return nil, ErrNoStatements
	}
	if err := s.drv.checkLimits(stmts); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionDone
	}
	token := s.bookmark
	if token == "" {
		token = string(s.constraint)
	}
	s.mu.Unlock()

	out, bookmark, err := s.tr.SendInSession(ctx, stmts, readOnly(stmts), token)
	if err != nil {
		// A failed request did not move the database, so it did not
		// move the session either. Keeping the old bookmark is what
		// makes a retry as consistent as the attempt that failed.
		return nil, err
	}

	if bookmark != "" {
		s.mu.Lock()
		s.bookmark = bookmark
		s.mu.Unlock()
	}
	return out, nil
}
