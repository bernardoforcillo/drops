package mysql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event sourcing primitives — an append-only log of domain events per
// aggregate plus optional snapshots for fast load. Pairs naturally
// with the outbox for transactional publishing.
//
//	store := mysql.NewEventStore(db, "events")
//
//	// Append events with optimistic concurrency (expectedVersion is
//	// the stream version after the previous append; -1 for a fresh
//	// stream).
//	err := db.InTx(ctx, func(tx *mysql.DB) error {
//	    return store.Append(tx, ctx, "match", "abc-123", -1,
//	        mysql.EventInput{Type: "matchStarted", Payload: started},
//	        mysql.EventInput{Type: "playerJoined", Payload: joined},
//	    )
//	})
//	if errors.Is(err, mysql.ErrConcurrencyConflict) { ... }
//
//	// Replay from the beginning. -1, not 0 — see Load.
//	events, _ := store.Load(ctx, "match", "abc-123", -1)
//
// # Concurrency under REPEATABLE READ
//
// Append's contract is that a second writer who computed the same
// expectedVersion loses, and on MySQL that rests on the PRIMARY KEY
// over (aggregateType, aggregateID, version) and on nothing else. It
// cannot rest on a re-read, and this is the place where a port that
// looks like the PostgreSQL one is wrong.
//
// MySQL's default isolation level is REPEATABLE READ, not READ
// COMMITTED. A transaction's first consistent read fixes a snapshot
// that every later plain SELECT in that transaction sees, however long
// it runs and whatever anyone else commits. So [EventStore.Load] and
// [EventStore.LatestVersion] inside an open transaction answer with
// the stream as it was, and a retry loop that re-reads the head
// *without leaving the transaction* recomputes the same expected
// version and conflicts forever.
//
// Writes are not snapshot reads: an INSERT is checked against the live
// index, so the duplicate is caught even though the reader could not
// see the row. That is what makes the key, rather than the read, the
// thing enforcing the contract. Retry by opening a new transaction.
//
// The conflict surfaces as MySQL's error 1062 and is translated to
// [ErrConcurrencyConflict]. A racing writer that has not yet committed
// blocks the second INSERT instead of failing it — the second waits on
// the first's lock and gets its 1062 the moment the first commits — so
// a caller should also be ready for a lock-wait timeout (1205) or, if
// it appends to several streams in one transaction, a deadlock (1213).
// Neither is translated: both mean "the transaction did not happen",
// which is a different thing from losing a version race.

// Event is one entry in the store.
type Event struct {
	// Offset is the store-wide append offset — monotonic across all
	// aggregates. Use it as the high-watermark when streaming for
	// projections.
	Offset int64

	// AggregateType / AggregateID identify the stream.
	AggregateType string
	AggregateID   string

	// Version is the per-stream offset starting at 0. Append's
	// expectedVersion compares against this.
	Version int64

	// Type is the application-level event name.
	Type string

	// Payload is the JSON-encoded event body.
	Payload json.RawMessage

	// Headers carry tracing / correlation metadata.
	Headers map[string]string

	// CreatedAt is the wall-clock time of the append.
	CreatedAt time.Time
}

// EventInput is the per-event payload passed to Append.
type EventInput struct {
	// Type is required — names the event.
	Type string

	// Payload is encoded with encoding/json. Pre-encoded
	// json.RawMessage / []byte / string pass through untouched.
	Payload any

	// Headers attach tracing / correlation metadata.
	Headers map[string]string
}

// EventStore is the append-only event log bound to a single table.
type EventStore struct {
	db    *DB
	table string
	now   func() time.Time
}

// NewEventStore returns a store bound to db. The table name is the SQL
// identifier; use NewEventStoreTable to declare matching DDL.
func NewEventStore(db *DB, table string) *EventStore {
	if table == "" {
		table = "events"
	}
	return &EventStore{db: db, table: table, now: time.Now}
}

// Table returns the event table's name.
func (s *EventStore) Table() string { return s.table }

