package mirror_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/pg"
)

// --- fakes -----------------------------------------------------------

// vfyDriver answers each Query from a queue of canned result sets and
// records what it was asked to run. The shared trace records the order
// the two drivers were called in, which is itself a behaviour verify
// promises.
type vfyDriver struct {
	name  string
	trace *[]string
	sql   []string
	args  [][]any
	sets  [][][]any
	fail  error
}

func (d *vfyDriver) Query(_ context.Context, sql string, args ...any) (drops.Rows, error) {
	d.sql = append(d.sql, sql)
	d.args = append(d.args, args)
	if d.trace != nil {
		*d.trace = append(*d.trace, d.name)
	}
	if d.fail != nil {
		return nil, d.fail
	}
	if len(d.sets) == 0 {
		return &vfyRows{}, nil
	}
	set := d.sets[0]
	d.sets = d.sets[1:]
	return &vfyRows{data: set}, nil
}

func (d *vfyDriver) Exec(context.Context, string, ...any) (drops.Result, error) {
	return nil, errors.New("unexpected Exec")
}

func (d *vfyDriver) Begin(context.Context) (drops.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

type vfyRows struct {
	data [][]any
	i    int
}

func (r *vfyRows) Next() bool {
	if r.i >= len(r.data) {
		return false
	}
	r.i++
	return true
}

func (r *vfyRows) Scan(dest ...any) error {
	row := r.data[r.i-1]
	if len(dest) != len(row) {
		return fmt.Errorf("scan wants %d values, canned row has %d", len(dest), len(row))
	}
	for i, d := range dest {
		if err := vfyAssign(d, row[i]); err != nil {
			return fmt.Errorf("value %d: %w", i, err)
		}
	}
	return nil
}

func (r *vfyRows) Columns() ([]string, error) { return nil, nil }
func (r *vfyRows) Close() error               { return nil }
func (r *vfyRows) Err() error                 { return nil }

// vfyAssign copies a canned value into a scan target the way a driver
// would: an *any takes it as it is, a typed target takes the
// conversion.
func vfyAssign(dest, v any) error {
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("scan target %T is not a non-nil pointer", dest)
	}
	elem := rv.Elem()
	if v == nil {
		elem.Set(reflect.Zero(elem.Type()))
		return nil
	}
	vv := reflect.ValueOf(v)
	if elem.Kind() == reflect.Interface {
		elem.Set(vv)
		return nil
	}
	if !vv.Type().ConvertibleTo(elem.Type()) {
		return fmt.Errorf("cannot scan %T into %s", v, elem.Type())
	}
	elem.Set(vv.Convert(elem.Type()))
	return nil
}

// --- fixtures --------------------------------------------------------

var vfyAt = time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)

func vfyTable() *pg.Table {
	t := pg.NewTable("docs")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.Text("title").NotNull())
	pg.Add(t, pg.Integer("views").NotNull())
	pg.Add(t, pg.Numeric("price", 10, 2))
	pg.Add(t, pg.Timestamp("created_at", true).NotNull())
	return t
}

func vfySrc(id int64, title string, views int32, price any) []any {
	return []any{id, title, views, price, vfyAt}
}

func vfyMir(id int64, title string, views int32, price any, version uint64, deleted uint8) []any {
	return []any{id, title, views, price, vfyAt, version, deleted}
}

// vfyRig wires a verifier to two scripted drivers.
func vfyRig(t *testing.T, tbl *pg.Table, srcSets, mirSets [][][]any) (*mirror.Verifier, *vfyDriver, *vfyDriver, *[]string) {
	t.Helper()
	trace := &[]string{}
	sd := &vfyDriver{name: "source", trace: trace, sets: srcSets}
	md := &vfyDriver{name: "mirror", trace: trace, sets: mirSets}
	v, err := mirror.NewVerifier(pg.New(sd), tbl, clickhouse.New(md), derive(t, tbl))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v, sd, md, trace
}

func vfyRun(t *testing.T, v *mirror.Verifier) mirror.VerifyReport {
	t.Helper()
	rep, err := v.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return rep
}

// --- the clean case --------------------------------------------------

func TestVerifyReportsAnAlignedMirrorAsClean(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, "1.00"), vfySrc(2, "b", 2, nil)}
	mir := [][]any{vfyMir(1, "a", 1, "1.00", 10, 0), vfyMir(2, "b", 2, nil, 11, 0)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if !rep.Aligned() {
		t.Fatalf("aligned tables reported divergence: %+v", rep.Divergences)
	}
	if rep.RangesChecked != 1 || rep.RangesDiverged != 0 {
		t.Errorf("ranges checked/diverged = %d/%d want 1/0", rep.RangesChecked, rep.RangesDiverged)
	}
	if rep.SourceRows != 2 || rep.MirrorRows != 2 {
		t.Errorf("rows = %d source / %d mirror, want 2/2", rep.SourceRows, rep.MirrorRows)
	}
	// An empty Resume is the report saying it reached the end of the
	// key space, which is what makes the pass meaningful at all.
	if rep.Resume != "" {
		t.Errorf("Resume = %q, want empty for a completed walk", rep.Resume)
	}
}

