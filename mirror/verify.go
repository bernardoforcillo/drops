package mirror

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/pg"
)

// DivergenceKind classifies how one key disagrees.
type DivergenceKind string

const (
	// MissingFromMirror is a key the source has and a reader of the
	// mirror cannot see: either no row at all, or a winning row that
	// is a tombstone.
	MissingFromMirror DivergenceKind = "missing_from_mirror"

	// ExtraInMirror is a key whose winning mirror row is live while
	// the source has no such row.
	ExtraInMirror DivergenceKind = "extra_in_mirror"

	// ValueMismatch is a key both sides hold, whose compared columns
	// do not agree.
	ValueMismatch DivergenceKind = "value_mismatch"
)

// Direction says which side holds more.
//
// It is a statement about the rows observed, not about time. A mirror
// that lacks a row the source has reads as [MirrorBehind] whether the
// insert is merely un-pumped or was lost outright; a mirror holding a
// row the source does not reads as [MirrorAhead] whether the delete is
// un-pumped or the row was never real. A single pass cannot tell those
// apart — [Verifier.Recheck] is what narrows it.
type Direction string

const (
	// MirrorBehind means the source holds data the mirror does not.
	MirrorBehind Direction = "behind"

	// MirrorAhead means the mirror holds data the source does not.
	MirrorAhead Direction = "ahead"

	// DirectionUnclear is a disagreement that points neither way:
	// both sides hold the key and the values differ, which a stale
	// mirror row and a corrupt one produce alike.
	DirectionUnclear Direction = "unclear"
)

// Divergence is one key on which the source and the mirror disagree.
type Divergence struct {
	// Key is the primary key in the same text form [Change.Key]
	// uses, so it can be fed straight back into a repair.
	Key string

	// Kind is what the disagreement looks like.
	Kind DivergenceKind

	// MirrorVersion is the [VersionColumn] of the winning mirror
	// row, or zero when the mirror has no row for this key. It is
	// what [Verifier.Recheck] compares to decide whether the mirror
	// moved between two reads.
	MirrorVersion uint64

	// Tombstoned reports that the mirror does hold a winning row for
	// this key and that row is a tombstone. It separates "the delete was
	// applied and should not have been" from "the insert never
	// arrived", which [MissingFromMirror] covers alike.
	Tombstoned bool

	// Settled is set by [Verifier.Recheck]: MirrorVersion is what it
	// was at the first read, so nothing this package writes was
	// applied for this key in between and lag arriving late does not
	// explain the difference. It is a necessary condition for a real
	// defect, not a sufficient one — see Recheck for both limits.
	Settled bool

	// SeenAt is when the reads behind this record were issued.
	SeenAt time.Time
}

// VersionOrigin says which of the three version bands a mirrored
// row's [VersionColumn] falls in, and so what put the row there.
type VersionOrigin string

const (
	// OriginAbsent is a key the mirror holds no row for at all.
	// Nothing in this package ever stamps version zero — [OutboxSource]
	// numbers a change and [Change.Normalized] backstops it — so a
	// zero version is the absence of a row and not a row that was
	// written carelessly.
	OriginAbsent VersionOrigin = "absent"

	// OriginFill is a row a fill-mode reseed wrote: its version is in
	// the seed band, at or below [MaxSeedVersion], which nothing that
	// travels the change stream can reach.
	OriginFill VersionOrigin = "fill"

	// OriginClock is a row stamped from a wall clock by
	// [ClockVersion] — a change from a source with no sequence of its
	// own, or a row a release older than the version bands wrote.
	OriginClock VersionOrigin = "clock"

	// OriginStream is a row the pump delivered, numbered by the
	// source's own sequence through [LiveVersion]: at or above
	// [LiveVersionBase].
	OriginStream VersionOrigin = "stream"
)

// Origin says what put the winning mirror row for this key there,
// read off the band [Divergence.MirrorVersion] falls in.
//
// It costs nothing — the version is already in hand — and it answers
// the question the three divergence kinds cannot: whether the row the
// mirror is holding ever came down the change stream. That decides
// which repair can work. A mismatch on an [OriginStream] row is
// beyond a fill-mode reseed, because a seed-band version loses to a
// live one by construction, so the remedy is [NewRepairReseeder],
// which re-emits through the outbox and lands above it. The same
// mismatch on an [OriginFill] row says the opposite: no change has
// ever been pumped over that key, so the stream does not cover it,
// and a fresh fill pass does outrank the row the last one left —
// seed versions increase from pass to pass.
func (d Divergence) Origin() VersionOrigin {
	switch {
	case d.MirrorVersion == 0:
		return OriginAbsent
	case d.MirrorVersion <= MaxSeedVersion:
		return OriginFill
	case d.MirrorVersion < LiveVersionBase:
		return OriginClock
	default:
		return OriginStream
	}
}

// Direction reports which side holds more for this key.
func (d Divergence) Direction() Direction {
	switch d.Kind {
	case MissingFromMirror:
		return MirrorBehind
	case ExtraInMirror:
		return MirrorAhead
	default:
		return DirectionUnclear
	}
}

// VerifyRange is the verdict for one key range, handed to the callback
// installed by [Verifier.OnRange].
type VerifyRange struct {
	// From is the exclusive lower bound, empty at the start of the
	// key space; Through is the inclusive upper bound, empty for the
	// final range, which runs to the end of the key space.
	From    string
	Through string

	// SourceRows and MirrorRows count what each side contributed.
	// Tombstones are not content, so they are not counted here.
	SourceRows int
	MirrorRows int

	// SourceDigest and MirrorDigest are the hex range checksums that
	// decided the verdict.
	SourceDigest string
	MirrorDigest string

	// Matched reports that the counts and the checksums agreed, so
	// no key in the range was examined individually.
	Matched bool

	// Divergences counts the keys named in this range.
	Divergences int
}

// VerifyReport is what one pass of [Verifier.Verify] found.
type VerifyReport struct {
	// Source and Mirror name the two tables compared.
	Source string
	Mirror string

	// RangesChecked counts the key ranges compared; RangesDiverged
	// counts those that named at least one key.
	RangesChecked  int
	RangesDiverged int

	// SourceRows and MirrorRows count the rows read on each side.
	// MirrorRows counts live rows: a tombstone is the absence of a
	// row, not a row.
	SourceRows int64
	MirrorRows int64

	// Behind, Ahead and Unclear count every divergence found, by
	// [Divergence.Direction], whether or not it fitted in
	// Divergences.
	Behind  int
	Ahead   int
	Unclear int

	// Settled counts the divergences [Verifier.Recheck] found the
	// mirror had not moved on.
	Settled int

	// Divergences names the keys, capped by
	// [Verifier.MaxDivergences]. Truncated reports that the cap was
	// reached and the counts above are the complete picture.
	Divergences []Divergence
	Truncated   bool

	// Resume is empty when the pass reached the end of the key
	// space. Otherwise — a cancelled context, a failed read — it is
	// the point to hand to [Verifier.From] to carry on from: the last
	// key fully compared, or, when the pass stopped before it finished
	// a single range, the key it was started after. A pass started at
	// the head of the key space has no such key, so an interrupted one
	// resumes from the head, which is where it began.
	Resume string

	StartedAt  time.Time
	FinishedAt time.Time
}

