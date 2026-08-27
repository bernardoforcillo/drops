package integration_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// Change data capture, against a server that actually decodes the WAL.
//
// The unit suite covers the slot statements by asserting on the SQL
// and the framing by replaying a scripted stream. Neither can say
// whether PostgreSQL accepts `pg_create_logical_replication_slot` with
// the arguments drops binds, whether `DropInactiveSlots` really
// declines an active slot, or whether the transaction boundaries drops
// reassembles are the ones the server emits. Those are facts about the
// server.
//
// # What the stream adapter here is, and is not
//
// drops defines [pg.LogicalStreamer] for the replication
// sub-protocol, which needs a connection opened in replication mode
// and a decoder for the wire format — a driver's job, and out of reach
// of a test that only has database/sql.
//
// PostgreSQL exposes the same decoding over ordinary SQL:
// `pg_logical_slot_peek_changes` reads without consuming and
// `pg_replication_slot_advance` confirms a position. That pair has
// exactly the shape of [pg.ReplicationStream] — Next and Ack — so the
// adapter below implements the interface over it, using the
// `test_decoding` output plugin that ships with every PostgreSQL
// build.
//
// So these tests exercise slot lifecycle, real decoded output, the
// begin/change/commit framing, acknowledgement actually advancing the
// slot, and [mirror.LogicalSource] end to end. They do not exercise
// the streaming replication protocol itself, nor snapshot export,
// which `pg_create_logical_replication_slot` cannot do — see
// TestPGWithSnapshotReadsAnInstant for the part of that which *is*
// reachable from SQL.

const testDecoding = "test_decoding"

// requireLogicalWAL skips (or fails, under DROPS_REQUIRE_ALL) when the
// server was not started with wal_level=logical.
//
// It is a configuration rather than a capability, so the message says
// what to change: a run that silently skips the whole of CDC because
// of one setting reports green for tests that never ran.
func requireLogicalWAL(t *testing.T, db *pg.DB) {
	t.Helper()
	rows, err := db.Query(context.Background(), "SHOW wal_level")
	if err != nil {
		t.Fatalf("reading wal_level: %v", err)
	}
	defer rows.Close()
	var level string
	if rows.Next() {
		if err := rows.Scan(&level); err != nil {
			t.Fatalf("scanning wal_level: %v", err)
		}
	}
	if level == "logical" {
		return
	}
	msg := fmt.Sprintf("wal_level is %q, not \"logical\" — no replication slot can be created. "+
		"docker-compose.yml and scripts/local-servers.sh both set it; a server started another way needs "+
		"-c wal_level=logical and a restart", level)
	if integration.RequireAll() {
		t.Fatal(msg)
	}
	t.Skip(msg)
}

// slotFixture creates a permanent slot for the test and drops it
// afterwards, so a failing run cannot leave one behind retaining WAL.
func slotFixture(t *testing.T, db *pg.DB) string {
	t.Helper()
	requireLogicalWAL(t, db)
	name := strings.ToLower(integration.UniqueName(t, "slot"))
	ctx := context.Background()

	// A leftover from a previous failing run must not make this one
	// read its changes.
	_ = pg.DropSlot(ctx, db, name)

	if _, err := pg.CreateSlot(ctx, db, name, testDecoding, false); err != nil {
		t.Fatalf("creating slot %q: %v", name, err)
	}
	t.Cleanup(func() {
		if err := pg.DropSlot(context.Background(), db, name); err != nil {
			t.Errorf("dropping slot %q: %v — it will retain WAL until somebody does", name, err)
		}
	})
	return name
}

// cdcTable creates a table for a CDC test, with REPLICA IDENTITY FULL
// so deletes and updates decode their old values.
func cdcTable(t *testing.T, db *pg.DB, full bool) *pg.Table {
	t.Helper()
	tbl := pg.NewTable(strings.ToLower(integration.UniqueName(t, "cdc")))
	pg.Add(tbl, pg.BigInt("id").PrimaryKey())
	pg.Add(tbl, pg.Text("title").NotNull())
	dropPG(t, db, tbl)
	execPG(t, db, pg.CreateTable(tbl))
	if full {
		if _, err := db.Exec(context.Background(), pg.ReplicaIdentityFull(tbl)); err != nil {
			t.Fatalf("REPLICA IDENTITY FULL: %v", err)
		}
	}
	return tbl
}