// NewEventStoreTable declares the canonical event-store layout.
// [CreateTable] alone is enough to create it — deliberately, since
// everything Append's contract depends on is in the table definition.
//
// [CreateTable] is also the only thing that can create it today.
// [BuildSnapshot] records a table's PRIMARY KEY and its registered
// [Table.AddIndex] indexes but not a column's own UNIQUE, and [Diff]
// emits secondary indexes as statements after the CREATE TABLE — so a
// pushed CREATE TABLE leaves the AUTO_INCREMENT id with no key of its
// own and the server refuses it with error 1075. Create this table and
// [NewSnapshotTable] with CreateTable, and keep them out of the
// [Schema] handed to [Push].
//
// The layout is inverted from drops/pg's, which makes the surrogate id
// the primary key and (aggregateType, aggregateID, version) a separate
// unique index. Here the stream key *is* the primary key and the id
// carries the unique index instead. Two reasons, one a limitation and
// one a preference.
//
// The limitation: drops/mysql can declare a composite PRIMARY KEY
// inline and cannot yet declare a composite UNIQUE inline, so this is
// the only arrangement in which one CREATE TABLE enforces the
// concurrency constraint. A store created without that constraint
// accepts two writers at the same version and never says a word, which
// is too sharp an edge to leave to a second statement someone might
// not run.
//
// The preference: InnoDB clusters the table on its primary key, so
// this layout stores each stream's events physically adjacent and
// [EventStore.Load] reads one range. The cost lands on
// [EventStore.Stream], whose id order is a secondary index and so
// costs a primary-key lookup per row, and on every secondary index,
// which carries the wide key. PostgreSQL's heap has no clustering to
// trade, which is why its layout differs.
//
// A stream key is two VARCHAR(255) columns under the table's binary
// collation (see [NewOutboxTable] for why the collation is declared
// and where); with the 8-byte version that is 2048 bytes of InnoDB's
// 3072-byte index limit.
func NewEventStoreTable(name string) *Table {
	t := NewTable(name).Collate(streamKeyCollation)
	// The id is the global append offset. It is AUTO_INCREMENT, so
	// MySQL requires it to lead an index — which the UNIQUE gives it,
	// while also making it a valid target for a projection's
	// high-watermark.
	Add(t, BigSerial("id").Unique().NotNull())
	Add(t, streamKeyColumn("aggregateType").PrimaryKey())
	Add(t, streamKeyColumn("aggregateID").PrimaryKey())
	Add(t, BigInt("version").PrimaryKey())
	Add(t, streamKeyColumn("eventType").NotNull())
	Add(t, JSON("payload").NotNull())
	Add(t, JSON("headers"))
	Add(t, Timestamp("createdAt", false).NotNull())
	return t
}

// ErrConcurrencyConflict is returned by Append when the
// expectedVersion no longer matches the stream's head — another writer
// beat us to that version. Wrap the callsite in a retry loop that
// opens a *new* transaction before re-reading the stream; see the
// package's note on REPEATABLE READ for why re-reading inside the
// failed transaction cannot work.
var ErrConcurrencyConflict = errors.New("drops/mysql: event store concurrency conflict")

