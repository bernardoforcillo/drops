package mirror

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/pg"
)

// seedPass is the highest seed version this process has handed out.
//
// A pass takes the millisecond count of the moment it started (see
// [SeedVersionAt]), which orders passes across restarts and across
// machines as far as their clocks agree. This counter orders the case
// the clock cannot: two passes that start inside one millisecond, as
// a script that fills ten tables in a row will do, and as a test
// will do every time.
var seedPass atomic.Uint64

// nextSeedVersion allocates the version one fill pass stamps on every
// row it writes.
//
// It is the clock reading, or one above the previous pass when the
// clock has not moved — or has moved backwards, which is the whole
// reason the previous value is remembered rather than trusted. The
// result never leaves the seed band, so however the clock behaves,
// nothing allocated here can outrank a change the stream carried.
func nextSeedVersion(now time.Time) uint64 {
	for {
		want := SeedVersionAt(now)
		prev := seedPass.Load()
		if want <= prev {
			want = prev + 1
		}
		if want > MaxSeedVersion {
			// A millisecond count reaches the top of the band in the
			// year 2527, so this is unreachable in practice.
			// Clamping rather than wrapping keeps the failure mode
			// "a later pass cannot overwrite an earlier one" instead
			// of "a seed outranks live traffic".
			want = MaxSeedVersion
		}
		if seedPass.CompareAndSwap(prev, want) {
			return want
		}
	}
}

// reseedMode selects which of the two legal ways a seeded row is
// versioned. It is unexported because the choice is made by
// picking a constructor — a mode that could be flipped afterwards
// would force every option to document what it means in the other
// mode, and the choice is too load-bearing to be a setter.
type reseedMode int

const (
	reseedFill reseedMode = iota
	reseedRepair
)

func (m reseedMode) String() string {
	if m == reseedRepair {
		return "repair"
	}
	return "fill"
}

