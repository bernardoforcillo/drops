package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// outboxRows fakes the result set Outbox.Drain scans.
type outboxRows struct {
	data [][]any
	pos  int
}

func (r *outboxRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *outboxRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		rv := reflect.ValueOf(d).Elem()
		v := reflect.ValueOf(row[i])
		if v.IsValid() {
			rv.Set(v)
		}
	}
	return nil
}

func (r *outboxRows) Columns() ([]string, error) { return nil, nil }
func (r *outboxRows) Close() error               { return nil }
func (r *outboxRows) Err() error                 { return nil }

type outboxDriver struct {
	rows  func() drops.Rows
	execs []string
	args  [][]any
}

func (d *outboxDriver) Query(_ context.Context, _ string, _ ...any) (drops.Rows, error) {
	if d.rows == nil {
		return &outboxRows{}, nil
	}
	return d.rows(), nil
}

func (d *outboxDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.execs = append(d.execs, sql)
	d.args = append(d.args, args)
	return recResult{}, nil
}

func (d *outboxDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

// event builds one drained outbox row.
func event(id int64, kind string, ch mirror.Change) []any {
	payload, _ := json.Marshal(ch)
	return []any{
		id, kind, any(nil), any(nil), json.RawMessage(payload),
		[]byte(nil), 0, any(nil), time.Unix(1700000000, 0).UTC(),
	}
}

func TestOutboxSourceDecodesChanges(t *testing.T) {
	d := &outboxDriver{rows: func() drops.Rows {
		return &outboxRows{data: [][]any{
			event(1, mirror.EventKind, mirror.Change{Op: mirror.OpInsert, Key: "7", Row: map[string]any{"title": "a"}}),
			event(2, mirror.EventKind, mirror.Change{Op: mirror.OpDelete, Key: "8"}),
		}}
	}}
	src, err := mirror.NewOutboxSource(pg.NewOutbox(pg.New(d), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	changes, commit, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want 2", len(changes))
	}
	if changes[0].Key != "7" || changes[0].Row["title"] != "a" {
		t.Errorf("first change = %+v", changes[0])
	}
	if !changes[1].IsDelete() {
		t.Errorf("second change should be a delete: %+v", changes[1])
	}
	// The outbox id is the version: one sequence in one database,
	// handed out in commit order per key, lifted into the live band
	// so a reseed has room below it.
	if changes[0].Version != mirror.LiveVersion(1) || changes[1].Version != mirror.LiveVersion(2) {
		t.Errorf("versions = %d, %d; want the outbox ids 1, 2 in the live band (%d, %d)",
			changes[0].Version, changes[1].Version, mirror.LiveVersion(1), mirror.LiveVersion(2))
	}
	if changes[0].Version < mirror.LiveVersionBase {
		t.Errorf("version %d is below the live band, so a seed could outrank it", changes[0].Version)
	}
	if commit == nil {
		t.Fatal("Fetch returned no commit function")
	}
	if err := commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(d.execs) != 1 || !strings.Contains(d.execs[0], "publishedAt") {
		t.Errorf("commit should mark the events published, got %v", d.execs)
	}
}

// A version in the payload is not honoured: it comes from a space
// that cannot be compared with the ids around it, and a mirror fed by
// two spaces keeps whichever was measured in the larger unit. Events
// written before EmitChange started refusing one arrive this way, so
// the drain has to be the backstop.
func TestOutboxSourceOverridesAPayloadVersion(t *testing.T) {
	d := &outboxDriver{rows: func() drops.Rows {
		return &outboxRows{data: [][]any{
			event(5, mirror.EventKind, mirror.Change{Op: mirror.OpUpdate, Key: "1", Version: 99}),
			// A stale wall-clock stamp of the kind this package used
			// to write. Left alone it would outrank every id.
			event(6, mirror.EventKind, mirror.Change{Op: mirror.OpUpdate, Key: "1", Version: uint64(time.Now().UnixNano())}),
		}}
	}}
	src, _ := mirror.NewOutboxSource(pg.NewOutbox(pg.New(d), "outbox"))
	changes, _, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Version != mirror.LiveVersion(5) {
		t.Errorf("Version = %d, want the event id 5 in the live band (%d), not the emitter's 99", changes[0].Version, mirror.LiveVersion(5))
	}
	if changes[1].Version != mirror.LiveVersion(6) {
		t.Errorf("Version = %d, want the event id 6 in the live band (%d), not a clock stamp", changes[1].Version, mirror.LiveVersion(6))
	}
	if changes[1].Version <= changes[0].Version {
		t.Errorf("event 6 (%d) must outrank event 5 (%d): the ids are the order", changes[1].Version, changes[0].Version)
	}
}

// The version belongs to the outbox, so an emitter that sets one is
// stopped at the call site — inside its own transaction, where the
// mistake is cheap — rather than having it silently replaced at drain
// or, worse, honoured.
func TestEmitChangeRefusesAPresetVersion(t *testing.T) {
	d := &outboxDriver{}
	ob := pg.NewOutbox(pg.New(d), "outbox")
	err := mirror.EmitChange(ob, pg.New(d), context.Background(), mirror.Change{
		Op: mirror.OpUpdate, Key: "7", Version: uint64(time.Now().UnixNano()),
	})
	if err == nil {
		t.Fatal("expected EmitChange to refuse a change that came with its own version")
	}
	for _, want := range []string{"Version", "7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q, got %v", want, err)
		}
	}
	if len(d.execs) != 0 {
		t.Errorf("nothing should have been written to the outbox, got %v", d.execs)
	}

	// The version is the only thing refused: At is the emitter's to
	// state, and a change that leaves both alone is written.
	if err := mirror.EmitChange(ob, pg.New(d), context.Background(), mirror.Change{
		Op: mirror.OpUpdate, Key: "7", At: time.Unix(1700000000, 0),
	}); err != nil {
		t.Fatalf("EmitChange with an explicit At: %v", err)
	}
	if len(d.execs) != 1 {
		t.Fatalf("expected one outbox insert, got %d", len(d.execs))
	}
}

