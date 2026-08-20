package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bernardoforcillo/drops"
)

// Outbox is the transactional outbox pattern wired to drops/mysql. It
// solves the "publish only if the DB write committed" problem by
// writing the event into a co-resident outbox table inside the same
// transaction as the business write; a background worker then drains
// the table and hands rows to a publisher.
//
//	// Schema setup (once)
//	outboxTable := mysql.NewOutboxTable("outbox")
//	for _, stmt := range mysql.OutboxDDL(outboxTable) {
//	    if _, err := db.ExecExpr(ctx, stmt); err != nil { return err }
//	}
//
//	// Emit inside the business transaction
//	ob := mysql.NewOutbox(db, "outbox")
//	err := db.InTx(ctx, func(tx *mysql.DB) error {
//	    if err := UserEntity.Create(tx, ctx, &u); err != nil { return err }
//	    return ob.EmitWith(tx, ctx, "user.created", u, mysql.EmitOptions{
//	        AggregateType: "user",
//	        AggregateID:   strconv.FormatInt(u.ID, 10),
//	        Headers:       map[string]string{"traceparent": tp},
//	    })
//	})
//
//	// Drain in a worker goroutine
//	worker := mysql.NewOutboxWorker(ob).
//	    WithInterval(time.Second).
//	    WithMaxAttempts(10).
//	    WithOrdering(mysql.OrderingPerAggregate).
//	    OnEvent(func(ctx context.Context, e mysql.OutboxEvent) error {
//	        return publisher.Publish(ctx, e.Kind, e.Payload)
//	    })
//	go worker.Run(ctx)
//
// The pattern is at-least-once: a crash between publish and the
// follow-up UPDATE marking the row published will replay it. Make the
// publisher idempotent on the event id.
//
// # What MySQL makes different
//
// Four things, none of them cosmetic.
//
// There is no LISTEN/NOTIFY, so there is no WithNotifyChannel and no
// sub-second wakeup: the worker polls, and [OutboxWorker.WithInterval]
// is the only latency knob. A shorter interval on an idle outbox is
// cheap — the drain query is a range scan over the index described
// below that stops at the first published row.
//
// Row locking is a server-version question rather than a given.
// PostgreSQL has had FOR UPDATE SKIP LOCKED since 9.5; MySQL got it in
// 8.0 and MariaDB in 10.6, and 5.7 has neither the clause nor a
// substitute. [Outbox.WithLocking] and [Outbox.ProbeLocking] are how
// that is faced rather than guessed at — see [DrainLocking].
//
// There are no partial indexes, so "index only the rows a worker will
// consider" is expressed by leading the index with the column the
// predicate tests for NULL. See [OutboxDDL].
//
// Advisory locks are session-scoped, not transaction-scoped. PostgreSQL
// releases pg_advisory_xact_lock at commit; MySQL's GET_LOCK survives
// the transaction and is only released by RELEASE_LOCK or by the
// connection dropping, so [Outbox.DrainAggregate] releases it itself.
// The lock namespace is also server-wide rather than per-database: two
// applications with a table called "outbox" on one server share it,
// which is why the lock name carries the table name and why a
// distinctive one is worth having.
//
// # Feeding drops/mirror
//
// drops/mirror numbers a replicated row by the outbox event id, offset
// into its live band (see that package's doc, "# Versions"). The
// contract it needs of the id is three properties, and this outbox has
// all three: it comes from one AUTO_INCREMENT column in one database,
// so no clock skew can reach it; it is totally ordered; and it agrees
// with commit order per key, because an emitter that mutates a row and
// emits in one transaction makes the next writer of that row wait on
// the row lock, so the second writer cannot take an id ahead of the
// first. That last one is a claim about InnoDB rather than about
// drops, and the integration suite pins it.
//
// There is a fourth thing the contract assumes without stating it,
// because on PostgreSQL it is free: that an id is never handed out
// twice. A sequence never goes backwards. A MySQL AUTO_INCREMENT
// counter can — see [Outbox.Cleanup], which is why it keeps a row.
//
// What this package cannot do is wire itself up: mirror.EmitChange and
// mirror.NewOutboxSource take a *pg.Outbox, so a MySQL outbox needs an
// adapter. mirror.Source is an interface and mirror.LiveVersion is
// exported, so the adapter is small — Drain, decode the payloads,
// stamp LiveVersion of each event id, and MarkPublished the ids the
// sinks accepted — but until drops/mirror grows a dialect-neutral
// source, it is code the application writes rather than something this
// package hands over.
type Outbox struct {
	db      *DB
	table   string
	locking DrainLocking
	now     func() time.Time
}

// OutboxEvent is one drained row.
type OutboxEvent struct {
	ID            int64
	Kind          string
	AggregateType string
	AggregateID   string
	Payload       json.RawMessage
	Headers       map[string]string
	Attempts      int
	LastError     string
	CreatedAt     time.Time
}

// EmitOptions extends Emit with aggregate metadata and tracing
// headers. Aggregate fields enable per-aggregate ordering in the
// worker (see WithOrdering(OrderingPerAggregate)).
type EmitOptions struct {
	// AggregateType is the entity kind, e.g. "player", "match",
	// "wallet". Optional but recommended — pairs with AggregateID
	// to identify the ordering scope.
	AggregateType string

	// AggregateID is the per-aggregate ordering key, e.g. "42",
	// "match-abc". Events sharing the same AggregateID are
	// delivered in id order when the worker is configured with
	// WithOrdering(OrderingPerAggregate).
	AggregateID string

	// Headers carry tracing metadata (traceparent, correlationID,
	// userID, ...). Stored as JSON on the row and surfaced to the
	// handler so context propagates from emitter to consumer.
	Headers map[string]string
}

