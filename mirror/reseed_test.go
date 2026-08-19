package mirror_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// The source table a reseed walks. Deliberately narrow — the mapping
// of exotic column types is clickhouse_test.go's problem; this file is
// about the walk, the versions and the cursor.
func reseedSourceTable() *pg.Table {
	t := pg.NewTable("docs")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("title").NotNull())
	pg.Add(t, pg.Integer("views").NotNull())
	return t
}

type reseedSrcRow struct {
	id    int64
	title string
	views int32
}

// reseedJob is one row of the persisted backfill state.
type reseedJob struct {
	lastID    int64
	processed int64
	done      bool
	lastError string
}

// reseedDriver fakes the three things a reseed touches: the source
// table, the state table the cursor lives in, and the outbox a repair
// emits into. Statements are recorded so the tests can assert on the
// SQL that was actually issued — a FOR SHARE that silently stopped
// being rendered is exactly the kind of regression that would not
// show up in the applied changes.
type reseedDriver struct {
	mu    sync.Mutex
	rows  []reseedSrcRow
	state map[string]reseedJob
	stmts []reseedStmt

	// vanish is a key that is deleted from the table the instant the
	// key scan has seen it, standing in for a row deleted at the
	// source while the reseed was running.
	vanish int64

	// keyAsText makes the row read hand the primary key back as a
	// string, which is what a driver the reseeder cannot work with
	// looks like from the inside.
	keyAsText bool
}

type reseedStmt struct {
	sql  string
	args []any
}

// A chunk issues two statements against the source table and they
// have to be told apart: the cursor scan selects the key alone, the
// row read selects every column. Both begin with the key, so the
// distinguishing token is what follows it.
const reseedKeyScanPrefix = `SELECT "docs"."id" FROM`

func newReseedDriver(ids ...int64) *reseedDriver {
	d := &reseedDriver{state: map[string]reseedJob{}}
	for _, id := range ids {
		d.rows = append(d.rows, reseedSrcRow{id: id, title: fmt.Sprintf("doc-%d", id), views: int32(id) * 10})
	}
	return d
}

func (d *reseedDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, reseedStmt{sql: sql, args: args})

	switch {
	case strings.Contains(sql, `"lastID"`):
		// The state table's name is settable, so the cursor row is
		// recognised by its columns rather than by the table it is in.
		job, ok := d.state[args[0].(string)]
		if !ok {
			return &reseedRows{}, nil
		}
		var completedAt, lastError any
		if job.done {
			completedAt = time.Unix(1700000000, 0).UTC()
		}
		if job.lastError != "" {
			lastError = job.lastError
		}
		return &reseedRows{data: [][]any{{
			args[0].(string), job.lastID, job.processed,
			completedAt, lastError, time.Unix(1700000000, 0).UTC(),
		}}}, nil

	case strings.HasPrefix(sql, reseedKeyScanPrefix):
		after := args[0].(int64)
		limit := int(args[len(args)-1].(int64))
		var data [][]any
		for _, r := range d.rows {
			if r.id > after && len(data) < limit {
				data = append(data, []any{r.id})
			}
		}
		if d.vanish != 0 {
			d.dropRow(d.vanish)
			d.vanish = 0
		}
		return &reseedRows{data: data}, nil

	case strings.Contains(sql, `FROM "docs"`):
		lo, hi := args[0].(int64), args[1].(int64)
		var data [][]any
		for _, r := range d.rows {
			if r.id >= lo && r.id <= hi {
				var key any = r.id
				if d.keyAsText {
					key = fmt.Sprint(r.id)
				}
				data = append(data, []any{key, r.title, r.views})
			}
		}
		return &reseedRows{data: data}, nil

	default:
		// Anything else — the replica-lag probe, say — is not what
		// these tests are about, and an empty result lets the caller
		// carry on.
		return &reseedRows{}, nil
	}
}

