package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event sourcing primitives — an append-only log of domain events
// per aggregate plus optional snapshots for fast load. Pairs
// naturally with the outbox for transactional publishing and the
// change feed for fan-out to projections.
//
// Why event sourcing here: for game state (inventory, match
// progression, currency balances) the source of truth is the
// sequence of transitions, not the current row. Replay rebuilds
// state on demand; audit is free; testing trivially exercises
// every transition path.
//
//	store := pg.NewEventStore(db, "events")
//
//	// Append events with optimistic concurrency (expectedVersion is
//	// the stream version after the previous append; -1 for a fresh
//	// stream).
//	err := db.InTx(ctx, func(tx *pg.DB) error {
//	    return store.Append(tx, ctx, "match", "abc-123", -1,
//	        pg.EventInput{Type: "matchStarted", Payload: started},
//	        pg.EventInput{Type: "playerJoined", Payload: joined},
//	    )
//	})
//	if errors.Is(err, pg.ErrConcurrencyConflict) { ... }
//
//	// Replay
//	events, _ := store.Load(ctx, "match", "abc-123", 0)
//	state := MatchState{}
//	for _, ev := range events {
//	    state.Apply(ev)
//	}
//
// Concurrency is enforced by the UNIQUE (aggregateType, aggregateID,
// version) index — duplicate (type, id, version) tuples surface as
// ErrConcurrencyConflict instead of a raw driver error so callers
// can branch cleanly on the retry path.

// Event is one entry in the store.
type Event struct {
	// Offset is the store-wide append offset — monotonic across
	// all aggregates. Use it as the high-watermark when streaming
	// for projections.
	Offset int64

	// AggregateType / AggregateID identify the stream.
	AggregateType string
	AggregateID   string

	// Version is the per-stream offset starting at 0. Append's
	// expectedVersion compares against this.
	Version int64

	// Type is the application-level event name (e.g.
	// "matchStarted", "playerJoined").
	Type string

	// Payload is the JSON-encoded event body — caller-defined
	// shape, decoded on read by the consumer.
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

	// Headers attach tracing / correlation metadata. Stored as
	// jsonb on the row.
	Headers map[string]string
}

// EventStore is the append-only event log bound to a single table.
type EventStore struct {
	db    *DB
	table string
}

// NewEventStore returns a store bound to db. The table name is the
// SQL identifier; use NewEventStoreTable to declare matching DDL.
func NewEventStore(db *DB, table string) *EventStore {
	if table == "" {
		table = "events"
	}
	return &EventStore{db: db, table: table}
}

// NewEventStoreTable declares the canonical event-store layout.
// Run the DDL once alongside the rest of your schema.
func NewEventStoreTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	aggT := Add(t, Text("aggregateType").NotNull())
	aggID := Add(t, Text("aggregateID").NotNull())
	version := Add(t, BigInt("version").NotNull())
	Add(t, Text("eventType").NotNull())
	Add(t, JSONB("payload").NotNull())
	Add(t, JSONB("headers"))
	Add(t, Timestamp("createdAt", true).NotNull().Default("now()"))

	// Concurrency check rides on this unique index — duplicate
	// (type, id, version) tuples are how parallel writers learn
	// they raced.
	t.AddUnique(name+"AggregateVersionUq", aggT, aggID, version)

	// Stream index keyed on aggregate for in-order replay.
	t.AddIndex(NewIndex(name+"StreamIdx", t,
		aggT.Column, aggID.Column, version.Column))

	return t
}

// ErrConcurrencyConflict is returned by Append when the
// expectedVersion no longer matches the stream's head — another
// writer beat us to that version. Wrap your callsite in a retry
// loop that re-reads the stream before retrying.
var ErrConcurrencyConflict = errors.New("drops/pg: event store concurrency conflict")

// Append writes events to the stream identified by (aggregateType,
// aggregateID) starting at expectedVersion+1. Returns
// ErrConcurrencyConflict if the stream's head has advanced past
// expectedVersion — typically resolved by reloading and retrying.
//
// Use tx so the append commits with the rest of the aggregate's
// transactional work (typically a write to a projection table or
// to the outbox).
func (s *EventStore) Append(tx *DB, ctx context.Context, aggregateType, aggregateID string, expectedVersion int64, events ...EventInput) error {
	if aggregateType == "" || aggregateID == "" {
		return errors.New("drops/pg: EventStore.Append requires aggregateType and aggregateID")
	}
	if len(events) == 0 {
		return nil
	}
	for i, ev := range events {
		if ev.Type == "" {
			return fmt.Errorf("drops/pg: EventStore.Append events[%d].Type is empty", i)
		}
	}

	rows := make([]string, len(events))
	args := make([]any, 0, len(events)*6)
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
		base := i * 6
		rows[i] = fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d)",
			base+1, base+2, base+3, base+4, base+5, base+6)
		args = append(args, aggregateType, aggregateID, version, ev.Type, payload, headers)
	}
	sql := fmt.Sprintf(`INSERT INTO "%s" ("aggregateType", "aggregateID", "version", "eventType", "payload", "headers") VALUES %s`,
		s.table, strings.Join(rows, ", "))
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		if isUniqueViolation(err) {
			return ErrConcurrencyConflict
		}
		return err
	}
	return nil
}