// NewOutbox returns an Outbox bound to db. Table is the SQL
// identifier of the outbox table; use NewOutboxTable to declare
// matching DDL.
func NewOutbox(db *DB, table string) *Outbox {
	if table == "" {
		table = "outbox"
	}
	return &Outbox{db: db, table: table, now: time.Now}
}

// Table returns the outbox table's name.
func (o *Outbox) Table() string { return o.table }

// DrainLocking selects the row-locking clause [Outbox.Drain] appends.
//
// The clause is what stops two workers from handing the same event to
// two publishers, and MySQL's support for it depends on the server:
// SKIP LOCKED arrived in MySQL 8.0 and MariaDB 10.6, and MySQL 5.7 has
// no equivalent at all. Rather than degrade silently on a server that
// lacks it — the one outcome that turns a duplicate delivery into a
// surprise — Drain fails with [ErrSkipLockedUnsupported] and names the
// two ways out.
type DrainLocking int

const (
	// LockSkipLocked appends FOR UPDATE SKIP LOCKED: a worker takes
	// the rows nobody else holds and leaves the rest. This is the
	// default and needs MySQL 8.0+ or MariaDB 10.6+.
	LockSkipLocked DrainLocking = iota

	// LockForUpdate appends a plain FOR UPDATE. On a server without
	// SKIP LOCKED this is the safe fallback: a second worker's drain
	// blocks behind the first rather than reading the same rows, so
	// a stalled consumer stalls the pool instead of duplicating
	// deliveries. Slower, never wrong.
	LockForUpdate

	// LockNone appends nothing. Correct only when exactly one worker
	// drains the table — with two, both read the same rows and both
	// publish them. Choose it deliberately, for a single-worker
	// deployment that would rather not pay for the locks.
	LockNone
)

// ErrSkipLockedUnsupported is returned by [Outbox.Drain] when the
// server rejected the SKIP LOCKED clause. Resolve it by calling
// [Outbox.ProbeLocking] at startup, or by choosing the mode outright
// with [Outbox.WithLocking].
var ErrSkipLockedUnsupported = errors.New("drops/mysql: the server does not support FOR UPDATE SKIP LOCKED")

// WithLocking sets the locking clause Drain appends. See
// [DrainLocking] for the trade-off each mode makes.
func (o *Outbox) WithLocking(m DrainLocking) *Outbox {
	o.locking = m
	return o
}

// Locking returns the configured locking mode.
func (o *Outbox) Locking() DrainLocking { return o.locking }

// ProbeLocking asks the server whether it parses SKIP LOCKED, sets the
// mode accordingly, and returns what it chose. A server that rejects
// the clause leaves the Outbox in [LockForUpdate] — the safe fallback,
// never [LockNone], which would trade a stall for duplicate deliveries.
//
// The probe is a drain query narrowed to no rows, so it asks the
// question the same way Drain will and costs nothing. Call it once at
// startup, before any worker runs: it writes the mode without
// synchronisation.
func (o *Outbox) ProbeLocking(ctx context.Context) (DrainLocking, error) {
	sql := fmt.Sprintf("SELECT `id` FROM %s WHERE 1 = 0 FOR UPDATE SKIP LOCKED", Dialect.QuoteIdent(o.table))
	rows, err := o.db.Query(ctx, sql)
	if err == nil {
		rows.Close()
		o.locking = LockSkipLocked
		return LockSkipLocked, rows.Err()
	}
	if isSkipLockedRejection(err) {
		o.locking = LockForUpdate
		return LockForUpdate, nil
	}
	return o.locking, err
}

// isSkipLockedRejection reports whether err is the server refusing to
// parse SKIP LOCKED.
//
// It is a string match because drops has no dependencies and so cannot
// see the driver's error type. A server without the clause answers
// error 1064 — a syntax error — and names the offending text, so both
// halves are required before the error is reinterpreted; a 1064 from
// somewhere else in the statement passes through untouched.
func isSkipLockedRejection(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "1064") && strings.Contains(msg, "SKIP LOCKED")
}