// Reseeder rebuilds a mirror from the source table.
//
// A mirror that has fallen behind has no way back on its own: the
// change stream only carries what happens next, so a table mirrored
// for the first time, a sink that was offline while the outbox was
// cleaned up, or a ClickHouse table restored from an older backup
// stays wrong forever. A reseed is the way back. It walks the source
// table in primary-key order, in chunks, and puts every row it reads
// into the mirror as an [OpInsert] change — which is what the sinks
// want anyway, since both of them upsert.
//
// # Versioning: two modes, and why there is no third
//
// The only difficult decision is the version a seeded row carries,
// because that version is the entire tie-break: the mirror keeps the
// highest per key and looks at nothing else. A seed that outranks a
// live change silently reverts it. A seed that loses to a stale row
// silently repairs nothing. Two choices are defensible and this
// package offers a constructor for each.
//
// [NewFillReseeder] stamps a version from the seed band — see
// [SeedVersionAt] — and writes straight to the sinks. The band lies
// below every version the change stream can produce, so every seeded
// row loses to every live change and a fill can only populate keys
// the stream never covered. It cannot correct a wrong value, and it
// cannot damage a right one. Each pass draws its own version, above
// the last, so a fill that has to be re-run overwrites its own
// earlier answer instead of tying with it: two rows for one key at
// equal versions is the one case ReplacingMergeTree cannot resolve,
// and it would make a FINAL read stop being a function of the key.
//
// [NewRepairReseeder] emits the row into the outbox instead, inside
// the chunk transaction and after locking the row FOR SHARE. The seed
// is then an ordinary change carrying an ordinary version — the
// outbox id, in the live band: it beats every mirror row written
// before it, and loses to any writer racing it, because that writer
// blocks on the row lock and so takes its id after the seed does.
// This is the only mode that can overwrite a wrong value. It costs a
// full pass through the outbox and every sink — Qdrant re-embeds
// every row it sees.
//
// The third option is the one to refuse: stamping the seed with the
// reseeder's own clock, "now". A change that commits after the seed
// read the row may carry a lower stamp, because it was emitted before
// the seed took its reading. The seed then wins, the row is stale
// forever, and nothing reports it. The bands narrow the blast radius
// — a clock reading cannot reach the live band, so it could not
// revert a sequenced change — but they do not make the option sound:
// rows written by a clock-stamped [Source], and every row a mirror
// carried over from before the bands existed, are in the clock band
// too, and a seed that invented a stamp would outrank them without
// ever having been ordered against them. The two modes above work
// precisely because neither invents a version.
//
// # Running against a live mirror
//
// A fill is safe next to live writers and a running [Pump], with no
// coordination: its rows lose to everything the stream carries, in
// either arrival order. "Lose" is the sink's verb, not the
// reseeder's — a fill hands a sink a low-versioned change and relies
// on it to discard it, so [NewFillReseeder] accepts only sinks that
// declare themselves [VersionAwareSink]. Against a sink that ignores
// the version there is no safety argument at all, only an unlocked
// read racing the pump.
//
// A repair is safe next to live writers, and needs a running Pump —
// it writes to the outbox and nothing else, so nothing reaches a sink
// until the pump drains it.
//
// What must not overlap is two reseeds of one table. Progress is
// keyed by job name and guarded by no lock, so two runs under one
// name interleave chunks and corrupt the cursor, and two runs under
// two names do the work twice. Serialise them.
//
// Repair inherits the version every live change gets: the id its
// outbox row is assigned, stamped by [OutboxSource] at drain. That is
// deliberate, and it is what makes the FOR SHARE lock enough. A seed
// is ordered against live writers by the same rule live writers are
// ordered against each other, and that rule is the sequence: a writer
// that wants a row the chunk holds waits for the chunk transaction,
// so it takes a later id and is applied after the seed. Nothing here
// invents a version, which is the third option this type refuses.
//
// # Resuming
//
// A reseed of a large table will be interrupted, so it is built on
// [github.com/bernardoforcillo/drops/pg.Backfill]: chunk size,
// throttling, replica-lag pauses and a persisted cursor already live
// there. Progress is committed after each chunk, keyed by
// [Reseeder.Name], so a restart resumes at the next unseeded key
// instead of walking the table again. Declare the state table with
// [github.com/bernardoforcillo/drops/pg.NewBackfillStateTable].
//
// A chunk that fails leaves the cursor where it was, so the chunk is
// replayed on the next run. The cursor moves only after the chunk
// transaction has committed, and it moves to the last key of that
// chunk and no further — a failure anywhere in a chunk, including in
// the third of three sinks, leaves it at the last key a committed
// chunk covered. What a failed chunk does not undo is a sink that
// already accepted the batch: in fill mode the sink writes leave the
// database, and nothing rolls a ClickHouse insert back because a
// later statement in the chunk failed. So a failed chunk can leave
// the first sink holding rows the second one never saw, and the
// replay is what closes that gap.
//
// The replay is not the same write twice: it re-reads the rows as
// they are now, and a row that changed in between — through a path
// that never emitted a change, which is why the table needed a reseed
// in the first place — comes back with different content. That is why
// a fill draws a fresh version per pass rather than a constant. The
// replay's rows outrank the ones the failed pass managed to write, so
// the mirror converges on the later read instead of holding two rows
// at one version and letting the merge pick.
//
// # Deletes
//
// A reseed inserts. It reads the source table, so it can only ever
// learn about rows that are there — a key deleted from the source
// while the mirror was behind is invisible to it, and its stale
// mirror row survives the reseed untouched and unreported. Nothing
// here fixes that, because finding those keys means reading the
// mirror's key set and subtracting the source's, which is a
// comparison over both stores rather than a walk of one. A reseed
// restores rows that are missing or wrong; it does not restore the
// absence of rows.
//
// It also reads the table unfiltered, so a row the application treats
// as deleted while leaving it in place — a soft delete — is a live
// row to the walk, and a repair pass re-inserts it over the tombstone
// the application emitted. Pass [Reseeder.Where] the same predicate
// the application considers "present" and the two agree again.
type Reseeder struct {
	db  *pg.DB
	src *pg.Table
	key *pg.Column
	// cols is snapshotted here, the way NewClickHouseSink snapshots
	// the mirror's: a source table that grows a column afterwards
	// needs a new Reseeder, or the walk keeps seeding the old shape of
	// the row into a mirror that has room for the new one.
	cols []*pg.Column
	mode reseedMode

	sinks []Sink
	ob    *pg.Outbox

	// pass is the seed version of the run in progress, allocated by
	// Run. Held on the reseeder rather than threaded through the
	// chunk loop because pg.Backfill's Process signature is fixed;
	// safe because a Reseeder walks one chunk at a time and two
	// concurrent runs of one table are already forbidden.
	pass uint64

	name       string
	stateTable string
	chunkSize  int
	throttle   time.Duration
	where      []drops.Expression
	onProgress func(processed, lastKey int64)
	repl       *pg.Replicated
	lagBytes   uint64
}