// A mirror can share an outbox table with the rest of the
// application. Another kind's event must be left pending, not
// swallowed by the mirror's acknowledgement.
func TestOutboxSourceLeavesForeignEventsPending(t *testing.T) {
	d := &outboxDriver{rows: func() drops.Rows {
		return &outboxRows{data: [][]any{
			event(1, "billing.invoice.paid", mirror.Change{}),
			event(2, mirror.EventKind, mirror.Change{Op: mirror.OpInsert, Key: "7"}),
		}}
	}}
	src, _ := mirror.NewOutboxSource(pg.NewOutbox(pg.New(d), "outbox"))
	changes, commit, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("only the mirror event should be decoded, got %d", len(changes))
	}
	if err := commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Only event 2 may be acknowledged. MarkPublished binds the id
	// list as a single array parameter, so flatten before checking.
	got := flattenArgs(d.args[0])
	if len(got) != 1 || got[0] != int64(2) {
		t.Errorf("acknowledged ids = %v, want just the mirror event's id 2", got)
	}
}

func TestOutboxSourceEmptyIsIdle(t *testing.T) {
	src, _ := mirror.NewOutboxSource(pg.NewOutbox(pg.New(&outboxDriver{}), "outbox"))
	changes, commit, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 || commit != nil {
		t.Errorf("empty fetch = %d changes, commit=%v", len(changes), commit != nil)
	}
}

func TestOutboxSourceSurfacesBadPayload(t *testing.T) {
	d := &outboxDriver{rows: func() drops.Rows {
		return &outboxRows{data: [][]any{{
			int64(1), mirror.EventKind, any(nil), any(nil), json.RawMessage(`{not json`),
			[]byte(nil), 0, any(nil), time.Unix(1, 0),
		}}}
	}}
	src, _ := mirror.NewOutboxSource(pg.NewOutbox(pg.New(d), "outbox"))
	if _, _, err := src.Fetch(context.Background(), 10); err == nil {
		t.Error("expected a decode error rather than a silently skipped change")
	}
}

func TestNewOutboxSourceValidates(t *testing.T) {
	if _, err := mirror.NewOutboxSource(nil); err == nil {
		t.Error("expected an error for a nil outbox")
	}
}

func TestEmitChangeValidates(t *testing.T) {
	ob := pg.NewOutbox(pg.New(&outboxDriver{}), "outbox")
	err := mirror.EmitChange(ob, pg.New(&outboxDriver{}), context.Background(), mirror.Change{Op: mirror.OpInsert})
	if !errors.Is(err, mirror.ErrNoKey) {
		t.Errorf("err = %v want ErrNoKey", err)
	}
	if err := mirror.EmitChange(nil, nil, context.Background(), mirror.Change{Op: mirror.OpInsert, Key: "1"}); err == nil {
		t.Error("expected an error for a nil outbox")
	}
}

// flattenArgs expands a bound slice parameter into its elements.
func flattenArgs(args []any) []any {
	var out []any
	for _, a := range args {
		rv := reflect.ValueOf(a)
		if rv.IsValid() && rv.Kind() == reflect.Slice {
			for i := 0; i < rv.Len(); i++ {
				out = append(out, rv.Index(i).Interface())
			}
			continue
		}
		out = append(out, a)
	}
	return out
}