// NewOutboxTable declares the canonical outbox table layout, with the
// two indexes the drain queries need already registered. [OutboxDDL]
// renders it as statements; [Push] creates it from a [Schema].
//
// Two schema choices differ from drops/pg and both are deliberate.
//
// The table carries an explicitly binary collation, which every string
// column in it inherits. MySQL's default collation is
// case-insensitive, so under it the aggregates "Cart-7" and "cart-7"
// are one ordering scope, where PostgreSQL's text compares byte for
// byte; utf8mb4_bin restores the case-sensitivity. It is declared once
// on the table rather than per column because a column that spells its
// own COLLATE does not read back out of information_schema the way it
// was written — the catalogue reports the type and the collation in
// separate fields — so [Diff] would see a difference that is not one
// and MODIFY the column on every push, for ever. The table's collation
// is compared by name and settles.
//
// One thing the binary collation does not fix: utf8mb4_bin is PAD
// SPACE on both servers, so "cart-7 " and "cart-7" remain the same
// aggregate. (MySQL 8's utf8mb4_0900_* collations are NO PAD, but they
// are also case-insensitive, so they buy the padding at the price of
// the property that matters here.) Do not let an aggregate id end in a
// space.
//
// The timestamps are DATETIME(6) that drops always writes from Go and
// never compares against the server's NOW(). A DATETIME stores the
// wall clock it was handed with no zone attached, so a comparison
// against NOW() is off by whatever the server's time_zone is set to,
// silently; binding both sides from one process removes the server's
// clock from the question entirely. Read them back with a driver
// configured for UTC (go-sql-driver's parseTime=true, the default
// loc=UTC) and they round-trip exactly.
//
// What that costs, and it is worth knowing: the clock is now the
// application's rather than the database's, so a fleet whose hosts
// disagree writes availableAt against one clock and tests it against
// another. The effect is bounded by the skew and lands on latency, not
// on correctness — an event drains a little early or a little late,
// and delivery was at-least-once either way. drops/pg, which does
// compare against now(), has one clock and no such window.
func NewOutboxTable(name string) *Table {
	t := NewTable(name).Collate(streamKeyCollation)
	Add(t, BigSerial("id").PrimaryKey())
	Add(t, streamKeyColumn("kind").NotNull())
	Add(t, streamKeyColumn("aggregateType"))
	Add(t, streamKeyColumn("aggregateID"))
	Add(t, JSON("payload").NotNull())
	Add(t, JSON("headers"))
	Add(t, Timestamp("createdAt", false).NotNull())
	Add(t, Timestamp("availableAt", false).NotNull())
	Add(t, Timestamp("publishedAt", false))
	Add(t, Timestamp("failedAt", false))
	Add(t, Integer("attempts").NotNull().Default("0"))
	Add(t, Text("lastError"))
	for _, idx := range outboxIndexes(t) {
		t.AddIndex(idx)
	}
	return t
}

// streamKeyCollation is the binary collation every table in this file
// declares. See [NewOutboxTable] for why, and for why it sits on the
// table rather than on the column.
const streamKeyCollation = "utf8mb4_bin"

// streamKeyColumn declares the VARCHAR(255) that every identifier in
// this file uses. The collation comes from the table.
//
// 255 characters is four bytes each under utf8mb4, so a key part costs
// 1020 bytes of the 3072 an InnoDB index allows. Three of them do not
// fit, which is what caps the event store's stream key at two.
func streamKeyColumn(name string) *Col[string] {
	return Varchar(name, 255)
}

// OutboxDDL returns every statement needed to create t: the table and
// the two indexes the drain queries run on. It is the whole schema
// setup for an application that manages its DDL by hand; one that
// hands the table to a [Schema] and calls [Push] needs none of it,
// because [NewOutboxTable] has already registered the same two indexes
// with the table.
//
// The indexes are separate statements because MySQL creates a
// secondary index with its own DDL rather than inside CREATE TABLE.
// Their names go through the same folding rule as the table's other
// derived constraints: MySQL caps an identifier at 64 bytes, and "the
// table's name plus a suffix" overruns that on a table name drops
// otherwise accepts.
//
// Neither is partial, because MySQL has no partial indexes. drops/pg
// indexes (availableAt, id) WHERE publishedAt IS NULL AND failedAt IS
// NULL so the index holds only the rows a worker would consider; the
// nearest MySQL equivalent is to *lead* with publishedAt, since the
// optimiser treats IS NULL as an equality and then reads id in order
// within it. That is what keeps the drain's ORDER BY id … LIMIT free
// of a sort no matter how many published rows are waiting for
// [Outbox.Cleanup] — verified with EXPLAIN, which reports a range scan
// and no filesort. The published rows still occupy the index, which
// the partial index would have avoided; Cleanup is what pays that off,
// and it matters more here than it does on PostgreSQL.
func OutboxDDL(t *Table) []drops.Expression {
	out := []drops.Expression{CreateTable(t)}
	for _, idx := range outboxIndexes(t) {
		out = append(out, idx)
	}
	return out
}

// outboxIndexes builds the two drain indexes over t. Shared so the
// statements [OutboxDDL] renders and the indexes [NewOutboxTable]
// registers with the table cannot drift apart.
func outboxIndexes(t *Table) []*Index {
	return []*Index{
		NewIndex(constraintName("idx_", t.Name(), "drain"), t, t.Col("publishedAt"), t.Col("id")),
		NewIndex(constraintName("idx_", t.Name(), "aggregate"), t,
			t.Col("publishedAt"), t.Col("aggregateType"), t.Col("aggregateID"), t.Col("id")),
	}
}

// Emit inserts an event using tx (typically the *DB passed into the
// InTx callback). Encodes payload as JSON; pass an already-encoded
// json.RawMessage to keep control of the serialisation.
//
// Equivalent to EmitWith with zero EmitOptions.
func (o *Outbox) Emit(tx *DB, ctx context.Context, kind string, payload any) error {
	_, err := o.EmitWithID(tx, ctx, kind, payload, EmitOptions{})
	return err
}

// EmitWith is the extended variant carrying aggregate metadata and
// tracing headers — see EmitOptions.
func (o *Outbox) EmitWith(tx *DB, ctx context.Context, kind string, payload any, opts EmitOptions) error {
	_, err := o.EmitWithID(tx, ctx, kind, payload, opts)
	return err
}

