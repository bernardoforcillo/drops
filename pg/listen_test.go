package pg_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// listeningDriver implements both drops.Driver and pg.Listener so
// the typed Listen wrapper has something to dispatch to.
type listeningDriver struct {
	notifyQueries []string
	notifyArgs    [][]any
	out           chan pg.Notification
}

func (d *listeningDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.notifyQueries = append(d.notifyQueries, sql)
	d.notifyArgs = append(d.notifyArgs, args)
	return nil, nil
}
func (d *listeningDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return &fakeRows{}, nil
}
func (d *listeningDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }
func (d *listeningDriver) Listen(ctx context.Context, channel string) (<-chan pg.Notification, error) {
	return d.out, nil
}

type invoiceEvent struct {
	ID    int64 `json:"id"`
	Total int64 `json:"total"`
}

func TestNotifyEmitsPgNotifyWithParams(t *testing.T) {
	drv := &listeningDriver{}
	db := pg.New(drv)
	if err := pg.Notify(db, context.Background(), "invoice_paid", invoiceEvent{ID: 7, Total: 100}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(drv.notifyQueries) != 1 {
		t.Fatalf("expected 1 query, got %d", len(drv.notifyQueries))
	}
	if !strings.Contains(drv.notifyQueries[0], "pg_notify($1, $2)") {
		t.Errorf("Notify must use pg_notify with params: %s", drv.notifyQueries[0])
	}
	args := drv.notifyArgs[0]
	if args[0] != "invoice_paid" {
		t.Errorf("channel arg: %v", args[0])
	}
	payload := args[1].(string)
	if !strings.Contains(payload, `"id":7`) {
		t.Errorf("payload should JSON-encode struct: %s", payload)
	}
}

func TestNotifyRejectsEmptyChannel(t *testing.T) {
	db := pg.New(&listeningDriver{})
	err := pg.Notify(db, context.Background(), "", "x")
	if err == nil {
		t.Error("empty channel should error")
	}
}

func TestListenDecodesPayloadsIntoT(t *testing.T) {
	out := make(chan pg.Notification, 4)
	drv := &listeningDriver{out: out}
	db := pg.New(drv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := pg.Listen[invoiceEvent](db, ctx, "invoice_paid")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	// Push two valid + one malformed payload.
	out <- pg.Notification{Channel: "invoice_paid", Payload: `{"id":1,"total":100}`}
	out <- pg.Notification{Channel: "invoice_paid", Payload: `{"id":2,"total":200}`}
	out <- pg.Notification{Channel: "invoice_paid", Payload: `{this is not json`}
	out <- pg.Notification{Channel: "invoice_paid", Payload: `{"id":3,"total":300}`}

	var got []invoiceEvent
loop:
	for i := 0; i < 3; i++ {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-time.After(200 * time.Millisecond):
			break loop
		}
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 decoded events (malformed dropped), got %d: %+v", len(got), got)
	}
	if got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Errorf("decoded ids: %+v", got)
	}
}

func TestListenClosesWhenCtxCancelled(t *testing.T) {
	out := make(chan pg.Notification)
	drv := &listeningDriver{out: out}
	db := pg.New(drv)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := pg.Listen[invoiceEvent](db, ctx, "x")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	cancel()
	// The goroutine should drain and close ch.
	select {
	case _, open := <-ch:
		if open {
			// drain
		}
	case <-time.After(200 * time.Millisecond):
	}
	// Reading again should not block forever.
	select {
	case _, open := <-ch:
		if open {
			t.Error("channel should be closed after ctx cancel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("channel did not close after ctx cancel")
	}
}

func TestListenReturnsErrWhenDriverLacksListener(t *testing.T) {
	db := pg.New(&fakeDriver{})
	_, err := pg.Listen[invoiceEvent](db, context.Background(), "x")
	if !errors.Is(err, pg.ErrListenNotSupported) {
		t.Errorf("expected ErrListenNotSupported, got %v", err)
	}
}

func TestSupportsListen(t *testing.T) {
	if !pg.SupportsListen(pg.New(&listeningDriver{})) {
		t.Error("SupportsListen should be true for Listener driver")
	}
	if pg.SupportsListen(pg.New(&fakeDriver{})) {
		t.Error("SupportsListen should be false for plain driver")
	}
}

func TestListenAcceptsJSONRawMessagePayload(t *testing.T) {
	drv := &listeningDriver{}
	db := pg.New(drv)
	raw := json.RawMessage(`{"already":"encoded"}`)
	if err := pg.Notify(db, context.Background(), "ch", raw); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	payload := drv.notifyArgs[0][1].(string)
	if !strings.Contains(payload, `"already":"encoded"`) {
		t.Errorf("RawMessage should pass through: %s", payload)
	}
}
