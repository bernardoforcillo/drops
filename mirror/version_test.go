package mirror_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// The table the version tests mirror. Two columns: this file is about
// which number a row is written at, not about type mapping.
func versionSourceTable() *pg.Table {
	t := pg.NewTable("docs")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("title").NotNull())
	return t
}

// verBound decodes the arguments one ClickHouseSink batch bound into
// one map per row, keyed by column name, so an assertion about the
// version a change was written at does not depend on column order.
func verBound(t *testing.T, tbl *clickhouse.Table, args []any) []map[string]any {
	t.Helper()
	cols := tbl.Columns()
	if len(args)%len(cols) != 0 {
		t.Fatalf("%d arguments do not divide into rows of %d columns", len(args), len(cols))
	}
	var out []map[string]any
	for i := 0; i < len(args); i += len(cols) {
		row := make(map[string]any, len(cols))
		for j, c := range cols {
			row[c.Name()] = args[i+j]
		}
		out = append(out, row)
	}
	return out
}

// verEmitted turns the nth outbox INSERT the fake driver recorded back
// into the row a drain would scan, under the event id the sequence
// would have given it. It carries the payload EmitChange actually
// wrote — which is the point: what is asserted downstream is what the
// emitter really stored, not a Change re-marshalled by the test.
func verEmitted(t *testing.T, d *outboxDriver, n int, id int64) []any {
	t.Helper()
	if n >= len(d.args) {
		t.Fatalf("only %d events were emitted, wanted #%d", len(d.args), n)
	}
	payload, ok := d.args[n][3].(json.RawMessage)
	if !ok {
		t.Fatalf("event %d payload is %T, want json.RawMessage", n, d.args[n][3])
	}
	return []any{
		id, mirror.EventKind, any(nil), any(nil), payload,
		[]byte(nil), 0, any(nil), time.Unix(1700000000, 0).UTC(),
	}
}

// verDrain replays prepared outbox rows through an OutboxSource.
func verDrain(t *testing.T, rows ...[]any) []mirror.Change {
	t.Helper()
	d := &outboxDriver{rows: func() drops.Rows { return &outboxRows{data: rows} }}
	src, err := mirror.NewOutboxSource(pg.NewOutbox(pg.New(d), "outbox"))
	if err != nil {
		t.Fatal(err)
	}
	changes, _, err := src.Fetch(context.Background(), 100)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	return changes
}

