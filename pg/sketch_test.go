package pg_test

import (
	"strconv"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestSketchNeverUndercounts(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{Width: 512, Depth: 5})
	truth := map[string]uint64{}
	for i := 0; i < 5000; i++ {
		v := "v" + strconv.Itoa(i%700)
		s.Add(v, 1)
		truth[v]++
	}
	// The guarantee that makes the structure safe here: an estimate
	// is an upper bound, so a caller is never told a predicate is
	// more selective than it is.
	for v, want := range truth {
		if got := s.Estimate(v); got < want {
			t.Fatalf("estimate for %s = %d, below the true count %d", v, got, want)
		}
	}
	if s.Total() != 5000 {
		t.Errorf("Total() = %d, want 5000", s.Total())
	}
}

// A wide sketch has to be close, or the number is not worth branching
// on.
func TestSketchIsAccurateForFrequentValues(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{})
	const hot = "pending"
	for i := 0; i < 100000; i++ {
		if i%4 == 0 {
			s.Add(hot, 1)
		} else {
			s.Add("v"+strconv.Itoa(i), 1)
		}
	}
	sel := s.Selectivity(hot)
	if sel < 0.24 || sel > 0.27 {
		t.Errorf("selectivity of a value in a quarter of rows = %v, want ~0.25", sel)
	}
	// And the answer a caller acts on: not worth an index.
	if sel <= 0.2 {
		t.Errorf("a quarter of the table reported as selective (%v)", sel)
	}
}

func TestSketchSelectivityOfARareValue(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{})
	for i := 0; i < 100000; i++ {
		s.Add("v"+strconv.Itoa(i%50000), 1)
	}
	if sel := s.Selectivity("v7"); sel > 0.01 {
		t.Errorf("a rare value reported at selectivity %v", sel)
	}
	if sel := s.Selectivity("never-added"); sel > 0.01 {
		t.Errorf("an absent value reported at selectivity %v", sel)
	}
}

func TestSketchEmptyIsSafe(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{})
	if s.Estimate("x") != 0 || s.Selectivity("x") != 0 || s.Total() != 0 {
		t.Error("an empty sketch reported something")
	}
	var nilS *pg.Sketch
	if nilS.Estimate("x") != 0 || nilS.Selectivity("x") != 0 || nilS.Total() != 0 ||
		nilS.DistinctEstimate() != 0 || nilS.ExpectedError() != 0 {
		t.Error("a nil sketch reported something")
	}
	// Adding nothing is not an event.
	s.Add("x", 0)
	if s.Total() != 0 {
		t.Error("Add(v, 0) counted")
	}
}

// Merging is what lets a sketch be built in parallel, and it has to
// be exact or that is not true.
func TestSketchMergeEqualsOnePass(t *testing.T) {
	opts := pg.SketchOptions{Width: 256, Depth: 4}
	whole := pg.NewSketch(opts)
	a := pg.NewSketch(opts)
	b := pg.NewSketch(opts)
	for i := 0; i < 2000; i++ {
		v := "v" + strconv.Itoa(i%300)
		whole.Add(v, 1)
		if i%2 == 0 {
			a.Add(v, 1)
		} else {
			b.Add(v, 1)
		}
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.Total() != whole.Total() {
		t.Fatalf("merged total %d, single-pass total %d", a.Total(), whole.Total())
	}
	for i := 0; i < 300; i++ {
		v := "v" + strconv.Itoa(i)
		if a.Estimate(v) != whole.Estimate(v) {
			t.Fatalf("%s: merged estimate %d, single-pass %d", v, a.Estimate(v), whole.Estimate(v))
		}
	}
	if err := a.Merge(nil); err != nil {
		t.Errorf("merging nil: %v", err)
	}
}

func TestSketchMergeRefusesMismatchedDimensions(t *testing.T) {
	a := pg.NewSketch(pg.SketchOptions{Width: 128, Depth: 4})
	b := pg.NewSketch(pg.SketchOptions{Width: 256, Depth: 4})
	if err := a.Merge(b); err == nil {
		t.Error("merged sketches of different shapes")
	}
}

func TestSketchRoundTrip(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{Width: 128, Depth: 3})
	for i := 0; i < 1000; i++ {
		s.Add("v"+strconv.Itoa(i%40), 2)
	}
	blob, err := s.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	var back pg.Sketch
	if err := back.UnmarshalBinary(blob); err != nil {
		t.Fatal(err)
	}
	if back.Total() != s.Total() || back.DistinctEstimate() != s.DistinctEstimate() {
		t.Errorf("header lost: total %d/%d", back.Total(), s.Total())
	}
	for i := 0; i < 40; i++ {
		v := "v" + strconv.Itoa(i)
		if back.Estimate(v) != s.Estimate(v) {
			t.Fatalf("%s: %d after round trip, %d before", v, back.Estimate(v), s.Estimate(v))
		}
	}
}

func TestSketchUnmarshalRejectsGarbage(t *testing.T) {
	var s pg.Sketch
	for _, bad := range [][]byte{
		nil,
		make([]byte, 10),
		make([]byte, 24),   // a zero-sized header
		make([]byte, 24+7), // not a whole number of counters
	} {
		if err := s.UnmarshalBinary(bad); err == nil {
			t.Errorf("accepted a %d-byte payload", len(bad))
		}
	}
	var nilS *pg.Sketch
	if _, err := nilS.MarshalBinary(); err == nil {
		t.Error("marshalled a nil sketch")
	}
}

// A value keys by its text form, so the sketch answers a query
// written with a different Go integer width than the driver scanned.
func TestSketchKeysByTextForm(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{})
	s.Add(int64(7), 10)
	if got := s.Estimate("7"); got < 10 {
		t.Errorf("estimate for the string form = %d, want at least 10", got)
	}
	if got := s.Estimate(7); got < 10 {
		t.Errorf("estimate for the int form = %d, want at least 10", got)
	}
}

func TestTopValues(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{})
	s.Add("pending", 100)
	s.Add("shipped", 900)
	s.Add("cancelled", 5)

	got := s.TopValues("pending", "shipped", "cancelled", "absent")
	if len(got) != 4 {
		t.Fatalf("got %d rows", len(got))
	}
	if got[0].Value != "shipped" {
		t.Errorf("first = %v, want shipped", got[0].Value)
	}
	if got[len(got)-1].Estimate != 0 {
		t.Errorf("an absent candidate estimated at %d", got[len(got)-1].Estimate)
	}
	if got[0].Selectivity < 0.85 {
		t.Errorf("selectivity of the dominant value = %v", got[0].Selectivity)
	}
}

func TestExpectedError(t *testing.T) {
	s := pg.NewSketch(pg.SketchOptions{Width: 100, Depth: 3})
	s.Add("x", 1000)
	if got := s.ExpectedError(); got != 10 {
		t.Errorf("ExpectedError() = %v, want 10", got)
	}
}