// NewFillReseeder returns a [Reseeder] that writes rows straight to
// the sinks in the seed band, bypassing the outbox and the [Pump].
//
// Use it to populate a mirror that is empty or has holes: a new
// ClickHouse table, a mirror whose outbox was truncated before a sink
// caught up. It fills what is missing and overwrites nothing the
// change stream wrote, so it is safe to run against a live mirror
// with writers and a pump both going. The one thing a pass does
// overwrite is what an earlier pass of its own left behind: seed
// versions increase from pass to pass, on purpose, so a re-run
// replaces its predecessor's answer instead of tying with it.
//
// It cannot correct a row that is present but wrong — that row's
// version is in the live band and beats a seed every time. Use
// [NewRepairReseeder] when a value has to be overwritten.
//
// # Only version-aware sinks
//
// Every sink must implement [VersionAwareSink] and answer yes, and a
// sink that does not is refused here rather than at 3am. The whole
// safety argument for writing outside the ordered stream is the
// version: the walk deliberately takes no row lock, so a chunk can
// read a row, the application can update it, the pump can deliver
// that update, and the chunk can then hand the sink the pre-image —
// out of order, on purpose, because the sink is supposed to drop it.
// A sink that ignores the version accepts it instead, and there is
// nothing left to repair the damage: no future change mentions that
// key, so the mirror holds the old value for good. Where the change
// the fill raced was a delete, a version-blind sink resurrects a row
// that no longer exists at the source.
//
// [QdrantSink] is refused for exactly this reason — Qdrant has no
// conditional upsert to build the check out of. Seed a collection
// with [NewRepairReseeder], which is ordered by delivery rather than
// by comparison, or wrap the sink in something that does the
// comparison itself and declares it.
//
// The sinks are applied in order and the chunk stops at the first one
// that fails, for the same reason [Pump.Step] does: the chunk will be
// replayed in full, so applying to the rest only widens the window in
// which the sinks disagree.
func NewFillReseeder(db *pg.DB, src *pg.Table, sinks ...Sink) (*Reseeder, error) {
	r, err := newReseeder(db, src, reseedFill)
	if err != nil {
		return nil, err
	}
	if len(sinks) == 0 {
		return nil, fmt.Errorf("drops/mirror: NewFillReseeder needs at least one sink to write %q into", src.Name())
	}
	for i, s := range sinks {
		if s == nil {
			return nil, fmt.Errorf("drops/mirror: NewFillReseeder sink %d is nil", i)
		}
		if !sinkHonoursVersion(s) {
			return nil, fmt.Errorf("drops/mirror: NewFillReseeder cannot fill through sink %d (%s): a fill writes seeded rows outside the ordered stream and relies on the sink to discard the ones a live change has already superseded, which only a VersionAwareSink does; use NewRepairReseeder to seed %q through the outbox instead",
				i, s.Name(), src.Name())
		}
	}
	r.sinks = sinks
	return r, nil
}

// NewRepairReseeder returns a [Reseeder] that emits each row into the
// outbox, so the seed travels the same path as a live change and a
// running [Pump] delivers it.
//
// This is the mode that corrects wrong values, and the expensive one:
// every row passes through the outbox and every sink, so a Qdrant
// mirror re-embeds the whole table. Reach for it when a mirror is
// known to hold wrong data — a sink that acknowledged batches it
// dropped, a partial restore — rather than merely incomplete data.
//
// Each chunk locks its rows FOR SHARE before reading them, which is
// what makes the seed lose to a concurrent writer instead of
// clobbering it: the writer's UPDATE waits for the chunk transaction,
// so its change is emitted after the seed and outranks it. Reading
// without the lock reintroduces exactly the inversion the mode exists
// to avoid.
func NewRepairReseeder(db *pg.DB, src *pg.Table, ob *pg.Outbox) (*Reseeder, error) {
	r, err := newReseeder(db, src, reseedRepair)
	if err != nil {
		return nil, err
	}
	if ob == nil {
		return nil, fmt.Errorf("drops/mirror: NewRepairReseeder needs an outbox — a repair is delivered as ordinary changes, not written to sinks directly")
	}
	r.ob = ob
	return r, nil
}

