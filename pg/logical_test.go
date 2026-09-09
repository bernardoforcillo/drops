package pg_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

func TestLSNVersionLandsInTheLiveBand(t *testing.T) {
	const liveBase uint64 = 1 << 63
	if got := pg.LSNVersion(0); got != liveBase {
		t.Errorf("LSNVersion(0) = %d, want the band floor %d", got, liveBase)
	}
	lsn, err := pg.ParseLSN("16/B374D848")
	if err != nil {
		t.Fatal(err)
	}
	if got := pg.LSNVersion(lsn); got <= liveBase {
		t.Errorf("LSNVersion(%s) = %d, below the band floor", pg.FormatLSN(lsn), got)
	}
	// Ordering has to survive the mapping — it is the whole point.
	a, _ := pg.ParseLSN("16/B374D848")
	b, _ := pg.ParseLSN("16/B374D849")
	if pg.LSNVersion(a) >= pg.LSNVersion(b) {
		t.Error("LSNVersion did not preserve LSN order")
	}
}

func TestSlotNameValidation(t *testing.T) {
	db := pg.New(&recordingDriver{})
	ctx := context.Background()
	bad := []string{
		"",
		"UPPERCASE",
		"has-dash",
		"has space",
		`quote"name`,
		strings.Repeat("a", 64),
	}
	for _, name := range bad {
		if _, err := pg.CreateSlot(ctx, db, name, pg.PluginPgOutput, false); !errors.Is(err, pg.ErrInvalidIdentifier) {
			t.Errorf("CreateSlot(%q) = %v, want ErrInvalidIdentifier", name, err)
		}
		if err := pg.DropSlot(ctx, db, name); !errors.Is(err, pg.ErrInvalidIdentifier) {
			t.Errorf("DropSlot(%q) = %v, want ErrInvalidIdentifier", name, err)
		}
		if _, err := pg.SlotLag(ctx, db, name); !errors.Is(err, pg.ErrInvalidIdentifier) {
			t.Errorf("SlotLag(%q) = %v, want ErrInvalidIdentifier", name, err)
		}
	}
	// The shape PostgreSQL actually allows goes through to the driver.
	if _, err := pg.CreateSlot(ctx, db, "mirror_orders_1", pg.PluginPgOutput, false); errors.Is(err, pg.ErrInvalidIdentifier) {
		t.Error("a legal slot name was rejected")
	}
}

func TestCreateSlotRequiresPlugin(t *testing.T) {
	db := pg.New(&recordingDriver{})
	if _, err := pg.CreateSlot(context.Background(), db, "s", "", false); err == nil {
		t.Error("expected an error for an empty plugin")
	}
}

// The slot name is bound, never interpolated.
func TestSlotStatementsBindTheName(t *testing.T) {
	drv := &recordingDriver{}
	db := pg.New(drv)
	_ = pg.DropSlot(context.Background(), db, "mirror_orders")
	if strings.Contains(drv.sql, "mirror_orders") {
		t.Errorf("slot name was interpolated into the statement: %q", drv.sql)
	}
	if !strings.Contains(drv.sql, "$1") {
		t.Errorf("slot name was not bound: %q", drv.sql)
	}
}

func TestPublicationAndReplicaIdentity(t *testing.T) {
	orders := pg.NewTable("orders")
	pg.Add(orders, pg.BigSerial("id").PrimaryKey())

	stmts := pg.Publication("drops_mirror", orders)
	if len(stmts) != 1 {
		t.Fatalf("Publication returned %d statements", len(stmts))
	}
	want := `CREATE PUBLICATION "drops_mirror" FOR TABLE "orders"`
	if stmts[0] != want {
		t.Errorf("got %q\nwant %q", stmts[0], want)
	}
	if got := pg.Publication("p"); got != nil {
		t.Errorf("Publication with no tables returned %v", got)
	}
	if got := pg.ReplicaIdentityFull(orders); got != `ALTER TABLE "orders" REPLICA IDENTITY FULL` {
		t.Errorf("ReplicaIdentityFull = %q", got)
	}
}

func TestWithSnapshotRefusesWithoutOne(t *testing.T) {
	db := pg.New(&recordingDriver{})
	err := pg.WithSnapshot(context.Background(), db, pg.SlotHandle{Name: "s"}, func(*pg.DB) error {
		t.Error("fn ran without a snapshot")
		return nil
	})
	if !errors.Is(err, pg.ErrNoSnapshot) {
		t.Errorf("got %v, want ErrNoSnapshot", err)
	}
}