func TestVerifyEmptyTablesAgree(t *testing.T) {
	v, _, _, _ := vfyRig(t, vfyTable(), nil, nil)
	rep := vfyRun(t, v)
	if !rep.Aligned() || rep.RangesChecked != 0 {
		t.Errorf("two empty tables should agree with nothing to check: %+v", rep)
	}
}

// --- reading the mirror ----------------------------------------------

func TestVerifyReadsTheMirrorThroughFinal(t *testing.T) {
	v, _, md, _ := vfyRig(t, vfyTable(), nil, nil)
	vfyRun(t, v)

	if len(md.sql) == 0 {
		t.Fatal("the mirror was never read")
	}
	if !strings.Contains(md.sql[0], "FINAL") {
		// Without FINAL a key that was updated twice appears twice
		// and every second row reads as a divergence.
		t.Errorf("mirror read is not FINAL: %s", md.sql[0])
	}
}

func TestVerifySelectsTheTombstoneRatherThanFilteringIt(t *testing.T) {
	v, _, md, _ := vfyRig(t, vfyTable(), nil, nil)
	vfyRun(t, v)

	// One mention: in the projection. A second would mean the marker
	// had been pushed into a predicate, which would throw away the
	// difference between "tombstoned" and "never arrived".
	if n := strings.Count(md.sql[0], mirror.DeletedColumn); n != 1 {
		t.Errorf("expected %s to be selected once and not filtered, saw it %d times: %s",
			mirror.DeletedColumn, n, md.sql[0])
	}
}

func TestVerifyReadsTheMirrorBeforeTheSource(t *testing.T) {
	v, _, _, trace := vfyRig(t, vfyTable(), nil, nil)
	vfyRun(t, v)

	if len(*trace) < 2 {
		t.Fatalf("expected both sides to be read, got %v", *trace)
	}
	// The source read must be the later of the two, so that every
	// difference is at least consistent with the mirror lagging.
	if (*trace)[0] != "mirror" || (*trace)[1] != "source" {
		t.Errorf("read order = %v, want mirror then source", (*trace)[:2])
	}
}

func TestVerifyIgnoresDefaultFilters(t *testing.T) {
	tbl := vfyTable()
	tbl.DefaultFilter(pg.Eq(tbl.Col("views"), 0))
	v, sd, _, _ := vfyRig(t, tbl, nil, nil)
	vfyRun(t, v)

	// A default filter is an application read scope. Applying it here
	// would hide source rows and report every one of them as extra in
	// the mirror.
	if strings.Contains(sd.sql[0], "WHERE") {
		t.Errorf("source read applied a default filter: %s", sd.sql[0])
	}
}

// --- tombstones ------------------------------------------------------

func TestVerifyTreatsATombstoneAsAbsence(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, nil)}
	mir := [][]any{vfyMir(1, "a", 1, nil, 10, 0), vfyMir(2, "gone", 0, nil, 11, 1)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if !rep.Aligned() {
		t.Fatalf("a tombstone for a key the source does not have is agreement, not divergence: %+v", rep.Divergences)
	}
	if rep.MirrorRows != 1 {
		t.Errorf("MirrorRows = %d, want 1: a tombstone is not a row", rep.MirrorRows)
	}
}

func TestVerifyReportsATombstonedKeyTheSourceStillHas(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil)}
	mir := [][]any{vfyMir(1, "a", 1, nil, 10, 0), vfyMir(2, "b", 2, nil, 99, 1)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if len(rep.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one", rep.Divergences)
	}
	d := rep.Divergences[0]
	if d.Key != "2" || d.Kind != mirror.MissingFromMirror {
		t.Errorf("divergence = %+v, want key 2 missing from the mirror", d)
	}
	if !d.Tombstoned {
		t.Error("the report should say the mirror holds a tombstone, not nothing at all")
	}
	if d.MirrorVersion != 99 {
		t.Errorf("MirrorVersion = %d, want 99", d.MirrorVersion)
	}
	if d.Direction() != mirror.MirrorBehind || rep.Behind != 1 {
		t.Errorf("direction = %s, behind = %d; want behind/1", d.Direction(), rep.Behind)
	}
}

// --- the three divergence kinds --------------------------------------

func TestVerifyReportsRowsMissingFromTheMirror(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil)}
	mir := [][]any{vfyMir(1, "a", 1, nil, 10, 0)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if rep.Behind != 1 || rep.Ahead != 0 {
		t.Fatalf("behind/ahead = %d/%d, want 1/0", rep.Behind, rep.Ahead)
	}
	if len(rep.Divergences) != 1 || rep.Divergences[0].Key != "2" ||
		rep.Divergences[0].Kind != mirror.MissingFromMirror {
		t.Errorf("divergences = %+v", rep.Divergences)
	}
	if rep.Divergences[0].Tombstoned {
		t.Error("there is no mirror row at all, so nothing is tombstoned")
	}
}

func TestVerifyReportsRowsOnlyTheMirrorHas(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, nil)}
	mir := [][]any{vfyMir(1, "a", 1, nil, 10, 0), vfyMir(2, "b", 2, nil, 11, 0)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if rep.Ahead != 1 || rep.Behind != 0 {
		t.Fatalf("behind/ahead = %d/%d, want 0/1", rep.Behind, rep.Ahead)
	}
	d := rep.Divergences[0]
	if d.Key != "2" || d.Kind != mirror.ExtraInMirror || d.Direction() != mirror.MirrorAhead {
		t.Errorf("divergence = %+v", d)
	}
}