// --- Slot lifecycle ---------------------------------------------------

func TestPGLogicalSlotLifecycle(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	name := slotFixture(t, db)

	status, err := pg.SlotStatusOf(ctx, db, name)
	if err != nil {
		t.Fatalf("SlotStatusOf: %v", err)
	}
	if status == nil {
		t.Fatal("the slot drops just created is not in pg_replication_slots")
	}
	if status.Plugin != testDecoding || status.Temporary {
		t.Errorf("status = %+v, want a permanent %s slot", status, testDecoding)
	}

	// EnsureSlot is idempotent, which is what lets two processes
	// start at once without one of them failing.
	created, err := pg.EnsureSlot(ctx, db, name, testDecoding)
	if err != nil {
		t.Fatalf("EnsureSlot on an existing slot: %v", err)
	}
	if created {
		t.Error("EnsureSlot reported it created a slot that already existed")
	}

	found := false
	all, err := pg.Slots(ctx, db)
	if err != nil {
		t.Fatalf("Slots: %v", err)
	}
	for _, s := range all {
		if s.Name == name {
			found = true
		}
	}
	if !found {
		t.Error("Slots() did not list the slot")
	}

	// A slot that does not exist is nil rather than an error, which
	// is what EnsureSlot branches on.
	missing, err := pg.SlotStatusOf(ctx, db, "no_such_slot_here")
	if err != nil || missing != nil {
		t.Errorf("SlotStatusOf(absent) = %v, %v; want nil, nil", missing, err)
	}
	if _, err := pg.SlotLag(ctx, db, "no_such_slot_here"); !errors.Is(err, pg.ErrNoSuchSlot) {
		t.Errorf("SlotLag(absent) = %v, want ErrNoSuchSlot", err)
	}
}

// The number to alert on has to move when the consumer stops reading,
// or the alert is decoration.
func TestPGSlotLagGrowsWithUnreadWAL(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	name := slotFixture(t, db)
	tbl := cdcTable(t, db, false)

	before, err := pg.SlotLag(ctx, db, name)
	if err != nil {
		t.Fatalf("SlotLag: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := db.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %q (id, title) VALUES ($1, $2)`, tbl.Name()),
			i, strings.Repeat("x", 200)); err != nil {
			t.Fatal(err)
		}
	}
	after, err := pg.SlotLag(ctx, db, name)
	if err != nil {
		t.Fatalf("SlotLag: %v", err)
	}
	if after <= before {
		t.Errorf("lag did not grow across 50 unread writes: %d then %d", before, after)
	}
}