// Aligned reports that the pass found nothing to act on.
//
// It is a statement about what was compared, not about the two
// tables, so it only means "these agree" for a pass that returned no
// error. A pass that stopped on a failed read compared the ranges up
// to [VerifyReport.Resume] and none after it, and reports the same
// true — read the error first.
func (r VerifyReport) Aligned() bool {
	return r.Behind == 0 && r.Ahead == 0 && r.Unclear == 0
}

// Verifier compares a source table with the ClickHouse mirror derived
// from it and reports where they disagree.
//
// # What it compares
//
// The compared column set is the source table's columns, each paired
// with its counterpart in the mirror. It is derived, not declared, for
// the same reason [DeriveClickHouse] derives the schema: a comparison
// against a hand-listed column set stops covering the columns added
// after it was written. [VersionColumn] and [DeletedColumn] are
// bookkeeping and never compared.
//
// # Why the checksum is computed in Go
//
// A range checksum is only meaningful if both engines compute it the
// same way over the same values, and no pair of SQL engines does.
// PostgreSQL and ClickHouse both have MD5, but they disagree on the
// text they would feed it: trailing zeros on a numeric, the rendering
// of a float, the timezone a timestamp prints in, whether a NULL
// concatenates away to nothing. A checksum built from each engine's
// own idea of a value's text reports divergence on a mirror that is
// perfectly correct.
//
// So neither engine hashes anything. Both return rows, and every value
// from either side is encoded in Go against one schema — the mirror
// column's declared ClickHouse type, which [MapType] derived from the
// source column's PostgreSQL type. An int32 and an int64 holding 7
// encode alike; "12.30" and "12.3" in a Decimal(10,2) column encode
// alike; the same instant in two timezones encodes alike. What the
// driver on either side happened to hand back cannot move the result,
// which is the property the comparison needs. The per-row encodings
// are hashed with SHA-256 and folded into a range checksum by XOR, so
// the fold does not depend on either engine's sort order — the one
// remaining thing they do not agree on.
//
// # How the mirror side is read
//
// Through FINAL, so superseded versions collapse and each key is
// represented by its winning row, and with [DeletedColumn] selected
// rather than filtered: a tombstone counts as absence when comparing,
// but reporting it as a tombstone tells the reader something a plain
// [NotDeleted] filter would have thrown away.
//
// # What a pass proves
//
// Less than it looks. Two engines admit no consistent cut: a row that
// changes between the mirror read and the source read differs
// legitimately, and no amount of care inside one process removes that.
// A pass therefore reports candidates, and the reads are ordered
// mirror first, source second, so that the source read is always the
// later of the two and every difference is at least consistent with
// the mirror lagging — which is the normal, self-healing state.
// [Verifier.Recheck] is how a candidate is discharged.
//
// # Cost
//
// Verification reads every row of both tables in the range walked. It
// is a full scan on each side, which is why [Verifier.Throttle] and
// [Verifier.From] exist. A Verifier is not safe for concurrent use;
// call the setters before Verify.
//
// A uuid key costs more than that, and the excess is not something
// this package can tune away. The two engines do not order the UUID
// type the same way, so both sides are walked in the canonical text
// order instead, and on the mirror that renders as
// WHERE toString(id) > ? ORDER BY toString(id). toString is not
// monotone in the order a UUID sorting key is stored in, so neither
// clause can use the primary index: ClickHouse cannot skip granules
// on the predicate and cannot read in order for the LIMIT. Every
// range therefore reads and FINAL-merges the whole mirror rather than
// one page of it, and a pass costs one full mirror scan per range —
// keys/RangeSize of them. On a ten-million-row uuid-keyed mirror at
// the default range size that is ten thousand full scans, which is to
// say the pass does not finish. The Postgres side has the same shape
// of problem for the same reason: it is ordered by
// (id::text COLLATE "C"), which the uuid primary key index cannot
// serve, so an expression index on that is what turns its scan back
// into a range read.
//
// There are two honest answers and no third. [Verifier.RangeSize] is
// the only lever inside the package: the work is quadratic in the row
// count and inversely proportional to the range size, so raising it
// far above the default divides the number of scans, at the cost of
// holding a range of both sides in memory. The other is to not key a
// mirrored table on a uuid. An integer key orders identically in both
// engines and is walked from the primary index on both sides, which is
// the only shape for which this doc's "a full scan on each side" is
// the whole story; a text key gets there too, once the source carries
// an index under the C collation.
type Verifier struct {
	src      *pg.DB
	srcTable *pg.Table
	mir      *clickhouse.DB
	mirTable *clickhouse.Table

	key  verifyKey
	cols []verifyColumn

	rangeSize int
	maxReport int
	throttle  time.Duration
	from      string
	skip      []string
	onRange   func(VerifyRange)
	now       func() time.Time
}

// verifyColumn is one column of the comparison: where to read it on
// each side, and the declared type both sides are encoded against.
type verifyColumn struct {
	name   string
	src    *pg.Column
	mir    *clickhouse.Column
	chType string
}

// NewVerifier pairs a source table with the mirror derived from it.
//
// It fails rather than compare a pair that cannot be compared: a
// source table with no single-column primary key has nothing to walk
// the key space by, and a mirror missing one of the source's columns
// would silently narrow the comparison.
func NewVerifier(src *pg.DB, srcTable *pg.Table, mir *clickhouse.DB, mirTable *clickhouse.Table) (*Verifier, error) {
	if src == nil || srcTable == nil || mir == nil || mirTable == nil {
		return nil, fmt.Errorf("drops/mirror: NewVerifier needs a source db and table and a mirror db and table")
	}
	if mirTable.Col(VersionColumn) == nil || mirTable.Col(DeletedColumn) == nil {
		return nil, fmt.Errorf("drops/mirror: mirror table %q is missing the %q/%q columns — build it with DeriveClickHouse",
			mirTable.Name(), VersionColumn, DeletedColumn)
	}

	var pk *pg.Column
	var pkMirror *clickhouse.Column
	cols := make([]verifyColumn, 0, len(srcTable.Columns()))
	for _, c := range srcTable.Columns() {
		switch c.Name() {
		case VersionColumn, DeletedColumn:
			return nil, fmt.Errorf("drops/mirror: source table %q has a column named %q, which the mirror uses for its own bookkeeping",
				srcTable.Name(), c.Name())
		}
		m := mirTable.Col(c.Name())
		if m == nil {
			return nil, fmt.Errorf("drops/mirror: mirror table %q has no column %q to compare with source table %q; derive the mirror with DeriveClickHouse so the two cannot drift",
				mirTable.Name(), c.Name(), srcTable.Name())
		}
		cols = append(cols, verifyColumn{name: c.Name(), src: c, mir: m, chType: m.Type().TypeSQL()})
		if c.IsPrimaryKey() {
			if pk != nil {
				return nil, fmt.Errorf("drops/mirror: source table %q has a composite primary key; verification walks one key column", srcTable.Name())
			}
			pk, pkMirror = c, m
		}
	}
	if pk == nil {
		return nil, fmt.Errorf("drops/mirror: source table %q has no primary key, so there is no key space to walk", srcTable.Name())
	}
	key, err := newVerifyKey(pk, pkMirror)
	if err != nil {
		return nil, err
	}
	return &Verifier{
		src:       src,
		srcTable:  srcTable,
		mir:       mir,
		mirTable:  mirTable,
		key:       key,
		cols:      cols,
		rangeSize: 1000,
		maxReport: 1000,
		now:       time.Now,
	}, nil
}

