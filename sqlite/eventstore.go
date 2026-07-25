package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Event sourcing primitives — an append-only log of domain events per
// aggregate plus optional snapshots for fast load. Mirrors drops/pg's
// EventStore over SQLite.
//
//	store := sqlite.NewEventStore(db, "events")
//	err := db.InTx(ctx, func(tx *sqlite.DB) error {
//	    return store.Append(tx, ctx, "match", "abc-123", -1,
//	        sqlite.EventInput{Type: "matchStarted", Payload: started},
//	    )
//	})
//	if errors.Is(err, sqlite.ErrConcurrencyConflict) { ... }
//
// Concurrency is enforced by the UNIQUE (aggregateType, aggregateID,
// version) constraint — a duplicate tuple surfaces as
// ErrConcurrencyConflict instead of a raw driver error.

// Event is one entry in the store.
type Event struct {
	Offset        int64
	AggregateType string
	AggregateID   string
	Version       int64
	Type          string
	Payload       json.RawMessage
	Headers       map[string]string
	CreatedAt     time.Time
}

// EventInput is the per-event payload passed to Append.
type EventInput struct {
	Type    string
	Payload any
	Headers map[string]string
}

// EventStore is the append-only event log bound to a single table.
type EventStore struct {
	db    *DB
	table string
}

// NewEventStore returns a store bound to db.
func NewEventStore(db *DB, table string) *EventStore {
	if table == "" {
		table = "events"
	}
	return &EventStore{db: db, table: table}
}

// NewEventStoreTable declares the canonical event-store layout. The
// concurrency constraint rides on the unique index, which also serves
// in-order stream replay (SQLite has no separate Index type).
func NewEventStoreTable(name string) *Table {
	t := NewTable(name)
	Add(t, BigSerial("id").PrimaryKey())
	aggT := Add(t, Text("aggregateType").NotNull())
	aggID := Add(t, Text("aggregateID").NotNull())
	version := Add(t, BigInt("version").NotNull())
	Add(t, Text("eventType").NotNull())
	Add(t, JSON("payload").NotNull())
	Add(t, JSON("headers"))
	Add(t, Timestamp("createdAt", false).NotNull().Default("CURRENT_TIMESTAMP"))
	t.AddUnique(name+"AggregateVersionUq", aggT, aggID, version)
	return t
}

// ErrConcurrencyConflict is returned by Append when expectedVersion no
// longer matches the stream's head — another writer beat us to it.
var ErrConcurrencyConflict = errors.New("drops/sqlite: event store concurrency conflict")

// Append writes events to the (aggregateType, aggregateID) stream
// starting at expectedVersion+1, returning ErrConcurrencyConflict if the
// head has advanced. Use tx so the append commits atomically with the
// rest of the aggregate's work.
func (s *EventStore) Append(tx *DB, ctx context.Context, aggregateType, aggregateID string, expectedVersion int64, events ...EventInput) error {
	if aggregateType == "" || aggregateID == "" {
		return errors.New("drops/sqlite: EventStore.Append requires aggregateType and aggregateID")
	}
	if len(events) == 0 {
		return nil
	}
	for i, ev := range events {
		if ev.Type == "" {
			return fmt.Errorf("drops/sqlite: EventStore.Append events[%d].Type is empty", i)
		}
	}

	placeholders := make([]string, len(events))
	args := make([]any, 0, len(events)*6)
	for i, ev := range events {
		version := expectedVersion + int64(i) + 1
		payload, err := encodeJSONValue(ev.Payload)
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
		placeholders[i] = "(?, ?, ?, ?, ?, ?)"
		args = append(args, aggregateType, aggregateID, version, ev.Type, payload, headers)
	}
	sql := fmt.Sprintf(`INSERT INTO %s ("aggregateType", "aggregateID", "version", "eventType", "payload", "headers") VALUES %s`,
		quoteIdent(s.table), strings.Join(placeholders, ", "))
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		if isUniqueViolation(err) {
			return ErrConcurrencyConflict
		}
		return err
	}
	return nil
}