func (d *reseedDriver) Exec(_ context.Context, sql string, args ...any) (drops.Result, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stmts = append(d.stmts, reseedStmt{sql: sql, args: args})

	switch {
	case strings.Contains(sql, `"lastID"`) && strings.Contains(sql, "ON CONFLICT"):
		name := args[0].(string)
		job := d.state[name]
		job.lastID = args[1].(int64)
		job.processed = args[2].(int64)
		switch {
		case strings.Contains(sql, `"completedAt"`):
			job.done = true
			job.lastError = ""
		case len(args) >= 4:
			job.lastError = args[3].(string)
		default:
			job.lastError = ""
		}
		d.state[name] = job
	case strings.HasPrefix(strings.TrimSpace(sql), "DELETE"):
		delete(d.state, args[0].(string))
	}
	return recResult{}, nil
}

func (d *reseedDriver) Begin(context.Context) (drops.Tx, error) { return reseedTx{d}, nil }

// dropRow removes a key from the fake table. Called with the lock held.
func (d *reseedDriver) dropRow(id int64) {
	out := d.rows[:0]
	for _, r := range d.rows {
		if r.id != id {
			out = append(out, r)
		}
	}
	d.rows = out
}

// matching returns the recorded statements whose SQL contains sub.
func (d *reseedDriver) matching(sub string) []reseedStmt {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []reseedStmt
	for _, s := range d.stmts {
		if strings.Contains(s.sql, sub) {
			out = append(out, s)
		}
	}
	return out
}

func (d *reseedDriver) keyScans() []reseedStmt {
	var out []reseedStmt
	for _, s := range d.matching(`FROM "docs"`) {
		if strings.HasPrefix(s.sql, reseedKeyScanPrefix) {
			out = append(out, s)
		}
	}
	return out
}

func (d *reseedDriver) rowReads() []reseedStmt {
	var out []reseedStmt
	for _, s := range d.matching(`FROM "docs"`) {
		if !strings.HasPrefix(s.sql, reseedKeyScanPrefix) {
			out = append(out, s)
		}
	}
	return out
}

func (d *reseedDriver) outboxInserts() []reseedStmt { return d.matching(`INSERT INTO "outbox"`) }

type reseedTx struct{ d *reseedDriver }

func (tx reseedTx) Exec(ctx context.Context, sql string, args ...any) (drops.Result, error) {
	return tx.d.Exec(ctx, sql, args...)
}
func (tx reseedTx) Query(ctx context.Context, sql string, args ...any) (drops.Rows, error) {
	return tx.d.Query(ctx, sql, args...)
}
func (tx reseedTx) Begin(ctx context.Context) (drops.Tx, error) { return tx.d.Begin(ctx) }
func (reseedTx) Commit(context.Context) error                   { return nil }
func (reseedTx) Rollback(context.Context) error                 { return nil }

type reseedRows struct {
	data [][]any
	pos  int
}

func (r *reseedRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}

func (r *reseedRows) Scan(dest ...any) error {
	row := r.data[r.pos-1]
	for i, d := range dest {
		if i >= len(row) {
			break
		}
		v := reflect.ValueOf(row[i])
		if !v.IsValid() {
			// SQL NULL: leave the destination at its zero value.
			continue
		}
		reflect.ValueOf(d).Elem().Set(v)
	}
	return nil
}

func (r *reseedRows) Columns() ([]string, error) { return nil, nil }
func (r *reseedRows) Close() error               { return nil }
func (r *reseedRows) Err() error                 { return nil }

// reseedChangeKeys flattens the keys a sink saw, batch by batch.
func reseedChangeKeys(batches [][]mirror.Change) []string {
	var out []string
	for _, b := range batches {
		for _, ch := range b {
			out = append(out, ch.Key)
		}
	}
	return out
}

func newFill(t *testing.T, d *reseedDriver, sinks ...mirror.Sink) *mirror.Reseeder {
	t.Helper()
	r, err := mirror.NewFillReseeder(pg.New(d), reseedSourceTable(), sinks...)
	if err != nil {
		t.Fatalf("NewFillReseeder: %v", err)
	}
	return r.ChunkSize(2).Throttle(0)
}

// --- fill mode -------------------------------------------------------