// EmitWithID is EmitWith returning the id the server assigned to the
// row, or 0 when the driver exposes no LastInsertId.
//
// This method exists because MySQL has no RETURNING and the id is
// worth having: it is the event's identity for a downstream dedup
// key, and it is the number drops/mirror orders a replicated row by.
// It comes from the driver's LastInsertId, which drops/pg would have
// read from a RETURNING clause instead.
//
// One row per statement is what makes that safe. LastInsertId reports
// the *first* id of a multi-row INSERT, and the rest are contiguous
// only under innodb_autoinc_lock_mode 0 or 1 — MariaDB's default, but
// not MySQL 8's, which is 2 and hands concurrent inserters interleaved
// blocks. So there is no batch Emit: a caller emitting n events issues
// n statements, each of which knows its own id exactly. Inside one
// transaction that costs n round-trips and nothing else — the rows
// commit together either way.
func (o *Outbox) EmitWithID(tx *DB, ctx context.Context, kind string, payload any, opts EmitOptions) (int64, error) {
	if kind == "" {
		return 0, errors.New("drops/mysql: Outbox.Emit kind cannot be empty")
	}
	raw, err := outboxEncodePayload(payload)
	if err != nil {
		return 0, err
	}
	var headers any
	if len(opts.Headers) > 0 {
		h, err := json.Marshal(opts.Headers)
		if err != nil {
			return 0, fmt.Errorf("drops/mysql: Outbox.Emit headers: %w", err)
		}
		headers = json.RawMessage(h)
	}
	now := o.now().UTC()
	sql := fmt.Sprintf("INSERT INTO %s (`kind`, `aggregateType`, `aggregateID`, `payload`, `headers`, `createdAt`, `availableAt`) VALUES (?, ?, ?, ?, ?, ?, ?)",
		Dialect.QuoteIdent(o.table))
	res, err := tx.Exec(ctx, sql, kind,
		outboxNullableString(opts.AggregateType),
		outboxNullableString(opts.AggregateID),
		raw, headers, now, now)
	if err != nil {
		return 0, fmt.Errorf("drops/mysql: Outbox.Emit: %w", err)
	}
	id, _ := LastInsertID(res)
	return id, nil
}

// outboxNullableString returns nil when s is empty so the column
// stores SQL NULL rather than an empty string — matters for the
// drain predicates and for downstream "is this set?" checks.
func outboxNullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Drain fetches up to limit unpublished, non-failed, available events
// for processing, in id order. Caller must mark rows published via
// MarkPublished when the handler succeeds, or MarkFailed when it
// errors.
//
// The locking clause comes from [Outbox.WithLocking] and defaults to
// FOR UPDATE SKIP LOCKED, so several workers drain in parallel without
// stepping on each other. A server that cannot parse it fails with
// [ErrSkipLockedUnsupported] rather than quietly dropping the clause.
//
// What the lock does and does not buy is worth being exact about. The
// statement runs in autocommit, so the rows are locked for as long as
// the SELECT runs and no longer — this is not a claim held across the
// handler. It keeps two simultaneous drains from returning the same
// batch; it does not make delivery exactly-once, and nothing here
// does. Hold the lock across the work with [Outbox.DrainAggregate],
// which runs its callback inside the transaction that took it.
func (o *Outbox) Drain(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := fmt.Sprintf("SELECT `id`, `kind`, `aggregateType`, `aggregateID`, `payload`, `headers`, `attempts`, `lastError`, `createdAt`\n"+
		"FROM %s\n"+
		"WHERE `publishedAt` IS NULL\n"+
		"  AND `failedAt` IS NULL\n"+
		"  AND `availableAt` <= ?\n"+
		"ORDER BY `id`\n"+
		"LIMIT ?%s", Dialect.QuoteIdent(o.table), lockingClause(o.locking))
	rows, err := o.db.Query(ctx, sql, o.now().UTC(), limit)
	if err != nil {
		if o.locking == LockSkipLocked && isSkipLockedRejection(err) {
			return nil, fmt.Errorf("%w: call Outbox.ProbeLocking at startup, or pick a mode with Outbox.WithLocking: %w", ErrSkipLockedUnsupported, err)
		}
		return nil, err
	}
	defer rows.Close()
	return scanOutboxRows(rows)
}

// lockingClause renders the trailing locking clause for a drain.
func lockingClause(m DrainLocking) string {
	switch m {
	case LockForUpdate:
		return "\nFOR UPDATE"
	case LockNone:
		return ""
	default:
		return "\nFOR UPDATE SKIP LOCKED"
	}
}

// DrainAggregate fetches events for a single aggregate in id order,
// under a named lock keyed on the aggregate. Parallel workers calling
// DrainAggregate for the same aggregate skip silently (the lock is
// taken with a zero timeout), so per-aggregate order is preserved
// without serialising the whole worker pool.
//
// The callback executes within the lock-holding transaction. Returning
// an error rolls the transaction back — typically used so partial
// progress (MarkPublished calls) inside the callback is undone on
// failure.
//
// MySQL's GET_LOCK is not the same animal as PostgreSQL's
// transaction-scoped advisory lock: it belongs to the *session*, so
// committing does not release it and this method must, which it does
// on every path including a failing callback. If the process dies
// mid-callback the server releases the lock when the connection drops,
// which is the same backstop pg relies on.
func (o *Outbox) DrainAggregate(ctx context.Context, aggregateType, aggregateID string, limit int, fn func(tx *DB, events []OutboxEvent) error) error {
	if aggregateID == "" {
		return errors.New("drops/mysql: Outbox.DrainAggregate requires non-empty aggregateID")
	}
	if limit <= 0 {
		limit = 50
	}
	return o.db.InTx(ctx, func(tx *DB) error {
		name := outboxLockName(o.table, aggregateType, aggregateID)
		got, err := tryNamedLock(tx, ctx, name)
		if err != nil {
			return err
		}
		if !got {
			return nil
		}
		defer releaseNamedLock(tx, ctx, name)

		sql := fmt.Sprintf("SELECT `id`, `kind`, `aggregateType`, `aggregateID`, `payload`, `headers`, `attempts`, `lastError`, `createdAt`\n"+
			"FROM %s\n"+
			"WHERE `publishedAt` IS NULL\n"+
			"  AND `failedAt` IS NULL\n"+
			"  AND `availableAt` <= ?\n"+
			"  AND `aggregateType` <=> ?\n"+
			"  AND `aggregateID` = ?\n"+
			"ORDER BY `id`\n"+
			"LIMIT ?", Dialect.QuoteIdent(o.table))
		eventRows, err := tx.Query(ctx, sql, o.now().UTC(),
			outboxNullableString(aggregateType), aggregateID, limit)
		if err != nil {
			return err
		}
		events, err := scanOutboxRows(eventRows)
		eventRows.Close()
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		return fn(tx, events)
	})
}

