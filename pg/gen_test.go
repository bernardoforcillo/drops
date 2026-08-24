package pg_test

import (
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/pg"
)

// draw exercises every generator in a fixed order, so two Gens agree
// only if all of them agree.
func draw(g *pg.Gen) []string {
	lo := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	out := []string{
		g.FullName(),
		g.Email(),
		g.Sentence(),
		g.Paragraph(2),
		g.Words(3),
		g.TimeBetween(lo, hi).Format(time.RFC3339Nano),
		pg.Pick(g, "alpha", "beta", "gamma"),
		pg.Weighted(g,
			pg.Choice[string]{Value: "common", Weight: 9},
			pg.Choice[string]{Value: "rare", Weight: 1}),
	}
	for i := 0; i < 8; i++ {
		out = append(out,
			time.Duration(g.IntRange(1, 100)).String(),
			time.Duration(g.IntN(1000)).String(),
		)
		if v := pg.Maybe(g, 0.5, "present"); v != nil {
			out = append(out, *v)
		} else {
			out = append(out, "<nil>")
		}
		if g.Chance(0.3) {
			out = append(out, "hit")
		}
		if g.Bool() {
			out = append(out, "true")
		}
	}
	xs := []int{1, 2, 3, 4, 5, 6, 7, 8}
	pg.Shuffle(g, xs)
	for _, x := range xs {
		out = append(out, time.Duration(x).String())
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The property the whole file exists for: an integer is enough to
// reproduce a run.
func TestGenIsReproducibleFromItsSeed(t *testing.T) {
	if !eq(draw(pg.NewGen(42)), draw(pg.NewGen(42))) {
		t.Error("two Gens with seed 42 produced different values")
	}
	if eq(draw(pg.NewGen(42)), draw(pg.NewGen(43))) {
		t.Error("seeds 42 and 43 produced the same values — the seed is not reaching the PRNG")
	}
}

func TestGenResetRewinds(t *testing.T) {
	g := pg.NewGen(7)
	first := draw(g)
	if eq(first, draw(g)) {
		t.Fatal("a Gen repeated itself without a Reset")
	}
	g.Reset()
	if !eq(first, draw(g)) {
		t.Error("Reset did not rewind the Gen to its seed")
	}
	if g.Seed() != 7 {
		t.Errorf("Seed(): got %d, want 7", g.Seed())
	}
}

// A generator that collides on a UNIQUE index is a generator whose
// output cannot be inserted, so the address carries a counter.
func TestGenEmailsAreDistinctWithinARun(t *testing.T) {
	g := pg.NewGen(3)
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		e := g.Email()
		if seen[e] {
			t.Fatalf("duplicate email %q after %d draws", e, i)
		}
		seen[e] = true
	}
	// And the counter is Gen state, so it rewinds with everything else
	// rather than making a rewound run differ from the first.
	g.Reset()
	if first := g.Email(); !seen[first] {
		t.Errorf("after Reset the first email was %q, which the first run never produced", first)
	}
}

func TestGenRanges(t *testing.T) {
	g := pg.NewGen(11)
	for i := 0; i < 1000; i++ {
		if n := g.IntRange(2, 5); n < 2 || n > 5 {
			t.Fatalf("IntRange(2,5) returned %d", n)
		}
		if f := g.Float64Range(-1, 1); f < -1 || f >= 1 {
			t.Fatalf("Float64Range(-1,1) returned %f", f)
		}
	}
	// Both ends are reachable — an exclusive upper bound would be a
	// quiet off-by-one in "between two and five books".
	lo, hi := false, false
	for i := 0; i < 200; i++ {
		switch g.IntRange(2, 5) {
		case 2:
			lo = true
		case 5:
			hi = true
		}
	}
	if !lo || !hi {
		t.Errorf("IntRange(2,5) never returned 2 (%v) or never returned 5 (%v)", lo, hi)
	}
	if n := g.IntRange(4, 4); n != 4 {
		t.Errorf("IntRange(4,4) returned %d", n)
	}
	if n := g.IntRange(5, 2); n < 2 || n > 5 {
		t.Errorf("a reversed range returned %d", n)
	}
}

func TestGenTimeBetween(t *testing.T) {
	g := pg.NewGen(5)
	lo := time.Date(2021, 3, 1, 0, 0, 0, 0, time.UTC)
	hi := time.Date(2021, 4, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 500; i++ {
		got := g.TimeBetween(lo, hi)
		if got.Before(lo) || !got.Before(hi) {
			t.Fatalf("TimeBetween returned %s, outside [%s, %s)", got, lo, hi)
		}
		// PostgreSQL stores microseconds; a value with nanoseconds in
		// it would not survive the round trip, and a test comparing
		// what it wrote to what it read would fail for no good reason.
		if got.Nanosecond()%1000 != 0 {
			t.Fatalf("TimeBetween returned sub-microsecond precision: %s", got)
		}
	}
}

func TestGenChanceAndMaybeEdges(t *testing.T) {
	g := pg.NewGen(9)
	for i := 0; i < 100; i++ {
		if g.Chance(0) {
			t.Fatal("Chance(0) returned true")
		}
		if !g.Chance(1) {
			t.Fatal("Chance(1) returned false")
		}
		if pg.Maybe(g, 0, "x") != nil {
			t.Fatal("Maybe with p=0 returned a value")
		}
		if pg.Maybe(g, 1, "x") == nil {
			t.Fatal("Maybe with p=1 returned nil")
		}
	}
	// And in between it does both, which is what "sometimes NULL" means.
	nils, vals := 0, 0
	for i := 0; i < 400; i++ {
		if pg.Maybe(g, 0.5, 1) == nil {
			nils++
		} else {
			vals++
		}
	}
	if nils == 0 || vals == 0 {
		t.Errorf("Maybe(0.5) produced %d nils and %d values", nils, vals)
	}
}

func TestWeightedRespectsWeights(t *testing.T) {
	g := pg.NewGen(13)
	counts := map[string]int{}
	for i := 0; i < 4000; i++ {
		counts[pg.Weighted(g,
			pg.Choice[string]{Value: "common", Weight: 9},
			pg.Choice[string]{Value: "rare", Weight: 1},
			pg.Choice[string]{Value: "never", Weight: 0},
		)]++
	}
	if counts["never"] != 0 {
		t.Errorf("a zero weight was chosen %d times", counts["never"])
	}
	if counts["common"] < 3300 || counts["common"] > 4000 {
		t.Errorf("9:1 weights gave %d common of 4000", counts["common"])
	}
	if counts["rare"] == 0 {
		t.Error("the 1-weight option was never chosen")
	}
	// No positive weight at all has no answer to give.
	if v := pg.Weighted(g, pg.Choice[string]{Value: "x", Weight: 0}); v != "" {
		t.Errorf("Weighted with no positive weight returned %q", v)
	}
	if v := pg.Pick[string](g); v != "" {
		t.Errorf("Pick with no options returned %q", v)
	}
}
