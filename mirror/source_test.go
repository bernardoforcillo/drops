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
	// The outbox id is monotonic, which is exactly what the version
	// column needs when the emitter did not set one.
	if changes[0].Version != 1 || changes[1].Version != 2 {
		t.Errorf("versions = %d, %d; want the outbox ids 1, 2", changes[0].Version, changes[1].Version)
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

// An explicit version from the emitter beats the outbox id.
func TestOutboxSourceKeepsExplicitVersion(t *testing.T) {
	d := &outboxDriver{rows: func() drops.Rows {
		return &outboxRows{data: [][]any{
			event(5, mirror.EventKind, mirror.Change{Op: mirror.OpUpdate, Key: "1", Version: 99}),
		}}
	}}
	src, _ := mirror.NewOutboxSource(pg.NewOutbox(pg.New(d), "outbox"))
	changes, _, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if changes[0].Version != 99 {
		t.Errorf("Version = %d, want the emitter's 99", changes[0].Version)
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