// RangeSize sets how many keys a range holds. Default 1000. The range
// is the unit of memory: one range of each side's rows is in flight at
// a time, which is what lets a table too large to hold be verified.
// Larger ranges make fewer round trips and hold more.
func (v *Verifier) RangeSize(n int) *Verifier {
	if n > 0 {
		v.rangeSize = n
	}
	return v
}

// MaxDivergences caps how many keys [VerifyReport.Divergences] names.
// Default 1000. The walk continues past the cap so the report's counts
// stay complete; cancel the context to stop early.
func (v *Verifier) MaxDivergences(n int) *Verifier {
	if n >= 0 {
		v.maxReport = n
	}
	return v
}

// Throttle sleeps between ranges to bound the load two full scans put
// on both engines. Default zero — verification is usually run
// deliberately, and the pace should be the operator's choice rather
// than a number chosen here.
func (v *Verifier) Throttle(d time.Duration) *Verifier {
	if d >= 0 {
		v.throttle = d
	}
	return v
}

// From starts the walk after the given key rather than at the
// beginning of the key space. Pass [VerifyReport.Resume] to carry on
// where an interrupted pass stopped. Keys at or below it are not
// examined at all, in either store.
func (v *Verifier) From(key string) *Verifier {
	v.from = key
	return v
}

// SkipColumns excludes columns from the comparison.
//
// The escape hatch exists for columns whose two representations are
// both correct and not identical — a jsonb column is the usual one,
// since PostgreSQL normalises key order and whitespace on the way in
// and the mirror stores whatever the application put in [Change.Row].
// Comparing such a column reports divergence forever; excluding it
// says so out loud instead. The primary key cannot be skipped: it is
// the identity of a row, not one of its values.
func (v *Verifier) SkipColumns(names ...string) *Verifier {
	v.skip = append(v.skip, names...)
	return v
}

// OnRange registers a callback fired once per range, before the next
// range is read. It is where progress, per-range checksums and an ETA
// come from.
func (v *Verifier) OnRange(fn func(VerifyRange)) *Verifier {
	v.onRange = fn
	return v
}

// verifyPlan is the resolved column set for one run.
type verifyPlan struct {
	cols     []verifyColumn
	keyIndex int
}

// plan resolves SkipColumns against the table, which cannot happen in
// the setter because it must be able to report an unknown name.
func (v *Verifier) plan() (verifyPlan, error) {
	for _, name := range v.skip {
		if v.srcTable.Col(name) == nil {
			return verifyPlan{}, fmt.Errorf("drops/mirror: SkipColumns names %q, which source table %q does not have", name, v.srcTable.Name())
		}
		if name == v.key.name {
			return verifyPlan{}, fmt.Errorf("drops/mirror: SkipColumns names %q, which is the primary key of source table %q; the key identifies the row being compared and cannot be excluded from the comparison", name, v.srcTable.Name())
		}
	}
	out := verifyPlan{cols: make([]verifyColumn, 0, len(v.cols)), keyIndex: -1}
	for _, c := range v.cols {
		if verifyContains(v.skip, c.name) {
			continue
		}
		if c.name == v.key.name {
			out.keyIndex = len(out.cols)
		}
		out.cols = append(out.cols, c)
	}
	if out.keyIndex < 0 {
		// Unreachable while the key is one of the source's columns
		// and cannot be skipped, both of which are enforced above.
		// The guard stays so that relaxing either rule fails loudly
		// instead of silently digesting another column as the key.
		return verifyPlan{}, fmt.Errorf("drops/mirror: primary key %q is not among the columns compared for source table %q", v.key.name, v.srcTable.Name())
	}
	return out, nil
}

func verifyContains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// Verify walks the key space and reports where the two tables
// disagree. See the [Verifier] doc for what the result does and does
// not prove.
//
// Each range is decided by its row count and its checksum. A range
// whose counts and checksums agree is clean and no key in it is looked
// at individually; only a range that disagrees is narrowed, which is
// what makes the report name keys rather than say that something,
// somewhere, is wrong. Narrowing costs no extra query: the per-key
// checksums the range checksum was folded from are still in hand.
//
// A read error aborts the pass and returns the report gathered so far
// alongside the error, with [VerifyReport.Resume] set so the walk can
// be continued rather than restarted.
func (v *Verifier) Verify(ctx context.Context) (VerifyReport, error) {
	plan, err := v.plan()
	if err != nil {
		return VerifyReport{}, err
	}
	rep := VerifyReport{Source: v.srcTable.Name(), Mirror: v.mirTable.Name(), StartedAt: v.now()}

	var cur verifyCursor
	if v.from != "" {
		if cur, err = v.key.cursor(v.from); err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		// Resume starts where the caller did. A pass that fails on
		// its first range has compared nothing, but it has also not
		// un-compared what came before it, and an empty Resume is
		// reserved for a walk that reached the end of the key space.
		// Reporting one here would send a caller feeding Resume back
		// to From on error to the head of the key space instead, so a
		// table large enough to need resuming would re-scan the same
		// prefix on every transient read error and never reach its
		// tail.
		rep.Resume = v.from
	}
	for {
		if err := ctx.Err(); err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		// The mirror is read first so that the source read is always
		// the later of the two. The pump only ever moves the mirror
		// towards the source, so in that order every difference is
		// at least consistent with the mirror lagging; in the other
		// order the mirror can catch up past the source read and
		// show as ahead of a source that never went backwards.
		mirRows, err := v.readMirrorRange(ctx, plan, cur)
		if err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		srcRows, err := v.readSourceRange(ctx, plan, cur)
		if err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		if len(mirRows) == 0 && len(srcRows) == 0 {
			break
		}
		through, bounded := v.rangeEnd(mirRows, srcRows)
		if bounded && cur.set && !v.key.less(cur.key, through) {
			// Every row read is past the cursor, so the range must
			// end past it too. If it does not, one of the two reads
			// ignored its lower bound, and carrying on would walk
			// the same range forever — an error beats a hang.
			rep.FinishedAt = v.now()
			return rep, fmt.Errorf("drops/mirror: verify range after key %q ended at key %q, so the walk would not advance; the range read did not honour its lower bound", cur.key, through)
		}
		if bounded {
			// Whichever side ran into the limit first sets the end
			// of the range; the rows the other side already
			// returned past it are dropped and read again with the
			// next range, so the two sides always cover the same
			// span of the key space.
			mirRows = verifyTrim(v.key, mirRows, through)
			srcRows = verifyTrim(v.key, srcRows, through)
			if len(mirRows) == 0 && len(srcRows) == 0 {
				// through is one of the two sides' last keys, so that
				// side keeps every row it returned and a bounded range
				// can never come out empty. If it did, the order the
				// range was cut by is not the order the rows arrived
				// in, and the two sides no longer describe the same
				// span. Comparing nothing would then report agreement
				// — zero rows against zero rows, checksums equal — for
				// a range neither side was actually asked about, which
				// is a clean verdict over data nobody looked at.
				rep.FinishedAt = v.now()
				return rep, fmt.Errorf("drops/mirror: verify range after key %q ending at key %q kept no rows from either side, so nothing in it was compared; the two sides are not being ordered the same way", cur.key, through)
			}
		}
		v.compareRange(&rep, cur.key, through, mirRows, srcRows)
		if !bounded {
			break
		}
		rep.Resume = through
		if cur, err = v.key.cursor(through); err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		if v.throttle > 0 && !sleepCtx(ctx, v.throttle) {
			rep.FinishedAt = v.now()
			return rep, ctx.Err()
		}
	}
	rep.Resume = ""
	rep.FinishedAt = v.now()
	return rep, nil
}