func TestVerifyReportsValueMismatches(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil)}
	mir := [][]any{vfyMir(1, "a", 1, nil, 10, 0), vfyMir(2, "stale", 2, nil, 11, 0)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if len(rep.Divergences) != 1 {
		t.Fatalf("divergences = %+v, want one", rep.Divergences)
	}
	d := rep.Divergences[0]
	if d.Key != "2" || d.Kind != mirror.ValueMismatch {
		t.Errorf("divergence = %+v, want key 2 mismatched", d)
	}
	// Neither store is obviously the stale one when both hold the key.
	if d.Direction() != mirror.DirectionUnclear || rep.Unclear != 1 {
		t.Errorf("direction = %s, unclear = %d", d.Direction(), rep.Unclear)
	}
	if rep.Aligned() {
		t.Error("a report holding a divergence is not aligned")
	}
}

// --- the checksum ----------------------------------------------------

func vfyWideTable() *pg.Table {
	t := pg.NewTable("wide")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	pg.Add(t, pg.DoublePrecision("ratio").NotNull())
	pg.Add(t, pg.Boolean("draft").NotNull())
	pg.Add(t, pg.UUID("author_id"))
	pg.Add(t, pg.Numeric("price", 10, 2))
	pg.Add(t, pg.Timestamp("created_at", true).NotNull())
	pg.Add(t, pg.Text("note"))
	return t
}

// The whole comparison rests on this: the same value read back through
// two different drivers, in different Go types, in a different
// timezone and with different trailing zeros, must hash the same. If
// it does not, every verification of a healthy mirror reports
// divergence and the feature is worse than useless.
func TestVerifyChecksumIgnoresHowTheDriversRepresentAValue(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	src := [][]any{{
		int32(1),
		2.25,
		true,
		"3F2504E0-4F89-11D3-9A0C-0305E82C3301",
		[]byte("12.30"),
		time.Date(2026, 8, 19, 14, 0, 0, 0, cest),
		[]byte("hi"),
	}}
	mir := [][]any{{
		int64(1),
		float32(2.25),
		uint8(1),
		"3f2504e04f8911d39a0c0305e82c3301",
		"12.3",
		time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		"hi",
		uint64(7),
		uint8(0),
	}}
	v, _, _, _ := vfyRig(t, vfyWideTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if !rep.Aligned() {
		t.Fatalf("representation differences were reported as divergence: %+v", rep.Divergences)
	}
}

func TestVerifyChecksumStillCatchesARealDifference(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	src := [][]any{{
		int32(1), 2.25, true, "3F2504E0-4F89-11D3-9A0C-0305E82C3301",
		[]byte("12.30"), time.Date(2026, 8, 19, 14, 0, 0, 0, cest), []byte("hi"),
	}}
	mir := [][]any{{
		int64(1), float32(2.25), uint8(1), "3f2504e04f8911d39a0c0305e82c3301",
		"12.31", time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC), "hi",
		uint64(7), uint8(0),
	}}
	v, _, _, _ := vfyRig(t, vfyWideTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if rep.Unclear != 1 {
		t.Fatalf("one cent of difference went unreported: %+v", rep)
	}
}

// NULL and the empty string are different values, and an encoding that
// let them collide would hide a whole class of mirroring bug.
func TestVerifyDistinguishesNullFromTheEmptyString(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	src := [][]any{{
		int32(1), 2.25, true, nil, nil,
		time.Date(2026, 8, 19, 14, 0, 0, 0, cest), nil,
	}}
	mir := [][]any{{
		int64(1), 2.25, true, nil, nil,
		time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC), "",
		uint64(7), uint8(0),
	}}
	v, _, _, _ := vfyRig(t, vfyWideTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	if rep.Unclear != 1 {
		t.Fatalf("NULL and \"\" compared equal: %+v", rep)
	}
}

// The fold is XOR precisely so that the verdict cannot depend on the
// order rows came back in — the one thing the two engines are not made
// to agree on for every key type.
func TestVerifyRangeChecksumDoesNotDependOnRowOrder(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil), vfySrc(3, "c", 3, nil)}
	mir := [][]any{vfyMir(3, "c", 3, nil, 12, 0), vfyMir(1, "a", 1, nil, 10, 0), vfyMir(2, "b", 2, nil, 11, 0)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	if rep := vfyRun(t, v); !rep.Aligned() {
		t.Fatalf("row order changed the verdict: %+v", rep.Divergences)
	}
}

// --- ranges ----------------------------------------------------------

