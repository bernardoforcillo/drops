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

// outboxDriver tracks INSERTs into the outbox table and serves
// canned drain responses on Query.
type outboxDriver struct {
	mu        sync.Mutex
	inserts   []outboxInsert
	updates   []outboxUpdate
	notifies  []outboxNotify
	deletes   int
	drainRows [][]any // each call to Query pops the next batch
	tryLock   bool    // pg_try_advisory_xact_lock response
	pending   [][]any // PendingAggregates responses
}

type outboxInsert struct {
	kind          string
	aggregateType string
	aggregateID   string
	payload       string
	headers       string
}

type outboxUpdate struct {
	kind string // "published" or "failed"
	ids  []int64
	args []any
}

type outboxNotify struct {
	channel string
	payload string
}

func (d *outboxDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.HasPrefix(sql, "INSERT INTO"):
		ins := outboxInsert{kind: argString(args, 0)}
		ins.aggregateType = argString(args, 1)
		ins.aggregateID = argString(args, 2)
		ins.payload = argRawJSON(args, 3)
		ins.headers = argRawJSON(args, 4)
		d.inserts = append(d.inserts, ins)
	case strings.Contains(sql, "pg_notify"):
		d.notifies = append(d.notifies, outboxNotify{
			channel: argString(args, 0),
			payload: argString(args, 1),
		})
	case strings.HasPrefix(sql, "UPDATE"):
		var u outboxUpdate
		switch {
		case strings.Contains(sql, `"publishedAt"`):
			u.kind = "published"
			ids := args[0].([]int64)
			u.ids = append([]int64(nil), ids...)
		case strings.Contains(sql, `"failedAt"`):
			u.kind = "terminal"
			u.ids = []int64{args[0].(int64)}
			u.args = append(u.args, args[1:]...)
		case strings.Contains(sql, `"availableAt"`):
			u.kind = "retry"
			u.ids = []int64{args[0].(int64)}
			u.args = append(u.args, args[1:]...)
		}
		d.updates = append(d.updates, u)
	case strings.HasPrefix(sql, "DELETE"):
		d.deletes++
		return outboxResult{rows: 1}, nil
	}
	return outboxResult{}, nil
}

func (d *outboxDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Contains(sql, "pg_try_advisory_xact_lock") {
		return &fakeRows{cols: []string{"ok"}, data: [][]any{{d.tryLock}}}, nil
	}
	if strings.Contains(sql, "SELECT DISTINCT") {
		// PendingAggregates query.
		if len(d.pending) == 0 {
			return &fakeRows{cols: []string{"aggregateType", "aggregateID"}}, nil
		}
		batch := d.pending[0]
		d.pending = d.pending[1:]
		var rows [][]any
		for i := 0; i < len(batch); i += 2 {
			rows = append(rows, []any{batch[i], batch[i+1]})
		}
		return &fakeRows{cols: []string{"aggregateType", "aggregateID"}, data: rows}, nil
	}
	if len(d.drainRows) == 0 {
		return &fakeRows{cols: outboxDrainCols()}, nil
	}
	rows := d.drainRows[0]
	d.drainRows = d.drainRows[1:]
	return &fakeRows{cols: outboxDrainCols(), data: [][]any{rows}}, nil
}

func (d *outboxDriver) Begin(_ context.Context) (drops.Tx, error) {
	return outboxTx{drv: d}, nil
}

type outboxTx struct{ drv *outboxDriver }

func (tx outboxTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.drv.Exec(ctx, sql, args...)
}
func (tx outboxTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.drv.Query(ctx, sql, args...)
}
func (tx outboxTx) Begin(ctx context.Context) (drops.Tx, error) { return tx.drv.Begin(ctx) }
func (outboxTx) Commit(_ context.Context) error                 { return nil }
func (outboxTx) Rollback(_ context.Context) error               { return nil }

type outboxResult struct{ rows int64 }

func (r outboxResult) RowsAffected() (int64, error) { return r.rows, nil }

func outboxDrainCols() []string {
	return []string{
		"id", "kind", "aggregateType", "aggregateID",
		"payload", "headers", "attempts", "lastError", "createdAt",
	}
}

func drainRow(id int64, kind string, payload string, attempts int) []any {
	return []any{
		id, kind, any(nil), any(nil),
		json.RawMessage(payload), []byte(nil), attempts, any(nil), time.Now(),
	}
}

