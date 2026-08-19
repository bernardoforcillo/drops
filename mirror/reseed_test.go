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

	// pred and excluded together model a WHERE the server actually
	// applies: a statement whose SQL carries pred does not see the
	// key excluded, and a statement that lost the predicate does.
	// Asserting on the rendered text alone cannot tell those two
	// apart, and the difference is a row seeded into a mirror the
	// caller scoped it out of.
	pred     string
	excluded int64

	// failOutboxNth refuses that emit, counting from one. It is what
	// lets a repair fail in the middle of the walk rather than at its
	// first statement.
	failOutboxNth int
	outboxCalls   int
}

// hides reports whether stmt is subject to the driver's predicate, in
// which case the excluded key is not in its result.
func (d *reseedDriver) hides(sql string) bool {
	return d.pred != "" && strings.Contains(sql, d.pred)
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
			if r.id == d.excluded && d.hides(sql) {
				continue
			}
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
			if r.id == d.excluded && d.hides(sql) {
				continue
			}
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
	case strings.Contains(sql, `INSERT INTO "outbox"`):
		d.outboxCalls++
		if d.outboxCalls == d.failOutboxNth {
			// Recorded above before failing: the statement was
			// issued, and a test that counts emits should see the
			// attempt as well as where the walk stopped.
			return nil, fmt.Errorf("outbox insert %d refused", d.outboxCalls)
		}
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

// chunkSink refuses one nominated batch and records the rest.
//
// recSink (pump_test.go) refuses the first n batches, which cannot
// express "the sink took chunk one and refused chunk two" — and that
// is the ordering that decides whether a committed chunk's cursor
// survives a later failure, and whether a sink that already accepted
// a batch sees the replay outrank it.
type chunkSink struct {
	name    string
	failNth int // 1-based batch to refuse; 0 refuses nothing
	calls   int
	seen    [][]mirror.Change
}

func (s *chunkSink) Name() string { return s.name }

// Version-aware so it can be a fill target at all. Like recSink it
// records rather than deduplicates; what these tests assert is the
// version it was handed, which is what a real version-aware sink
// would compare.
func (s *chunkSink) VersionAware() bool { return true }

func (s *chunkSink) Apply(_ context.Context, c []mirror.Change) error {
	s.calls++
	if s.calls == s.failNth {
		return fmt.Errorf("sink %s refused batch %d", s.name, s.calls)
	}
	s.seen = append(s.seen, append([]mirror.Change(nil), c...))
	return nil
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

// One pass, one version: every row a run seeds carries the same
// seed-band value, so the pass is a unit and nothing inside it can
// tie with anything else inside it. Where that version sits relative
// to live traffic, and how two passes are ordered against each other,
// is version_test.go's subject.
func TestFillReseedStampsOneSeedBandVersionPerPass(t *testing.T) {
	d := newReseedDriver(1, 2, 3)
	sink := &recSink{name: "sink"}
	if err := newFill(t, d, sink).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var first uint64
	for _, b := range sink.seen {
		for _, ch := range b {
			if ch.Version == 0 || ch.Version > mirror.MaxSeedVersion {
				t.Errorf("key %s seeded at version %d, which is outside the seed band [1, %d]", ch.Key, ch.Version, mirror.MaxSeedVersion)
			}
			if first == 0 {
				first = ch.Version
				continue
			}
			if ch.Version != first {
				t.Errorf("key %s seeded at %d but the pass started at %d; one pass must be one version", ch.Key, ch.Version, first)
			}
		}
	}
	if first == 0 {
		t.Fatal("nothing was seeded")
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
// stays where it was and the next run re-seeds the same keys. Not at
// the same version — the replay re-reads the rows as they are now and
// draws a fresh, higher seed version, which is what
// TestReplayedChunkOutranksWhatTheFailedPassWrote pins.
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

// The failure that matters is not the one on the first chunk — that
// one leaves the cursor at zero, which any implementation that never
// writes the cursor at all would also do. A chunk that fails after an
// earlier chunk committed has to leave the cursor exactly at the last
// committed key: one key further and the failed chunk is skipped for
// good, one key back and the committed one is seeded twice.
func TestReseedCursorStaysAtTheLastCommittedChunk(t *testing.T) {
	// Sparse keys, because the cursor is a key and not a count: a
	// walk that advanced by the number of rows it processed would
	// agree with this one on 1,2,3,4 and disagree here.
	d := newReseedDriver(10, 20, 30, 40)
	sink := &chunkSink{name: "clickhouse:docs", failNth: 2}
	r := newFill(t, d, sink)
	ctx := context.Background()

	if err := r.Run(ctx); err == nil {
		t.Fatal("expected the second chunk's failure to surface")
	}
	status, err := r.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.LastID != 20 {
		t.Errorf("cursor at %d after chunk one committed and chunk two failed, want 20 — the last key of the chunk that committed", status.LastID)
	}
	if status.Processed != 2 {
		t.Errorf("processed = %d, want the two keys of the one chunk that committed", status.Processed)
	}

	if err := r.Run(ctx); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}
	if got := reseedChangeKeys(sink.seen); !reflect.DeepEqual(got, []string{"10", "20", "30", "40"}) {
		t.Errorf("seeded %v across both runs, want each key exactly once", got)
	}
	if len(sink.seen) != 2 {
		t.Errorf("the sink accepted %d batches, want one per chunk", len(sink.seen))
	}
}

// A failed chunk is replayed in full, and a sink that already
// accepted it keeps what it was given — nothing rolls a ClickHouse
// insert back because a later sink in the same chunk refused. So the
// replay has to outrank what the failed pass managed to write, or the
// mirror holds two rows for one key at one version and the merge, not
// the data, decides which one a FINAL read returns.
func TestReplayedChunkOutranksWhatTheFailedPassWrote(t *testing.T) {
	d := newReseedDriver(1, 2)
	took := &chunkSink{name: "clickhouse:docs"}
	refused := &chunkSink{name: "second:docs", failNth: 1}
	r, err := mirror.NewFillReseeder(pg.New(d), reseedSourceTable(), took, refused)
	if err != nil {
		t.Fatalf("NewFillReseeder: %v", err)
	}
	r.ChunkSize(2).Throttle(0)
	ctx := context.Background()

	if err := r.Run(ctx); err == nil {
		t.Fatal("expected the second sink's refusal to surface")
	}
	if len(took.seen) != 1 {
		t.Fatalf("the first sink accepted %d batches, want the one it was handed before the second refused", len(took.seen))
	}
	first := took.seen[0][0].Version

	// The row changed before the replay, through a path that emitted
	// nothing — the reason the table needed a reseed at all. The
	// replay therefore carries different content for the same key.
	d.rows[0].title = "changed by a path that never emitted"
	if err := r.Run(ctx); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(took.seen) != 2 {
		t.Fatalf("the replay handed the first sink %d batches in total, want 2", len(took.seen))
	}
	replay := took.seen[1][0]
	if replay.Key != "1" || replay.Row["title"] != "changed by a path that never emitted" {
		t.Errorf("replay seeded %+v, want key 1 as it now reads", replay)
	}
	if replay.Version <= first {
		t.Errorf("replay seeded key 1 at %d, the failed pass at %d: the later read must outrank the earlier one", replay.Version, first)
	}
	if replay.Version > mirror.MaxSeedVersion {
		t.Errorf("replay seeded at %d, outside the seed band, so it can outrank live changes", replay.Version)
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
	// The repair is numbered by the outbox, exactly like live
	// traffic, so the payload leaves the field unset. Any value here
	// would be one this type invented, and would sort the repair
	// somewhere below the stale rows it exists to overwrite —
	// EmitChange refuses it outright, which is why the Run above
	// would already have failed.
	if ch.Version != 0 {
		t.Errorf("payload version = %d, want 0 so the outbox id orders the repair", ch.Version)
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

// The other half of the cursor argument: a repair writes nothing to a
// sink, so its only failure mode is the emit, and it has to stop the
// walk the same way a refused sink does. The chunk transaction rolls
// the emits back; the cursor has to stay behind them, or the rows the
// aborted chunk covered are never repaired and nothing says so.
func TestRepairReseedFailedEmitDoesNotAdvanceTheCursor(t *testing.T) {
	d := newReseedDriver(1, 2, 3, 4)
	d.failOutboxNth = 3 // the first row of the second chunk
	r := newRepair(t, d)
	ctx := context.Background()

	err := r.Run(ctx)
	if err == nil {
		t.Fatal("expected the refused emit to surface")
	}
	if !contains(err.Error(), "emitting key 3") {
		t.Errorf("error should name the key the walk stopped on, got %v", err)
	}
	status, statusErr := r.Status(ctx)
	if statusErr != nil {
		t.Fatalf("Status: %v", statusErr)
	}
	if status.LastID != 2 {
		t.Errorf("cursor at %d after the second chunk failed to emit, want 2", status.LastID)
	}
	// Two emits from the chunk that committed, plus the attempt that
	// failed. Key 4 must not have been tried: the chunk is going to be
	// replayed in full, so emitting the rest of it only writes outbox
	// rows the replay will write again.
	if n := len(d.outboxInserts()); n != 3 {
		t.Errorf("%d outbox statements were issued, want 2 for the committed chunk and 1 refused attempt", n)
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

// Both halves of a chunk have to be scoped the same way. A predicate
// the key scan applies and the row read does not seeds rows the
// caller excluded from the mirror; the other way round the scan
// counts keys as seeded that were never sent, and the cursor walks
// past them. The driver here applies the predicate the way a server
// would — the excluded key is absent from any statement carrying it
// and present in any statement that lost it — so the assertion is on
// what was seeded, not on the text of the SQL.
func TestReseedWhereNarrowsBothStatements(t *testing.T) {
	src := reseedSourceTable()
	d := newReseedDriver(1, 2, 3)
	d.pred, d.excluded = `"docs"."views" <> $`, 2
	sink := &recSink{name: "sink"}
	r, err := mirror.NewFillReseeder(pg.New(d), src, sink)
	if err != nil {
		t.Fatalf("NewFillReseeder: %v", err)
	}
	// views is id*10, so this is doc 2 and nothing else. A bound
	// argument rather than IS NULL on purpose: the two statements
	// number their placeholders differently — the key scan's LIMIT
	// comes after the predicate, the row read's range before it — and
	// a predicate whose argument went to the wrong position would
	// render fine and filter on the wrong value.
	r.ChunkSize(2).Throttle(0).Where(pg.Ne(src.Col("views"), int32(20)))
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := reseedChangeKeys(sink.seen); !reflect.DeepEqual(got, []string{"1", "3"}) {
		t.Errorf("seeded %v, want only the keys the predicate leaves in the mirror", got)
	}
	// One chunk, not two: the key scan skipped doc 2 as well, so its
	// LIMIT 2 reached doc 3 and the walk finished in one pass. A scan
	// that lost the predicate would have stopped at doc 2 and needed a
	// second chunk to reach doc 3 — the same seeded keys, a different
	// walk.
	if len(sink.seen) != 1 {
		t.Errorf("the walk took %d chunks, want 1: the key scan must skip the excluded key too", len(sink.seen))
	}
	scans := d.keyScans()
	if len(scans) < 2 || scans[1].args[0] != int64(3) {
		t.Fatalf("second key scan resumed from %v, want 3", scans[1].args[0])
	}

	for _, s := range append(d.keyScans(), d.rowReads()...) {
		if !strings.Contains(s.sql, `"docs"."views" <> $`) {
			t.Errorf("predicate missing from %s", s.sql)
		}
		if !containsArg(s.args, int32(20)) {
			t.Errorf("predicate argument missing from %s: args %v", s.sql, s.args)
		}
	}
}

func containsArg(args []any, want any) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
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