// The re-check inside DropInactiveSlots is the whole safety property:
// a consumer that was merely restarting must not lose its position.
func TestPGDropInactiveSlotsLeavesActiveOnesAlone(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	name := slotFixture(t, db)

	// Nothing is streaming from it, so it is inactive and eligible.
	inactive, err := pg.InactiveSlots(ctx, db, strings.SplitN(name, "_", 2)[0])
	if err != nil {
		t.Fatalf("InactiveSlots: %v", err)
	}
	var listed bool
	for _, s := range inactive {
		if s.Name == name {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("InactiveSlots did not list %q; got %+v", name, inactive)
	}

	// The prefix filter must not sweep up somebody else's slots.
	others, err := pg.InactiveSlots(ctx, db, "definitely_not_a_real_prefix")
	if err != nil {
		t.Fatalf("InactiveSlots(prefix): %v", err)
	}
	if len(others) != 0 {
		t.Errorf("prefix filter matched %d unrelated slots", len(others))
	}

	dropped, err := pg.DropInactiveSlots(ctx, db, []string{name})
	if err != nil {
		t.Fatalf("DropInactiveSlots: %v", err)
	}
	if len(dropped) != 1 || dropped[0] != name {
		t.Fatalf("dropped = %v, want [%s]", dropped, name)
	}
	if s, _ := pg.SlotStatusOf(ctx, db, name); s != nil {
		t.Error("the slot is still there after being dropped")
	}
	// Re-create it so the fixture's cleanup has something to drop and
	// does not report a failure.
	if _, err := pg.CreateSlot(ctx, db, name, testDecoding, false); err != nil {
		t.Fatal(err)
	}

	// A name that is not inactive — here, one that no longer exists —
	// is skipped rather than erroring, because the filter and the
	// drop are one statement.
	dropped, err = pg.DropInactiveSlots(ctx, db, []string{"no_such_slot_here"})
	if err != nil {
		t.Errorf("DropInactiveSlots on an absent slot: %v", err)
	}
	if len(dropped) != 0 {
		t.Errorf("reported dropping %v", dropped)
	}
}

func TestPGCurrentWALLSNRoundTrips(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	requireLogicalWAL(t, db)

	lsn, err := pg.CurrentWALLSN(ctx, db)
	if err != nil {
		t.Fatalf("CurrentWALLSN: %v", err)
	}
	if lsn == 0 {
		t.Fatal("current WAL position is zero")
	}
	// drops must render an LSN the way the server does, or a value
	// logged here cannot be pasted into a query against
	// pg_replication_slots.
	rows, err := db.Query(ctx, `SELECT $1::pg_lsn = pg_current_wal_lsn()
	                            OR $1::pg_lsn < pg_current_wal_lsn()`, pg.FormatLSN(lsn))
	if err != nil {
		t.Fatalf("the server rejected drops' LSN text %q: %v", pg.FormatLSN(lsn), err)
	}
	defer rows.Close()
	var ok bool
	if rows.Next() {
		_ = rows.Scan(&ok)
	}
	if !ok {
		t.Errorf("round-tripped LSN %s is ahead of the server's own position", pg.FormatLSN(lsn))
	}
}

// --- Decoding, framing and acknowledgement ---------------------------

func TestPGLogicalReassembleFramesRealTransactions(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	slot := slotFixture(t, db)
	tbl := cdcTable(t, db, true)
	q := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}

	// Two transactions, one of them multi-statement.
	if err := db.InTx(ctx, func(tx *pg.DB) error {
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (1, 'a')`, tbl.Name())); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %q SET title = 'b' WHERE id = 1`, tbl.Name()))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	q(fmt.Sprintf(`DELETE FROM %q WHERE id = 1`, tbl.Name()))

	stream := newSQLStream(db, slot, tbl.Name())
	defer stream.Close()

	var txs []pg.Transaction
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := pg.Reassemble(runCtx, stream, func(_ context.Context, tx pg.Transaction) error {
		txs = append(txs, tx)
		return nil
	})
	if !errors.Is(err, errStreamDrained) {
		t.Fatalf("Reassemble: %v", err)
	}
	if len(txs) != 2 {
		t.Fatalf("got %d transactions, want 2: %+v", len(txs), txs)
	}

	// The multi-statement transaction arrives whole, not as two.
	if len(txs[0].Changes) != 2 {
		t.Errorf("first transaction has %d changes, want 2 (INSERT and UPDATE together)", len(txs[0].Changes))
	}
	if txs[0].Changes[0].Op != pg.OpInsert || txs[0].Changes[1].Op != pg.OpUpdate {
		t.Errorf("ops = %v, %v", txs[0].Changes[0].Op, txs[0].Changes[1].Op)
	}
	if got := txs[0].Changes[1].New["title"]; got != "b" {
		t.Errorf("updated title = %v, want b", got)
	}
	// REPLICA IDENTITY FULL is why the old value is here at all.
	if got := txs[0].Changes[1].Old["title"]; got != "a" {
		t.Errorf("old title = %v, want a — REPLICA IDENTITY FULL should carry it", got)
	}
	if len(txs[1].Changes) != 1 || txs[1].Changes[0].Op != pg.OpDelete {
		t.Fatalf("second transaction = %+v, want one delete", txs[1].Changes)
	}
	if got := txs[1].Changes[0].Old["id"]; got != "1" {
		t.Errorf("deleted row's key = %v, want 1", got)
	}

	// Commit positions must be increasing and non-zero — they are
	// what a mirror versions by.
	if txs[0].CommitLSN == 0 || txs[1].CommitLSN <= txs[0].CommitLSN {
		t.Errorf("commit LSNs are not increasing: %s then %s",
			pg.FormatLSN(txs[0].CommitLSN), pg.FormatLSN(txs[1].CommitLSN))
	}
}

