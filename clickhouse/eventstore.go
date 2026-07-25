package clickhouse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event sourcing primitives — an append-only log of domain events per
// aggregate plus optional snapshots for fast load.
//
// ClickHouse is a natural fit for append-only event logs: columnar
// storage, high-throughput inserts, and cheap time-range scans. The
// trade-offs versus a PostgreSQL event store are:
//
//   - No unique-constraint enforcement on MergeTree tables, so the
//     concurrency check (expectedVersion) is advisory — a check-then-
//     insert rather than a database-enforced lock. Under normal single-
//     writer-per-aggregate patterns this is fine; if you have concurrent
//     writers on the same aggregate, add a Kafka queue in front.
//
//   - Snapshots use ReplacingMergeTree so CH merges away older versions
//     asynchronously. Reads against the snapshot table use FINAL to
//     force an immediate merge.
//
//	store := clickhouse.NewEventStore(db, "events")
//
//	// Append (expectedVersion = -1 for a brand-new stream).
//	err := store.Append(ctx, "match", "abc-123", -1,
//	    clickhouse.EventInput{Type: "matchStarted", Payload: started},
//	    clickhouse.EventInput{Type: "playerJoined",  Payload: joined},
//	)
//	if errors.Is(err, clickhouse.ErrConcurrencyConflict) { ... }
//
//	// Replay
//	events, _ := store.Load(ctx, "match", "abc-123", 0)
//	state := MatchState{}
//	for _, ev := range events {
//	    state.Apply(ev)
//	}

// Event is one entry in the store.
type Event struct {
	// Offset is a nanosecond-epoch timestamp that acts as the
	// store-wide ordering key. Use it as the high-watermark when
	// streaming for projections.
	Offset int64

	// AggregateType / AggregateID identify the stream.
	AggregateType string
	AggregateID   string

	// Version is the per-stream offset starting at 0. Append's
	// expectedVersion compares against this.
	Version int64

	// Type is the application-level event name (e.g. "matchStarted").
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

	// Headers attach tracing / correlation metadata stored as JSON.
	Headers map[string]string
}

// EventStore is the append-only event log bound to a single table.
type EventStore struct {
	db    *DB
	table string
}

// NewEventStore returns a store bound to db. The table name is the
// SQL identifier used for all queries; use NewEventStoreTable to
// declare matching DDL.
func NewEventStore(db *DB, table string) *EventStore {
	if table == "" {
		table = "events"
	}
	return &EventStore{db: db, table: table}
}

// NewEventStoreTable declares the canonical ClickHouse event-store
// layout. Run the DDL alongside the rest of your schema.
//
// Engine: MergeTree, ORDER BY (aggregateType, aggregateID, version).
// There is no auto-increment ID column in ClickHouse; the global
// ordering cursor is the createdAt DateTime64(9) timestamp.
func NewEventStoreTable(name string) *Table {
	t := NewTable(name)
	aggT := Add(t, String("aggregateType"))
	aggID := Add(t, String("aggregateID"))
	version := Add(t, Int64("version"))
	Add(t, String("eventType"))
	Add(t, String("payload"))
	Add(t, String("headers").Nullable())
	Add(t, DateTime64("createdAt", 9, "UTC").Default("now64(9)"))

	t.Engine(MergeTree()).OrderBy(aggT, aggID, version)
	return t
}

// ErrConcurrencyConflict is returned by Append when the expectedVersion
// no longer matches the stream's head — another writer beat us to that
// version. Wrap your callsite in a retry loop that re-reads the stream
// before retrying.
//
// Note: unlike the PostgreSQL variant this check is advisory (not
// database-enforced), so it catches concurrent writers only under
// normal single-writer-per-aggregate usage.
var ErrConcurrencyConflict = errors.New("drops/clickhouse: event store concurrency conflict")

// Append writes events to the stream identified by (aggregateType,
// aggregateID) starting at expectedVersion+1. Returns
// ErrConcurrencyConflict if the stream's head has advanced past
// expectedVersion.
//
// The check is non-atomic in ClickHouse (no unique-constraint
// enforcement on MergeTree). For strict single-writer semantics,
// serialize appends to the same aggregate outside the database.
func (s *EventStore) Append(ctx context.Context, aggregateType, aggregateID string, expectedVersion int64, events ...EventInput) error {
	if aggregateType == "" || aggregateID == "" {
		return errors.New("drops/clickhouse: EventStore.Append requires aggregateType and aggregateID")
	}
	if len(events) == 0 {
		return nil
	}
	for i, ev := range events {
		if ev.Type == "" {
			return fmt.Errorf("drops/clickhouse: EventStore.Append events[%d].Type is empty", i)
		}
	}

	// Advisory concurrency check — not atomic.
	latest, err := s.LatestVersion(ctx, aggregateType, aggregateID)
	if err != nil {
		return err
	}
	if latest != expectedVersion {
		return ErrConcurrencyConflict
	}

	rows := make([]string, len(events))
	args := make([]any, 0, len(events)*6)
	for i, ev := range events {
		version := expectedVersion + int64(i) + 1
		payload, err := encodeEventPayload(ev.Payload)
		if err != nil {
			return err
		}
		var headers any
		if len(ev.Headers) > 0 {
			b, err := json.Marshal(ev.Headers)
			if err != nil {
				return err
			}
			headers = string(b)
		}
		rows[i] = "(?, ?, ?, ?, ?, ?)"
		args = append(args, aggregateType, aggregateID, version, ev.Type, string(payload), headers)
	}
	sql := fmt.Sprintf(`INSERT INTO "%s" ("aggregateType", "aggregateID", "version", "eventType", "payload", "headers") VALUES %s`,
		s.table, strings.Join(rows, ", "))
	_, err = s.db.Exec(ctx, sql, args...)
	return err
}