func drainRowAgg(id int64, kind, aggType, aggID, payload string, attempts int) []any {
	return []any{
		id, kind, any(aggType), any(aggID),
		json.RawMessage(payload), []byte(nil), attempts, any(nil), time.Now(),
	}
}

func argString(args []any, i int) string {
	if i >= len(args) || args[i] == nil {
		return ""
	}
	s, _ := args[i].(string)
	return s
}

func argRawJSON(args []any, i int) string {
	if i >= len(args) || args[i] == nil {
		return ""
	}
	switch v := args[i].(type) {
	case json.RawMessage:
		return string(v)
	case []byte:
		return string(v)
	case string:
		return v
	}
	return ""
}

func TestOutboxEmitInsertsRow(t *testing.T) {
	drv := &outboxDriver{}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")

	if err := db.InTx(context.Background(), func(tx *pg.DB) error {
		return ob.Emit(tx, context.Background(), "user.created", map[string]any{"id": 7})
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(drv.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(drv.inserts))
	}
	if drv.inserts[0].kind != "user.created" {
		t.Errorf("kind: %q", drv.inserts[0].kind)
	}
	if drv.inserts[0].payload != `{"id":7}` {
		t.Errorf("payload: %q", drv.inserts[0].payload)
	}
}

func TestOutboxEmitWithAggregateAndHeaders(t *testing.T) {
	drv := &outboxDriver{}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")

	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		return ob.EmitWith(tx, context.Background(), "match.ended",
			map[string]any{"score": 3},
			pg.EmitOptions{
				AggregateType: "match",
				AggregateID:   "abc",
				Headers:       map[string]string{"traceparent": "t-1"},
			})
	})
	if err != nil {
		t.Fatalf("EmitWith: %v", err)
	}
	if len(drv.inserts) != 1 {
		t.Fatalf("expected 1 insert, got %d", len(drv.inserts))
	}
	ins := drv.inserts[0]
	if ins.aggregateType != "match" || ins.aggregateID != "abc" {
		t.Errorf("aggregate fields: %+v", ins)
	}
	if !strings.Contains(ins.headers, "traceparent") {
		t.Errorf("headers: %q", ins.headers)
	}
}

func TestOutboxEmitFiresNotifyWhenConfigured(t *testing.T) {
	drv := &outboxDriver{}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox").WithNotifyChannel("outbox_event")

	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		return ob.Emit(tx, context.Background(), "user.created", nil)
	})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(drv.notifies) != 1 || drv.notifies[0].channel != "outbox_event" {
		t.Errorf("expected NOTIFY on outbox_event, got: %+v", drv.notifies)
	}
}

func TestOutboxEmitRejectsEmptyKind(t *testing.T) {
	drv := &outboxDriver{}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")
	err := db.InTx(context.Background(), func(tx *pg.DB) error {
		return ob.Emit(tx, context.Background(), "", nil)
	})
	if err == nil {
		t.Error("Emit with empty kind must error")
	}
}