// The snapshot name is the one value this package concatenates into
// SQL, so its format is checked exactly.
func TestWithSnapshotValidatesTheName(t *testing.T) {
	db := pg.New(&recordingDriver{})
	bad := []string{
		"'; DROP TABLE users; --",
		"00000003-0000001B",
		"0000003-0000001B-1",
		"00000003-0000001B-",
		"00000003-0000001B-x",
		"zzzzzzzz-0000001B-1",
	}
	for _, name := range bad {
		err := pg.WithSnapshot(context.Background(), db,
			pg.SlotHandle{Name: "s", SnapshotName: name}, func(*pg.DB) error { return nil })
		if !errors.Is(err, pg.ErrInvalidIdentifier) {
			t.Errorf("WithSnapshot(%q) = %v, want ErrInvalidIdentifier", name, err)
		}
	}
}

func TestStreamNotSupported(t *testing.T) {
	db := pg.New(&recordingDriver{})
	if _, err := pg.Stream(db, context.Background(), "s", 0, nil); !errors.Is(err, pg.ErrStreamNotSupported) {
		t.Errorf("got %v, want ErrStreamNotSupported", err)
	}
	if _, ok := pg.Streamer(db); ok {
		t.Error("a plain driver reported a streamer")
	}
}

// --- Reassembly -----------------------------------------------------

// scriptedStream replays a fixed list of messages and records acks.
type scriptedStream struct {
	msgs []pg.Message
	i    int
	acks []uint64
	err  error
}

func (s *scriptedStream) Next(ctx context.Context) (pg.Message, error) {
	if s.i >= len(s.msgs) {
		if s.err != nil {
			return pg.Message{}, s.err
		}
		return pg.Message{}, context.Canceled
	}
	m := s.msgs[s.i]
	s.i++
	return m, nil
}

func (s *scriptedStream) Ack(_ context.Context, lsn uint64) error {
	s.acks = append(s.acks, lsn)
	return nil
}

func (s *scriptedStream) Close() error { return nil }

func change(lsn uint64, table string, op pg.ChangeOp, row map[string]any) pg.Message {
	return pg.Message{Kind: pg.MessageChange, LSN: lsn, Schema: "public", Table: table, Op: op, New: row}
}