// encodeEventPayload marshals payload to JSON. Pre-encoded
// json.RawMessage / []byte / string pass through untouched.
func encodeEventPayload(payload any) (json.RawMessage, error) {
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

// Load returns events for an aggregate in version order, starting
// after fromVersion. Pass 0 to read from the beginning.
func (s *EventStore) Load(ctx context.Context, aggregateType, aggregateID string, fromVersion int64) ([]Event, error) {
	sql := fmt.Sprintf(`
		SELECT "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"
		FROM "%s"
		WHERE "aggregateType" = ?
		  AND "aggregateID" = ?
		  AND "version" > ?
		ORDER BY "version"`, s.table)
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID, fromVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// Stream returns events from the global log starting after fromOffset,
// up to limit rows. fromOffset is a nanosecond-epoch timestamp — use
// Event.Offset from the last row of the previous batch. Pass 0 to
// read from the beginning. Used by projection workers to fan events
// out into derived tables.
func (s *EventStore) Stream(ctx context.Context, fromOffset int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	// Convert nanosecond epoch to time.Time for the DateTime64 comparison.
	from := time.Unix(0, fromOffset).UTC()
	sql := fmt.Sprintf(`
		SELECT "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"
		FROM "%s"
		WHERE "createdAt" > ?
		ORDER BY "createdAt"
		LIMIT ?`, s.table)
	rows, err := s.db.Query(ctx, sql, from, int64(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// LatestVersion returns the highest version recorded for the stream,
// or -1 when the stream is empty. Use this before Append to compute
// the expectedVersion for a fresh write.
func (s *EventStore) LatestVersion(ctx context.Context, aggregateType, aggregateID string) (int64, error) {
	sql := fmt.Sprintf(`SELECT coalesce(max("version"), -1) FROM "%s" WHERE "aggregateType" = ? AND "aggregateID" = ?`, s.table)
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
			headers *string
		)
		if err := rows.Scan(&e.AggregateType, &e.AggregateID, &e.Version, &e.Type, &e.Payload, &headers, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Offset = e.CreatedAt.UnixNano()
		if headers != nil && *headers != "" && *headers != "null" {
			m := map[string]string{}
			if err := json.Unmarshal([]byte(*headers), &m); err == nil {
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
// at a specific version so reads avoid replaying every event since the
// beginning of time. Optional — small aggregates can replay fast enough
// without.
type AggregateSnapshot struct {
	AggregateType string
	AggregateID   string
	Version       int64
	State         json.RawMessage
	CreatedAt     time.Time
}

// NewSnapshotTable declares the snapshot store. The engine is
// ReplacingMergeTree(version) so ClickHouse automatically merges away
// older snapshots, keeping only the row with the highest version per
// (aggregateType, aggregateID). Reads use FINAL to force an immediate
// merge.
func NewSnapshotTable(name string) *Table {
	t := NewTable(name)
	aggT := Add(t, String("aggregateType"))
	aggID := Add(t, String("aggregateID"))
	version := Add(t, Int64("version"))
	Add(t, String("state"))
	Add(t, DateTime64("createdAt", 9, "UTC").Default("now64(9)"))

	_ = version // referenced in engine below
	t.Engine(ReplacingMergeTree("version")).OrderBy(aggT, aggID)
	return t
}

// SaveSnapshot inserts a new snapshot. With ReplacingMergeTree(version)
// as the table engine, ClickHouse will eventually deduplicate to the
// highest-version row per aggregate. Use LoadSnapshot (which reads FINAL)
// to always get the latest.
func (s *EventStore) SaveSnapshot(ctx context.Context, table string, snap AggregateSnapshot) error {
	if snap.AggregateType == "" || snap.AggregateID == "" {
		return errors.New("drops/clickhouse: SaveSnapshot requires aggregateType and aggregateID")
	}
	state := snap.State
	if state == nil {
		state = json.RawMessage("null")
	}
	sql := fmt.Sprintf(`INSERT INTO "%s" ("aggregateType", "aggregateID", "version", "state", "createdAt") VALUES (?, ?, ?, ?, now64(9))`, table)
	_, err := s.db.Exec(ctx, sql, snap.AggregateType, snap.AggregateID, snap.Version, string(state))
	return err
}

// LoadSnapshot fetches the latest snapshot for an aggregate. The query
// uses FINAL so it reflects the deduplicated (latest-version) row even
// before CH background merges have run. Returns ok=false when no
// snapshot exists — fall back to replaying from version 0.
func (s *EventStore) LoadSnapshot(ctx context.Context, table, aggregateType, aggregateID string) (AggregateSnapshot, bool, error) {
	sql := fmt.Sprintf(`
		SELECT "aggregateType", "aggregateID", "version", "state", "createdAt"
		FROM "%s" FINAL
		WHERE "aggregateType" = ? AND "aggregateID" = ?`, table)
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID)
	if err != nil {
		return AggregateSnapshot{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return AggregateSnapshot{}, false, rows.Err()
	}
	var (
		snap  AggregateSnapshot
		state string
	)
	if err := rows.Scan(&snap.AggregateType, &snap.AggregateID, &snap.Version, &state, &snap.CreatedAt); err != nil {
		return AggregateSnapshot{}, false, err
	}
	snap.State = json.RawMessage(state)
	return snap, true, rows.Err()
}