// tryNamedLock takes MySQL's user-level lock without waiting,
// reporting whether it was acquired.
//
// GET_LOCK answers 1 for acquired, 0 for "someone else holds it", and
// NULL for an error such as running out of the server's lock slots — a
// NULL is neither an acquisition nor a clean miss, so it is reported
// as an error rather than silently skipping the aggregate forever.
func tryNamedLock(tx *DB, ctx context.Context, name string) (bool, error) {
	rows, err := tx.Query(ctx, "SELECT GET_LOCK(?, 0)", name)
	if err != nil {
		return false, err
	}
	var got *int64
	if rows.Next() {
		if err := rows.Scan(&got); err != nil {
			rows.Close()
			return false, err
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	if got == nil {
		return false, fmt.Errorf("drops/mysql: GET_LOCK(%q) returned NULL", name)
	}
	return *got == 1, nil
}

// releaseNamedLock drops a lock taken by tryNamedLock.
//
// The release runs on a context detached from the caller's, the way
// [DB.InTx] rolls back, and for a sharper reason: a user-level lock
// belongs to the session, and database/sql hands the session back to
// the pool rather than closing it. A release skipped because the
// caller's context was cancelled would leave the lock held by an idle
// pooled connection, and every later drain of that aggregate would
// skip forever. If the release itself fails there is nothing left to
// try — the lock then lives until that connection is closed.
//
// The error is discarded because the caller is on its way out of the
// transaction either way and has a more interesting one to report.
func releaseNamedLock(tx *DB, ctx context.Context, name string) {
	relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	rows, err := tx.Query(relCtx, "SELECT RELEASE_LOCK(?)", name)
	if err != nil {
		return
	}
	rows.Close()
}

// outboxLockName derives the GET_LOCK name for an aggregate.
//
// MySQL 8.0 rejects a lock name longer than 64 characters outright;
// MariaDB 10.11 allows 192 and rejects past that. 64 is the portable
// ceiling, so an over-long name is folded back under it with a hash of
// the full derivation — deterministic across processes, which is what
// a lock needs of its name.
//
// The cut backs off to a rune boundary, the way [constraintName] does.
// An aggregate id of non-ASCII text is otherwise sliced mid-rune and
// the lock name is no longer valid UTF-8: MariaDB takes it anyway,
// which is exactly why it would go unnoticed until a server that does
// not.
func outboxLockName(table, aggregateType, aggregateID string) string {
	name := "drops:" + table + ":" + aggregateType + ":" + aggregateID
	if len(name) <= 64 {
		return name
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	suffix := ":" + strconv.FormatUint(h.Sum64(), 36)
	cut := 64 - len(suffix)
	for cut > 0 && !utf8.ValidString(name[:cut]) {
		cut--
	}
	return name[:cut] + suffix
}

// PendingAggregates returns up to limit distinct aggregate keys that
// have unpublished work available right now. Used by the per-aggregate
// worker mode to discover which locks to try.
func (o *Outbox) PendingAggregates(ctx context.Context, limit int) ([]AggregateRef, error) {
	if limit <= 0 {
		limit = 50
	}
	sql := fmt.Sprintf("SELECT DISTINCT `aggregateType`, `aggregateID`\n"+
		"FROM %s\n"+
		"WHERE `publishedAt` IS NULL\n"+
		"  AND `failedAt` IS NULL\n"+
		"  AND `availableAt` <= ?\n"+
		"  AND `aggregateID` IS NOT NULL\n"+
		"LIMIT ?", Dialect.QuoteIdent(o.table))
	rows, err := o.db.Query(ctx, sql, o.now().UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AggregateRef
	for rows.Next() {
		var ref AggregateRef
		var typ, id *string
		if err := rows.Scan(&typ, &id); err != nil {
			return nil, err
		}
		if typ != nil {
			ref.Type = *typ
		}
		if id != nil {
			ref.ID = *id
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// AggregateRef identifies an aggregate that has pending outbox work.
type AggregateRef struct {
	Type string
	ID   string
}

// scanOutboxRows reads rows produced by the SELECT shape used in
// Drain / DrainAggregate. Pulled out so both paths share scanning.
//
// The nullable columns are scanned through pointers rather than any:
// database/sql fills an *any from a MySQL TEXT column with []byte, not
// string, so the type switch drops/pg uses would silently leave every
// aggregate id empty.
func scanOutboxRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]OutboxEvent, error) {
	var out []OutboxEvent
	for rows.Next() {
		var (
			e         OutboxEvent
			aggType   *string
			aggID     *string
			headers   []byte
			lastError *string
		)
		if err := rows.Scan(&e.ID, &e.Kind, &aggType, &aggID, &e.Payload, &headers, &e.Attempts, &lastError, &e.CreatedAt); err != nil {
			return nil, err
		}
		if aggType != nil {
			e.AggregateType = *aggType
		}
		if aggID != nil {
			e.AggregateID = *aggID
		}
		if lastError != nil {
			e.LastError = *lastError
		}
		if len(headers) > 0 && string(headers) != "null" {
			m := map[string]string{}
			if err := json.Unmarshal(headers, &m); err == nil {
				e.Headers = m
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkPublished stamps publishedAt on the supplied event ids. Safe to
// call repeatedly; subsequent drains skip published rows.
//
// The ids are expanded into an IN list because MySQL has no array
// type to bind against drops/pg's "= ANY($1)". A batch larger than the
// worker's drain limit is therefore worth splitting.
func (o *Outbox) MarkPublished(ctx context.Context, ids ...int64) error {
	return markPublishedOn(ctx, o.db, o.table, o.now().UTC(), ids...)
}

// markPublishedOn is the shared body used by Outbox.MarkPublished and
// the in-transaction path in the per-aggregate worker.
func markPublishedOn(ctx context.Context, exec interface {
	Exec(context.Context, string, ...any) (drops.Result, error)
}, table string, now time.Time, ids ...int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	sql := fmt.Sprintf("UPDATE %s SET `publishedAt` = ? WHERE `id` IN (%s)",
		Dialect.QuoteIdent(table), strings.Join(placeholders, ", "))
	_, err := exec.Exec(ctx, sql, args...)
	return err
}

// MarkFailed records a handler failure on the supplied event. When
// nextRetryAt is the zero value the row is parked as terminally
// failed (failedAt set, won't be drained again). Otherwise the row
// becomes available for retry at nextRetryAt with attempts bumped
// and the error message stored in lastError.
func (o *Outbox) MarkFailed(ctx context.Context, id int64, attempts int, nextRetryAt time.Time, lastErr string) error {
	if nextRetryAt.IsZero() {
		sql := fmt.Sprintf("UPDATE %s SET `attempts` = ?, `lastError` = ?, `failedAt` = ? WHERE `id` = ?", Dialect.QuoteIdent(o.table))
		_, err := o.db.Exec(ctx, sql, attempts, lastErr, o.now().UTC(), id)
		return err
	}
	sql := fmt.Sprintf("UPDATE %s SET `attempts` = ?, `lastError` = ?, `availableAt` = ? WHERE `id` = ?", Dialect.QuoteIdent(o.table))
	_, err := o.db.Exec(ctx, sql, attempts, lastErr, nextRetryAt.UTC(), id)
	return err
}

// Cleanup deletes rows that were published more than retainAfter ago,
// returning how many it removed. Run it periodically — without it the
// table grows unbounded, and here that costs more than it does on
// PostgreSQL: the drain index is not partial, so every published row
// still occupies it.
//
// Terminally failed rows are kept deliberately — they are the audit
// log of poison messages. Drop them by hand after triage.
//
// The newest row in the table is also kept, whatever its age, and that
// is not tidiness. A MySQL AUTO_INCREMENT counter is a property of the
// table's contents in a way a PostgreSQL sequence is not: MySQL before
// 8.0 and MariaDB before 10.2 recompute it as MAX(id)+1 when the table
// is opened, and every server resets it on TRUNCATE or a table
// rebuild. Empty the outbox completely and the next id can be one that
// has already been delivered — which for drops/mirror, where the event
// id *is* the row version, means a replayed row silently loses to the
// stale copy it was meant to replace. One retained row is a floor the
// counter cannot fall below.
func (o *Outbox) Cleanup(ctx context.Context, retainAfter time.Duration) (int64, error) {
	if retainAfter < 0 {
		retainAfter = 0
	}
	floor, err := o.maxID(ctx)
	if err != nil {
		return 0, err
	}
	if floor == 0 {
		return 0, nil
	}
	cutoff := o.now().UTC().Add(-retainAfter)
	sql := fmt.Sprintf("DELETE FROM %s WHERE `publishedAt` IS NOT NULL AND `publishedAt` < ? AND `id` < ?", Dialect.QuoteIdent(o.table))
	res, err := o.db.Exec(ctx, sql, cutoff, floor)
	if err != nil {
		return 0, err
	}
	if res == nil {
		return 0, nil
	}
	return res.RowsAffected()
}

// maxID reads the highest id in the table, or 0 when it is empty. The
// read is a separate statement because MySQL refuses a subquery over
// the table a DELETE targets (error 1093), and it costs nothing: the
// primary key's rightmost entry answers it.
func (o *Outbox) maxID(ctx context.Context) (int64, error) {
	rows, err := o.db.Query(ctx, fmt.Sprintf("SELECT COALESCE(MAX(`id`), 0) FROM %s", Dialect.QuoteIdent(o.table)))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err()
	}
	var id int64
	if err := rows.Scan(&id); err != nil {
		return 0, err
	}
	return id, rows.Err()
}

// outboxEncodePayload converts payload to json.RawMessage. Already
// encoded values pass through.
func outboxEncodePayload(payload any) (json.RawMessage, error) {
	switch v := payload.(type) {
	case nil:
		return json.RawMessage(`null`), nil
	case json.RawMessage:
		return v, nil
	case []byte:
		return json.RawMessage(v), nil
	case string:
		return json.RawMessage([]byte(v)), nil
	default:
		return json.Marshal(payload)
	}
}

// ----------------------------------------------------------------------
// Worker
// ----------------------------------------------------------------------

// OutboxOrdering controls how the worker schedules events.
type OutboxOrdering int

const (
	// OrderingNone delivers events as soon as drain returns them.
	// Highest throughput; events for different aggregates can
	// interleave, and parallel workers may deliver out of order
	// within the same aggregate.
	OrderingNone OutboxOrdering = iota

	// OrderingPerAggregate preserves emission order within each
	// (AggregateType, AggregateID) by routing each aggregate
	// through a single worker at a time via named locks. Events
	// that lack an AggregateID fall back to OrderingNone.
	OrderingPerAggregate
)

// OutboxHandler is the per-event callback the worker invokes when
// configured via OnEvent. Returning nil marks the row published;
// returning an error schedules a retry (or terminal failure when
// MaxAttempts has been reached).
type OutboxHandler func(ctx context.Context, e OutboxEvent) error

// OutboxBatchHandler is the batched alternative — receives the entire
// drain batch in one call. Useful for message brokers with expensive
// per-call overhead.
//
// Returning nil marks every event in the batch published; returning an
// error fails every event in the batch. Use OnEvent if you need
// per-event success granularity.
type OutboxBatchHandler func(ctx context.Context, events []OutboxEvent) error

// OutboxWorker polls the outbox table and forwards rows to a handler.
// Single goroutine per worker; spin up several to parallelise drain,
// provided the outbox is in a locking mode that permits it — see
// [DrainLocking].
type OutboxWorker struct {
	ob           *Outbox
	interval     time.Duration
	batch        int
	maxAttempts  int
	backoff      func(attempt int) time.Duration
	ordering     OutboxOrdering
	handler      OutboxHandler
	batchHandler OutboxBatchHandler
	onError      func(error)
	now          func() time.Time
}

// NewOutboxWorker returns a worker with sensible defaults: 1s polling
// interval, 50-row batch, exponential backoff with jitter (1s base,
// capped at 5min), unlimited retries.
func NewOutboxWorker(ob *Outbox) *OutboxWorker {
	return &OutboxWorker{
		ob:       ob,
		interval: time.Second,
		batch:    50,
		backoff:  exponentialJitter(time.Second, 5*time.Minute),
		now:      time.Now,
	}
}

// WithInterval overrides the polling cadence. Returns the worker for
// chaining.
func (w *OutboxWorker) WithInterval(d time.Duration) *OutboxWorker {
	if d > 0 {
		w.interval = d
	}
	return w
}

// WithBatch overrides the per-tick batch size.
func (w *OutboxWorker) WithBatch(n int) *OutboxWorker {
	if n > 0 {
		w.batch = n
	}
	return w
}

// WithMaxAttempts caps the retry count. After n attempts the row is
// marked terminally failed (failedAt set) and never drained again.
// Zero means unlimited retries.
func (w *OutboxWorker) WithMaxAttempts(n int) *OutboxWorker {
	if n > 0 {
		w.maxAttempts = n
	}
	return w
}

// WithBackoff overrides the per-attempt retry delay. The function
// receives the new attempt count (1-based) and returns how long to
// wait before the next try.
func (w *OutboxWorker) WithBackoff(fn func(attempt int) time.Duration) *OutboxWorker {
	if fn != nil {
		w.backoff = fn
	}
	return w
}

// WithOrdering selects the scheduling strategy. See OutboxOrdering.
func (w *OutboxWorker) WithOrdering(m OutboxOrdering) *OutboxWorker {
	w.ordering = m
	return w
}

// OnEvent attaches the per-event handler. Mutually exclusive with
// OnBatch — whichever was set last wins.
func (w *OutboxWorker) OnEvent(fn OutboxHandler) *OutboxWorker {
	w.handler = fn
	w.batchHandler = nil
	return w
}

// OnBatch attaches the batch handler. Mutually exclusive with OnEvent
// — whichever was set last wins.
func (w *OutboxWorker) OnBatch(fn OutboxBatchHandler) *OutboxWorker {
	w.batchHandler = fn
	w.handler = nil
	return w
}

// OnError attaches an error sink for drain / mark-published failures.
// Handler errors are reported through MarkFailed and do not surface
// here.
func (w *OutboxWorker) OnError(fn func(error)) *OutboxWorker {
	w.onError = fn
	return w
}

// ErrNoHandler is returned by Run when neither OnEvent nor OnBatch was
// attached.
var ErrNoHandler = errors.New("drops/mysql: OutboxWorker has no handler")

// Run drains the outbox until ctx is cancelled. Blocks the calling
// goroutine; typically invoked via go worker.Run(ctx). Returns the
// reason for termination — typically ctx.Err().
//
// MySQL has no LISTEN/NOTIFY, so the tick is the only wakeup: latency
// is bounded by WithInterval and nothing shortens it.
func (w *OutboxWorker) Run(ctx context.Context) error {
	if w.handler == nil && w.batchHandler == nil {
		return ErrNoHandler
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if err := w.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			if w.onError != nil {
				w.onError(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Tick performs one drain-and-dispatch pass. Exposed so callers can
// drive the outbox from their own scheduler instead of Run's ticker.
func (w *OutboxWorker) Tick(ctx context.Context) error {
	if w.handler == nil && w.batchHandler == nil {
		return ErrNoHandler
	}
	if w.ordering == OrderingPerAggregate {
		return w.tickPerAggregate(ctx)
	}
	return w.tickNone(ctx)
}

// tickNone is the unordered drain — events are processed in id order
// across the batch but no per-aggregate guarantee is held.
func (w *OutboxWorker) tickNone(ctx context.Context) error {
	events, err := w.ob.Drain(ctx, w.batch)
	if err != nil || len(events) == 0 {
		return err
	}
	if w.batchHandler != nil {
		return w.runBatch(ctx, events)
	}
	for _, e := range events {
		if herr := w.handler(ctx, e); herr == nil {
			if err := w.ob.MarkPublished(ctx, e.ID); err != nil && w.onError != nil {
				w.onError(err)
			}
		} else {
			w.failOne(ctx, e, herr)
		}
	}
	return nil
}

// tickPerAggregate processes one aggregate at a time inside its named
// lock so per-aggregate order is preserved even when many workers are
// running in parallel. Events without an AggregateID fall through to
// the unordered path so the worker still drains everything.
func (w *OutboxWorker) tickPerAggregate(ctx context.Context) error {
	aggs, err := w.ob.PendingAggregates(ctx, w.batch)
	if err != nil {
		return err
	}
	for _, agg := range aggs {
		err := w.ob.DrainAggregate(ctx, agg.Type, agg.ID, w.batch, func(tx *DB, events []OutboxEvent) error {
			if w.batchHandler != nil {
				return w.runBatchInTx(ctx, tx, events)
			}
			return w.runSequentialInTx(ctx, tx, events)
		})
		if err != nil && w.onError != nil {
			w.onError(err)
		}
	}
	// Fall back to the unordered drain so events without an
	// aggregate id still flow.
	return w.tickNone(ctx)
}

// runBatch publishes a whole batch via the OnBatch handler. On success
// every event is marked published; on failure every event is marked
// failed.
func (w *OutboxWorker) runBatch(ctx context.Context, events []OutboxEvent) error {
	herr := w.batchHandler(ctx, events)
	if herr == nil {
		ids := make([]int64, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		if err := w.ob.MarkPublished(ctx, ids...); err != nil && w.onError != nil {
			w.onError(err)
		}
		return nil
	}
	for _, e := range events {
		w.failOne(ctx, e, herr)
	}
	return nil
}

// runBatchInTx is the in-transaction variant used by the per-aggregate
// path. Uses the tx for MarkPublished so the publish-state change
// rolls back if anything later fails.
func (w *OutboxWorker) runBatchInTx(ctx context.Context, tx *DB, events []OutboxEvent) error {
	herr := w.batchHandler(ctx, events)
	if herr == nil {
		ids := make([]int64, len(events))
		for i, e := range events {
			ids[i] = e.ID
		}
		return markPublishedOn(ctx, tx, w.ob.table, w.now().UTC(), ids...)
	}
	for _, e := range events {
		w.failOne(ctx, e, herr)
	}
	return nil
}

// runSequentialInTx delivers events one by one in id order. Stops at
// the first failure so per-aggregate order is preserved — the stuck
// event blocks the queue for its aggregate until it is resolved (or
// hits MaxAttempts and is parked).
func (w *OutboxWorker) runSequentialInTx(ctx context.Context, tx *DB, events []OutboxEvent) error {
	for _, e := range events {
		if herr := w.handler(ctx, e); herr != nil {
			w.failOne(ctx, e, herr)
			return nil
		}
		if err := markPublishedOn(ctx, tx, w.ob.table, w.now().UTC(), e.ID); err != nil {
			return err
		}
	}
	return nil
}

// failOne handles a per-event failure: bump attempts, compute next
// retry time, mark terminal if MaxAttempts is reached.
//
// The failure is recorded on the Outbox's own connection rather than
// on any transaction the caller is inside, so a rollback cannot undo
// the attempt count — a poison event that rolls its transaction back
// on every try would otherwise never reach MaxAttempts.
func (w *OutboxWorker) failOne(ctx context.Context, e OutboxEvent, herr error) {
	attempts := e.Attempts + 1
	var nextRetry time.Time
	if w.maxAttempts == 0 || attempts < w.maxAttempts {
		nextRetry = w.now().Add(w.computeBackoff(attempts))
	}
	if err := w.ob.MarkFailed(ctx, e.ID, attempts, nextRetry, herr.Error()); err != nil && w.onError != nil {
		w.onError(err)
	}
}

func (w *OutboxWorker) computeBackoff(attempt int) time.Duration {
	if w.backoff == nil {
		return time.Second
	}
	return w.backoff(attempt)
}

// exponentialJitter returns a backoff that doubles each attempt from
// base, caps at maxN, and adds [0, base) of jitter so retrying workers
// do not synchronise into a thundering herd.
func exponentialJitter(base, maxN time.Duration) func(attempt int) time.Duration {
	if base <= 0 {
		base = 10 * time.Millisecond
	}
	if maxN <= 0 || maxN < base {
		maxN = base * 256
	}
	return func(attempt int) time.Duration {
		if attempt < 1 {
			attempt = 1
		}
		shift := attempt - 1
		if shift > 30 {
			shift = 30
		}
		d := base * time.Duration(1<<shift)
		if d > maxN {
			d = maxN
		}
		// Decorrelation, not secrecy — math/rand is the right tool.
		return d + time.Duration(rand.Int63n(int64(base)))
	}
}