func TestOutboxWorkerProcessesAndMarks(t *testing.T) {
	drv := &outboxDriver{
		drainRows: [][]any{drainRow(1, "user.created", `{"id":7}`, 0)},
	}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")

	delivered := atomic.Int32{}
	worker := pg.NewOutboxWorker(ob).
		WithInterval(10 * time.Millisecond).
		OnEvent(func(ctx context.Context, e pg.OutboxEvent) error {
			delivered.Add(1)
			if e.Kind != "user.created" {
				t.Errorf("kind: %s", e.Kind)
			}
			return nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = worker.Run(ctx)

	if delivered.Load() == 0 {
		t.Error("worker did not deliver any event")
	}
	var sawPub bool
	for _, u := range drv.updates {
		if u.kind == "published" && len(u.ids) == 1 && u.ids[0] == 1 {
			sawPub = true
		}
	}
	if !sawPub {
		t.Errorf("worker did not mark event published: %+v", drv.updates)
	}
}

func TestOutboxWorkerSchedulesRetryWithBackoff(t *testing.T) {
	drv := &outboxDriver{
		drainRows: [][]any{drainRow(7, "user.created", `{}`, 0)},
	}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")

	worker := pg.NewOutboxWorker(ob).
		WithInterval(10 * time.Millisecond).
		WithBackoff(func(int) time.Duration { return 500 * time.Millisecond }).
		OnEvent(func(context.Context, pg.OutboxEvent) error { return errors.New("boom") })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = worker.Run(ctx)

	var retry *outboxUpdate
	for i := range drv.updates {
		if drv.updates[i].kind == "retry" {
			retry = &drv.updates[i]
			break
		}
	}
	if retry == nil {
		t.Fatalf("expected retry update, got: %+v", drv.updates)
	}
	if retry.ids[0] != 7 {
		t.Errorf("retry id: %d", retry.ids[0])
	}
	if retry.args[0].(int) != 1 {
		t.Errorf("retry attempts: %v", retry.args[0])
	}
	if msg, _ := retry.args[1].(string); msg != "boom" {
		t.Errorf("retry lastError: %v", retry.args[1])
	}
}

func TestOutboxWorkerMarksTerminalAtMaxAttempts(t *testing.T) {
	drv := &outboxDriver{
		drainRows: [][]any{drainRow(9, "k", `{}`, 2)},
	}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")

	worker := pg.NewOutboxWorker(ob).
		WithInterval(10 * time.Millisecond).
		WithMaxAttempts(3).
		OnEvent(func(context.Context, pg.OutboxEvent) error { return errors.New("dead") })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = worker.Run(ctx)

	var terminal *outboxUpdate
	for i := range drv.updates {
		if drv.updates[i].kind == "terminal" {
			terminal = &drv.updates[i]
			break
		}
	}
	if terminal == nil {
		t.Fatalf("expected terminal failure, got: %+v", drv.updates)
	}
	if terminal.ids[0] != 9 {
		t.Errorf("terminal id: %d", terminal.ids[0])
	}
}

func TestOutboxWorkerBatchHandlerPublishesAll(t *testing.T) {
	drv := &outboxDriver{
		drainRows: [][]any{
			{
				int64(1), "a", any(nil), any(nil), json.RawMessage(`{}`), []byte(nil), 0, any(nil), time.Now(),
			},
		},
	}
	// Two-event batch in one drain call.
	drv.drainRows = [][]any{
		{int64(1), "a", any(nil), any(nil), json.RawMessage(`{}`), []byte(nil), 0, any(nil), time.Now()},
	}
	// Override Query to return both events at once.
	db := pg.New(&batchDriver{outboxDriver: drv})
	ob := pg.NewOutbox(db, "outbox")

	var received atomic.Int32
	worker := pg.NewOutboxWorker(ob).
		WithInterval(10 * time.Millisecond).
		OnBatch(func(_ context.Context, events []pg.OutboxEvent) error {
			received.Add(int32(len(events)))
			return nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = worker.Run(ctx)

	if received.Load() < 2 {
		t.Errorf("expected batch with 2 events, got %d", received.Load())
	}
}

// batchDriver wraps outboxDriver and returns a fixed two-row batch
// for every drain query — used by the batch-handler test.
type batchDriver struct{ *outboxDriver }

func (b *batchDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	if strings.Contains(sql, "pg_try_advisory_xact_lock") || strings.Contains(sql, "DISTINCT") {
		return &fakeRows{cols: outboxDrainCols()}, nil
	}
	return &fakeRows{
		cols: outboxDrainCols(),
		data: [][]any{
			{int64(1), "a", any(nil), any(nil), json.RawMessage(`{}`), []byte(nil), 0, any(nil), time.Now()},
			{int64(2), "b", any(nil), any(nil), json.RawMessage(`{}`), []byte(nil), 0, any(nil), time.Now()},
		},
	}, nil
}

func TestOutboxWorkerPerAggregatePreservesOrder(t *testing.T) {
	drv := &orderedDriver{
		tryLock:   true,
		pending:   [][]any{{"match", "abc"}},
		ordered:   []int64{1, 2, 3},
		processed: nil,
	}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")

	var seen []int64
	var mu sync.Mutex
	worker := pg.NewOutboxWorker(ob).
		WithInterval(10 * time.Millisecond).
		WithOrdering(pg.OrderingPerAggregate).
		OnEvent(func(_ context.Context, e pg.OutboxEvent) error {
			mu.Lock()
			seen = append(seen, e.ID)
			mu.Unlock()
			return nil
		})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = worker.Run(ctx)

	mu.Lock()
	got := append([]int64(nil), seen...)
	mu.Unlock()
	if len(got) < 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Errorf("per-aggregate order: %v", got)
	}
}

// orderedDriver feeds the per-aggregate worker a fixed sequence of
// events for the same aggregate so order can be asserted.
type orderedDriver struct {
	mu        sync.Mutex
	tryLock   bool
	pending   [][]any
	ordered   []int64
	processed []int64
	updates   []outboxUpdate
}

func (d *orderedDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.HasPrefix(sql, "UPDATE") && strings.Contains(sql, `"publishedAt"`) {
		ids := args[0].([]int64)
		d.updates = append(d.updates, outboxUpdate{kind: "published", ids: append([]int64(nil), ids...)})
	}
	return outboxResult{}, nil
}

func (d *orderedDriver) Query(_ context.Context, sql string, _ ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Contains(sql, "pg_try_advisory_xact_lock") {
		return &fakeRows{cols: []string{"ok"}, data: [][]any{{d.tryLock}}}, nil
	}
	if strings.Contains(sql, "SELECT DISTINCT") {
		if len(d.pending) == 0 {
			return &fakeRows{cols: []string{"aggregateType", "aggregateID"}}, nil
		}
		batch := d.pending[0]
		d.pending = d.pending[1:]
		rows := [][]any{{batch[0], batch[1]}}
		return &fakeRows{cols: []string{"aggregateType", "aggregateID"}, data: rows}, nil
	}
	if strings.Contains(sql, `"aggregateID" = $2`) {
		if len(d.ordered) == 0 {
			return &fakeRows{cols: outboxDrainCols()}, nil
		}
		rows := make([][]any, 0, len(d.ordered))
		for _, id := range d.ordered {
			rows = append(rows, drainRowAgg(id, "k", "match", "abc", `{}`, 0))
		}
		d.ordered = nil
		return &fakeRows{cols: outboxDrainCols(), data: rows}, nil
	}
	return &fakeRows{cols: outboxDrainCols()}, nil
}

func (d *orderedDriver) Begin(_ context.Context) (drops.Tx, error) {
	return orderedTx{d}, nil
}

type orderedTx struct{ d *orderedDriver }

func (t orderedTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return t.d.Exec(ctx, sql, args...)
}
func (t orderedTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return t.d.Query(ctx, sql, args...)
}
func (t orderedTx) Begin(ctx context.Context) (drops.Tx, error) { return t.d.Begin(ctx) }
func (orderedTx) Commit(_ context.Context) error                { return nil }
func (orderedTx) Rollback(_ context.Context) error              { return nil }

func TestOutboxCleanupDeletesPublished(t *testing.T) {
	drv := &outboxDriver{}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")
	if _, err := ob.Cleanup(context.Background(), 24*time.Hour); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if drv.deletes != 1 {
		t.Errorf("expected 1 DELETE, got %d", drv.deletes)
	}
}

func TestOutboxTableHasDrainIndex(t *testing.T) {
	tbl := pg.NewOutboxTable("outbox")
	if len(tbl.Indexes()) < 2 {
		t.Errorf("expected at least 2 indexes (drain + aggregate), got %d", len(tbl.Indexes()))
	}
	names := map[string]bool{}
	for _, idx := range tbl.Indexes() {
		names[idx.Name()] = true
	}
	if !names["outboxDrainIdx"] {
		t.Errorf("missing drain index, got: %v", names)
	}
	if !names["outboxAggIdx"] {
		t.Errorf("missing aggregate index, got: %v", names)
	}
}

func TestOutboxWorkerOnErrorFires(t *testing.T) {
	// Force Drain into Query → returns ErrFoo by overriding Query.
	failingDB := pg.New(&failingDriver{err: errors.New("drain fail")})
	ob := pg.NewOutbox(failingDB, "outbox")

	var caught atomic.Value
	worker := pg.NewOutboxWorker(ob).
		WithInterval(5 * time.Millisecond).
		OnEvent(func(context.Context, pg.OutboxEvent) error { return nil }).
		OnError(func(err error) { caught.Store(err) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = worker.Run(ctx)

	if caught.Load() == nil {
		t.Error("OnError must capture drain failure")
	}
}

func TestOutboxWorkerRunWithoutHandler(t *testing.T) {
	drv := &outboxDriver{}
	db := pg.New(drv)
	ob := pg.NewOutbox(db, "outbox")
	w := pg.NewOutboxWorker(ob)
	err := w.Run(context.Background())
	if !errors.Is(err, pg.ErrNoHandler) {
		t.Errorf("expected ErrNoHandler, got %v", err)
	}
}

type failingDriver struct{ err error }

func (d *failingDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, d.err
}
func (d *failingDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, d.err
}
func (d *failingDriver) Begin(context.Context) (drops.Tx, error) { return nil, d.err }
