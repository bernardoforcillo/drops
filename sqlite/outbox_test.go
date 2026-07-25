package sqlite_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

func TestOutboxEmitSQL(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv)
	ob := sqlite.NewOutbox(db, "outbox")
	if err := ob.Emit(db, context.Background(), "user.created", map[string]int{"id": 1}); err != nil {
		t.Fatal(err)
	}
	q := drv.queries[0]
	if !strings.Contains(q, `INSERT INTO "outbox"`) || strings.Count(q, "?") != 7 {
		t.Errorf("emit SQL:\n%s", q)
	}
	if len(drv.args[0]) != 7 {
		t.Errorf("want 7 args, got %d", len(drv.args[0]))
	}
}

func TestOutboxMarkPublishedSQL(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv)
	ob := sqlite.NewOutbox(db, "outbox")
	if err := ob.MarkPublished(context.Background(), 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	q := drv.queries[0]
	if !strings.Contains(q, `WHERE "id" IN (?, ?, ?)`) {
		t.Errorf("markPublished SQL:\n%s", q)
	}
	if len(drv.args[0]) != 4 { // publishedAt + 3 ids
		t.Errorf("want 4 args, got %d", len(drv.args[0]))
	}
}

func TestOutboxMarkFailed(t *testing.T) {
	drv := &entDriver{}
	db := sqlite.New(drv)
	ob := sqlite.NewOutbox(db, "outbox")
	ctx := context.Background()

	// Terminal failure parks the row (failedAt).
	if err := ob.MarkFailed(ctx, 5, 3, time.Time{}, "boom"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.queries[0], `"failedAt"`) {
		t.Errorf("terminal fail SQL:\n%s", drv.queries[0])
	}
	// A retry reschedules via availableAt.
	if err := ob.MarkFailed(ctx, 5, 1, time.Now().Add(time.Minute), "boom"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(drv.queries[1], `"availableAt"`) {
		t.Errorf("retry fail SQL:\n%s", drv.queries[1])
	}
}

// --- worker drain path with a row-returning driver --------------------

type obRows struct {
	done bool
}

func (r *obRows) Next() bool {
	if r.done {
		return false
	}
	r.done = true
	return true
}
func (r *obRows) Columns() ([]string, error) { return nil, nil }
func (r *obRows) Close() error               { return nil }
func (r *obRows) Err() error                 { return nil }
func (r *obRows) Scan(dest ...any) error {
	// id, kind, aggType, aggID, payload, headers, attempts, lastError, createdAt
	vals := []any{int64(1), "user.created", any(nil), any(nil), json.RawMessage(`{"id":1}`), []byte(nil), 0, any(nil), int64(0)}
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = vals[i].(int64)
		case *string:
			*p = vals[i].(string)
		case *int:
			*p = vals[i].(int)
		case *any:
			*p = vals[i]
		case *json.RawMessage:
			*p = vals[i].(json.RawMessage)
		case *[]byte:
			if vals[i] != nil {
				*p = vals[i].([]byte)
			}
		}
	}
	return nil
}

type obDriver struct {
	entDriver
	drained bool
}

func (d *obDriver) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	d.entDriver.queries = append(d.entDriver.queries, sql)
	// Return one pending row for the first (drain) SELECT only.
	if !d.drained && strings.Contains(sql, "SELECT") {
		d.drained = true
		return &obRows{}, nil
	}
	return &entRows{}, nil
}

func TestOutboxWorkerTick(t *testing.T) {
	drv := &obDriver{}
	db := sqlite.New(drv)
	ob := sqlite.NewOutbox(db, "outbox")

	var got sqlite.OutboxEvent
	handled := 0
	w := sqlite.NewOutboxWorker(ob).OnEvent(func(_ context.Context, e sqlite.OutboxEvent) error {
		handled++
		got = e
		return nil
	})
	if err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if handled != 1 || got.ID != 1 || got.Kind != "user.created" {
		t.Errorf("handler: n=%d ev=%+v", handled, got)
	}
	// A MarkPublished UPDATE must have been issued after success.
	joined := strings.Join(drv.entDriver.queries, "\n")
	if !strings.Contains(joined, `UPDATE "outbox" SET "publishedAt"`) {
		t.Errorf("not marked published:\n%s", joined)
	}
}

func TestOutboxWorkerNoHandler(t *testing.T) {
	db := sqlite.New(&entDriver{})
	w := sqlite.NewOutboxWorker(sqlite.NewOutbox(db, "outbox"))
	if err := w.Tick(context.Background()); err != sqlite.ErrNoHandler {
		t.Errorf("want ErrNoHandler, got %v", err)
	}
}
