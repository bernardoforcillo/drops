package pg_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

func TestInstallChangeFeedEmitsTriggerDDL(t *testing.T) {
	tbl := pg.NewTable("players")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())

	stmts, err := pg.InstallChangeFeed(tbl)
	if err != nil {
		t.Fatalf("InstallChangeFeed: %v", err)
	}
	if len(stmts) != 3 {
		t.Fatalf("expected 3 statements, got %d", len(stmts))
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		"CREATE OR REPLACE FUNCTION",
		"pg_notify",
		"drops_players",
		`COALESCE(NEW."id"::text, OLD."id"::text)`,
		"AFTER INSERT OR UPDATE OR DELETE",
		"FOR EACH ROW EXECUTE FUNCTION",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("DDL missing %q in:\n%s", want, joined)
		}
	}
}

func TestInstallChangeFeedHonoursChannelOption(t *testing.T) {
	tbl := pg.NewTable("matches")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	stmts, err := pg.InstallChangeFeed(tbl, pg.ChangeFeedOptions{Channel: "match_events"})
	if err != nil {
		t.Fatalf("InstallChangeFeed: %v", err)
	}
	if !strings.Contains(stmts[0], "'match_events'") {
		t.Errorf("custom channel missing from CREATE FUNCTION: %s", stmts[0])
	}
}

func TestInstallChangeFeedRejectsTableWithoutPK(t *testing.T) {
	tbl := pg.NewTable("audit")
	pg.Add(tbl, pg.Text("event").NotNull())
	if _, err := pg.InstallChangeFeed(tbl); err == nil {
		t.Error("expected error for table without PK")
	}
}

func TestUninstallChangeFeedEmitsDropDDL(t *testing.T) {
	tbl := pg.NewTable("players")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	stmts := pg.UninstallChangeFeed(tbl)
	if len(stmts) != 2 {
		t.Fatalf("expected 2 statements, got %d", len(stmts))
	}
	if !strings.Contains(stmts[0], "DROP TRIGGER") {
		t.Errorf("missing DROP TRIGGER: %s", stmts[0])
	}
	if !strings.Contains(stmts[1], "DROP FUNCTION") {
		t.Errorf("missing DROP FUNCTION: %s", stmts[1])
	}
}

func TestChangeEventJSONShape(t *testing.T) {
	ev := pg.ChangeEvent{Op: pg.OpInsert, ID: "42"}
	body, err := json.Marshal(&ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(body), `"op":"INSERT"`) || !strings.Contains(string(body), `"id":"42"`) {
		t.Errorf("payload: %s", body)
	}
	var out pg.ChangeEvent
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.Op != pg.OpInsert || out.ID != "42" {
		t.Errorf("round trip: %+v", out)
	}
}

func TestSubscribeReturnsErrorWhenDriverHasNoListener(t *testing.T) {
	tbl := pg.NewTable("players")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	type Player struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	ent := pg.NewEntity[Player](tbl)
	db := pg.New(noListenDriver{})
	_, err := pg.Subscribe[Player](db, context.Background(), ent)
	if err == nil {
		t.Error("expected ErrListenNotSupported")
	}
}

func TestSubscribeDeliversTypedChangesFromListener(t *testing.T) {
	tbl := pg.NewTable("players")
	pg.Add(tbl, pg.BigSerial("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	type Player struct {
		ID   int64  `drop:"id"`
		Name string `drop:"name"`
	}
	ent := pg.NewEntity[Player](tbl)

	drv := newListenDriver()
	db := pg.New(drv)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := pg.Subscribe[Player](db, ctx, ent, pg.SubscribeOptions{Buffer: 4})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Wait for subscription goroutine to register on the channel.
	drv.waitForListener("drops_players")

	drv.publish("drops_players", `{"op":"INSERT","id":"7"}`)
	drv.publish("drops_players", `{"op":"UPDATE","id":"7"}`)
	drv.publish("drops_players", `{"op":"DELETE","id":"9"}`)

	got := drainChanges(ch, 3, 200*time.Millisecond)
	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	if got[0].Op != pg.OpInsert || got[0].ID != "7" {
		t.Errorf("event[0]: %+v", got[0])
	}
	if got[1].Op != pg.OpUpdate {
		t.Errorf("event[1]: %+v", got[1])
	}
	if got[2].Op != pg.OpDelete || got[2].ID != "9" {
		t.Errorf("event[2]: %+v", got[2])
	}
}

func drainChanges[T any](ch <-chan pg.TypedChange[T], n int, timeout time.Duration) []pg.TypedChange[T] {
	out := make([]pg.TypedChange[T], 0, n)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for len(out) < n {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timer.C:
			return out
		}
	}
	return out
}

// noListenDriver lacks the Listener interface so Subscribe must
// return ErrListenNotSupported.
type noListenDriver struct{}

func (noListenDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}
func (noListenDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, nil
}
func (noListenDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

// listenDriver implements drops.Driver + pg.Listener. Tests push
// fake notifications via publish() and the subscription delivers
// them through the typed channel.
type listenDriver struct {
	mu       sync.Mutex
	channels map[string]chan pg.Notification
	ready    chan string
}

func newListenDriver() *listenDriver {
	return &listenDriver{
		channels: map[string]chan pg.Notification{},
		ready:    make(chan string, 8),
	}
}

func (d *listenDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, nil
}
func (d *listenDriver) Query(context.Context, string, ...any) (drops.Rows, error) {
	return nil, nil
}
func (d *listenDriver) Begin(context.Context) (drops.Tx, error) { return nil, nil }

func (d *listenDriver) Listen(ctx context.Context, channel string) (<-chan pg.Notification, error) {
	d.mu.Lock()
	ch := make(chan pg.Notification, 16)
	d.channels[channel] = ch
	d.mu.Unlock()
	d.ready <- channel
	go func() {
		<-ctx.Done()
		d.mu.Lock()
		delete(d.channels, channel)
		close(ch)
		d.mu.Unlock()
	}()
	return ch, nil
}

func (d *listenDriver) publish(channel, payload string) {
	d.mu.Lock()
	ch, ok := d.channels[channel]
	d.mu.Unlock()
	if !ok {
		return
	}
	ch <- pg.Notification{Channel: channel, Payload: payload}
}

func (d *listenDriver) waitForListener(channel string) {
	deadline := time.NewTimer(200 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case c := <-d.ready:
			if c == channel {
				return
			}
		case <-deadline.C:
			return
		}
	}
}