// The shape of the whole design in one test: three ways to produce a
// version, three bands, and an order between them that no input can
// invert.
func TestVersionBandsAreOrdered(t *testing.T) {
	now := time.Now()
	seed := mirror.SeedVersionAt(now)
	clock := mirror.ClockVersion(now)
	live := mirror.LiveVersion(1)

	if seed == 0 || seed > mirror.MaxSeedVersion {
		t.Errorf("seed version %d is outside [1, %d]", seed, mirror.MaxSeedVersion)
	}
	if clock <= mirror.MaxSeedVersion {
		t.Errorf("clock version %d is inside the seed band, so a fill could overwrite live data", clock)
	}
	if clock >= mirror.LiveVersionBase {
		t.Errorf("clock version %d reaches the live band, so a wall clock could outrank a sequenced change", clock)
	}
	if live < mirror.LiveVersionBase {
		t.Errorf("live version %d is below the base", live)
	}
	if !(seed < clock && clock < live) {
		t.Errorf("bands out of order: seed=%d clock=%d live=%d", seed, clock, live)
	}

	// The lowest sequence number there is — the first event ever
	// written to an outbox — still outranks a seed taken far in the
	// future. This is the property the offset exists for: raw ids
	// start at 1 and leave nothing underneath.
	if mirror.LiveVersion(1) <= mirror.SeedVersionAt(now.AddDate(100, 0, 0)) {
		t.Error("the first outbox event does not outrank a seed, so a fill can overwrite live data")
	}

	// A row already in a mirror from before the bands existed carries
	// a raw UnixNano stamp, written by a release that did not clamp
	// it. Every one of them must lose to a sequenced change — else an
	// upgrade freezes the mirror on its stale rows — and must beat a
	// seed, else a fill would overwrite live data on the way in.
	for _, legacy := range []time.Time{
		time.Date(1971, 1, 1, 0, 0, 0, 0, time.UTC), now, now.AddDate(200, 0, 0),
	} {
		v := uint64(legacy.UnixNano())
		if v >= mirror.LiveVersionBase {
			t.Errorf("legacy stamp %d for %s reaches the live band", v, legacy)
		}
		if v <= mirror.MaxSeedVersion {
			t.Errorf("legacy stamp %d for %s is inside the seed band", v, legacy)
		}
	}
	// The bound the package doc states for those old rows: a reading
	// is above the seed band once it is more than about five hours
	// past the epoch, which every real change time is.
	if uint64(time.Unix(6*3600, 0).UnixNano()) <= mirror.MaxSeedVersion {
		t.Error("the stated bound for pre-band rows is wrong: five hours past the epoch is not above the seed band")
	}

	// A seed has to survive the trip a change makes through the pump:
	// Normalized rewrites a zero version into a clock reading, which
	// would lift the seed clean out of its band and above the rows it
	// exists to lose to.
	if got := (mirror.Change{Op: mirror.OpInsert, Key: "1", Version: seed}).Normalized(now); got.Version != seed {
		t.Errorf("normalizing a seeded change moved it from %d to %d", seed, got.Version)
	}

	// The live band is exactly as large as the sequence it carries,
	// so no id can wrap into another change's version.
	if got := mirror.LiveVersion(1<<63 - 1); got != 1<<64-1 {
		t.Errorf("LiveVersion(maxint64) = %d, want the top of the space", got)
	}
	if mirror.LiveVersion(0) < mirror.LiveVersionBase || mirror.LiveVersion(-1) < mirror.LiveVersionBase {
		t.Error("a nonsensical sequence number must clamp to the band, not wrap out of it")
	}
}

// uint64(negative nanoseconds) is a number near the top of the live
// band. A change dated before the epoch — a zero time.Time is the
// common way to get one — would then outrank everything the source
// ever emitted.
func TestClockVersionCannotWrapIntoTheLiveBand(t *testing.T) {
	for _, at := range []time.Time{{}, time.Unix(0, 0), time.Unix(-1, 0), time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if v := mirror.ClockVersion(at); v >= mirror.LiveVersionBase || v <= mirror.MaxSeedVersion {
			t.Errorf("ClockVersion(%s) = %d, which is outside the clock band", at, v)
		}
	}
	// And the fallback the pump applies is that same function, so a
	// change carrying such a time cannot be normalized into the live
	// band either.
	got := mirror.Change{Op: mirror.OpUpdate, Key: "1", At: time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)}.Normalized(time.Now())
	if got.Version >= mirror.LiveVersionBase {
		t.Errorf("Normalized produced %d, which outranks every sequenced change", got.Version)
	}
}

// The property every other fix in this package leans on: a row a fill
// wrote loses to a change that travelled the stream, whichever
// arrives first and however small the outbox is.
func TestALiveChangeOutranksEverySeed(t *testing.T) {
	d := newReseedDriver(1, 2)
	sink := &recSink{name: "clickhouse:docs"}
	if err := newFill(t, d, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.seen) == 0 {
		t.Fatal("nothing was seeded")
	}

	// The weakest live change there is: the very first event in the
	// outbox, whose id is 1.
	live := verDrain(t, verEmitted(t, verEmit(t, mirror.Change{Op: mirror.OpUpdate, Key: "1"}), 0, 1))
	if len(live) != 1 {
		t.Fatalf("drained %d changes, want 1", len(live))
	}
	for _, b := range sink.seen {
		for _, ch := range b {
			if ch.Version >= live[0].Version {
				t.Errorf("seeded key %s at %d, which is not below the first outbox event's %d — a fill would overwrite live data",
					ch.Key, ch.Version, live[0].Version)
			}
		}
	}
}