func newReseeder(db *pg.DB, src *pg.Table, mode reseedMode) (*Reseeder, error) {
	if db == nil {
		return nil, fmt.Errorf("drops/mirror: a %s reseed needs a database handle to read the source table from", mode)
	}
	if src == nil {
		return nil, fmt.Errorf("drops/mirror: a %s reseed needs a source table", mode)
	}
	key, err := reseedKeyColumn(src)
	if err != nil {
		return nil, err
	}
	return &Reseeder{
		db:         db,
		src:        src,
		key:        key,
		cols:       src.Columns(),
		mode:       mode,
		name:       "mirror-reseed:" + src.Name() + ":" + mode.String(),
		stateTable: "backfillJobs",
		chunkSize:  1000,
		throttle:   50 * time.Millisecond,
	}, nil
}

// reseedKeyColumn finds the column the walk is ordered and resumed by.
//
// The restrictions are inherited, not invented: the mirror
// deduplicates on a single key (see [DeriveClickHouse]), and
// pg.Backfill persists its resume cursor as a bigint, which is what
// makes an interrupted reseed resumable at all. Reimplementing the
// chunk loop to carry a text cursor would duplicate the throttling,
// the state table and the lag gate to widen the key space; refusing
// the table says so instead.
func reseedKeyColumn(src *pg.Table) (*pg.Column, error) {
	var key *pg.Column
	for _, c := range src.Columns() {
		if !c.IsPrimaryKey() {
			continue
		}
		if key != nil {
			return nil, fmt.Errorf("drops/mirror: source table %q has a composite primary key (%q and %q); a reseed walks one key column in order", src.Name(), key.Name(), c.Name())
		}
		key = c
	}
	if key == nil {
		return nil, fmt.Errorf("drops/mirror: source table %q has no primary key, so there is no order to walk it in and no key to address mirrored rows by", src.Name())
	}
	base, _ := splitType(key.Type().TypeSQL())
	switch base {
	case "smallint", "integer", "serial", "bigint", "bigserial":
		return key, nil
	default:
		return nil, fmt.Errorf("drops/mirror: primary key %q of table %q is %s, but a reseed resumes from a cursor stored as a bigint, so it can only walk an integer key",
			key.Name(), src.Name(), key.Type().TypeSQL())
	}
}

// Named overrides the job name, which is the key the resume cursor is
// stored under. The default names the table and the mode, so a fill
// and a repair of one table never share a cursor — they mean
// different things and finish at different times. Override it to run
// a second, differently scoped pass (see [Reseeder.Where]) without
// resuming the first one's progress.
func (r *Reseeder) Named(name string) *Reseeder {
	if name != "" {
		r.name = name
	}
	return r
}

// Name returns the job name the resume cursor is stored under. An
// operator needs it to read [Reseeder.Status] or to clear the row by
// hand.
func (r *Reseeder) Name() string { return r.name }

// ChunkSize caps how many keys one chunk covers. Defaults to 1000.
// Larger chunks make fewer, bigger sink writes and hold the chunk
// transaction — and, in repair mode, its row locks — for longer.
func (r *Reseeder) ChunkSize(n int) *Reseeder {
	if n > 0 {
		r.chunkSize = n
	}
	return r
}

// Throttle sleeps between chunks to bound the load a reseed puts on
// the source, the sinks and (in repair mode) the outbox. Defaults to
// 50ms; zero runs flat out.
func (r *Reseeder) Throttle(d time.Duration) *Reseeder {
	if d >= 0 {
		r.throttle = d
	}
	return r
}

// StateTable overrides the table the resume cursor lives in.
// Defaults to "backfillJobs", the name
// [github.com/bernardoforcillo/drops/pg.NewBackfillStateTable]
// declares.
func (r *Reseeder) StateTable(name string) *Reseeder {
	if name != "" {
		r.stateTable = name
	}
	return r
}