// Recheck re-reads the keys a pass named and reports which of them
// still disagree.
//
// This is how a candidate becomes a finding. A key that now agrees was
// a row in flight, or lag that has since drained, and it is dropped
// from the result. A key that still disagrees is returned with
// [Divergence.Settled] set when the mirror's version for it is the
// same as it was.
//
// What an unchanged version proves is that nothing this package wrote
// landed on that key in between, and it proves it because every writer
// here moves the version when it rewrites a row: the pump stamps
// [LiveVersion] of an outbox id that only ever goes up, and a
// fill-mode reseed draws a fresh seed version per pass, so even
// re-running a fill over the same key changes the number. A change
// still on its way through the pump therefore does not explain a
// settled difference.
//
// Two things it does not see, both worth knowing before acting on it:
//
//   - A writer that does not maintain [VersionColumn] — a hand-run
//     INSERT into the mirror, a second ingestion path — moves the row
//     without moving the version, and reads as settled. It is also
//     invisible to ReplacingMergeTree's tie-break, so it is a hazard
//     well beyond this report.
//   - Settled is a necessary condition for a defect, not a sufficient
//     one: a change sitting in the outbox that neither read saw
//     applied looks exactly like one that will never arrive. The
//     discharge that closes that gap is timing, and it belongs to the
//     caller, who is the only one who knows their pump: recheck once
//     the outbox backlog that existed at the first read has drained.
//     Recheck deliberately has no timer of its own.
//
// [Divergence.Origin] is the other half of the picture and does not
// need a second read: it says whether the row that is still wrong was
// ever delivered by the pump at all, which is what decides whether a
// fill or a repair can correct it.
func (v *Verifier) Recheck(ctx context.Context, prior []Divergence) (VerifyReport, error) {
	plan, err := v.plan()
	if err != nil {
		return VerifyReport{}, err
	}
	rep := VerifyReport{Source: v.srcTable.Name(), Mirror: v.mirTable.Name(), StartedAt: v.now()}

	keys := make([]string, 0, len(prior))
	was := make(map[string]Divergence, len(prior))
	for _, d := range prior {
		if d.Key == "" {
			return rep, fmt.Errorf("drops/mirror: Recheck was given a divergence with no key")
		}
		if _, seen := was[d.Key]; !seen {
			keys = append(keys, d.Key)
		}
		was[d.Key] = d
	}

	for start := 0; start < len(keys); start += v.rangeSize {
		if err := ctx.Err(); err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		end := start + v.rangeSize
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		// Mirror first, for the reason Verify reads in that order.
		mir, err := v.readMirrorKeys(ctx, plan, chunk)
		if err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		src, err := v.readSourceKeys(ctx, plan, chunk)
		if err != nil {
			rep.FinishedAt = v.now()
			return rep, err
		}
		at := v.now()
		rep.RangesChecked++
		rep.SourceRows += int64(len(src))
		found := 0
		for _, key := range chunk {
			m, inMirror := mir[key]
			s, inSource := src[key]
			live := inMirror && !m.deleted
			if live {
				rep.MirrorRows++
			}
			d := Divergence{Key: key, MirrorVersion: m.version, Tombstoned: inMirror && m.deleted, SeenAt: at}
			switch {
			case inSource && !live:
				d.Kind = MissingFromMirror
			case !inSource && live:
				d.Kind = ExtraInMirror
			case inSource && live && m.digest != s.digest:
				d.Kind = ValueMismatch
			default:
				continue
			}
			d.Settled = m.version == was[key].MirrorVersion
			if d.Settled {
				rep.Settled++
			}
			found++
			v.record(&rep, d)
		}
		if found > 0 {
			rep.RangesDiverged++
		}
	}
	rep.FinishedAt = v.now()
	return rep, nil
}

// record files a divergence, counting it whether or not it fits.
func (v *Verifier) record(rep *VerifyReport, d Divergence) {
	switch d.Direction() {
	case MirrorBehind:
		rep.Behind++
	case MirrorAhead:
		rep.Ahead++
	default:
		rep.Unclear++
	}
	if len(rep.Divergences) >= v.maxReport {
		rep.Truncated = true
		return
	}
	rep.Divergences = append(rep.Divergences, d)
}

// rangeEnd picks the key the range stops at: the lower of the two
// sides' last keys, considering only a side that hit the row limit and
// therefore has more to give. When neither did, both sides are
// exhausted and the range runs to the end of the key space.
func (v *Verifier) rangeEnd(mir []verifyMirrorRow, src []verifySourceRow) (string, bool) {
	mirFull := len(mir) == v.rangeSize
	srcFull := len(src) == v.rangeSize
	switch {
	case !mirFull && !srcFull:
		return "", false
	case mirFull && srcFull:
		last, other := mir[len(mir)-1].key, src[len(src)-1].key
		if v.key.less(other, last) {
			return other, true
		}
		return last, true
	case mirFull:
		return mir[len(mir)-1].key, true
	default:
		return src[len(src)-1].key, true
	}
}