// verEmit writes one change through EmitChange and hands back the
// driver that recorded it.
func verEmit(t *testing.T, changes ...mirror.Change) *outboxDriver {
	t.Helper()
	d := &outboxDriver{}
	ob := pg.NewOutbox(pg.New(d), "outbox")
	for _, ch := range changes {
		if err := mirror.EmitChange(ob, pg.New(d), context.Background(), ch); err != nil {
			t.Fatalf("EmitChange %s: %v", ch.Key, err)
		}
	}
	return d
}

// Two passes must be ordered against each other, or the second one
// writes different content at the version the first one used and
// ReplacingMergeTree resolves the tie by merge state rather than by
// the data — at which point a FINAL read has stopped being a function
// of the key.
func TestTwoFillPassesAreOrdered(t *testing.T) {
	d := newReseedDriver(1, 2)
	sink := &recSink{name: "sink"}
	r := newFill(t, d, sink)
	ctx := context.Background()

	if err := r.Run(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	first := sink.seen[0][0].Version

	// The documented way to run a second pass over the same table.
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// The row changed in between, through a path that emitted
	// nothing — which is the reason a table needs a fill at all.
	d.rows[0].title = "changed by a path that never emitted"
	if err := r.Run(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if len(sink.seen) < 2 {
		t.Fatalf("expected a second pass, got %d batches", len(sink.seen))
	}
	second := sink.seen[len(sink.seen)-1][0].Version

	if second <= first {
		t.Errorf("second pass seeded at %d, first at %d: the later read must outrank the earlier one", second, first)
	}
	if second > mirror.MaxSeedVersion {
		t.Errorf("second pass left the seed band at %d, so it can outrank live changes", second)
	}

	// A third reseeder — a fresh process, the same table — must not
	// reuse a version an earlier pass already spent.
	other := newFill(t, newReseedDriver(1, 2), sink)
	if err := other.Run(ctx); err != nil {
		t.Fatalf("third pass: %v", err)
	}
	if third := sink.seen[len(sink.seen)-1][0].Version; third <= second {
		t.Errorf("third pass seeded at %d, second at %d", third, second)
	}
}

// A row deleted and re-created has to come back. The two mutations
// commit a millisecond apart; the hosts that run them have clocks a
// few milliseconds out of step, and the one that re-inserts is the
// one that is behind. Ordered by those clocks the re-insert looks
// older than the delete and the tombstone wins for good — the row is
// gone from analytics and from search with nothing to report it.
// Ordered by the outbox id, commit order is commit order.
func TestDeleteThenReinsertUndeletesTheMirroredRow(t *testing.T) {
	ctx := context.Background()
	deletedAt := time.Now()
	emitter := verEmit(t,
		mirror.Change{Op: mirror.OpDelete, Key: "7", At: deletedAt},
		mirror.Change{Op: mirror.OpInsert, Key: "7", At: deletedAt.Add(-5 * time.Millisecond),
			Row: map[string]any{"id": int64(7), "title": "back"}},
	)

	changes := verDrain(t,
		verEmitted(t, emitter, 0, 41),
		verEmitted(t, emitter, 1, 42),
	)
	if len(changes) != 2 {
		t.Fatalf("drained %d changes, want 2", len(changes))
	}
	if !changes[0].IsDelete() || changes[1].Op != mirror.OpInsert {
		t.Fatalf("drained %v, want the delete then the re-insert", changes)
	}
	if changes[1].Version <= changes[0].Version {
		t.Fatalf("the re-insert is version %d and the delete %d: the row stays deleted forever",
			changes[1].Version, changes[0].Version)
	}

	// And what ClickHouse is told: the tombstone, then a row at a
	// higher version with the marker clear, which is what makes a
	// FINAL read return the document again.
	tbl := derive(t, versionSourceTable())
	chd := &recDriver{}
	sink, err := mirror.NewClickHouseSink(clickhouse.New(chd), tbl)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Apply(ctx, changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rows := verBound(t, tbl, chd.args[0])
	if len(rows) != 2 {
		t.Fatalf("bound %d rows, want 2", len(rows))
	}
	if rows[0][mirror.DeletedColumn] != uint8(1) || rows[1][mirror.DeletedColumn] != uint8(0) {
		t.Errorf("markers = %v, %v; want the tombstone then a live row",
			rows[0][mirror.DeletedColumn], rows[1][mirror.DeletedColumn])
	}
	v0 := rows[0][mirror.VersionColumn].(uint64)
	v1 := rows[1][mirror.VersionColumn].(uint64)
	if v1 <= v0 {
		t.Errorf("re-insert written at %d, tombstone at %d: ReplacingMergeTree keeps the tombstone", v1, v0)
	}
}

// A fill writes outside the ordered stream and relies on the sink to
// drop what it must. A sink that cannot do that is refused where the
// operator is watching, not at 3am.
func TestFillReseederRefusesAVersionBlindSink(t *testing.T) {
	db := pg.New(newReseedDriver(1))
	src := reseedSourceTable()

	cli, _ := qdrantStub(t)
	qs, err := mirror.NewQdrantSink(cli, "docs", constEmbedder(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}

	blind := []mirror.Sink{
		qs,
		mirror.SinkFunc{SinkName: "webhook", Fn: func(context.Context, []mirror.Change) error { return nil }},
	}
	for _, s := range blind {
		_, err := mirror.NewFillReseeder(db, src, s)
		if err == nil {
			t.Fatalf("a fill through %s was accepted; it ignores the version the fill's safety rests on", s.Name())
		}
		if !strings.Contains(err.Error(), s.Name()) {
			t.Errorf("error should name the sink, got %v", err)
		}
		if !strings.Contains(err.Error(), "NewRepairReseeder") {
			t.Errorf("error should point at the mode that can seed this sink, got %v", err)
		}
	}

	// A version-blind sink hidden behind a version-aware one is still
	// a version-blind sink.
	if _, err := mirror.NewFillReseeder(db, src, &recSink{name: "clickhouse:docs"}, qs); err == nil {
		t.Error("a fill with one blind sink among several was accepted")
	}

	// And the sinks that do compare are taken.
	chSink, _, _ := newSinkFor(t, versionSourceTable())
	if _, err := mirror.NewFillReseeder(db, src, chSink); err != nil {
		t.Errorf("ClickHouseSink refused: %v", err)
	}
	versioned := mirror.SinkFunc{
		SinkName: "mine",
		Fn:       func(context.Context, []mirror.Change) error { return nil },
		// Declared, because this one really does drop a change whose
		// version is below the stored one.
		Versioned: true,
	}
	if _, err := mirror.NewFillReseeder(db, src, versioned); err != nil {
		t.Errorf("a sink that declares itself version-aware was refused: %v", err)
	}
}

// newSinkFor builds a ClickHouseSink over the mirror of src.
func newSinkFor(t *testing.T, src *pg.Table) (*mirror.ClickHouseSink, *clickhouse.Table, *recDriver) {
	t.Helper()
	tbl := derive(t, src)
	d := &recDriver{}
	s, err := mirror.NewClickHouseSink(clickhouse.New(d), tbl)
	if err != nil {
		t.Fatalf("NewClickHouseSink: %v", err)
	}
	return s, tbl, d
}

// The two sinks this package ships answer the question honestly:
// ClickHouse compares versions in the engine, Qdrant has nothing to
// compare with.
func TestShippedSinksDeclareWhetherTheyCompareVersions(t *testing.T) {
	chSink, _, _ := newSinkFor(t, versionSourceTable())
	if !chSink.VersionAware() {
		t.Error("ClickHouseSink should be version-aware: ReplacingMergeTree keeps the highest version")
	}
	cli, _ := qdrantStub(t)
	qs, err := mirror.NewQdrantSink(cli, "docs", constEmbedder(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if qs.VersionAware() {
		t.Error("QdrantSink claims to compare versions, but Qdrant's upsert takes no condition")
	}
	var _ mirror.VersionAwareSink = chSink
	var _ mirror.VersionAwareSink = qs
}