// Where narrows the walk to the rows the mirror is supposed to hold.
// Predicates are ANDed and applied to both statements of a chunk.
//
// Two uses. A mirror of part of a table — one tenant, one year —
// needs the reseed scoped the same way the change stream is, or the
// reseed puts rows into the mirror that the stream would never have
// sent. And a source table with soft deletes needs the same predicate
// the application calls "present": a reseed reads the table, so
// without it a repair re-inserts rows the application has already
// tombstoned in the mirror.
//
// Whatever is passed here has to match what the emitters do. A
// predicate the change stream does not share does not make the mirror
// smaller, it makes the two disagree about what belongs in it.
func (r *Reseeder) Where(preds ...drops.Expression) *Reseeder {
	for _, p := range preds {
		if p != nil {
			r.where = append(r.where, p)
		}
	}
	return r
}

// OnProgress fires after each chunk commits, with the number of keys
// seeded so far and the key the cursor now stands at. Pair the second
// value with the table's max key for an ETA.
func (r *Reseeder) OnProgress(fn func(processed, lastKey int64)) *Reseeder {
	r.onProgress = fn
	return r
}

// PauseIfLag holds the walk while replica lag exceeds thresholdBytes,
// so a reseed of a large table cannot push the read replicas the rest
// of the application depends on into arrears.
func (r *Reseeder) PauseIfLag(repl *pg.Replicated, thresholdBytes uint64) *Reseeder {
	r.repl = repl
	r.lagBytes = thresholdBytes
	return r
}

// Status reports the persisted cursor: how many keys have been
// seeded, where the walk stands, whether it finished, and the last
// error if it stopped on one.
func (r *Reseeder) Status(ctx context.Context) (pg.BackfillStatus, error) {
	return r.backfill().Status(ctx)
}

// Reset clears the persisted cursor so the next [Reseeder.Run] walks
// the table from the start. A finished job is a no-op until it is
// reset — that is what stops a scheduled reseed from re-running
// forever, and what a caller who wants a second pass has to undo.
func (r *Reseeder) Reset(ctx context.Context) error {
	return r.backfill().Reset(ctx)
}

// Run walks the table until it ends, ctx is cancelled, or a chunk
// fails.
//
// A failed chunk leaves the cursor at the last committed position and
// returns the error, so the next Run resumes there rather than
// starting over — including when the failure was a sink refusing the
// batch. Run on a job that already finished returns nil without
// reading a row; call [Reseeder.Reset] first to walk it again.
//
// A fill draws its seed version here, so every row one Run writes
// carries the same one and the next Run carries a higher one. That is
// what orders a resumed walk, a replayed chunk and a second pass
// against the rows an earlier pass left behind.
func (r *Reseeder) Run(ctx context.Context) error {
	if r.mode == reseedFill {
		r.pass = nextSeedVersion(time.Now())
	}
	return r.backfill().Run(ctx)
}

// backfill wires the chunk loop. It is rebuilt per call rather than
// held, because the job name and the state table are settable and a
// stale pg.Backfill would keep resuming the wrong cursor.
func (r *Reseeder) backfill() *pg.Backfill {
	bf := pg.NewBackfill(r.db, r.name).
		ChunkSize(r.chunkSize).
		Throttle(r.throttle).
		StateTable(r.stateTable).
		Fetch(r.fetchKeys).
		Process(r.processChunk)
	if r.onProgress != nil {
		bf.OnProgress(r.onProgress)
	}
	if r.repl != nil {
		bf.PauseIfLag(r.repl, r.lagBytes)
	}
	return bf
}

// fetchKeys advances the cursor by one chunk.
//
// It reads keys only — an index-only scan over the primary key — and
// leaves the row bodies to processChunk, which re-reads them inside
// the chunk transaction. That is what lets repair mode hold FOR SHARE
// over exactly the rows it emits: a lock taken here would be released
// the moment this statement's implicit transaction ended.
func (r *Reseeder) fetchKeys(ctx context.Context, lastKey int64, limit int) ([]int64, int64, error) {
	sql, args := r.keysSQL(lastKey, limit)
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, lastKey, fmt.Errorf("drops/mirror: reseed %s: reading keys of %q: %w", r.name, r.src.Name(), err)
	}
	defer rows.Close()

	keys := make([]int64, 0, limit)
	next := lastKey
	for rows.Next() {
		var k int64
		if err := rows.Scan(&k); err != nil {
			return nil, lastKey, fmt.Errorf("drops/mirror: reseed %s: scanning key %q of %q: %w", r.name, r.key.Name(), r.src.Name(), err)
		}
		keys = append(keys, k)
		if k > next {
			next = k
		}
	}
	if err := rows.Err(); err != nil {
		return nil, lastKey, fmt.Errorf("drops/mirror: reseed %s: reading keys of %q: %w", r.name, r.src.Name(), err)
	}
	return keys, next, nil
}