func TestVerifyNarrowsOnlyTheRangesThatDisagree(t *testing.T) {
	srcSets := [][][]any{
		{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil)},
		{vfySrc(3, "c", 3, nil), vfySrc(4, "d", 4, nil)},
		{vfySrc(5, "e", 5, nil)},
	}
	mirSets := [][][]any{
		{vfyMir(1, "a", 1, nil, 10, 0), vfyMir(2, "b", 2, nil, 11, 0)},
		{vfyMir(3, "WRONG", 3, nil, 12, 0), vfyMir(4, "d", 4, nil, 13, 0)},
		{vfyMir(5, "e", 5, nil, 14, 0)},
	}
	v, _, _, _ := vfyRig(t, vfyTable(), srcSets, mirSets)
	var seen []mirror.VerifyRange
	v.RangeSize(2).OnRange(func(r mirror.VerifyRange) { seen = append(seen, r) })

	rep := vfyRun(t, v)
	if rep.RangesChecked != 3 || rep.RangesDiverged != 1 {
		t.Fatalf("ranges checked/diverged = %d/%d, want 3/1", rep.RangesChecked, rep.RangesDiverged)
	}
	if len(seen) != 3 {
		t.Fatalf("OnRange fired %d times, want 3", len(seen))
	}
	if !seen[0].Matched || seen[1].Matched || !seen[2].Matched {
		t.Errorf("range verdicts = %v/%v/%v, want matched/diverged/matched",
			seen[0].Matched, seen[1].Matched, seen[2].Matched)
	}
	// A matched range is decided by counts and checksums alone; only
	// the failing one is narrowed to a key.
	if seen[0].SourceDigest != seen[0].MirrorDigest {
		t.Errorf("a matched range must have equal checksums: %+v", seen[0])
	}
	if seen[1].SourceDigest == seen[1].MirrorDigest {
		t.Errorf("a diverged range must have different checksums: %+v", seen[1])
	}
	if seen[1].From != "2" || seen[1].Through != "4" {
		t.Errorf("range bounds = (%q, %q], want (2, 4]", seen[1].From, seen[1].Through)
	}
	if len(rep.Divergences) != 1 || rep.Divergences[0].Key != "3" {
		t.Errorf("divergences = %+v, want key 3 alone", rep.Divergences)
	}
}

func TestVerifyRangeEndsAtTheShorterSide(t *testing.T) {
	// The mirror is missing key 2, so its page of two reaches key 3
	// while the source's reaches key 2. The range has to stop at 2,
	// or key 3 would be compared against a source page that never
	// covered it and would be reported missing.
	srcSets := [][][]any{
		{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil)},
		{vfySrc(3, "c", 3, nil)},
	}
	mirSets := [][][]any{
		{vfyMir(1, "a", 1, nil, 10, 0), vfyMir(3, "c", 3, nil, 12, 0)},
		{vfyMir(3, "c", 3, nil, 12, 0)},
	}
	v, _, _, _ := vfyRig(t, vfyTable(), srcSets, mirSets)
	v.RangeSize(2)

	rep := vfyRun(t, v)
	if len(rep.Divergences) != 1 || rep.Divergences[0].Key != "2" {
		t.Fatalf("divergences = %+v, want key 2 alone", rep.Divergences)
	}
	if rep.Behind != 1 {
		t.Errorf("behind = %d, want 1", rep.Behind)
	}
}

func TestVerifyResumesFromAKey(t *testing.T) {
	v, sd, md, _ := vfyRig(t, vfyTable(), nil, nil)
	v.From("41")
	vfyRun(t, v)

	for name, args := range map[string][][]any{"source": sd.args, "mirror": md.args} {
		if len(args) == 0 || len(args[0]) == 0 {
			t.Fatalf("%s query bound no arguments", name)
		}
		if args[0][0] != int64(41) {
			t.Errorf("%s lower bound = %v (%T), want int64(41)", name, args[0][0], args[0][0])
		}
	}
}

func TestVerifyReportsAResumePointWhenInterrupted(t *testing.T) {
	srcSets := [][][]any{
		{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil)},
		{vfySrc(3, "c", 3, nil)},
	}
	mirSets := [][][]any{
		{vfyMir(1, "a", 1, nil, 10, 0), vfyMir(2, "b", 2, nil, 11, 0)},
		{vfyMir(3, "c", 3, nil, 12, 0)},
	}
	v, _, _, _ := vfyRig(t, vfyTable(), srcSets, mirSets)
	ctx, cancel := context.WithCancel(context.Background())
	v.RangeSize(2).OnRange(func(mirror.VerifyRange) { cancel() })

	rep, err := v.Verify(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if rep.Resume != "2" {
		t.Errorf("Resume = %q, want the last fully compared key 2", rep.Resume)
	}
}

func TestVerifySurfacesReadFailures(t *testing.T) {
	trace := &[]string{}
	tbl := vfyTable()
	boom := errors.New("clickhouse is down")
	md := &vfyDriver{name: "mirror", trace: trace, fail: boom}
	v, err := mirror.NewVerifier(pg.New(&vfyDriver{name: "source", trace: trace}), tbl,
		clickhouse.New(md), derive(t, tbl))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	if _, err := v.Verify(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want the driver failure wrapped", err)
	}
}

// --- the report ------------------------------------------------------

func TestVerifyCapsTheNamedKeysButNotTheCounts(t *testing.T) {
	src := [][]any{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil), vfySrc(3, "c", 3, nil)}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, nil)
	v.MaxDivergences(1)

	rep := vfyRun(t, v)
	if len(rep.Divergences) != 1 || !rep.Truncated {
		t.Fatalf("divergences = %d, truncated = %v; want 1 and true", len(rep.Divergences), rep.Truncated)
	}
	// The counts are the complete picture even when the list is not.
	if rep.Behind != 3 {
		t.Errorf("behind = %d, want all 3 counted", rep.Behind)
	}
}