// Acknowledging has to move the slot on the server, or the WAL is
// never released and the disk fills.
func TestPGAcknowledgementAdvancesTheSlot(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	slot := slotFixture(t, db)
	tbl := cdcTable(t, db, false)

	for i := 1; i <= 3; i++ {
		if _, err := db.Exec(ctx,
			fmt.Sprintf(`INSERT INTO %q VALUES ($1, $2)`, tbl.Name()), i, "row"); err != nil {
			t.Fatal(err)
		}
	}

	before, err := pg.SlotStatusOf(ctx, db, slot)
	if err != nil || before == nil {
		t.Fatalf("SlotStatusOf: %v", err)
	}

	stream := newSQLStream(db, slot, tbl.Name())
	defer stream.Close()

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var n int
	// Drained rather than stopped: Reassemble acknowledges *after*
	// the callback returns, so a callback that stops on the last
	// transaction stops before that one is confirmed — and the test
	// would then be asserting the opposite of what it means to.
	err = pg.Reassemble(runCtx, stream, func(_ context.Context, tx pg.Transaction) error {
		n++
		return nil
	})
	if !errors.Is(err, errStreamDrained) {
		t.Fatalf("Reassemble: %v", err)
	}
	if n != 3 {
		t.Fatalf("saw %d transactions, want 3", n)
	}

	after, err := pg.SlotStatusOf(ctx, db, slot)
	if err != nil || after == nil {
		t.Fatalf("SlotStatusOf: %v", err)
	}
	if after.ConfirmedFlushLSN <= before.ConfirmedFlushLSN {
		t.Errorf("confirmed position did not move: %s then %s",
			pg.FormatLSN(before.ConfirmedFlushLSN), pg.FormatLSN(after.ConfirmedFlushLSN))
	}

	// And what was confirmed is gone: a restart resumes after it
	// rather than replaying it.
	left := stream.peek(t, 100)
	for _, m := range left {
		if m.Kind == pg.MessageChange {
			t.Errorf("a confirmed change is still pending: %+v", m)
		}
	}
}