// compareRange decides one range and names the keys in it if it fails.
func (v *Verifier) compareRange(rep *VerifyReport, from, through string, mir []verifyMirrorRow, src []verifySourceRow) {
	rng := VerifyRange{From: from, Through: through, SourceRows: len(src)}

	var mirSum, srcSum [32]byte
	for i := range mir {
		if mir[i].deleted {
			// A tombstone is the absence of a row. Folding it in
			// would make every mirror carrying its own deletes fail
			// against a source that simply does not have those rows.
			continue
		}
		rng.MirrorRows++
		verifyXor(&mirSum, mir[i].digest)
	}
	for i := range src {
		verifyXor(&srcSum, src[i].digest)
	}
	rng.MirrorDigest = hex.EncodeToString(mirSum[:])
	rng.SourceDigest = hex.EncodeToString(srcSum[:])

	rep.RangesChecked++
	rep.SourceRows += int64(rng.SourceRows)
	rep.MirrorRows += int64(rng.MirrorRows)

	if rng.SourceRows == rng.MirrorRows && mirSum == srcSum {
		// Every per-key digest covers the key as well as the values,
		// so two different key sets cannot fold to the same
		// checksum, and a clean verdict here is sound up to a
		// SHA-256 collision.
		rng.Matched = true
		if v.onRange != nil {
			v.onRange(rng)
		}
		return
	}

	at := v.now()
	i, j := 0, 0
	for i < len(mir) || j < len(src) {
		switch {
		case j == len(src) || (i < len(mir) && v.key.less(mir[i].key, src[j].key)):
			if !mir[i].deleted {
				rng.Divergences++
				v.record(rep, Divergence{Key: mir[i].key, Kind: ExtraInMirror, MirrorVersion: mir[i].version, SeenAt: at})
			}
			i++
		case i == len(mir) || v.key.less(src[j].key, mir[i].key):
			rng.Divergences++
			v.record(rep, Divergence{Key: src[j].key, Kind: MissingFromMirror, SeenAt: at})
			j++
		default:
			switch {
			case mir[i].deleted:
				rng.Divergences++
				v.record(rep, Divergence{Key: src[j].key, Kind: MissingFromMirror, MirrorVersion: mir[i].version, Tombstoned: true, SeenAt: at})
			case mir[i].digest != src[j].digest:
				rng.Divergences++
				v.record(rep, Divergence{Key: src[j].key, Kind: ValueMismatch, MirrorVersion: mir[i].version, SeenAt: at})
			}
			i++
			j++
		}
	}
	if rng.Divergences > 0 {
		rep.RangesDiverged++
	}
	if v.onRange != nil {
		v.onRange(rng)
	}
}

// --- reads -----------------------------------------------------------

// verifyMirrorRow is one winning mirror row, reduced to what the
// comparison needs.
type verifyMirrorRow struct {
	key     string
	digest  [32]byte
	version uint64
	deleted bool
}

// verifySourceRow is one source row, reduced the same way.
type verifySourceRow struct {
	key    string
	digest [32]byte
}

func (r verifyMirrorRow) rowKey() string { return r.key }
func (r verifySourceRow) rowKey() string { return r.key }

// verifyRow is the sliver the two row shapes share, so one trim serves
// both.
type verifyRow interface{ rowKey() string }

// verifyTrim drops the rows past the end of the range. The slice is
// ordered by key, so the first row past it ends the range.
func verifyTrim[T verifyRow](k verifyKey, rows []T, through string) []T {
	for i := range rows {
		if k.less(through, rows[i].rowKey()) {
			return rows[:i]
		}
	}
	return rows
}

// readMirrorRange reads one range of winning mirror rows.
func (v *Verifier) readMirrorRange(ctx context.Context, plan verifyPlan, after verifyCursor) ([]verifyMirrorRow, error) {
	q := v.mirrorSelect(plan).
		OrderBy(v.key.mirOrder).
		Limit(int64(v.rangeSize))
	if after.set {
		q = q.Where(clickhouse.Gt(v.key.mirOrder, after.bind))
	}
	return v.scanMirror(ctx, plan, q)
}

// readMirrorKeys reads the winning mirror rows for an explicit key set.
func (v *Verifier) readMirrorKeys(ctx context.Context, plan verifyPlan, keys []string) (map[string]verifyMirrorRow, error) {
	binds, err := v.key.binds(keys)
	if err != nil {
		return nil, err
	}
	q := v.mirrorSelect(plan).Where(clickhouse.In(v.key.mirOrder, binds...))
	rows, err := v.scanMirror(ctx, plan, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]verifyMirrorRow, len(rows))
	for _, r := range rows {
		out[r.key] = r
	}
	return out, nil
}

// mirrorSelect builds the read every mirror query shares.
//
// FINAL collapses the superseded versions so each key is represented
// by the row with the highest [VersionColumn], and [DeletedColumn] is
// selected rather than filtered so a tombstone can be reported as one.
//
// Unscoped skips the table's default filters, and the filter it is
// really there for is [NotDeleted] — the natural thing to hang on a
// mirror table so that application reads cannot forget it. Left
// applied, it would hide from the comparison exactly the rows the
// comparison is here to see: a key whose mirror row is a wrongly
// applied tombstone would come back as no row at all, and be reported
// MissingFromMirror with Tombstoned false — "the insert never
// arrived" instead of "the delete was applied and should not have
// been", which is the distinction [Divergence.Tombstoned] exists to
// preserve. Any other filter hides live rows the same way and reports
// them missing from a mirror that holds them.
func (v *Verifier) mirrorSelect(plan verifyPlan) *clickhouse.SelectBuilder {
	sel := make([]drops.Expression, 0, len(plan.cols)+2)
	for _, c := range plan.cols {
		sel = append(sel, c.mir)
	}
	sel = append(sel, v.mirTable.Col(VersionColumn), v.mirTable.Col(DeletedColumn))
	return v.mir.Select(sel...).From(v.mirTable).Final().Unscoped()
}