func TestReassembleGroupsWholeTransactions(t *testing.T) {
	stream := &scriptedStream{msgs: []pg.Message{
		{Kind: pg.MessageBegin, LSN: 10, XID: 7},
		change(11, "orders", pg.OpInsert, map[string]any{"id": int64(1)}),
		change(12, "orders", pg.OpUpdate, map[string]any{"id": int64(1)}),
		{Kind: pg.MessageCommit, LSN: 13, XID: 7, CommitTime: time.Unix(1000, 0)},
		{Kind: pg.MessageBegin, LSN: 20, XID: 8},
		change(21, "orders", pg.OpDelete, nil),
		{Kind: pg.MessageCommit, LSN: 22, XID: 8},
	}}

	var got []pg.Transaction
	err := pg.Reassemble(context.Background(), stream, func(_ context.Context, tx pg.Transaction) error {
		got = append(got, tx)
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Reassemble ended with %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d transactions, want 2", len(got))
	}
	if got[0].XID != 7 || len(got[0].Changes) != 2 || got[0].CommitLSN != 13 {
		t.Errorf("first transaction = %+v", got[0])
	}
	if !got[0].CommitTime.Equal(time.Unix(1000, 0)) {
		t.Errorf("commit time = %v", got[0].CommitTime)
	}
	if got[1].XID != 8 || len(got[1].Changes) != 1 || got[1].CommitLSN != 22 {
		t.Errorf("second transaction = %+v", got[1])
	}
	// One ack per commit, at the commit position.
	if len(stream.acks) != 2 || stream.acks[0] != 13 || stream.acks[1] != 22 {
		t.Errorf("acks = %v, want [13 22]", stream.acks)
	}
}

// An empty transaction still has to move the position, or an idle
// publication holds the slot back forever.
func TestReassembleAcksEmptyTransactionsWithoutCalling(t *testing.T) {
	stream := &scriptedStream{msgs: []pg.Message{
		{Kind: pg.MessageBegin, LSN: 10},
		{Kind: pg.MessageCommit, LSN: 11},
	}}
	called := false
	_ = pg.Reassemble(context.Background(), stream, func(context.Context, pg.Transaction) error {
		called = true
		return nil
	})
	if called {
		t.Error("fn was called for an empty transaction")
	}
	if len(stream.acks) != 1 || stream.acks[0] != 11 {
		t.Errorf("acks = %v, want [11]", stream.acks)
	}
}

// Acknowledging inside a transaction would tell the server the
// consumer is done with changes it has not seen.
func TestReassembleIgnoresKeepalivesInsideATransaction(t *testing.T) {
	stream := &scriptedStream{msgs: []pg.Message{
		{Kind: pg.MessageKeepalive, LSN: 5},
		{Kind: pg.MessageBegin, LSN: 10},
		{Kind: pg.MessageKeepalive, LSN: 11},
		change(12, "orders", pg.OpInsert, map[string]any{"id": int64(1)}),
		{Kind: pg.MessageCommit, LSN: 13},
	}}
	_ = pg.Reassemble(context.Background(), stream, func(context.Context, pg.Transaction) error { return nil })
	if len(stream.acks) != 2 || stream.acks[0] != 5 || stream.acks[1] != 13 {
		t.Errorf("acks = %v, want [5 13] — the keepalive at 11 must not be acknowledged", stream.acks)
	}
}

func TestReassembleRejectsBrokenFraming(t *testing.T) {
	cases := map[string][]pg.Message{
		"change outside a transaction": {change(1, "orders", pg.OpInsert, nil)},
		"commit without begin":         {{Kind: pg.MessageCommit, LSN: 1}},
		"nested begin": {
			{Kind: pg.MessageBegin, LSN: 1},
			{Kind: pg.MessageBegin, LSN: 2},
		},
	}
	for name, msgs := range cases {
		err := pg.Reassemble(context.Background(), &scriptedStream{msgs: msgs},
			func(context.Context, pg.Transaction) error { return nil })
		if !errors.Is(err, pg.ErrStreamOutOfOrder) {
			t.Errorf("%s: got %v, want ErrStreamOutOfOrder", name, err)
		}
	}
}

// A handler that fails must leave the position where it was, so the
// next run replays the transaction rather than skipping it.
func TestReassembleDoesNotAckWhenTheHandlerFails(t *testing.T) {
	stream := &scriptedStream{msgs: []pg.Message{
		{Kind: pg.MessageBegin, LSN: 10},
		change(11, "orders", pg.OpInsert, map[string]any{"id": int64(1)}),
		{Kind: pg.MessageCommit, LSN: 12},
	}}
	boom := errors.New("sink refused")
	err := pg.Reassemble(context.Background(), stream, func(context.Context, pg.Transaction) error {
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want the handler's error", err)
	}
	if len(stream.acks) != 0 {
		t.Errorf("acks = %v, want none", stream.acks)
	}
}

// The deferred variant is what a consumer that buffers needs: nothing
// may be confirmed at hand-over, because confirming a position takes
// everything before it — including a transaction still in the buffer.
func TestReassembleDeferredNeverAcks(t *testing.T) {
	stream := &scriptedStream{msgs: []pg.Message{
		{Kind: pg.MessageKeepalive, LSN: 5},
		{Kind: pg.MessageBegin, LSN: 10},
		change(11, "orders", pg.OpInsert, map[string]any{"id": int64(1)}),
		{Kind: pg.MessageCommit, LSN: 12},
		// An empty transaction: something touched only tables
		// outside the publication.
		{Kind: pg.MessageBegin, LSN: 20},
		{Kind: pg.MessageCommit, LSN: 21},
	}}

	var got []pg.Transaction
	_ = pg.ReassembleDeferred(context.Background(), stream, func(_ context.Context, tx pg.Transaction) error {
		got = append(got, tx)
		return nil
	})
	if len(stream.acks) != 0 {
		t.Errorf("acks = %v, want none — the caller acknowledges", stream.acks)
	}
	// Keepalive, the real transaction, and the empty one all travel,
	// because their positions cannot be confirmed out of order.
	if len(got) != 3 {
		t.Fatalf("got %d handovers, want 3: %+v", len(got), got)
	}
	if got[0].CommitLSN != 5 || len(got[0].Changes) != 0 {
		t.Errorf("keepalive handover = %+v", got[0])
	}
	if got[1].CommitLSN != 12 || len(got[1].Changes) != 1 {
		t.Errorf("transaction handover = %+v", got[1])
	}
	if got[2].CommitLSN != 21 || len(got[2].Changes) != 0 {
		t.Errorf("empty-transaction handover = %+v", got[2])
	}
}

// The framing check is shared, so it must hold in both variants.
func TestReassembleDeferredRejectsBrokenFraming(t *testing.T) {
	err := pg.ReassembleDeferred(context.Background(),
		&scriptedStream{msgs: []pg.Message{{Kind: pg.MessageCommit, LSN: 1}}},
		func(context.Context, pg.Transaction) error { return nil })
	if !errors.Is(err, pg.ErrStreamOutOfOrder) {
		t.Errorf("got %v, want ErrStreamOutOfOrder", err)
	}
}