// --- skipping columns ------------------------------------------------

func TestVerifySkipColumnsExcludesAColumnFromTheComparison(t *testing.T) {
	src := [][]any{{int64(1), "a", int32(1), vfyAt}}
	mir := [][]any{{int64(1), "a", int32(1), vfyAt, uint64(10), uint8(0)}}
	v, sd, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})
	v.SkipColumns("price")

	rep := vfyRun(t, v)
	if !rep.Aligned() {
		t.Fatalf("skipped column still compared: %+v", rep.Divergences)
	}
	if strings.Contains(sd.sql[0], `"price"`) {
		t.Errorf("a skipped column was still read: %s", sd.sql[0])
	}
}

func TestVerifySkipColumnsRejectsNamesItCannotHonour(t *testing.T) {
	t.Run("unknown column", func(t *testing.T) {
		v, _, _, _ := vfyRig(t, vfyTable(), nil, nil)
		v.SkipColumns("nope")
		if _, err := v.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Errorf("err = %v, want it to name the unknown column", err)
		}
	})
	t.Run("primary key", func(t *testing.T) {
		v, _, _, _ := vfyRig(t, vfyTable(), nil, nil)
		v.SkipColumns("id")
		if _, err := v.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "primary key") {
			t.Errorf("err = %v, want a refusal to drop the key", err)
		}
	})
}

// --- recheck ---------------------------------------------------------

func TestRecheckDischargesLagAndSettlesAStuckKey(t *testing.T) {
	prior := []mirror.Divergence{
		{Key: "1", Kind: mirror.MissingFromMirror},
		{Key: "2", Kind: mirror.ValueMismatch, MirrorVersion: 5},
		{Key: "3", Kind: mirror.ValueMismatch, MirrorVersion: 7},
	}
	src := [][]any{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil), vfySrc(3, "c", 3, nil)}
	mir := [][]any{
		vfyMir(1, "a", 1, nil, 20, 0),      // caught up
		vfyMir(2, "stale", 2, nil, 5, 0),   // unchanged and still wrong
		vfyMir(3, "another", 3, nil, 9, 0), // still wrong, but the mirror moved
	}
	v, _, md, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep, err := v.Recheck(context.Background(), prior)
	if err != nil {
		t.Fatalf("Recheck: %v", err)
	}
	if len(rep.Divergences) != 2 {
		t.Fatalf("divergences = %+v, want keys 2 and 3", rep.Divergences)
	}
	byKey := map[string]mirror.Divergence{}
	for _, d := range rep.Divergences {
		byKey[d.Key] = d
	}
	if _, ok := byKey["1"]; ok {
		t.Error("a key that now agrees must be dropped, not carried forward")
	}
	if !byKey["2"].Settled {
		t.Error("key 2's mirror version did not move, so it should be settled")
	}
	if byKey["3"].Settled {
		t.Error("key 3's mirror version moved, so lag still explains it")
	}
	if rep.Settled != 1 {
		t.Errorf("Settled = %d, want 1", rep.Settled)
	}
	if !strings.Contains(md.sql[0], "IN (") {
		t.Errorf("recheck should read the named keys directly: %s", md.sql[0])
	}
}

func TestRecheckRejectsAKeylessDivergence(t *testing.T) {
	v, _, _, _ := vfyRig(t, vfyTable(), nil, nil)
	if _, err := v.Recheck(context.Background(), []mirror.Divergence{{Kind: mirror.ValueMismatch}}); err == nil {
		t.Error("expected a rejection: a divergence with no key names nothing to re-read")
	}
}

// --- key spaces ------------------------------------------------------