// The deferred variant must confirm nothing, and must hand over the
// empty transactions and keepalives whose positions cannot be
// confirmed out of order.
func TestPGReassembleDeferredConfirmsNothing(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	// The table before the slot, so the CREATE TABLE's own
	// (empty) transaction is behind the slot's starting position and
	// the count below is exact.
	tbl := cdcTable(t, db, false)
	slot := slotFixture(t, db)

	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (1, 'a')`, tbl.Name())); err != nil {
		t.Fatal(err)
	}
	// A DDL touching no rows produces a transaction with no changes,
	// which is the case the deferred path has to deliver rather than
	// swallow.
	if _, err := db.Exec(ctx, fmt.Sprintf(`ALTER TABLE %q REPLICA IDENTITY FULL`, tbl.Name())); err != nil {
		t.Fatal(err)
	}

	before, _ := pg.SlotStatusOf(ctx, db, slot)

	stream := newSQLStream(db, slot, tbl.Name())
	defer stream.Close()

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var withChanges, empty int
	err := pg.ReassembleDeferred(runCtx, stream, func(_ context.Context, tx pg.Transaction) error {
		if len(tx.Changes) > 0 {
			withChanges++
		} else {
			empty++
		}
		return nil
	})
	if !errors.Is(err, errStreamDrained) {
		t.Fatalf("ReassembleDeferred: %v", err)
	}
	if withChanges != 1 {
		t.Errorf("got %d transactions with changes, want 1", withChanges)
	}
	if empty != 1 {
		t.Errorf("got %d empty transactions, want 1 — the deferred path must deliver them", empty)
	}
	if stream.acks != 0 {
		t.Errorf("the deferred path acknowledged %d positions; the caller confirms", stream.acks)
	}

	after, _ := pg.SlotStatusOf(ctx, db, slot)
	if after.ConfirmedFlushLSN != before.ConfirmedFlushLSN {
		t.Errorf("the slot advanced without an acknowledgement: %s then %s",
			pg.FormatLSN(before.ConfirmedFlushLSN), pg.FormatLSN(after.ConfirmedFlushLSN))
	}
}

// --- mirror.LogicalSource end to end ----------------------------------

// recordingSink collects what the pump applies.
type recordingSink struct {
	mu      sync.Mutex
	changes []mirror.Change
}

func (s *recordingSink) Name() string { return "recording" }

func (s *recordingSink) Apply(_ context.Context, changes []mirror.Change) error {
	s.mu.Lock()
	s.changes = append(s.changes, changes...)
	s.mu.Unlock()
	return nil
}

func (s *recordingSink) seen() []mirror.Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]mirror.Change(nil), s.changes...)
}

func TestPGLogicalSourceDrivesAMirror(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	slot := slotFixture(t, db)
	tbl := cdcTable(t, db, true)

	q := func(sql string, args ...any) {
		t.Helper()
		if _, err := db.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	q(fmt.Sprintf(`INSERT INTO %q VALUES (1, 'first')`, tbl.Name()))
	q(fmt.Sprintf(`UPDATE %q SET title = 'second' WHERE id = 1`, tbl.Name()))
	q(fmt.Sprintf(`DELETE FROM %q WHERE id = 1`, tbl.Name()))

	stream := newSQLStream(db, slot, tbl.Name())
	defer stream.Close()

	src, err := mirror.NewLogicalSource(stream, mirror.LogicalOptions{
		Tables: map[string]string{tbl.Name(): "id"},
		Wait:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = src.Run(runCtx) }()
	defer src.Stop()

	sink := &recordingSink{}
	pump, err := mirror.NewPump(src, sink)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && len(sink.seen()) < 3 {
		if _, err := pump.Step(runCtx); err != nil {
			t.Fatalf("pump: %v", err)
		}
	}

	got := sink.seen()
	if len(got) != 3 {
		t.Fatalf("mirror saw %d changes, want 3: %+v", len(got), got)
	}
	wantOps := []mirror.Op{mirror.OpInsert, mirror.OpUpdate, mirror.OpDelete}
	for i, want := range wantOps {
		if got[i].Op != want {
			t.Errorf("change %d op = %s, want %s", i, got[i].Op, want)
		}
		if got[i].Key != "1" {
			t.Errorf("change %d key = %q, want \"1\"", i, got[i].Key)
		}
	}
	// A delete carries no row: the source is gone and a sink only
	// needs to tombstone its copy.
	if got[2].Row != nil {
		t.Errorf("delete carried a row: %+v", got[2].Row)
	}
	// Versions come from the commit position, so they must be
	// strictly increasing across separate transactions and land in
	// the live band.
	for i := 1; i < len(got); i++ {
		if got[i].Version <= got[i-1].Version {
			t.Errorf("versions are not increasing: %d then %d", got[i-1].Version, got[i].Version)
		}
	}
	if got[0].Version < mirror.LiveVersionBase {
		t.Errorf("version %d is below the live band floor %d", got[0].Version, mirror.LiveVersionBase)
	}

	// The pump acknowledged, so the slot moved.
	status, _ := pg.SlotStatusOf(ctx, db, slot)
	if status.ConfirmedFlushLSN == 0 {
		t.Error("the slot was never confirmed; the WAL would never be released")
	}
}

// A relation the mirror does not hold still has to advance the slot,
// or a busy unmirrored table holds it back forever.
func TestPGLogicalSourceAdvancesPastUnmirroredTables(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	slot := slotFixture(t, db)
	mirrored := cdcTable(t, db, false)

	other := pg.NewTable(strings.ToLower(integration.UniqueName(t, "other")))
	pg.Add(other, pg.BigInt("id").PrimaryKey())
	dropPG(t, db, other)
	execPG(t, db, pg.CreateTable(other))

	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (7)`, other.Name())); err != nil {
		t.Fatal(err)
	}

	stream := newSQLStream(db, slot, mirrored.Name(), other.Name())
	defer stream.Close()

	src, err := mirror.NewLogicalSource(stream, mirror.LogicalOptions{
		Tables: map[string]string{mirrored.Name(): "id"},
		Wait:   50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = src.Run(runCtx) }()
	defer src.Stop()

	before, _ := pg.SlotStatusOf(ctx, db, slot)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		changes, commit, err := src.Fetch(runCtx, 100)
		if err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		if commit == nil {
			continue
		}
		if len(changes) != 0 {
			t.Fatalf("got %d changes for a table the mirror does not hold", len(changes))
		}
		if err := commit(runCtx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		break
	}

	after, _ := pg.SlotStatusOf(ctx, db, slot)
	if after.ConfirmedFlushLSN <= before.ConfirmedFlushLSN {
		t.Errorf("the slot did not advance past an unmirrored table's changes: %s then %s",
			pg.FormatLSN(before.ConfirmedFlushLSN), pg.FormatLSN(after.ConfirmedFlushLSN))
	}
}