func (v *Verifier) scanMirror(ctx context.Context, plan verifyPlan, q *clickhouse.SelectBuilder) ([]verifyMirrorRow, error) {
	rows, err := q.Rows(ctx)
	if err != nil {
		return nil, fmt.Errorf("drops/mirror: verify read mirror table %q: %w", v.mirTable.Name(), err)
	}
	defer rows.Close()

	vals := make([]any, len(plan.cols))
	dest := make([]any, 0, len(plan.cols)+2)
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	var version uint64
	var deleted uint8
	dest = append(dest, &version, &deleted)

	out := make([]verifyMirrorRow, 0, v.rangeSize)
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("drops/mirror: verify scan mirror table %q: %w", v.mirTable.Name(), err)
		}
		row := verifyMirrorRow{version: version, deleted: deleted != 0}
		if row.key, err = v.key.text(vals[plan.keyIndex]); err != nil {
			return nil, fmt.Errorf("drops/mirror: verify mirror table %q: %w", v.mirTable.Name(), err)
		}
		if row.digest, err = verifyDigest(plan.cols, vals); err != nil {
			return nil, fmt.Errorf("drops/mirror: verify mirror table %q key %s: %w", v.mirTable.Name(), row.key, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// readSourceRange reads one range of source rows.
func (v *Verifier) readSourceRange(ctx context.Context, plan verifyPlan, after verifyCursor) ([]verifySourceRow, error) {
	q := v.sourceSelect(plan).
		OrderBy(v.key.srcOrder).
		Limit(int64(v.rangeSize))
	if after.set {
		q = q.Where(pg.Gt(v.key.srcOrder, after.bind))
	}
	return v.scanSource(ctx, plan, q)
}

// readSourceKeys reads the source rows for an explicit key set.
func (v *Verifier) readSourceKeys(ctx context.Context, plan verifyPlan, keys []string) (map[string]verifySourceRow, error) {
	binds, err := v.key.binds(keys)
	if err != nil {
		return nil, err
	}
	q := v.sourceSelect(plan).Where(pg.In(v.key.srcOrder, binds...))
	rows, err := v.scanSource(ctx, plan, q)
	if err != nil {
		return nil, err
	}
	out := make(map[string]verifySourceRow, len(rows))
	for _, r := range rows {
		out[r.key] = r
	}
	return out, nil
}

// sourceSelect builds the read every source query shares. Unscoped for
// the reason mirrorSelect is: a default filter is a read scope, and a
// soft-delete scope would otherwise turn every soft-deleted row into a
// reported divergence.
func (v *Verifier) sourceSelect(plan verifyPlan) *pg.SelectBuilder {
	sel := make([]drops.Expression, 0, len(plan.cols))
	for _, c := range plan.cols {
		sel = append(sel, c.src)
	}
	return v.src.Select(sel...).From(v.srcTable).Unscoped()
}

func (v *Verifier) scanSource(ctx context.Context, plan verifyPlan, q *pg.SelectBuilder) ([]verifySourceRow, error) {
	rows, err := q.Rows(ctx)
	if err != nil {
		return nil, fmt.Errorf("drops/mirror: verify read source table %q: %w", v.srcTable.Name(), err)
	}
	defer rows.Close()

	vals := make([]any, len(plan.cols))
	dest := make([]any, len(plan.cols))
	for i := range vals {
		dest[i] = &vals[i]
	}

	out := make([]verifySourceRow, 0, v.rangeSize)
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("drops/mirror: verify scan source table %q: %w", v.srcTable.Name(), err)
		}
		var row verifySourceRow
		if row.key, err = v.key.text(vals[plan.keyIndex]); err != nil {
			return nil, fmt.Errorf("drops/mirror: verify source table %q: %w", v.srcTable.Name(), err)
		}
		if row.digest, err = verifyDigest(plan.cols, vals); err != nil {
			return nil, fmt.Errorf("drops/mirror: verify source table %q key %s: %w", v.srcTable.Name(), row.key, err)
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// --- the key space ---------------------------------------------------

// verifyKey is how the two engines are made to agree on the order of
// the key space, which is what lets a range mean the same thing on
// both sides.
//
// An integer key is ordered numerically, which both engines do
// identically and both can serve from the primary key index. Anything
// else is ordered as text under a bytewise comparison — PostgreSQL's C
// collation, which is what ClickHouse's String comparison already is.
// A uuid is compared as its canonical text on both sides, because
// ClickHouse orders the UUID type by an internal layout that is not
// the order of the text it prints.
//
// Both text cases cost an index, and the uuid case costs both of them.
// On a database whose default collation is not C, a text key's source
// scan cannot use the primary key index unless an index exists on the
// key under the C collation. A uuid key is worse on both sides at
// once: the source is ordered by an expression over the key rather
// than the key, and toString on the mirror side puts every range
// beyond the reach of the ClickHouse sorting key, which turns each one
// into a full scan of the mirror. The Verifier's "# Cost" section
// spells that out, because it is the difference between a pass that
// finishes and one that does not.
type verifyKey struct {
	name     string
	chType   string
	numeric  bool
	srcOrder drops.Expression
	mirOrder drops.Expression
}

func newVerifyKey(src *pg.Column, mir *clickhouse.Column) (verifyKey, error) {
	k := verifyKey{name: src.Name(), chType: mir.Type().TypeSQL()}
	base, _ := splitType(src.Type().TypeSQL())
	switch base {
	case "smallint", "integer", "bigint", "serial", "bigserial":
		k.numeric = true
		k.srcOrder = src
		k.mirOrder = mir
	case "text", "varchar", "char":
		k.srcOrder = verifyCollateC(src)
		k.mirOrder = mir
	case "uuid":
		k.srcOrder = verifyCollateC(pg.Cast(src, "text"))
		k.mirOrder = clickhouse.Func("toString", mir)
	default:
		return verifyKey{}, fmt.Errorf("drops/mirror: primary key %q has type %q; verification walks the key space in ranges and only integer, text and uuid keys order identically in PostgreSQL and ClickHouse",
			src.Name(), src.Type().TypeSQL())
	}
	return k, nil
}

// verifyCollateC pins a comparison to the bytewise C collation, which
// is the order ClickHouse compares strings in.
func verifyCollateC(e drops.Expression) drops.Expression {
	return drops.ExprFunc(func(b *drops.Builder) {
		b.WriteByte('(')
		b.Append(e)
		b.WriteString(` COLLATE "C")`)
	})
}

// verifyCursor is the exclusive lower bound of a range: the key in the
// text form the report uses, plus the value to bind against it.
type verifyCursor struct {
	set  bool
	key  string
	bind any
}

func (k verifyKey) cursor(key string) (verifyCursor, error) {
	bind, err := k.bind(key)
	if err != nil {
		return verifyCursor{}, err
	}
	return verifyCursor{set: true, key: key, bind: bind}, nil
}

// bind converts a text key into the value the comparison binds.
func (k verifyKey) bind(key string) (any, error) {
	if !k.numeric {
		return key, nil
	}
	n, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("drops/mirror: %q is not a valid value for integer primary key %q: %w", key, k.name, err)
	}
	return n, nil
}

func (k verifyKey) binds(keys []string) ([]any, error) {
	out := make([]any, 0, len(keys))
	for _, key := range keys {
		b, err := k.bind(key)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// text renders a scanned key value in the form the report carries.
//
// It goes through the same canonical encoding the checksum uses, so a
// key can never be reported in one form and compared in another. Every
// encoding is a one-byte type tag followed by the value's text, which
// is what is dropped here.
func (k verifyKey) text(v any) (string, error) {
	enc, err := canonicalValue(k.chType, v)
	if err != nil {
		return "", fmt.Errorf("primary key %q: %w", k.name, err)
	}
	if enc == verifyNullTag {
		return "", fmt.Errorf("primary key %q came back NULL, which no row can be addressed by", k.name)
	}
	return enc[1:], nil
}

// less orders two text keys the way both engines order the column.
func (k verifyKey) less(a, b string) bool {
	if !k.numeric {
		return a < b
	}
	x, errA := strconv.ParseInt(a, 10, 64)
	y, errB := strconv.ParseInt(b, 10, 64)
	if errA != nil || errB != nil {
		// Unreachable for keys this package produced — an integer
		// key always renders as digits — but a comparator has to be
		// total, and a text order is the only remaining answer.
		return a < b
	}
	return x < y
}

// --- checksums -------------------------------------------------------

// verifyNullTag encodes SQL NULL. It is a single NUL byte, which no
// other encoding begins with, so a NULL cannot be confused with the
// empty string that a naive encoding would give it.
const verifyNullTag = "\x00"

// verifyDigest hashes one row's compared columns.
//
// The column name goes into the hash alongside the value, and every
// field is length-prefixed, so neither a renamed column nor two
// adjacent values running together can produce the digest of a
// different row.
func verifyDigest(cols []verifyColumn, vals []any) ([32]byte, error) {
	buf := make([]byte, 0, 64*len(cols))
	for i, c := range cols {
		enc, err := canonicalValue(c.chType, vals[i])
		if err != nil {
			return [32]byte{}, fmt.Errorf("column %q: %w", c.name, err)
		}
		buf = verifyAppendField(buf, c.name)
		buf = verifyAppendField(buf, enc)
	}
	return sha256.Sum256(buf), nil
}

func verifyAppendField(dst []byte, s string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(s)))
	return append(dst, s...)
}

// verifyXor folds one row digest into a range checksum. XOR is used
// because it does not depend on the order the rows arrived in, which
// is the one thing about the two engines this package cannot make
// identical for every key type.
func verifyXor(sum *[32]byte, d [32]byte) {
	for i := range d {
		sum[i] ^= d[i]
	}
}

// canonicalValue encodes a value the way the comparison understands
// it, against the type the mirror column is declared with.
//
// The declared type is the schema of the comparison. Both sides are
// coerced to it before anything is hashed, which is what makes a value
// read through two different drivers, in two different Go types,
// compare equal when it is in fact the same value.
func canonicalValue(chType string, v any) (string, error) {
	rv := reflect.ValueOf(v)
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			return verifyNullTag, nil
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return verifyNullTag, nil
	}
	base, args := splitType(verifyUnwrapType(chType))
	switch base {
	case "Int8", "Int16", "Int32", "Int64", "Int128", "Int256",
		"UInt8", "UInt16", "UInt32", "UInt64", "UInt128", "UInt256":
		return canonicalInt(rv, chType)
	case "Float32":
		return canonicalFloat(rv, chType, 32)
	case "Float64":
		return canonicalFloat(rv, chType, 64)
	case "Bool":
		return canonicalBool(rv, chType)
	case "UUID":
		return canonicalUUID(rv, chType)
	case "Date", "Date32":
		return canonicalDate(rv, chType)
	case "DateTime", "DateTime64":
		return canonicalTimestamp(rv, chType)
	case "Decimal", "Decimal32", "Decimal64", "Decimal128", "Decimal256":
		return canonicalDecimal(rv, chType, base, args)
	case "String", "FixedString":
		return canonicalString(rv, chType)
	}
	return "", fmt.Errorf("no canonical encoding for mirrored type %q; exclude the column with SkipColumns or teach canonicalValue the type", chType)
}

// verifyUnwrapType strips the wrappers that do not change a value's
// text, leaving the type that decides the encoding.
func verifyUnwrapType(t string) string {
	for {
		base, inner := splitType(t)
		if base != "Nullable" && base != "LowCardinality" {
			return t
		}
		t = inner
	}
}

func canonicalInt(rv reflect.Value, chType string) (string, error) {
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "i" + strconv.FormatInt(rv.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "i" + strconv.FormatUint(rv.Uint(), 10), nil
	}
	if s, ok := verifyText(rv); ok {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return "", fmt.Errorf("a column declared %s came back as %q, which is not an integer", chType, s)
		}
		return "i" + strconv.FormatInt(n, 10), nil
	}
	return "", verifyTypeError(chType, "an integer", rv)
}