func TestFillReseedSeedsEveryRowInKeyOrder(t *testing.T) {
	d := newReseedDriver(1, 2, 3)
	sink := &recSink{name: "clickhouse:docs"}
	var lastKeys []int64
	r := newFill(t, d, sink).OnProgress(func(_, lastKey int64) { lastKeys = append(lastKeys, lastKey) })

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := reseedChangeKeys(sink.seen); !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
		t.Errorf("seeded keys = %v, want 1,2,3 in order", got)
	}
	if len(sink.seen) != 2 {
		t.Errorf("chunk size 2 over 3 rows should be 2 batches, got %d", len(sink.seen))
	}
	if !reflect.DeepEqual(lastKeys, []int64{2, 3}) {
		t.Errorf("progress cursor = %v, want 2 then 3", lastKeys)
	}
	first := sink.seen[0][0]
	if first.Op != mirror.OpInsert {
		t.Errorf("op = %q, want %q — a sink upserts, so a seed is an insert", first.Op, mirror.OpInsert)
	}
	// A seed states the whole row. A partial one would leave the
	// mirror holding a mixture of the seed and an older change.
	want := map[string]any{"id": int64(1), "title": "doc-1", "views": int32(10)}
	if !reflect.DeepEqual(first.Row, want) {
		t.Errorf("row = %v, want %v", first.Row, want)
	}
	if first.At.IsZero() {
		t.Error("seeded change has no timestamp")
	}
}