// Append writes events to the stream identified by (aggregateType,
// aggregateID) starting at expectedVersion+1. Returns
// ErrConcurrencyConflict if the stream's head has advanced past
// expectedVersion.
//
// Use tx so the append commits with the rest of the aggregate's
// transactional work (typically a write to a projection table or to
// the outbox).
func (s *EventStore) Append(tx *DB, ctx context.Context, aggregateType, aggregateID string, expectedVersion int64, events ...EventInput) error {
	if aggregateType == "" || aggregateID == "" {
		return errors.New("drops/mysql: EventStore.Append requires aggregateType and aggregateID")
	}
	if len(events) == 0 {
		return nil
	}
	for i, ev := range events {
		if ev.Type == "" {
			return fmt.Errorf("drops/mysql: EventStore.Append events[%d].Type is empty", i)
		}
	}

	now := s.now().UTC()
	placeholders := make([]string, len(events))
	args := make([]any, 0, len(events)*7)
	for i, ev := range events {
		version := expectedVersion + int64(i) + 1
		payload, err := outboxEncodePayload(ev.Payload)
		if err != nil {
			return err
		}
		var headers any
		if len(ev.Headers) > 0 {
			b, err := json.Marshal(ev.Headers)
			if err != nil {
				return err
			}
			headers = json.RawMessage(b)
		}
		placeholders[i] = "(?, ?, ?, ?, ?, ?, ?)"
		args = append(args, aggregateType, aggregateID, version, ev.Type, payload, headers, now)
	}
	sql := fmt.Sprintf("INSERT INTO %s (`aggregateType`, `aggregateID`, `version`, `eventType`, `payload`, `headers`, `createdAt`) VALUES %s",
		Dialect.QuoteIdent(s.table), strings.Join(placeholders, ", "))
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		if isDuplicateKeyError(err) {
			return ErrConcurrencyConflict
		}
		return err
	}
	return nil
}

// isDuplicateKeyError reports whether err is MySQL's duplicate-key
// failure — error 1062, or 1586, which is the same failure reported
// with the key's name.
//
// The classification this package already does is what answers first:
// [DB.Exec] wraps a server error in a [ServerError] carrying the
// number, so errors.Is against [ErrUniqueViolation] reads the number
// rather than the prose. The text match behind it is the fallback for
// an error that never went through Exec — one a caller wrapped itself,
// or one from a Driver that formats its errors and nothing else. It
// looks for the message rather than the number, because "1062" occurs
// inside plenty of strings that are not this error and "duplicate
// entry" does not.
func isDuplicateKeyError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUniqueViolation) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate entry")
}