func TestVerifyOrdersUUIDKeysTheSameWayInBothEngines(t *testing.T) {
	tbl := pg.NewTable("things")
	pg.Add(tbl, pg.UUID("id").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	v, sd, md, _ := vfyRig(t, tbl, nil, nil)
	vfyRun(t, v)

	// PostgreSQL orders uuid by its own byte layout and ClickHouse by
	// another; the canonical text under the C collation is the one
	// order both can be made to produce.
	if !strings.Contains(sd.sql[0], `COLLATE "C"`) || !strings.Contains(sd.sql[0], "::text") {
		t.Errorf("source read does not pin the uuid order: %s", sd.sql[0])
	}
	if !strings.Contains(md.sql[0], "toString(") {
		t.Errorf("mirror read does not pin the uuid order: %s", md.sql[0])
	}
}

func TestVerifyOrdersTextKeysUnderTheCCollation(t *testing.T) {
	tbl := pg.NewTable("slugs")
	pg.Add(tbl, pg.Text("slug").PrimaryKey())
	pg.Add(tbl, pg.Text("name").NotNull())
	v, sd, _, _ := vfyRig(t, tbl, nil, nil)
	vfyRun(t, v)

	if !strings.Contains(sd.sql[0], `COLLATE "C"`) {
		t.Errorf("a text key must be ordered bytewise to match ClickHouse: %s", sd.sql[0])
	}
}

func TestVerifyIntegerKeysAreComparedNumerically(t *testing.T) {
	// Text order would put 10 before 9 on one side of the range
	// boundary and not the other.
	srcSets := [][][]any{
		{vfySrc(9, "a", 1, nil), vfySrc(10, "b", 2, nil)},
		{},
	}
	mirSets := [][][]any{
		{vfyMir(9, "a", 1, nil, 10, 0), vfyMir(10, "b", 2, nil, 11, 0)},
		{},
	}
	v, sd, _, _ := vfyRig(t, vfyTable(), srcSets, mirSets)
	v.RangeSize(2)

	rep := vfyRun(t, v)
	if !rep.Aligned() {
		t.Fatalf("numeric keys compared as text: %+v", rep.Divergences)
	}
	// Agreement alone proves nothing here: a comparator that puts "10"
	// before "9" makes the range trim throw away both sides' rows, and
	// a range holding nothing agrees with itself. The counts are what
	// says the rows were actually compared.
	if rep.SourceRows != 2 || rep.MirrorRows != 2 {
		t.Errorf("rows compared = %d source / %d mirror, want 2/2", rep.SourceRows, rep.MirrorRows)
	}
	if len(sd.args) < 2 || sd.args[1][0] != int64(10) {
		t.Errorf("range boundary = %v, want the numerically largest key 10", sd.args[1])
	}
}

// --- construction ----------------------------------------------------

func TestNewVerifierValidates(t *testing.T) {
	tbl := vfyTable()
	mir := derive(t, tbl)
	db := pg.New(&vfyDriver{})
	ch := clickhouse.New(&vfyDriver{})

	t.Run("nil arguments", func(t *testing.T) {
		if _, err := mirror.NewVerifier(nil, tbl, ch, mir); err == nil {
			t.Error("expected a rejection for a nil source db")
		}
		if _, err := mirror.NewVerifier(db, tbl, ch, nil); err == nil {
			t.Error("expected a rejection for a nil mirror table")
		}
	})

	t.Run("underived mirror table", func(t *testing.T) {
		plain := clickhouse.NewTable("plain")
		clickhouse.Add(plain, clickhouse.Int64("id"))
		_, err := mirror.NewVerifier(db, tbl, ch, plain)
		if err == nil || !strings.Contains(err.Error(), mirror.VersionColumn) {
			t.Errorf("err = %v, want it to name the missing bookkeeping column", err)
		}
	})

	t.Run("no primary key", func(t *testing.T) {
		nokey := pg.NewTable("nokey")
		pg.Add(nokey, pg.Text("a"))
		if _, err := mirror.NewVerifier(db, nokey, ch, mir); err == nil {
			t.Error("expected a rejection: there is no key space to walk")
		}
	})

	t.Run("composite primary key", func(t *testing.T) {
		comp := pg.NewTable("comp")
		pg.Add(comp, pg.BigInt("a").PrimaryKey())
		pg.Add(comp, pg.BigInt("b").PrimaryKey())
		if _, err := mirror.NewVerifier(db, comp, ch, mir); err == nil {
			t.Error("expected a rejection for a composite key")
		}
	})

	t.Run("mirror missing a source column", func(t *testing.T) {
		wider := vfyTable()
		pg.Add(wider, pg.Text("added_later"))
		_, err := mirror.NewVerifier(db, wider, ch, mir)
		if err == nil || !strings.Contains(err.Error(), "added_later") {
			t.Errorf("err = %v, want it to name the column the mirror lacks", err)
		}
	})

	t.Run("key type with no shared order", func(t *testing.T) {
		odd := pg.NewTable("odd")
		pg.Add(odd, pg.Numeric("id", 10, 2).PrimaryKey())
		pg.Add(odd, pg.Text("name").NotNull())
		_, err := mirror.NewVerifier(db, odd, ch, derive(t, odd))
		if err == nil || !strings.Contains(err.Error(), "numeric") {
			t.Errorf("err = %v, want it to name the key type it cannot order", err)
		}
	})

	t.Run("source column colliding with bookkeeping", func(t *testing.T) {
		collide := pg.NewTable("collide")
		pg.Add(collide, pg.BigSerial("id").PrimaryKey())
		pg.Add(collide, pg.BigInt(mirror.VersionColumn))
		_, err := mirror.NewVerifier(db, collide, ch, mir)
		if err == nil || !strings.Contains(err.Error(), mirror.VersionColumn) {
			t.Errorf("err = %v, want a collision error", err)
		}
	})
}

// --- the encoding schema ---------------------------------------------

// vfyRigTables wires a verifier to two scripted drivers over a mirror
// table the caller declared rather than derived. NewVerifier accepts
// any mirror whose columns line up with the source's, so the types the
// comparison is encoded against are not limited to the ones
// DeriveClickHouse emits.
func vfyRigTables(t *testing.T, src *pg.Table, mir *clickhouse.Table, srcSets, mirSets [][][]any) (*mirror.Verifier, *vfyDriver, *vfyDriver) {
	t.Helper()
	sd := &vfyDriver{name: "source", sets: srcSets}
	md := &vfyDriver{name: "mirror", sets: mirSets}
	v, err := mirror.NewVerifier(pg.New(sd), src, clickhouse.New(md), mir)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v, sd, md
}

// vfyDecimalTables declares a one-column mirror whose decimal is
// spelled with the scale as its only argument, which is the form
// DeriveClickHouse never emits and canonicalValue nonetheless claims
// to support.
func vfyDecimalTables(t *testing.T, chType string) (*pg.Table, *clickhouse.Table) {
	t.Helper()
	src := pg.NewTable("prices")
	pg.Add(src, pg.BigSerial("id").PrimaryKey())
	pg.Add(src, pg.Numeric("amount", 18, 4).NotNull())

	mir := clickhouse.NewTable("prices")
	clickhouse.Add(mir, clickhouse.Custom[int64]("id", "Int64"))
	clickhouse.Add(mir, clickhouse.Custom[string]("amount", chType))
	clickhouse.Add(mir, clickhouse.UInt64(mirror.VersionColumn))
	clickhouse.Add(mir, clickhouse.UInt8(mirror.DeletedColumn))
	return src, mir
}

// Decimal64(4) keeps four decimal places: the scale is the sole
// argument, not the part after a comma. Reading it as scale 0 rounds
// both sides to whole numbers before hashing, so two genuinely
// different amounts encode alike and the verifier calls a corrupt
// mirror clean — the one failure a verifier must never have.
func TestVerifyReadsTheScaleOfADecimalDeclaredWithOneArgument(t *testing.T) {
	src, mir := vfyDecimalTables(t, "Decimal64(4)")

	t.Run("a difference below the scale is a divergence", func(t *testing.T) {
		v, _, _ := vfyRigTables(t, src, mir,
			[][][]any{{{int64(1), "10.9999"}}},
			[][][]any{{{int64(1), "10.5001", uint64(7), uint8(0)}}})
		rep := vfyRun(t, v)
		if rep.Unclear != 1 {
			t.Fatalf("10.9999 and 10.5001 compared equal in a Decimal64(4) column: %+v", rep)
		}
	})

	t.Run("the same value spelled two ways still agrees", func(t *testing.T) {
		v, _, _ := vfyRigTables(t, src, mir,
			[][][]any{{{int64(1), "10.9999"}}},
			[][][]any{{{int64(1), "10.99990", uint64(7), uint8(0)}}})
		rep := vfyRun(t, v)
		if !rep.Aligned() {
			t.Fatalf("trailing zeros at the declared scale reported as divergence: %+v", rep.Divergences)
		}
	})
}

// A scale is required, not defaulted. Silently encoding at scale 0 is
// what made the one-argument forms compare wrong in the first place,
// so a type that carries no readable scale is an error naming itself.
func TestVerifyRefusesADecimalWithNoScale(t *testing.T) {
	src, mir := vfyDecimalTables(t, "Decimal64")
	v, _, _ := vfyRigTables(t, src, mir,
		[][][]any{{{int64(1), "10.9999"}}},
		[][][]any{{{int64(1), "10.9999", uint64(7), uint8(0)}}})
	_, err := v.Verify(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Decimal64") {
		t.Errorf("err = %v, want a refusal naming the type it cannot read a scale from", err)
	}
}

func vfyInstantTable() *pg.Table {
	t := pg.NewTable("events")
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	// timestamp without time zone, which MapType renders as
	// DateTime64(6) — no timezone argument.
	pg.Add(t, pg.Timestamp("happened_at", false).NotNull())
	return t
}

// DateTime64 is a tick count since the epoch whatever its arguments
// say; a missing timezone argument means "render in the server
// timezone", not "this is a wall clock". A ClickHouse server that is
// not on UTC therefore hands back the same instant as a different
// reading, and comparing the readings makes every row of every
// timestamp column diverge — a false positive on a perfect mirror,
// which is the whole reason the encoding happens in Go.
func TestVerifyComparesATimestampWithoutATimezoneArgumentAsAnInstant(t *testing.T) {
	rome := time.FixedZone("CEST", 2*60*60)
	tbl := vfyInstantTable()

	t.Run("one instant read in two zones agrees", func(t *testing.T) {
		src := [][]any{{int64(1), time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)}}
		mir := [][]any{{int64(1), time.Date(2026, 8, 19, 14, 0, 0, 0, rome), uint64(7), uint8(0)}}
		v, _, _, _ := vfyRig(t, tbl, [][][]any{src}, [][][]any{mir})
		rep := vfyRun(t, v)
		if !rep.Aligned() {
			t.Fatalf("a mirror on a server east of UTC reported as divergent: %+v", rep.Divergences)
		}
	})

	t.Run("two instants still disagree", func(t *testing.T) {
		src := [][]any{{int64(1), time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)}}
		mir := [][]any{{int64(1), time.Date(2026, 8, 19, 14, 0, 0, 0, time.UTC), uint64(7), uint8(0)}}
		v, _, _, _ := vfyRig(t, tbl, [][][]any{src}, [][][]any{mir})
		rep := vfyRun(t, v)
		if rep.Unclear != 1 {
			t.Fatalf("two hours of difference went unreported: %+v", rep)
		}
	})
}