func canonicalFloat(rv reflect.Value, chType string, bits int) (string, error) {
	var f float64
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		f = rv.Float()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		f = float64(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		f = float64(rv.Uint())
	default:
		s, ok := verifyText(rv)
		if !ok {
			return "", verifyTypeError(chType, "a float", rv)
		}
		parsed, err := strconv.ParseFloat(s, bits)
		if err != nil {
			return "", fmt.Errorf("a column declared %s came back as %q, which is not a float", chType, s)
		}
		f = parsed
	}
	if bits == 32 {
		// A float4 reaches Go as float32 through one driver and as
		// the float64 that widened from it through another. Rounding
		// back through float32 makes the two encode alike.
		f = float64(float32(f))
	}
	return "f" + strconv.FormatFloat(f, 'g', -1, bits), nil
}

func canonicalBool(rv reflect.Value, chType string) (string, error) {
	switch rv.Kind() {
	case reflect.Bool:
		return verifyBoolTag(rv.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return verifyBoolTag(rv.Int() != 0), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// ClickHouse's Bool is a UInt8 underneath, and drivers that
		// do not map it back hand over the byte.
		return verifyBoolTag(rv.Uint() != 0), nil
	}
	if s, ok := verifyText(rv); ok {
		switch strings.ToLower(s) {
		case "t", "true", "1", "y", "yes":
			return verifyBoolTag(true), nil
		case "f", "false", "0", "n", "no":
			return verifyBoolTag(false), nil
		}
		return "", fmt.Errorf("a column declared %s came back as %q, which is not a boolean", chType, s)
	}
	return "", verifyTypeError(chType, "a boolean", rv)
}

func verifyBoolTag(b bool) string {
	if b {
		return "b1"
	}
	return "b0"
}

func canonicalUUID(rv reflect.Value, chType string) (string, error) {
	if b, ok := verifyBytes(rv); ok && len(b) == 16 {
		const hexdigits = "0123456789abcdef"
		out := make([]byte, 0, 36)
		for i, c := range b {
			if i == 4 || i == 6 || i == 8 || i == 10 {
				out = append(out, '-')
			}
			out = append(out, hexdigits[c>>4], hexdigits[c&0x0f])
		}
		return "u" + string(out), nil
	}
	s, ok := verifyText(rv)
	if !ok {
		return "", verifyTypeError(chType, "a uuid", rv)
	}
	bare := strings.ToLower(strings.NewReplacer("-", "", "{", "", "}", "").Replace(strings.TrimSpace(s)))
	if len(bare) != 32 {
		return "", fmt.Errorf("a column declared %s came back as %q, which is not a uuid", chType, s)
	}
	for i := 0; i < len(bare); i++ {
		if !verifyIsHex(bare[i]) {
			return "", fmt.Errorf("a column declared %s came back as %q, which is not a uuid", chType, s)
		}
	}
	return "u" + bare[0:8] + "-" + bare[8:12] + "-" + bare[12:16] + "-" + bare[16:20] + "-" + bare[20:32], nil
}

func verifyIsHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// canonicalDate encodes the calendar date as it stands. A date has no
// instant behind it to convert, and converting one would move it a day
// whenever a driver hands back midnight in a zone east of UTC.
func canonicalDate(rv reflect.Value, chType string) (string, error) {
	if t, ok := rv.Interface().(time.Time); ok {
		return "d" + t.Format("2006-01-02"), nil
	}
	s, ok := verifyText(rv)
	if !ok {
		return "", verifyTypeError(chType, "a time.Time", rv)
	}
	if len(s) >= 10 {
		if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
			return "d" + t.Format("2006-01-02"), nil
		}
	}
	return "", fmt.Errorf("a column declared %s came back as %q, which is not a date", chType, s)
}