func TestFillReseedStampsTheFloorVersion(t *testing.T) {
	d := newReseedDriver(1, 2)
	sink := &recSink{name: "sink"}
	if err := newFill(t, d, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, b := range sink.seen {
		for _, ch := range b {
			if ch.Version != mirror.SeedVersion {
				t.Errorf("key %s seeded at version %d, want SeedVersion (%d)", ch.Key, ch.Version, mirror.SeedVersion)
			}
		}
	}
}

// The ordering argument SeedVersion rests on: it must sit below every
// version a live change can carry, and it must survive a trip through
// Normalized — a zero would be rewritten into a clock reading and
// would then outrank the stream it is supposed to lose to.
func TestSeedVersionLosesToEveryLiveChange(t *testing.T) {
	if mirror.SeedVersion == 0 {
		t.Fatal("SeedVersion must be non-zero: Normalized treats 0 as unset and replaces it")
	}
	live := mirror.Change{Op: mirror.OpUpdate, Key: "1"}.Normalized(time.Now())
	if live.Version <= mirror.SeedVersion {
		t.Errorf("a clock-stamped live change is version %d, which does not beat the seed floor %d", live.Version, mirror.SeedVersion)
	}
	// The outbox id space starts at 1, so anything past the very first
	// event outranks a seed too.
	if mirror.SeedVersion > 1 {
		t.Errorf("SeedVersion is %d, which outranks outbox event ids below it", mirror.SeedVersion)
	}
	seeded := mirror.Change{Op: mirror.OpInsert, Key: "1", Version: mirror.SeedVersion}.Normalized(time.Now())
	if seeded.Version != mirror.SeedVersion {
		t.Errorf("normalizing a seeded change moved it to %d", seeded.Version)
	}
}

// A fill must not take row locks: it overwrites nothing, so blocking a
// writer for the length of a chunk would be pure cost.
func TestFillReseedReadsWithoutLockingRows(t *testing.T) {
	d := newReseedDriver(1, 2)
	if err := newFill(t, d, &recSink{name: "sink"}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, s := range d.rowReads() {
		if strings.Contains(s.sql, "FOR SHARE") || strings.Contains(s.sql, "FOR UPDATE") {
			t.Errorf("fill mode locked the source rows: %s", s.sql)
		}
	}
}

func TestReseedWalkIsCursoredNotOffset(t *testing.T) {
	d := newReseedDriver(1, 2, 3)
	if err := newFill(t, d, &recSink{name: "sink"}).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	scans := d.keyScans()
	if len(scans) == 0 {
		t.Fatal("no key scan was issued")
	}
	sql := scans[0].sql
	for _, want := range []string{`"docs"."id" > $1`, `ORDER BY "docs"."id"`, "LIMIT $2"} {
		if !strings.Contains(sql, want) {
			t.Errorf("key scan missing %s\ngot %s", want, sql)
		}
	}
	if strings.Contains(sql, "OFFSET") {
		t.Errorf("the walk must resume from a key, not an offset: %s", sql)
	}
	// Chunk two starts where chunk one stopped.
	if len(scans) < 2 || scans[1].args[0] != int64(2) {
		t.Errorf("second scan resumed from %v, want 2", scans[1].args[0])
	}
	reads := d.rowReads()
	if len(reads) == 0 {
		t.Fatal("no row read was issued")
	}
	for _, want := range []string{`"docs"."id" >= $1`, `"docs"."id" <= $2`, `ORDER BY "docs"."id"`} {
		if !strings.Contains(reads[0].sql, want) {
			t.Errorf("row read missing %s\ngot %s", want, reads[0].sql)
		}
	}
}

func TestReseedResumesFromThePersistedCursor(t *testing.T) {
	d := newReseedDriver(1, 2, 3, 4)
	d.state["mirror-reseed:docs:fill"] = reseedJob{lastID: 2, processed: 2}
	sink := &recSink{name: "sink"}
	if err := newFill(t, d, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := reseedChangeKeys(sink.seen); !reflect.DeepEqual(got, []string{"3", "4"}) {
		t.Errorf("resumed run seeded %v, want only the keys past the cursor", got)
	}
}

// A chunk the sinks refused must be replayed, not skipped: the cursor
// stays where it was and the next run re-seeds the same keys with the
// same versions, which is the idempotence every sink already owes the
// pump.
func TestReseedFailedChunkDoesNotAdvanceTheCursor(t *testing.T) {
	d := newReseedDriver(1, 2, 3)
	sink := &recSink{name: "sink", failFor: 1}
	r := newFill(t, d, sink)

	if err := r.Run(context.Background()); err == nil {
		t.Fatal("expected the sink failure to surface")
	}
	status, err := r.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.LastID != 0 {
		t.Errorf("cursor advanced to %d over a chunk that was never applied", status.LastID)
	}
	if status.LastError == "" {
		t.Error("the failure should be recorded on the job")
	}
	if len(sink.seen) != 0 {
		t.Fatalf("the failing sink recorded %d batches", len(sink.seen))
	}

	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if got := reseedChangeKeys(sink.seen); !reflect.DeepEqual(got, []string{"1", "2", "3"}) {
		t.Errorf("replay seeded %v, want the whole table including the failed chunk", got)
	}
}

func TestReseedStopsAtTheFirstFailingSink(t *testing.T) {
	d := newReseedDriver(1)
	bad := &recSink{name: "bad", failFor: 1}
	later := &recSink{name: "later"}
	if err := newFill(t, d, bad, later).Run(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if len(later.seen) != 0 {
		t.Error("a sink after the failing one must not see a chunk that is going to be replayed")
	}
}

func TestReseedNamesTheFailingSink(t *testing.T) {
	d := newReseedDriver(1)
	err := newFill(t, d, &recSink{name: "qdrant:docs", failFor: 1}).Run(context.Background())
	if err == nil || !contains(err.Error(), "qdrant:docs") {
		t.Errorf("error should name the sink, got %v", err)
	}
}

// Keys are read before rows, so a row can be deleted in between. It is
// simply not seeded — its delete is in the change stream, and the
// reseed has nothing to say about it.
func TestReseedToleratesRowsDeletedMidWalk(t *testing.T) {
	d := newReseedDriver(1, 2)
	d.vanish = 1
	sink := &recSink{name: "sink"}
	if err := newFill(t, d, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := reseedChangeKeys(sink.seen); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("seeded %v, want only the row that still existed", got)
	}
}

func TestReseedOfAnEmptyTableTouchesNoSink(t *testing.T) {
	d := newReseedDriver()
	sink := &recSink{name: "sink"}
	if err := newFill(t, d, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.seen) != 0 {
		t.Errorf("empty table produced %d batches", len(sink.seen))
	}
	if len(d.rowReads()) != 0 {
		t.Errorf("empty table produced %d row reads", len(d.rowReads()))
	}
}

// A finished job stays finished, so a scheduled reseed does not walk
// the table forever. Reset is the only way back.
func TestReseedRunsAgainOnlyAfterReset(t *testing.T) {
	d := newReseedDriver(1, 2)
	sink := &recSink{name: "sink"}
	r := newFill(t, d, sink)
	ctx := context.Background()

	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(sink.seen) != 1 {
		t.Errorf("a completed reseed re-walked the table: %d batches", len(sink.seen))
	}
	if err := r.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if err := r.Run(ctx); err != nil {
		t.Fatalf("Run after Reset: %v", err)
	}
	if len(sink.seen) != 2 {
		t.Errorf("Reset should have let the walk run again: %d batches", len(sink.seen))
	}
}

// --- repair mode -----------------------------------------------------

func newRepair(t *testing.T, d *reseedDriver) *mirror.Reseeder {
	t.Helper()
	r, err := mirror.NewRepairReseeder(pg.New(d), reseedSourceTable(), pg.NewOutbox(pg.New(d), "outbox"))
	if err != nil {
		t.Fatalf("NewRepairReseeder: %v", err)
	}
	return r.ChunkSize(2).Throttle(0)
}

func TestRepairReseedEmitsThroughTheOutbox(t *testing.T) {
	d := newReseedDriver(1, 2)
	if err := newRepair(t, d).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	emitted := d.outboxInserts()
	if len(emitted) != 2 {
		t.Fatalf("emitted %d events, want one per row", len(emitted))
	}
	if kind := emitted[0].args[0].(string); kind != mirror.EventKind {
		t.Errorf("event kind = %q, want %q so OutboxSource picks it up", kind, mirror.EventKind)
	}
	if agg := emitted[0].args[2]; agg != "1" {
		t.Errorf("aggregate id = %v, want the row key so per-key ordering holds", agg)
	}
	var ch mirror.Change
	if err := json.Unmarshal(emitted[0].args[3].(json.RawMessage), &ch); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if ch.Op != mirror.OpInsert || ch.Key != "1" {
		t.Errorf("payload = %+v, want an insert of key 1", ch)
	}
	if ch.Row["title"] != "doc-1" {
		t.Errorf("payload row = %v, want the whole row", ch.Row)
	}
	// The repair takes whatever version the emitter stamps on live
	// traffic. Stamping the seed floor here would sort the repair
	// below the stale rows it exists to overwrite.
	if ch.Version == mirror.SeedVersion {
		t.Error("a repair must not carry the fill floor version")
	}
}

// Without the lock a writer can update the row and emit its change
// before the seed reads the pre-image and emits after it — the stale
// value would then win and stay.
func TestRepairReseedLocksTheRowsItReads(t *testing.T) {
	d := newReseedDriver(1, 2)
	if err := newRepair(t, d).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reads := d.rowReads()
	if len(reads) == 0 {
		t.Fatal("no row read was issued")
	}
	for _, s := range reads {
		if !strings.Contains(s.sql, "FOR SHARE") {
			t.Errorf("repair read without FOR SHARE: %s", s.sql)
		}
		if strings.Contains(s.sql, "FOR UPDATE") {
			t.Errorf("FOR UPDATE blocks writers for the whole chunk; FOR SHARE is enough: %s", s.sql)
		}
	}
}

// The two modes finish at different times and mean different things,
// so they must not share a resume cursor.
func TestFillAndRepairDoNotShareACursor(t *testing.T) {
	d := newReseedDriver(1)
	fill := newFill(t, d, &recSink{name: "sink"})
	repair := newRepair(t, d)
	if fill.Name() == repair.Name() {
		t.Fatalf("both modes resume under %q", fill.Name())
	}
	for _, r := range []*mirror.Reseeder{fill, repair} {
		if !strings.Contains(r.Name(), "docs") {
			t.Errorf("job name %q should name the table it walks", r.Name())
		}
	}
	if got := fill.Named("custom").Name(); got != "custom" {
		t.Errorf("Named = %q", got)
	}
}

// --- operational knobs ------------------------------------------------

func TestReseedStateTableIsSettable(t *testing.T) {
	d := newReseedDriver(1, 2)
	r := newFill(t, d, &recSink{name: "sink"}).StateTable("mirrorJobs")
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(d.matching(`"mirrorJobs"`)) == 0 {
		t.Error("the cursor was not persisted to the named state table")
	}
	if len(d.matching(`"backfillJobs"`)) != 0 {
		t.Error("the default state table was used despite StateTable")
	}
}

// The gate itself belongs to pg.Backfill; what this pins is that
// installing it leaves the walk alone when the probe reports nothing.
func TestReseedWalksWithALagGateInstalled(t *testing.T) {
	d := newReseedDriver(1, 2)
	sink := &recSink{name: "sink"}
	r := newFill(t, d, sink).PauseIfLag(pg.NewReplicated(d), 1<<20)
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := reseedChangeKeys(sink.seen); !reflect.DeepEqual(got, []string{"1", "2"}) {
		t.Errorf("seeded %v, want the whole table", got)
	}
}

// --- scoping and validation ------------------------------------------

func TestReseedWhereNarrowsBothStatements(t *testing.T) {
	src := reseedSourceTable()
	d := newReseedDriver(1, 2)
	r, err := mirror.NewFillReseeder(pg.New(d), src, &recSink{name: "sink"})
	if err != nil {
		t.Fatalf("NewFillReseeder: %v", err)
	}
	r.ChunkSize(2).Throttle(0).Where(pg.IsNull(src.Col("title")))
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both halves of a chunk have to agree on what the mirror holds:
	// keys the scan yields but the read filters out would be counted
	// as seeded and never sent.
	for _, s := range append(d.keyScans(), d.rowReads()...) {
		if !strings.Contains(s.sql, `"docs"."title" IS NULL`) {
			t.Errorf("predicate missing from %s", s.sql)
		}
	}
}

// A key the reseeder cannot render as text is a stop, not a guess: a
// key formatted from something it does not understand would address
// the wrong mirrored row.
func TestReseedRefusesAKeyItCannotRender(t *testing.T) {
	d := newReseedDriver(1)
	d.keyAsText = true
	sink := &recSink{name: "sink"}
	err := newFill(t, d, sink).Run(context.Background())
	if err == nil {
		t.Fatal("expected the walk to stop")
	}
	if !contains(err.Error(), `"id"`) || !contains(err.Error(), "string") {
		t.Errorf("error should name the column and what it scanned, got %v", err)
	}
	if len(sink.seen) != 0 {
		t.Errorf("nothing should have been seeded, got %d batches", len(sink.seen))
	}
}

func TestReseedRejectsUnwalkableSources(t *testing.T) {
	db := pg.New(newReseedDriver())
	sink := &recSink{name: "sink"}

	t.Run("no primary key", func(t *testing.T) {
		tbl := pg.NewTable("nokey")
		pg.Add(tbl, pg.Text("a"))
		if _, err := mirror.NewFillReseeder(db, tbl, sink); err == nil {
			t.Error("expected an error: there is no order to walk the table in")
		}
	})

	t.Run("composite primary key", func(t *testing.T) {
		tbl := pg.NewTable("composite")
		pg.Add(tbl, pg.BigInt("a").PrimaryKey())
		pg.Add(tbl, pg.BigInt("b").PrimaryKey())
		if _, err := mirror.NewFillReseeder(db, tbl, sink); err == nil {
			t.Error("expected an error for a composite key")
		}
	})

	// The resume cursor is persisted as a bigint, so a uuid or text
	// key cannot be walked. The error has to say that rather than
	// leaving a caller to discover it at run time.
	t.Run("non-integer primary key", func(t *testing.T) {
		tbl := pg.NewTable("uuidkeyed")
		pg.Add(tbl, pg.UUID("id").PrimaryKey())
		_, err := mirror.NewFillReseeder(db, tbl, sink)
		if err == nil {
			t.Fatal("expected an error for a uuid key")
		}
		if !contains(err.Error(), "uuid") || !contains(err.Error(), "id") {
			t.Errorf("error should name the column and its type, got %v", err)
		}
	})

	t.Run("no database", func(t *testing.T) {
		if _, err := mirror.NewFillReseeder(nil, reseedSourceTable(), sink); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("no source table", func(t *testing.T) {
		if _, err := mirror.NewFillReseeder(db, nil, sink); err == nil {
			t.Error("expected an error")
		}
	})

	t.Run("no sink", func(t *testing.T) {
		if _, err := mirror.NewFillReseeder(db, reseedSourceTable()); err == nil {
			t.Error("expected an error: a fill writes to sinks and there are none")
		}
	})

	t.Run("nil sink", func(t *testing.T) {
		if _, err := mirror.NewFillReseeder(db, reseedSourceTable(), nil); err == nil {
			t.Error("expected an error for a nil sink")
		}
	})

	t.Run("no outbox", func(t *testing.T) {
		if _, err := mirror.NewRepairReseeder(db, reseedSourceTable(), nil); err == nil {
			t.Error("expected an error: a repair is delivered through the outbox")
		}
	})
}