// encodeJSONValue turns an event/snapshot payload into JSON bytes.
// Pre-encoded json.RawMessage / []byte / string pass through untouched.
func encodeJSONValue(payload any) (json.RawMessage, error) {
	switch p := payload.(type) {
	case nil:
		return json.RawMessage("null"), nil
	case json.RawMessage:
		return p, nil
	case []byte:
		return json.RawMessage(p), nil
	case string:
		return json.RawMessage(p), nil
	default:
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		return json.RawMessage(b), nil
	}
}

// isUniqueViolation reports whether err is SQLite's unique-constraint
// failure. SQLite drivers surface it as "UNIQUE constraint failed: ...".
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}

// Load returns events for an aggregate in version order, starting after
// fromVersion (pass 0 for the beginning).
func (s *EventStore) Load(ctx context.Context, aggregateType, aggregateID string, fromVersion int64) ([]Event, error) {
	sql := fmt.Sprintf(`
		SELECT "id", "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"
		FROM %s
		WHERE "aggregateType" = ? AND "aggregateID" = ? AND "version" > ?
		ORDER BY "version"`, quoteIdent(s.table))
	rows, err := s.db.Query(ctx, sql, aggregateType, aggregateID, fromVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// Stream returns events from the global append log after fromOffset, up
// to limit rows — used by projection workers.
func (s *EventStore) Stream(ctx context.Context, fromOffset int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	sql := fmt.Sprintf(`
		SELECT "id", "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"
		FROM %s
		WHERE "id" > ?
		ORDER BY "id"
		LIMIT ?`, quoteIdent(s.table))
	rows, err := s.db.Query(ctx, sql, fromOffset, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows)
}

// LatestVersion returns the highest version recorded for the stream, or
// -1 when empty. Use before Append to compute expectedVersion.
func (s *EventStore) LatestVersion(ctx context.Context, aggregateType, aggregateID string) (int64, error) {
	sql := fmt.Sprintf(`SELECT COALESCE(MAX("version"), -1) FROM %s WHERE "aggregateType" = ? AND "aggregateID" = ?`, quoteIdent(s.table))
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

// AggregateSnapshot persists the materialised state of an aggregate at a
// version so reads avoid replaying every event.
type AggregateSnapshot struct {
	AggregateType string
	AggregateID   string
	Version       int64
	State         json.RawMessage
	CreatedAt     time.Time
}

// NewSnapshotTable declares the snapshot store.
func NewSnapshotTable(name string) *Table {
	t := NewTable(name)
	aggT := Add(t, Text("aggregateType").NotNull())
	aggID := Add(t, Text("aggregateID").NotNull())
	Add(t, BigInt("version").NotNull())
	Add(t, JSON("state").NotNull())
	Add(t, Timestamp("createdAt", false).NotNull().Default("CURRENT_TIMESTAMP"))
	t.PrimaryKey(aggT, aggID)
	return t
}

// SaveSnapshot upserts the supplied snapshot — newer versions replace
// older ones in place.
func (s *EventStore) SaveSnapshot(ctx context.Context, table string, snap AggregateSnapshot) error {
	if snap.AggregateType == "" || snap.AggregateID == "" {
		return errors.New("drops/sqlite: SaveSnapshot requires aggregateType and aggregateID")
	}
	state := snap.State
	if state == nil {
		state = json.RawMessage("null")
	}
	sql := fmt.Sprintf(`
		INSERT INTO %s ("aggregateType", "aggregateID", "version", "state", "createdAt")
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT ("aggregateType", "aggregateID") DO UPDATE
		SET "version" = excluded."version",
		    "state" = excluded."state",
		    "createdAt" = excluded."createdAt"`, quoteIdent(table))
	_, err := s.db.Exec(ctx, sql, snap.AggregateType, snap.AggregateID, snap.Version, state)
	return err
}

// LoadSnapshot fetches the latest snapshot for an aggregate (ok=false
// when none exists).
func (s *EventStore) LoadSnapshot(ctx context.Context, table, aggregateType, aggregateID string) (AggregateSnapshot, bool, error) {
	sql := fmt.Sprintf(`SELECT "aggregateType", "aggregateID", "version", "state", "createdAt" FROM %s WHERE "aggregateType" = ? AND "aggregateID" = ?`, quoteIdent(table))
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