// --- Publication and replica identity DDL -----------------------------

func TestPGPublicationDDLIsAccepted(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := cdcTable(t, db, false)
	name := strings.ToLower(integration.UniqueName(t, "pub"))

	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), fmt.Sprintf("DROP PUBLICATION IF EXISTS %q", name))
	})
	for _, stmt := range pg.Publication(name, tbl) {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("PostgreSQL rejected the publication DDL: %v\n%s", err, stmt)
		}
	}

	rows, err := db.Query(ctx,
		`SELECT count(*) FROM pg_publication_tables WHERE pubname = $1 AND tablename = $2`,
		name, tbl.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		_ = rows.Scan(&n)
	}
	if n != 1 {
		t.Errorf("the publication does not name the table; a consumer would never hear about it")
	}
}

// REPLICA IDENTITY FULL is not decoration: without it a delete decodes
// with the key alone, and a consumer that needs old values gets none.
func TestPGReplicaIdentityChangesWhatADeleteCarries(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := cdcTable(t, db, false) // default identity: the key only

	q := func(sql string) {
		t.Helper()
		if _, err := db.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}

	// Two slots rather than one, because a slot only ever moves
	// forward: the second half has to start after the first half's
	// changes, and creating a slot is the cheapest way to say that.
	deleteUnder := func(slot string, id int) pg.Message {
		t.Helper()
		q(fmt.Sprintf(`INSERT INTO %q VALUES (%d, 'gone')`, tbl.Name(), id))
		q(fmt.Sprintf(`DELETE FROM %q WHERE id = %d`, tbl.Name(), id))

		stream := newSQLStream(db, slot, tbl.Name())
		defer stream.Close()
		return firstChangeOfOp(t, stream, pg.OpDelete)
	}

	deleted := deleteUnder(namedSlot(t, db, "default"), 1)
	if _, ok := deleted.Old["title"]; ok {
		t.Errorf("the default replica identity carried a non-key column: %+v", deleted.Old)
	}
	if deleted.Old["id"] != "1" {
		t.Errorf("the key is missing from the delete: %+v", deleted.Old)
	}

	slotFull := namedSlot(t, db, "full")
	q(pg.ReplicaIdentityFull(tbl))

	deleted2 := deleteUnder(slotFull, 2)
	if deleted2.Old["title"] != "gone" {
		t.Errorf("REPLICA IDENTITY FULL did not carry the old row: %+v", deleted2.Old)
	}
}

// namedSlot creates a slot for one phase of a test and drops it after.
func namedSlot(t *testing.T, db *pg.DB, suffix string) string {
	t.Helper()
	requireLogicalWAL(t, db)
	name := strings.ToLower(integration.UniqueName(t, "slot_"+suffix))
	ctx := context.Background()
	_ = pg.DropSlot(ctx, db, name)
	if _, err := pg.CreateSlot(ctx, db, name, testDecoding, false); err != nil {
		t.Fatalf("creating slot %q: %v", name, err)
	}
	t.Cleanup(func() { _ = pg.DropSlot(context.Background(), db, name) })
	return name
}