// --- the mirror read scope -------------------------------------------

// The filter a mirror table is most likely to carry is NotDeleted, and
// it is the one that must not reach this read: it hides the tombstones,
// which turns "the delete was applied and should not have been" into
// "the insert never arrived" and throws away the distinction
// Divergence.Tombstoned exists to carry.
func TestVerifyIgnoresTheMirrorsDefaultFilters(t *testing.T) {
	tbl := vfyTable()
	mir := derive(t, tbl)
	mir.DefaultFilter(mirror.NotDeleted(mir))
	v, _, md := vfyRigTables(t, tbl, mir, nil, nil)
	vfyRun(t, v)

	if len(md.sql) == 0 {
		t.Fatal("the mirror was never read")
	}
	if strings.Contains(md.sql[0], "WHERE") {
		t.Errorf("mirror read applied a default filter: %s", md.sql[0])
	}
	// One mention, in the projection. A second is the filter's.
	if n := strings.Count(md.sql[0], mirror.DeletedColumn); n != 1 {
		t.Errorf("expected %s to be selected once and not filtered, saw it %d times: %s",
			mirror.DeletedColumn, n, md.sql[0])
	}
}

// --- the key order the ranges are cut by ------------------------------

// A range is cut at a key, and both sides are trimmed to it. If the
// trim orders keys differently from the reads, the trim can throw away
// every row of both sides — and a range with no rows on either side
// agrees with itself. The verdict is then "clean" over data neither
// side was asked about.
func TestVerifyComparesTheRowsOfABoundedRangeRatherThanTrimmingThemAway(t *testing.T) {
	// Keys 9 and 10 with a range size of 2: the range ends at 10, and
	// under a text order "10" sorts before "9", so a text trim drops
	// both rows from both sides.
	srcSets := [][][]any{
		{vfySrc(9, "a", 1, nil), vfySrc(10, "b", 2, nil)},
		{},
	}
	mirSets := [][][]any{
		{vfyMir(9, "WRONG", 1, nil, 10, 0), vfyMir(10, "b", 2, nil, 11, 0)},
		{},
	}
	v, _, _, _ := vfyRig(t, vfyTable(), srcSets, mirSets)
	v.RangeSize(2)

	rep := vfyRun(t, v)
	if rep.SourceRows != 2 || rep.MirrorRows != 2 {
		t.Fatalf("rows compared = %d source / %d mirror, want 2/2: a range that keeps no rows agrees vacuously",
			rep.SourceRows, rep.MirrorRows)
	}
	if len(rep.Divergences) != 1 || rep.Divergences[0].Key != "9" ||
		rep.Divergences[0].Kind != mirror.ValueMismatch {
		t.Fatalf("divergences = %+v, want key 9 mismatched", rep.Divergences)
	}
}