// isUniqueViolation reports whether err is the PG unique-violation
// error class — uses string matching as a fallback for drivers that
// don't surface SQLSTATE directly.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrUniqueViolation) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint") || strings.Contains(msg, "23505")
}

// Load returns events for an aggregate in version order, starting
// after fromVersion. Pass 0 to read from the beginning.
func (s *EventStore) Load(ctx context.Context, aggregateType, aggregateID string, fromVersion int64) ([]Event, error) {
	sql := fmt.Sprintf(`
		SELECT "id", "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"
		FROM "%s"
		WHERE "aggregateType" = $1
		  AND "aggregateID" = $2
		  AND "version" > $3
		ORDER BY "version"`, s.table)
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID, fromVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// Stream returns events from the global append log starting after
// fromOffset, up to limit rows. Used by projection workers to fan
// events out into derived tables; the offset is the high-watermark
// the projection persists between runs.
func (s *EventStore) Stream(ctx context.Context, fromOffset int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	sql := fmt.Sprintf(`
		SELECT "id", "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"
		FROM "%s"
		WHERE "id" > $1
		ORDER BY "id"
		LIMIT $2`, s.table)
	rows, err := s.db.Query(ctx, sql, fromOffset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// LatestVersion returns the highest version recorded for the
// stream, or -1 when the stream is empty. Use this before Append
// to compute the expectedVersion for a fresh write.
func (s *EventStore) LatestVersion(ctx context.Context, aggregateType, aggregateID string) (int64, error) {
	sql := fmt.Sprintf(`SELECT COALESCE(MAX("version"), -1) FROM "%s" WHERE "aggregateType" = $1 AND "aggregateID" = $2`, s.table)
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID)
	if err != nil {
		return -1, err
	}
	defer rows.Close()
	if !rows.Next() {
		return -1, nil
	}
	var v int64
	if err := rows.Scan(&v); err != nil {
		return -1, err
	}
	return v, nil
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

// AggregateSnapshot persists the materialised state of an aggregate
// at a specific version so reads avoid replaying every event since
// the beginning of time. Optional — small aggregates can replay
// fast enough without.
type AggregateSnapshot struct {
	AggregateType string
	AggregateID   string
	Version       int64
	State         json.RawMessage
	CreatedAt     time.Time
}

// NewSnapshotTable declares the snapshot store. Pair with
// NewEventStoreTable; snapshots live in a separate table so the
// event log stays append-only.
func NewSnapshotTable(name string) *Table {
	t := NewTable(name)
	aggT := Add(t, Text("aggregateType").NotNull())
	aggID := Add(t, Text("aggregateID").NotNull())
	Add(t, BigInt("version").NotNull())
	Add(t, JSONB("state").NotNull())
	Add(t, Timestamp("createdAt", true).NotNull().Default("now()"))
	t.PrimaryKey(aggT, aggID)
	return t
}

// SaveSnapshot upserts the supplied snapshot — newer versions
// replace older ones in place.
func (s *EventStore) SaveSnapshot(ctx context.Context, table string, snap AggregateSnapshot) error {
	if snap.AggregateType == "" || snap.AggregateID == "" {
		return errors.New("drops/pg: SaveSnapshot requires aggregateType and aggregateID")
	}
	state := snap.State
	if state == nil {
		state = json.RawMessage("null")
	}
	sql := fmt.Sprintf(`
		INSERT INTO "%s" ("aggregateType", "aggregateID", "version", "state", "createdAt")
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT ("aggregateType", "aggregateID") DO UPDATE
		SET "version" = EXCLUDED."version",
		    "state" = EXCLUDED."state",
		    "createdAt" = EXCLUDED."createdAt"`, table)
	_, err := s.db.Exec(ctx, sql, snap.AggregateType, snap.AggregateID, snap.Version, state)
	return err
}

// LoadSnapshot fetches the latest snapshot for an aggregate.
// Returns ok=false when no snapshot exists; callers should fall
// back to replaying from version 0.
func (s *EventStore) LoadSnapshot(ctx context.Context, table, aggregateType, aggregateID string) (AggregateSnapshot, bool, error) {
	sql := fmt.Sprintf(`SELECT "aggregateType", "aggregateID", "version", "state", "createdAt" FROM "%s" WHERE "aggregateType" = $1 AND "aggregateID" = $2`, table)
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID)
	if err != nil {
		return AggregateSnapshot{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return AggregateSnapshot{}, false, nil
	}
	var snap AggregateSnapshot
	if err := rows.Scan(&snap.AggregateType, &snap.AggregateID, &snap.Version, &snap.State, &snap.CreatedAt); err != nil {
		return AggregateSnapshot{}, false, err
	}
	return snap, true, nil
}
