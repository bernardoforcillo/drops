package mirror_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// pushStream hands out messages a test feeds it, and records acks.
type pushStream struct {
	mu   sync.Mutex
	msgs chan pg.Message
	acks []uint64
}

func newPushStream() *pushStream {
	return &pushStream{msgs: make(chan pg.Message, 64)}
}

func (s *pushStream) Next(ctx context.Context) (pg.Message, error) {
	select {
	case m, open := <-s.msgs:
		if !open {
			return pg.Message{}, errors.New("stream ended")
		}
		return m, nil
	case <-ctx.Done():
		return pg.Message{}, ctx.Err()
	}
}

func (s *pushStream) Ack(_ context.Context, lsn uint64) error {
	s.mu.Lock()
	s.acks = append(s.acks, lsn)
	s.mu.Unlock()
	return nil
}

func (s *pushStream) Close() error { return nil }

func (s *pushStream) ackedLSNs() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.acks...)
}

// tx pushes a whole transaction onto the stream.
func (s *pushStream) tx(commitLSN uint64, changes ...pg.Message) {
	s.msgs <- pg.Message{Kind: pg.MessageBegin, LSN: commitLSN - uint64(len(changes)) - 1}
	for _, c := range changes {
		s.msgs <- c
	}
	s.msgs <- pg.Message{Kind: pg.MessageCommit, LSN: commitLSN, CommitTime: time.Unix(1700000000, 0)}
}

func row(table string, op pg.ChangeOp, cols map[string]any) pg.Message {
	m := pg.Message{Kind: pg.MessageChange, Schema: "public", Table: table, Op: op}
	if op == pg.OpDelete {
		m.Old = cols
	} else {
		m.New = cols
	}
	return m
}

func newSource(t *testing.T, stream pg.ReplicationStream, opts mirror.LogicalOptions) (*mirror.LogicalSource, context.CancelFunc) {
	t.Helper()
	if opts.Wait == 0 {
		opts.Wait = 200 * time.Millisecond
	}
	src, err := mirror.NewLogicalSource(stream, opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = src.Run(ctx) }()
	return src, func() { cancel(); src.Stop() }
}

func TestLogicalSourceDecodesChanges(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{"orders": "id"},
	})
	defer stop()

	stream.tx(100,
		row("orders", pg.OpInsert, map[string]any{"id": int64(1), "total": 10}),
		row("orders", pg.OpUpdate, map[string]any{"id": int64(1), "total": 20}),
		row("orders", pg.OpDelete, map[string]any{"id": int64(2)}),
	)

	changes, commit, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("got %d changes, want 3: %+v", len(changes), changes)
	}
	if changes[0].Op != mirror.OpInsert || changes[0].Key != "1" {
		t.Errorf("insert = %+v", changes[0])
	}
	if changes[1].Op != mirror.OpUpdate || changes[1].Row["total"] != 20 {
		t.Errorf("update = %+v", changes[1])
	}
	// A delete carries the key and no row: the row is gone at the
	// source and a sink only needs to tombstone its copy.
	if changes[2].Op != mirror.OpDelete || changes[2].Key != "2" || changes[2].Row != nil {
		t.Errorf("delete = %+v", changes[2])
	}
	// Every change in a transaction takes the commit position.
	want := pg.LSNVersion(100)
	for i, c := range changes {
		if c.Version != want {
			t.Errorf("change %d version = %d, want %d", i, c.Version, want)
		}
		if !c.At.Equal(time.Unix(1700000000, 0)) {
			t.Errorf("change %d At = %v", i, c.At)
		}
	}

	// Nothing is acknowledged until the pump says the sinks accepted.
	if got := stream.ackedLSNs(); len(got) != 0 {
		t.Fatalf("acked %v before commit; the WAL would be released before the sinks saw it", got)
	}
	if err := commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stream.ackedLSNs(); len(got) != 1 || got[0] != 100 {
		t.Errorf("acks = %v, want [100]", got)
	}
}

// A publication is usually wider than one mirror.
func TestLogicalSourceDropsUnlistedTables(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{"orders": "id"},
	})
	defer stop()

	stream.tx(50, row("audit_log", pg.OpInsert, map[string]any{"id": int64(9)}))

	changes, commit, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("got %d changes for an unmirrored table", len(changes))
	}
	// There is nothing to apply, but the position still has to move
	// or a busy table nobody mirrors would hold the slot back
	// forever — so an empty batch still carries a commit function,
	// which is the case Pump already handles.
	if commit == nil {
		t.Fatal("an empty batch came back with no commit function; the slot would never advance")
	}
	if got := stream.ackedLSNs(); len(got) != 0 {
		t.Fatalf("acked %v before the pump committed", got)
	}
	if err := commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stream.ackedLSNs(); len(got) != 1 || got[0] != 50 {
		t.Errorf("acks = %v, want [50]", got)
	}
}

func TestLogicalSourceSchemaQualifiedTablesWin(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{
			"orders":        "id",
			"public.orders": "order_uuid",
		},
	})
	defer stop()

	stream.tx(60, row("orders", pg.OpInsert, map[string]any{
		"id": int64(1), "order_uuid": "8f3b-1",
	}))
	changes, _, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Key != "8f3b-1" {
		t.Fatalf("exact schema.table match did not win: %+v", changes)
	}
}