// Load returns events for an aggregate in version order, starting
// strictly after fromVersion — hand it the version of the last event
// you already applied and it resumes with the next one.
//
// Pass -1, not 0, to read a stream from the beginning: versions start
// at 0, so 0 is the first event's own version and asking for what
// comes after it skips it. -1 is the same "no event yet" sentinel
// LatestVersion returns for an empty stream and Append takes as
// expectedVersion for a fresh one.
//
// Note that this is a plain read: called inside an open transaction on
// a REPEATABLE READ server it answers from that transaction's
// snapshot, not from the stream's current head.
func (s *EventStore) Load(ctx context.Context, aggregateType, aggregateID string, fromVersion int64) ([]Event, error) {
	sql := fmt.Sprintf("SELECT `id`, `aggregateType`, `aggregateID`, `version`, `eventType`, `payload`, `headers`, `createdAt`\n"+
		"FROM %s\n"+
		"WHERE `aggregateType` = ?\n"+
		"  AND `aggregateID` = ?\n"+
		"  AND `version` > ?\n"+
		"ORDER BY `version`", Dialect.QuoteIdent(s.table))
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID, fromVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// Stream returns events from the global append log starting after
// fromOffset, up to limit rows. Used by projection workers to fan
// events out into derived tables; the offset is the high-watermark the
// projection persists between runs.
func (s *EventStore) Stream(ctx context.Context, fromOffset int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	sql := fmt.Sprintf("SELECT `id`, `aggregateType`, `aggregateID`, `version`, `eventType`, `payload`, `headers`, `createdAt`\n"+
		"FROM %s\n"+
		"WHERE `id` > ?\n"+
		"ORDER BY `id`\n"+
		"LIMIT ?", Dialect.QuoteIdent(s.table))
	rows, err := s.db.Query(ctx, sql, fromOffset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// LatestVersion returns the highest version recorded for the stream,
// or -1 when the stream is empty. Use it before Append to compute the
// expectedVersion for a fresh write — from outside the transaction
// that will do the appending, or from a transaction that has read
// nothing yet, since a REPEATABLE READ snapshot older than the last
// commit answers with a head that has already moved.
func (s *EventStore) LatestVersion(ctx context.Context, aggregateType, aggregateID string) (int64, error) {
	sql := fmt.Sprintf("SELECT COALESCE(MAX(`version`), -1) FROM %s WHERE `aggregateType` = ? AND `aggregateID` = ?", Dialect.QuoteIdent(s.table))
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID)
	if err != nil {
		return -1, err
	}
	defer rows.Close()
	if !rows.Next() {
		return -1, rows.Err()
	}
	var v int64
	if err := rows.Scan(&v); err != nil {
		return -1, err
	}
	return v, rows.Err()
}

// scanEventRows reads the SELECT shape used by Load / Stream.
func scanEventRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var (
			e       Event
			headers []byte
		)
		if err := rows.Scan(&e.Offset, &e.AggregateType, &e.AggregateID, &e.Version, &e.Type, &e.Payload, &headers, &e.CreatedAt); err != nil {
			return nil, err
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

// ----------------------------------------------------------------------
// Snapshots
// ----------------------------------------------------------------------

// AggregateSnapshot persists the materialised state of an aggregate at
// a specific version so reads avoid replaying every event since the
// beginning of time. Optional — small aggregates replay fast enough
// without.
type AggregateSnapshot struct {
	AggregateType string
	AggregateID   string
	Version       int64
	State         json.RawMessage
	CreatedAt     time.Time
}

// NewSnapshotTable declares the snapshot store. Pair it with
// NewEventStoreTable; snapshots live in a separate table so the event
// log stays append-only.
func NewSnapshotTable(name string) *Table {
	t := NewTable(name).Collate(streamKeyCollation)
	Add(t, streamKeyColumn("aggregateType").PrimaryKey())
	Add(t, streamKeyColumn("aggregateID").PrimaryKey())
	Add(t, BigInt("version").NotNull())
	Add(t, JSON("state").NotNull())
	Add(t, Timestamp("createdAt", false).NotNull())
	return t
}

// SaveSnapshot upserts the supplied snapshot — newer versions replace
// older ones in place.
//
// MySQL's upsert fires on a collision with any unique index rather
// than a named conflict target, which on this table is the primary key
// and nothing else, so the two readings coincide. The assignments use
// the VALUES(col) spelling rather than MySQL 8.0.19's row alias, which
// MariaDB does not have; see [NewValueOf].
func (s *EventStore) SaveSnapshot(ctx context.Context, table string, snap AggregateSnapshot) error {
	if snap.AggregateType == "" || snap.AggregateID == "" {
		return errors.New("drops/mysql: SaveSnapshot requires aggregateType and aggregateID")
	}
	state := snap.State
	if state == nil {
		state = json.RawMessage("null")
	}
	sql := fmt.Sprintf("INSERT INTO %s (`aggregateType`, `aggregateID`, `version`, `state`, `createdAt`)\n"+
		"VALUES (?, ?, ?, ?, ?)\n"+
		"ON DUPLICATE KEY UPDATE `version` = VALUES(`version`), `state` = VALUES(`state`), `createdAt` = VALUES(`createdAt`)",
		Dialect.QuoteIdent(table))
	_, err := s.db.Exec(ctx, sql, snap.AggregateType, snap.AggregateID, snap.Version, state, s.now().UTC())
	return err
}

// LoadSnapshot fetches the latest snapshot for an aggregate. Returns
// ok=false when none exists; callers should fall back to replaying
// from version 0.
func (s *EventStore) LoadSnapshot(ctx context.Context, table, aggregateType, aggregateID string) (AggregateSnapshot, bool, error) {
	sql := fmt.Sprintf("SELECT `aggregateType`, `aggregateID`, `version`, `state`, `createdAt` FROM %s WHERE `aggregateType` = ? AND `aggregateID` = ?",
		Dialect.QuoteIdent(table))
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID)
	if err != nil {
		return AggregateSnapshot{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return AggregateSnapshot{}, false, rows.Err()
	}
	var snap AggregateSnapshot
	if err := rows.Scan(&snap.AggregateType, &snap.AggregateID, &snap.Version, &snap.State, &snap.CreatedAt); err != nil {
		return AggregateSnapshot{}, false, err
	}
	return snap, true, rows.Err()
}
