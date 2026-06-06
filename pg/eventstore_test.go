package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// eventDriver tracks INSERTs into the event store and serves
// canned SELECT responses on Query.
type eventDriver struct {
	mu          sync.Mutex
	inserts     [][]any // per-event args
	queries     []string
	execs       atomic.Int32
	failUnique  bool // simulate 23505 on next Exec
	loadRows    []eventRow
	streamRows  []eventRow
	maxVersion  int64
	snapshotRow *eventRow
	storedSnap  *eventRow
}

type eventRow struct {
	id            int64
	aggregateType string
	aggregateID   string
	version       int64
	eventType     string
	payload       string
	headers       string
	createdAt     time.Time
	state         string // snapshot state
}

func (d *eventDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execs.Add(1)
	if d.failUnique {
		d.failUnique = false
		return nil, errors.New(`pq: duplicate key value violates unique constraint "events_aggregateVersion"`)
	}
	switch {
	case strings.Contains(sql, `INSERT INTO`) && strings.Contains(sql, "ON CONFLICT"):
		// Snapshot upsert.
		d.storedSnap = &eventRow{
			aggregateType: args[0].(string),
			aggregateID:   args[1].(string),
			version:       args[2].(int64),
			state:         string(args[3].(json.RawMessage)),
		}
	case strings.Contains(sql, "INSERT INTO"):
		// Events insert; args come in groups of 6.
		for i := 0; i < len(args); i += 6 {
			d.inserts = append(d.inserts, args[i:i+6])
		}
	}
	return eventResult{}, nil
}

func (d *eventDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, sqlPrefix(sql))
	switch {
	case strings.Contains(sql, "MAX("):
		return &fakeRows{cols: []string{"v"}, data: [][]any{{d.maxVersion}}}, nil
	case strings.Contains(sql, `"state"`):
		if d.snapshotRow == nil {
			return &fakeRows{cols: []string{"aggregateType", "aggregateID", "version", "state", "createdAt"}}, nil
		}
		r := d.snapshotRow
		return &fakeRows{
			cols: []string{"aggregateType", "aggregateID", "version", "state", "createdAt"},
			data: [][]any{{r.aggregateType, r.aggregateID, r.version, json.RawMessage(r.state), r.createdAt}},
		}, nil
	case strings.Contains(sql, `ORDER BY "version"`):
		return d.eventRowsTo(d.loadRows), nil
	case strings.Contains(sql, `ORDER BY "id"`):
		return d.eventRowsTo(d.streamRows), nil
	}
	return &fakeRows{}, nil
}

func (d *eventDriver) Begin(_ context.Context) (drops.Tx, error) {
	return eventTx{d}, nil
}

func (d *eventDriver) eventRowsTo(rows []eventRow) drops.Rows {
	cols := []string{"id", "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"}
	var data [][]any
	for _, r := range rows {
		var headers any
		if r.headers != "" {
			headers = []byte(r.headers)
		}
		data = append(data, []any{
			r.id, r.aggregateType, r.aggregateID, r.version, r.eventType,
			json.RawMessage(r.payload), headers, r.createdAt,
		})
	}
	return &fakeRows{cols: cols, data: data}
}

type eventTx struct{ drv *eventDriver }

func (tx eventTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx eventTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx eventTx) Begin(ctx context.Context) (drops.Tx, error) { return tx.drv.Begin(ctx) }
func (eventTx) Commit(_ context.Context) error                 { return nil }
func (eventTx) Rollback(_ context.Context) error               { return nil }

type eventResult struct{}

func (eventResult) RowsAffected() (int64, error) { return 1, nil }

func TestEventStoreAppendInsertsEvents(t *testing.T) {
	drv := &eventDriver{}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")

	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		return store.Append(tx, context.Background(), "match", "abc", -1,
			pg.EventInput{Type: "matchStarted", Payload: map[string]any{"by": "p1"}},
			pg.EventInput{Type: "playerJoined", Payload: map[string]any{"id": 7}},
		)
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(drv.inserts) != 2 {
		t.Fatalf("expected 2 inserts, got %d", len(drv.inserts))
	}
	// Versions: -1 + 1 = 0, -1 + 2 = 1
	if drv.inserts[0][2].(int64) != 0 || drv.inserts[1][2].(int64) != 1 {
		t.Errorf("versions: %v %v", drv.inserts[0][2], drv.inserts[1][2])
	}
	if drv.inserts[0][3].(string) != "matchStarted" {
		t.Errorf("event type: %v", drv.inserts[0][3])
	}
}

func TestEventStoreAppendRejectsEmptyType(t *testing.T) {
	drv := &eventDriver{}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")
	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		return store.Append(tx, context.Background(), "x", "y", -1, pg.EventInput{Type: ""})
	})
	if err == nil {
		t.Error("expected error on empty event type")
	}
}