// --- resuming ---------------------------------------------------------

// The documented resume loop feeds Resume back to From on every error.
// A pass that fails on its first range has compared nothing, but it has
// not un-compared what came before it either: reporting an empty Resume
// there sends the loop back to the head of the key space, so a table
// large enough to need resuming re-scans the same prefix on every
// transient read error and never reaches its tail.
func TestVerifyKeepsTheCallersStartingPointWhenTheFirstRangeFails(t *testing.T) {
	tbl := vfyTable()
	boom := errors.New("clickhouse is down")
	v, err := mirror.NewVerifier(
		pg.New(&vfyDriver{name: "source"}), tbl,
		clickhouse.New(&vfyDriver{name: "mirror", fail: boom}), derive(t, tbl))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	v.From("500000")

	rep, err := v.Verify(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the driver failure wrapped", err)
	}
	if rep.Resume != "500000" {
		t.Errorf("Resume = %q, want the caller's own starting point 500000; empty means the walk finished", rep.Resume)
	}
	if rep.RangesChecked != 0 {
		t.Errorf("RangesChecked = %d, want 0: nothing was compared", rep.RangesChecked)
	}
}

// --- what wrote the mirror row ---------------------------------------

// A version says more than whether the row moved: which band it is in
// says what put it there, and that decides which repair can work. A
// seed-band row means the change stream has never covered the key.
func TestDivergenceOriginNamesTheBandTheMirrorVersionIsIn(t *testing.T) {
	seeded := mirror.SeedVersionAt(time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC))
	streamed := mirror.LiveVersion(4242)

	src := [][]any{vfySrc(1, "a", 1, nil), vfySrc(2, "b", 2, nil), vfySrc(3, "c", 3, nil)}
	mir := [][]any{
		vfyMir(1, "filled", 1, nil, seeded, 0),
		vfyMir(2, "streamed", 2, nil, streamed, 0),
		// key 3 has no mirror row at all.
	}
	v, _, _, _ := vfyRig(t, vfyTable(), [][][]any{src}, [][][]any{mir})

	rep := vfyRun(t, v)
	got := map[string]mirror.VersionOrigin{}
	for _, d := range rep.Divergences {
		got[d.Key] = d.Origin()
	}
	want := map[string]mirror.VersionOrigin{
		"1": mirror.OriginFill,
		"2": mirror.OriginStream,
		"3": mirror.OriginAbsent,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("origins = %v, want %v", got, want)
	}
	// The clock band is the third: a source with no sequence of its
	// own, or a row written before the bands existed.
	clocked := mirror.Divergence{MirrorVersion: mirror.ClockVersion(vfyAt)}
	if clocked.Origin() != mirror.OriginClock {
		t.Errorf("a wall-clock version reads as %s, want %s", clocked.Origin(), mirror.OriginClock)
	}
}