// processChunk seeds one chunk. pg.Backfill runs it inside a
// transaction, which repair mode needs and fill mode merely tolerates
// — the fill's sink writes happen while that read transaction is
// open, which is the price of sharing one chunk loop and is bounded
// by ChunkSize.
func (r *Reseeder) processChunk(ctx context.Context, tx *pg.DB, keys []int64) error {
	if len(keys) == 0 {
		return nil
	}
	lo, hi := keys[0], keys[0]
	for _, k := range keys {
		if k < lo {
			lo = k
		}
		if k > hi {
			hi = k
		}
	}
	changes, err := r.readRange(ctx, tx, lo, hi)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		// Nothing the key scan saw is still in scope: the rows were
		// deleted between the two reads, or — where [Reseeder.Where]
		// is in play — changed so that they no longer belong in the
		// mirror. Either way there is nothing to seed and the cursor
		// moves past them, because a reseed reads the source and a
		// key that is not there is one it can neither seed nor say
		// anything about. What that costs where the mirror still
		// holds the key is the Deletes section above: the stale row
		// survives the reseed untouched and unreported.
		return nil
	}
	if r.mode == reseedRepair {
		for _, ch := range changes {
			if err := EmitChange(r.ob, tx, ctx, ch); err != nil {
				return fmt.Errorf("drops/mirror: reseed %s: emitting key %s: %w", r.name, ch.Key, err)
			}
		}
		return nil
	}
	for _, s := range r.sinks {
		if err := s.Apply(ctx, changes); err != nil {
			return fmt.Errorf("drops/mirror: reseed %s: sink %s: %w", r.name, s.Name(), err)
		}
	}
	return nil
}