// firstChangeOfOp drains a stream until it sees a change of the given
// kind, or the slot runs dry.
func firstChangeOfOp(t *testing.T, s *sqlStream, op pg.ChangeOp) pg.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		msg, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for a %s: %v", op, err)
		}
		if msg.Kind == pg.MessageChange && msg.Op == op {
			return msg
		}
	}
}

// --- Snapshot handoff -------------------------------------------------

// WithSnapshot's whole job is to read the database as it stood at an
// instant. pg_create_logical_replication_slot cannot export a snapshot
// — that belongs to the replication protocol — but pg_export_snapshot
// produces one of the same kind, so the statements WithSnapshot issues
// can be held to the property that matters: a row committed after the
// snapshot must be invisible inside it.
func TestPGWithSnapshotReadsAnInstant(t *testing.T) {
	db := openPG(t)
	ctx := context.Background()
	tbl := cdcTable(t, db, false)

	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (1, 'before')`, tbl.Name())); err != nil {
		t.Fatal(err)
	}

	// Hold a transaction open and export its snapshot. It has to stay
	// open for the name to remain importable, which is the same
	// constraint a replication connection imposes.
	exporter, err := db.Driver().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exporter.Rollback(context.Background()) }()

	if _, err := exporter.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ"); err != nil {
		t.Fatal(err)
	}
	rows, err := exporter.Query(ctx, "SELECT pg_export_snapshot()")
	if err != nil {
		t.Fatal(err)
	}
	var snapshot string
	if rows.Next() {
		_ = rows.Scan(&snapshot)
	}
	_ = rows.Close()
	if snapshot == "" {
		t.Fatal("pg_export_snapshot returned nothing")
	}

	// Something else commits after the instant.
	if _, err := db.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (2, 'after')`, tbl.Name())); err != nil {
		t.Fatal(err)
	}

	handle := pg.SlotHandle{Name: "unused", SnapshotName: snapshot}
	var seen []string
	err = pg.WithSnapshot(ctx, db, handle, func(snap *pg.DB) error {
		r, err := snap.Query(ctx, fmt.Sprintf(`SELECT title FROM %q ORDER BY id`, tbl.Name()))
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var title string
			if err := r.Scan(&title); err != nil {
				return err
			}
			seen = append(seen, title)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("WithSnapshot: %v", err)
	}
	if len(seen) != 1 || seen[0] != "before" {
		t.Errorf("the snapshot saw %v; a row committed after it must be invisible", seen)
	}

	// And the transaction it ran in was read-only, which is what
	// keeps a fill from writing into the source it is reading.
	err = pg.WithSnapshot(ctx, db, handle, func(snap *pg.DB) error {
		_, err := snap.Exec(ctx, fmt.Sprintf(`INSERT INTO %q VALUES (99, 'nope')`, tbl.Name()))
		return err
	})
	if !pg.IsReadOnly(err) {
		t.Errorf("a write inside the snapshot returned %v, want a read-only refusal (25006)", err)
	}
}

func TestPGWithSnapshotRejectsABadName(t *testing.T) {
	db := openPG(t)
	err := pg.WithSnapshot(context.Background(), db,
		pg.SlotHandle{Name: "s", SnapshotName: "'; DROP TABLE users; --"},
		func(*pg.DB) error {
			t.Error("the callback ran with an unusable snapshot name")
			return nil
		})
	if !errors.Is(err, pg.ErrInvalidIdentifier) {
		t.Errorf("got %v, want ErrInvalidIdentifier", err)
	}
}

// A driver with no replication support says so rather than pretending.
func TestPGStreamNotSupportedByTheSQLDriver(t *testing.T) {
	db := openPG(t)
	if _, err := pg.Stream(db, context.Background(), "any_slot", 0, nil); !errors.Is(err, pg.ErrStreamNotSupported) {
		t.Errorf("got %v, want ErrStreamNotSupported", err)
	}
}