func TestEventStoreAppendSurfacesConcurrencyConflict(t *testing.T) {
	drv := &eventDriver{failUnique: true}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")
	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		return store.Append(tx, context.Background(), "match", "abc", -1, pg.EventInput{Type: "x", Payload: nil})
	})
	if !errors.Is(err, pg.ErrConcurrencyConflict) {
		t.Errorf("expected ErrConcurrencyConflict, got %v", err)
	}
}

func TestEventStoreLoadReturnsEventsInVersionOrder(t *testing.T) {
	drv := &eventDriver{
		loadRows: []eventRow{
			{id: 1, aggregateType: "m", aggregateID: "a", version: 0, eventType: "started", payload: `{}`, createdAt: time.Now()},
			{id: 2, aggregateType: "m", aggregateID: "a", version: 1, eventType: "joined", payload: `{}`, createdAt: time.Now()},
		},
	}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")
	events, err := store.Load(context.Background(), "m", "a", 0)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len: %d", len(events))
	}
	if events[0].Type != "started" || events[1].Type != "joined" {
		t.Errorf("order: %+v", events)
	}
}

func TestEventStoreLatestVersionReturnsMinusOneForEmptyStream(t *testing.T) {
	drv := &eventDriver{maxVersion: -1}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")
	v, err := store.LatestVersion(context.Background(), "m", "a")
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if v != -1 {
		t.Errorf("version: %d", v)
	}
}

func TestEventStoreStreamReturnsEventsByGlobalOffset(t *testing.T) {
	drv := &eventDriver{
		streamRows: []eventRow{
			{id: 10, aggregateType: "m", aggregateID: "a", version: 0, eventType: "started", payload: `{}`, createdAt: time.Now()},
			{id: 11, aggregateType: "m", aggregateID: "b", version: 0, eventType: "started", payload: `{}`, createdAt: time.Now()},
		},
	}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")
	events, err := store.Stream(context.Background(), 5, 100)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len: %d", len(events))
	}
	if events[0].Offset != 10 || events[1].Offset != 11 {
		t.Errorf("offsets: %v %v", events[0].Offset, events[1].Offset)
	}
}

func TestEventStoreSaveAndLoadSnapshot(t *testing.T) {
	drv := &eventDriver{}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")

	snap := pg.AggregateSnapshot{
		AggregateType: "m",
		AggregateID:   "a",
		Version:       5,
		State:         json.RawMessage(`{"score": 3}`),
	}
	if err := store.SaveSnapshot(context.Background(), "eventSnapshots", snap); err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if drv.storedSnap == nil || drv.storedSnap.version != 5 {
		t.Errorf("snapshot not stored: %+v", drv.storedSnap)
	}

	// Now arrange the driver to return the snapshot on Load.
	drv.snapshotRow = &eventRow{
		aggregateType: "m", aggregateID: "a", version: 5,
		state: `{"score": 3}`, createdAt: time.Now(),
	}
	loaded, ok, err := store.LoadSnapshot(context.Background(), "eventSnapshots", "m", "a")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if !ok {
		t.Fatal("expected snapshot to be found")
	}
	if loaded.Version != 5 {
		t.Errorf("version: %d", loaded.Version)
	}
}

func TestEventStoreLoadSnapshotReturnsFalseWhenMissing(t *testing.T) {
	drv := &eventDriver{snapshotRow: nil}
	db := pg.New(drv)
	store := pg.NewEventStore(db, "events")
	_, ok, err := store.LoadSnapshot(context.Background(), "eventSnapshots", "m", "a")
	if err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if ok {
		t.Error("expected ok=false for missing snapshot")
	}
}

func TestEventStoreTableHasExpectedShape(t *testing.T) {
	tbl := pg.NewEventStoreTable("events")
	names := map[string]bool{}
	for _, c := range tbl.Columns() {
		names[c.Name()] = true
	}
	for _, want := range []string{"id", "aggregateType", "aggregateID", "version", "eventType", "payload", "headers", "createdAt"} {
		if !names[want] {
			t.Errorf("missing column %q", want)
		}
	}
	if len(tbl.CompositeUniques()) == 0 {
		t.Error("expected composite UNIQUE on (aggregateType, aggregateID, version)")
	}
	if len(tbl.Indexes()) == 0 {
		t.Error("expected stream index")
	}
}

func TestSnapshotTableHasCompositePK(t *testing.T) {
	tbl := pg.NewSnapshotTable("eventSnapshots")
	pk := tbl.CompositePrimaryKey()
	if len(pk) != 2 {
		t.Errorf("expected 2-column PK, got %d", len(pk))
	}
}
