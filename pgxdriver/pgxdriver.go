// Package pgxdriver connects drops/pg to a real PostgreSQL server
// through jackc/pgx.
//
//	pool, err := pgxpool.New(ctx, dsn)
//	if err != nil {
//	    return err
//	}
//	defer pool.Close()
//	db := pg.New(pgxdriver.New(pool))
//
// # Why this exists when drops/stdlib already works
//
// drops/stdlib wraps a *sql.DB and satisfies [drops.Driver], which is
// everything the query builders and the entity CRUD need. What it
// cannot satisfy is the four OPTIONAL interfaces drops/pg probes the
// driver for, because database/sql has no way to express any of them:
//
//   - [pg.Copier] — COPY FROM STDIN. database/sql has no COPY at all,
//     so [pg.CopyFrom] answered ErrCopyNotSupported for every caller,
//     and the bulk-load path drops documents was unreachable.
//   - [pg.Listener] — LISTEN / NOTIFY. It needs a connection held open
//     and read from, which is the one thing a pool of interchangeable
//     connections will not give you, so [pg.Subscribe] and the change
//     feed had nothing to run on.
//   - [pg.PoolStatsProvider] — the numbers [pg.StartPoolMetrics]
//     samples. database/sql has its own DBStats and drops/stdlib does
//     report those; pgx's are the ones that describe the pool the
//     statements actually go through.
//   - [pg.ConnAcquirer] — one connection out of the pool, for the
//     queue-time instrumentation and for anything that must not be
//     rescheduled onto a second connection halfway through.
//
// So this is not a faster stdlib. It is the driver that makes four
// shipped features work, and reaching for it is how you turn them on.
//
// # What it does not change
//
// Errors. drops/pg reads a SQLSTATE from any error exposing
// SQLState() and a constraint name from *pgconn.PgError, which pgx
// returns natively — so a violation raised here reaches the caller
// naming the constraint it broke, exactly as it does through
// drops/stdlib. Nothing here translates anything.
package pgxdriver

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// New wraps a pgx pool as a drops driver.
//
// The pool's lifetime stays the caller's: this does not close it, and
// a driver whose pool has been closed answers with pgx's own error
// rather than a wrapper's. Closing a pool a *pg.DB still holds is the
// caller's mistake to make and their error to read.
func New(pool *pgxpool.Pool) drops.Driver { return &poolDriver{pool: pool} }

// poolDriver is the pool as a drops.Driver, plus the four optional
// interfaces the pool can answer for.
type poolDriver struct{ pool *pgxpool.Pool }

// Compile-time proof that every interface this package exists for is
// actually satisfied.
//
// Each one is optional — drops/pg probes for it with a type assertion
// and falls back when it is absent — so a method whose signature has
// drifted does not fail to compile anywhere. It stops being found, and
// the feature it carries goes quietly back to being unavailable, which
// is the state this package was written to end. The assertions are what
// turn that into a build error.
var (
	_ drops.Driver         = (*poolDriver)(nil)
	_ pg.Copier            = (*poolDriver)(nil)
	_ pg.Listener          = (*poolDriver)(nil)
	_ pg.PoolStatsProvider = (*poolDriver)(nil)
	_ pg.ConnAcquirer      = (*poolDriver)(nil)
	_ drops.Tx             = (*txDriver)(nil)
	_ drops.Driver         = (*connDriver)(nil)
	_ pg.PoolStatsProvider = (*connDriver)(nil)
)

func (d *poolDriver) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	tag, err := d.pool.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return commandTag(tag), nil
}

func (d *poolDriver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	rows, err := d.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

func (d *poolDriver) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &txDriver{tx: tx}, nil
}

// Copy implements [pg.Copier] with COPY FROM STDIN, which is the whole
// reason a bulk load is faster than an INSERT: the rows go over the
// wire as a stream in the binary format rather than as parameters of a
// statement, so there is no per-row parse and no parameter ceiling.
//
// The table name is split on a dot so a schema-qualified destination
// works. pgx.Identifier quotes each part, so a name that needs quoting
// gets it and a name containing a dot that is NOT a qualifier is the
// one shape this cannot tell apart — the same ambiguity every tool
// that accepts "schema.table" as one string has.
func (d *poolDriver) Copy(ctx context.Context, table string, cols []string, rows [][]any) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	return d.pool.CopyFrom(ctx, identifier(table), cols, pgx.CopyFromRows(rows))
}