// canonicalTimestamp encodes to microseconds, the resolution both
// PostgreSQL's timestamp types and the DateTime64(6) they map to
// carry, and always in UTC.
//
// Always, because DateTime and DateTime64 are a tick count since the
// epoch whatever their arguments say. A timezone argument names the
// zone the server renders the ticks in and parses text against; it
// does not make the column an instant, because the column already was
// one. Reading the argument as "this is an instant, that is a wall
// clock" and skipping the conversion for the second — which is what
// this did — compares the source's reading against the mirror's
// reading of the same tick in another zone, so every row of a
// timestamp column diverges on a ClickHouse server that is not on
// UTC. That is the false positive the Go-side encoding exists to
// prevent.
//
// canonicalDate is the genuine wall-clock case and is deliberately
// not converted: a Date has no instant behind it.
func canonicalTimestamp(rv reflect.Value, chType string) (string, error) {
	t, ok := rv.Interface().(time.Time)
	if !ok {
		s, isText := verifyText(rv)
		if !isText {
			return "", verifyTypeError(chType, "a time.Time", rv)
		}
		parsed, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return "", fmt.Errorf("a column declared %s came back as %q, which is not a timestamp", chType, s)
		}
		t = parsed
	}
	return "t" + t.UTC().Format("2006-01-02T15:04:05.000000"), nil
}

// canonicalDecimal encodes at the column's declared scale, so "12.3",
// "12.30" and 12.3 all reach the hash as the same digits. big.Rat
// carries the value exactly on the way, which a float64 would not.
func canonicalDecimal(rv reflect.Value, chType, base, args string) (string, error) {
	scale, err := verifyDecimalScale(chType, base, args)
	if err != nil {
		return "", err
	}
	var s string
	switch rv.Kind() {
	case reflect.Float32, reflect.Float64:
		s = strconv.FormatFloat(rv.Float(), 'f', -1, 64)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		s = strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s = strconv.FormatUint(rv.Uint(), 10)
	default:
		text, ok := verifyText(rv)
		if !ok {
			return "", verifyTypeError(chType, "a decimal", rv)
		}
		s = strings.TrimSpace(text)
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return "", fmt.Errorf("a column declared %s came back as %q, which is not a decimal", chType, s)
	}
	return "m" + r.FloatString(scale), nil
}

// verifyDecimalScale reads the number of decimal places a Decimal
// column keeps. Where that number sits differs between the two
// spellings ClickHouse accepts, which is the whole of the problem.
//
// Decimal(P, S) carries the precision first and the scale second.
// Decimal32(S) through Decimal256(S) fix the precision in the type
// name, so their sole argument is the scale. Taking the scale from
// after a comma alone would read every one-argument form as scale 0,
// round both sides to whole numbers, and hash 10.9999 and 10.5001
// alike — a verifier reporting a corrupt mirror as clean, which is the
// one failure it must not have. So the scale is required, and a type
// that does not carry one is an error rather than a silent zero.
// verifyMaxDecimalScale is ClickHouse's own ceiling: Decimal256 tops
// out at 76 digits of precision and the scale cannot exceed it.
const verifyMaxDecimalScale = 76

func verifyDecimalScale(chType, base, args string) (int, error) {
	text := strings.TrimSpace(args)
	i := strings.IndexByte(text, ',')
	switch {
	case base == "Decimal" && i >= 0:
		text = strings.TrimSpace(text[i+1:])
	case base == "Decimal":
		return 0, fmt.Errorf("mirrored type %q has no readable scale; Decimal takes a precision and a scale", chType)
	case i >= 0:
		return 0, fmt.Errorf("mirrored type %q has no readable scale; %s takes the scale as its only argument", chType, base)
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < 0 || n > verifyMaxDecimalScale {
		// Out of range is refused rather than clamped: a scale beyond
		// 76 is not a type ClickHouse would have accepted, so the
		// declaration is wrong and encoding at some other scale would
		// only hide it — while rendering at it would ask big.Rat for
		// a string as long as the number claims.
		return 0, fmt.Errorf("mirrored type %q has no readable scale", chType)
	}
	return n, nil
}

func canonicalString(rv reflect.Value, chType string) (string, error) {
	if s, ok := verifyText(rv); ok {
		return "s" + s, nil
	}
	return "", verifyTypeError(chType, "a string", rv)
}

// verifyText pulls the text out of whatever the driver returned:
// a string, a byte slice, or a type that knows how to print itself —
// which is how the uuid and decimal types drivers use travel, without
// this package depending on any of them.
func verifyText(rv reflect.Value) (string, bool) {
	if rv.Kind() == reflect.String {
		return rv.String(), true
	}
	if b, ok := verifyBytes(rv); ok {
		return string(b), true
	}
	if !rv.CanInterface() {
		return "", false
	}
	if s, ok := rv.Interface().(fmt.Stringer); ok {
		return s.String(), true
	}
	return "", false
}

func verifyBytes(rv reflect.Value) ([]byte, bool) {
	switch rv.Kind() {
	case reflect.Slice:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return rv.Bytes(), true
		}
	case reflect.Array:
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			out := make([]byte, rv.Len())
			for i := range out {
				out[i] = byte(rv.Index(i).Uint())
			}
			return out, true
		}
	}
	return nil, false
}

func verifyTypeError(chType, want string, rv reflect.Value) error {
	return fmt.Errorf("a column declared %s needs %s, but the driver returned %s", chType, want, rv.Type())
}