// readRange reads the chunk's rows and turns them into changes.
//
// The range comes from the keys themselves rather than from the
// cursor, so nothing here depends on how the fetch that produced them
// was called. Rows inserted into the range between the two reads are
// picked up as well, which costs one redundant seed and no
// correctness: their own change is in the stream and outranks it.
func (r *Reseeder) readRange(ctx context.Context, tx *pg.DB, lo, hi int64) ([]Change, error) {
	sql, args := r.rowsSQL(lo, hi)
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("drops/mirror: reseed %s: reading rows %d..%d of %q: %w", r.name, lo, hi, r.src.Name(), err)
	}
	defer rows.Close()

	// Sized by the chunk, not by the key range: keys are sparse after
	// a mass delete, and hi-lo can be billions where the chunk is a
	// thousand rows.
	out := make([]Change, 0, r.chunkSize)
	for rows.Next() {
		vals := make([]any, len(r.cols))
		dest := make([]any, len(r.cols))
		for i := range vals {
			dest[i] = &vals[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("drops/mirror: reseed %s: scanning a row of %q: %w", r.name, r.src.Name(), err)
		}
		row := make(map[string]any, len(r.cols))
		for i, c := range r.cols {
			row[c.Name()] = vals[i]
		}
		key, ok := reseedKeyText(row[r.key.Name()])
		if !ok {
			return nil, fmt.Errorf("drops/mirror: reseed %s: primary key %q of %q scanned as %T, want an integer",
				r.name, r.key.Name(), r.src.Name(), row[r.key.Name()])
		}
		// Every column is carried, so a seeded row states the whole
		// row rather than patching part of it — a mirror row assembled
		// from a partial seed and an older change would match neither.
		out = append(out, Change{Op: OpInsert, Key: key, Row: row, Version: r.seedVersion()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("drops/mirror: reseed %s: reading rows %d..%d of %q: %w", r.name, lo, hi, r.src.Name(), err)
	}
	// One reading for the chunk, taken here rather than before the
	// scan: in repair mode every row of the chunk is locked by now, so
	// no writer can commit ahead of this stamp and still be ordered
	// behind it.
	now := time.Now()
	for i := range out {
		out[i].At = now
	}
	return out, nil
}

// seedVersion is the mode's version choice in one place, because it
// is the decision the whole type exists to get right. Fill carries
// this run's seed-band version; repair leaves the field unset, which
// is what [EmitChange] requires, so the change is numbered by the
// outbox exactly like live traffic and inherits its ordering instead
// of inventing one.
//
// The fallback matters as much as the value: a repair that returned
// anything non-zero here would be refused by EmitChange rather than
// quietly landing below the rows it exists to overwrite.
func (r *Reseeder) seedVersion() uint64 {
	if r.mode == reseedRepair {
		return 0
	}
	if r.pass == 0 {
		// Run allocates the pass; this covers a chunk driven some
		// other way, and keeps a fill from ever stamping zero, which
		// Normalized would rewrite into the clock band and hand the
		// seed a version above the rows it must lose to.
		r.pass = nextSeedVersion(time.Now())
	}
	return r.pass
}

// keysSQL renders the cursor-advancing scan.
func (r *Reseeder) keysSQL(lastKey int64, limit int) (string, []any) {
	b := drops.NewBuilder()
	b.WriteString("SELECT ")
	b.Append(r.key)
	b.WriteString(" FROM ")
	b.Append(r.src)
	b.WriteString(" WHERE ")
	b.Append(r.key)
	b.WriteString(" > ")
	b.AddArg(lastKey)
	r.appendWhere(b)
	b.WriteString(" ORDER BY ")
	b.Append(r.key)
	b.WriteString(" LIMIT ")
	b.AddArg(int64(limit))
	return b.SQL()
}

// rowsSQL renders the chunk's row read.
//
// Both statements are built here rather than through pg's SelectBuilder
// because that builder renders FOR UPDATE and not FOR SHARE, and the
// difference matters: FOR UPDATE would block every writer touching the
// chunk for as long as the chunk takes, where FOR SHARE only makes the
// seed wait for writers already in flight — which is all the ordering
// argument in [NewRepairReseeder] needs.
func (r *Reseeder) rowsSQL(lo, hi int64) (string, []any) {
	b := drops.NewBuilder()
	b.WriteString("SELECT ")
	for i, c := range r.cols {
		if i > 0 {
			b.WriteString(", ")
		}
		b.Append(c)
	}
	b.WriteString(" FROM ")
	b.Append(r.src)
	b.WriteString(" WHERE ")
	b.Append(r.key)
	b.WriteString(" >= ")
	b.AddArg(lo)
	b.WriteString(" AND ")
	b.Append(r.key)
	b.WriteString(" <= ")
	b.AddArg(hi)
	r.appendWhere(b)
	// Key order twice over: the sinks see the batch in key order, and
	// two reseeds that were never meant to overlap take their locks in
	// the same order instead of deadlocking against each other.
	b.WriteString(" ORDER BY ")
	b.Append(r.key)
	if r.mode == reseedRepair {
		b.WriteString(" FOR SHARE")
	}
	return b.SQL()
}

func (r *Reseeder) appendWhere(b *drops.Builder) {
	for _, p := range r.where {
		b.WriteString(" AND (")
		b.Append(p)
		b.WriteString(")")
	}
}

// reseedKeyText renders a scanned primary key the way [Change.Key]
// wants it. Which Go integer a driver hands back for a given column
// width is the driver's business, so all of them are accepted and
// anything else is refused rather than formatted into a key that
// would address the wrong mirrored row.
func reseedKeyText(v any) (string, bool) {
	switch k := v.(type) {
	case int64:
		return strconv.FormatInt(k, 10), true
	case int32:
		return strconv.FormatInt(int64(k), 10), true
	case int16:
		return strconv.FormatInt(int64(k), 10), true
	case int:
		return strconv.FormatInt(int64(k), 10), true
	case uint64:
		return strconv.FormatUint(k, 10), true
	case uint32:
		return strconv.FormatUint(uint64(k), 10), true
	default:
		return "", false
	}
}