// Listen implements [pg.Listener].
//
// It takes a connection OUT of the pool and keeps it: a listener is a
// connection the server writes to unprompted, and there is no such
// thing as listening on a pool — the next statement would be handed a
// different connection, which is not listening to anything. The
// connection is returned when ctx is cancelled, and cancelling the ctx
// is how a caller stops listening.
//
// The channel is unbuffered on purpose. A notification carries no
// payload drops needs — see [pg.Notification] — and a buffer would only
// decide how many of them to keep while the consumer is not reading.
// Dropping the ones a stopped consumer would never read is the honest
// answer, and NOTIFY is at-most-once anyway: a consumer that must not
// miss one reads the table the notification is about.
func (d *poolDriver) Listen(ctx context.Context, channel string) (<-chan pg.Notification, error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	// LISTEN takes an identifier and PostgreSQL has nowhere to bind
	// one, so the channel name is quoted rather than bound. It comes
	// from the caller's own code — a channel name is a constant in
	// every use of this — and quoting is what makes that true rather
	// than assumed.
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{channel}.Sanitize()); err != nil {
		conn.Release()
		return nil, err
	}
	out := make(chan pg.Notification)
	go func() {
		defer conn.Release()
		defer close(out)
		for {
			n, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				// A cancelled ctx is how a caller stops listening,
				// and every other error has closed the connection
				// under us. Either way there is nothing left to
				// deliver, and closing the channel is what tells the
				// consumer so.
				return
			}
			select {
			case out <- pg.Notification{Channel: n.Channel, Payload: n.Payload, PID: int(n.PID)}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// Stats implements [pg.PoolStatsProvider].
//
// Four of drops' fields have no counterpart in pgx and stay zero:
// MaxIdleClosed, MaxIdleTimeClosed and MaxLifetimeClosed are
// database/sql's accounting of WHY a connection was closed, and pgx
// reports the closures without the reason. Reporting a plausible
// number for them would be worse than reporting none — a dashboard
// cannot tell an invented zero from a measured one, and these are
// exactly the numbers somebody reads to decide whether a pool is
// churning.
func (d *poolDriver) Stats() pg.PoolStats {
	s := d.pool.Stat()
	return pg.PoolStats{
		MaxOpenConnections: int(s.MaxConns()),
		OpenConnections:    int(s.TotalConns()),
		InUse:              int(s.AcquiredConns()),
		Idle:               int(s.IdleConns()),
		WaitCount:          s.EmptyAcquireCount(),
		WaitDuration:       s.AcquireDuration(),
	}
}

// AcquireConn implements [pg.ConnAcquirer]: one connection out of the
// pool, and the release that must be called exactly once.
//
// It acquires and does nothing else, which is the interface's own
// requirement — everything this function does is counted as queue time,
// so a round trip here would be reported as time spent waiting for a
// connection that was already in hand.
func (d *poolDriver) AcquireConn(ctx context.Context) (drops.Driver, func() error, error) {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	release := func() error {
		conn.Release()
		return nil
	}
	return &connDriver{conn: conn, pool: d.pool}, release, nil
}

// connDriver is one acquired connection as a driver. Every statement
// through it goes to that connection, which is what an acquirer's
// caller asked for.
type connDriver struct {
	conn *pgxpool.Conn
	pool *pgxpool.Pool
}

func (c *connDriver) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	tag, err := c.conn.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return commandTag(tag), nil
}

func (c *connDriver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	rows, err := c.conn.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

func (c *connDriver) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := c.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &txDriver{tx: tx}, nil
}

// Stats answers for the pool the connection came out of, so the
// instrumentation that acquired it can still read the pool it is
// measuring.
func (c *connDriver) Stats() pg.PoolStats { return (&poolDriver{pool: c.pool}).Stats() }

// txDriver is an in-flight transaction.
type txDriver struct{ tx pgx.Tx }

func (t *txDriver) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	tag, err := t.tx.Exec(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return commandTag(tag), nil
}

func (t *txDriver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	rows, err := t.tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &pgxRows{rows: rows}, nil
}

// Begin inside a transaction is a SAVEPOINT, which is what pgx's own
// tx.Begin does. drops/pg documents nesting as driver-dependent — see
// DB.InTxAs — and this is the driver that implements it: an inner
// rollback undoes the inner work and leaves the outer transaction
// live, where database/sql refuses the call outright.
func (t *txDriver) Begin(ctx context.Context) (drops.Tx, error) {
	tx, err := t.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &txDriver{tx: tx}, nil
}

func (t *txDriver) Commit(ctx context.Context) error { return t.tx.Commit(ctx) }

// Rollback treats "already closed" as success.
//
// drops rolls back in a defer, so the ordinary successful path is
// commit-then-rollback and the second call finds nothing to undo. pgx
// reports that as ErrTxClosed, and returning it would turn every
// successful transaction into one whose cleanup failed.
func (t *txDriver) Rollback(ctx context.Context) error {
	err := t.tx.Rollback(ctx)
	if errors.Is(err, pgx.ErrTxClosed) {
		return nil
	}
	return err
}

// Copy on a transaction is the same COPY, inside it — so a bulk load
// can be part of a larger unit of work and rolled back with it.
func (t *txDriver) Copy(ctx context.Context, table string, cols []string, rows [][]any) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	return t.tx.CopyFrom(ctx, identifier(table), cols, pgx.CopyFromRows(rows))
}

// pgxRows adapts pgx.Rows to drops.Rows.
type pgxRows struct{ rows pgx.Rows }

func (r *pgxRows) Next() bool             { return r.rows.Next() }
func (r *pgxRows) Scan(dest ...any) error { return r.rows.Scan(dest...) }
func (r *pgxRows) Err() error             { return r.rows.Err() }

// Close returns no error because pgx's does not: it releases the
// connection and any failure is reported by Err.
func (r *pgxRows) Close() error { r.rows.Close(); return nil }

// Columns reads the names off the field descriptions, which is where
// pgx keeps them.
func (r *pgxRows) Columns() ([]string, error) {
	fds := r.rows.FieldDescriptions()
	out := make([]string, len(fds))
	for i, fd := range fds {
		out[i] = fd.Name
	}
	return out, nil
}

// commandTag is a pgconn.CommandTag as a drops.Result.
type commandTag pgconn.CommandTag

func (t commandTag) RowsAffected() (int64, error) {
	return pgconn.CommandTag(t).RowsAffected(), nil
}

// identifier splits a possibly schema-qualified name for pgx, which
// quotes each part it is given.
func identifier(table string) pgx.Identifier {
	for i := 0; i < len(table); i++ {
		if table[i] == '.' {
			return pgx.Identifier{table[:i], table[i+1:]}
		}
	}
	return pgx.Identifier{table}
}
