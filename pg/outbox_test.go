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
	updates   [][]int64
	drainRows [][]any // each call to Query pops the next batch
}

type outboxInsert struct {
	kind    string
	payload string
}

func (d *outboxDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.HasPrefix(sql, "INSERT INTO"):
		d.inserts = append(d.inserts, outboxInsert{
			kind:    args[0].(string),
			payload: string(args[1].(json.RawMessage)),
		})
	case strings.HasPrefix(sql, "UPDATE"):
		ids := args[0].([]int64)
		d.updates = append(d.updates, append([]int64(nil), ids...))
	}
	return nil, nil
}
func (d *outboxDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.drainRows) == 0 {
		return &fakeRows{cols: []string{"id", "kind", "payload", "createdAt"}}, nil
	}
	rows := d.drainRows[0]
	d.drainRows = d.drainRows[1:]
	return &fakeRows{
		cols: []string{"id", "kind", "payload", "createdAt"},
		data: [][]any{rows},
	}, nil
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
		drainRows: [][]any{
			{int64(1), "user.created", json.RawMessage(`{"id":7}`), time.Now()},
		},
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
	if len(drv.updates) == 0 || drv.updates[0][0] != 1 {
		t.Errorf("worker did not mark event published: %+v", drv.updates)
	}
}

func TestOutboxWorkerOnErrorFires(t *testing.T) {
	drv := &outboxDriver{}
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
	_ = drv
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