// max is a floor, not a ceiling: a transaction is never split.
func TestLogicalSourceNeverSplitsATransaction(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{"orders": "id"},
	})
	defer stop()

	stream.tx(70,
		row("orders", pg.OpInsert, map[string]any{"id": int64(1)}),
		row("orders", pg.OpInsert, map[string]any{"id": int64(2)}),
		row("orders", pg.OpInsert, map[string]any{"id": int64(3)}),
	)
	changes, _, err := src.Fetch(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 3 {
		t.Fatalf("got %d changes with max=1, want the whole transaction (3)", len(changes))
	}
}

// One ack covers a batch, because a stream position is cumulative.
func TestLogicalSourceBatchesAndAcksTheLastCommit(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{"orders": "id"},
	})
	defer stop()

	stream.tx(80, row("orders", pg.OpInsert, map[string]any{"id": int64(1)}))
	stream.tx(90, row("orders", pg.OpInsert, map[string]any{"id": int64(2)}))

	// Give the reader a moment to buffer both.
	deadline := time.Now().Add(time.Second)
	var changes []mirror.Change
	var commit func(context.Context) error
	for time.Now().Before(deadline) {
		c, cm, err := src.Fetch(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		changes, commit = c, cm
		if len(changes) == 2 {
			break
		}
	}
	if len(changes) != 2 {
		t.Fatalf("got %d changes, want both transactions in one batch", len(changes))
	}
	if err := commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	acks := stream.ackedLSNs()
	if len(acks) != 1 || acks[len(acks)-1] != 90 {
		t.Errorf("acks = %v, want a single ack at the last commit (90)", acks)
	}
}

// An empty batch means "nothing right now" — the pump is built on it.
func TestLogicalSourceReturnsNothingWhenIdle(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{"orders": "id"},
		Wait:   20 * time.Millisecond,
	})
	defer stop()

	start := time.Now()
	changes, commit, err := src.Fetch(context.Background(), 10)
	if err != nil {
		t.Fatalf("idle Fetch returned an error: %v", err)
	}
	if changes != nil {
		t.Fatalf("idle Fetch returned %d changes", len(changes))
	}
	if commit != nil {
		t.Fatal("idle Fetch returned a commit function with no batch")
	}
	if time.Since(start) > time.Second {
		t.Error("idle Fetch blocked far longer than Wait")
	}
}

func TestLogicalSourceKeyCoercion(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{"t": "k"},
	})
	defer stop()

	stream.tx(200,
		row("t", pg.OpInsert, map[string]any{"k": int64(42)}),
		row("t", pg.OpInsert, map[string]any{"k": "8f3b-1"}),
		row("t", pg.OpInsert, map[string]any{"k": []byte("raw")}),
		row("t", pg.OpInsert, map[string]any{"k": int32(7)}),
		row("t", pg.OpInsert, map[string]any{"k": uint64(9)}),
	)
	changes, _, err := src.Fetch(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"42", "8f3b-1", "raw", "7", "9"}
	if len(changes) != len(want) {
		t.Fatalf("got %d changes, want %d", len(changes), len(want))
	}
	for i, w := range want {
		if changes[i].Key != w {
			t.Errorf("key %d = %q, want %q", i, changes[i].Key, w)
		}
	}
}

// A key with no unambiguous text form addresses no mirrored row, so
// it is refused rather than formatted into one.
func TestLogicalSourceRefusesUnusableKeys(t *testing.T) {
	for name, val := range map[string]any{
		"float":  3.14,
		"nil":    nil,
		"empty":  "",
		"struct": struct{ A int }{1},
	} {
		stream := newPushStream()
		src, stop := newSource(t, stream, mirror.LogicalOptions{
			Tables: map[string]string{"t": "k"},
		})
		stream.tx(300, row("t", pg.OpInsert, map[string]any{"k": val}))
		_, _, err := src.Fetch(context.Background(), 10)
		if err == nil {
			t.Errorf("%s: expected a refusal", name)
		}
		stop()
	}
}

// A missing key column is the symptom of a REPLICA IDENTITY that
// carries less than the mirror needs, and the error says so.
func TestLogicalSourceReportsMissingKeyColumn(t *testing.T) {
	stream := newPushStream()
	src, stop := newSource(t, stream, mirror.LogicalOptions{
		Tables: map[string]string{"orders": "id"},
	})
	defer stop()

	stream.tx(400, row("orders", pg.OpDelete, map[string]any{"other": 1}))
	_, _, err := src.Fetch(context.Background(), 10)
	if err == nil {
		t.Fatal("expected an error for a missing key column")
	}
	if !strings.Contains(err.Error(), "REPLICA IDENTITY") {
		t.Errorf("error does not point at the cause: %v", err)
	}
}

func TestNewLogicalSourceValidatesOptions(t *testing.T) {
	if _, err := mirror.NewLogicalSource(nil, mirror.LogicalOptions{
		Tables: map[string]string{"t": "id"},
	}); err == nil {
		t.Error("expected an error for a nil stream")
	}
	if _, err := mirror.NewLogicalSource(newPushStream(), mirror.LogicalOptions{}); err == nil {
		t.Error("expected an error for no tables")
	}
	if _, err := mirror.NewLogicalSource(newPushStream(), mirror.LogicalOptions{
		Tables: map[string]string{"t": ""},
	}); err == nil {
		t.Error("expected an error for an empty key column")
	}
}

// The source satisfies the interface the pump drains.
var _ mirror.Source = (*mirror.LogicalSource)(nil)
