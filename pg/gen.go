package pg

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// Generated values for seed data.
//
// The property that matters is reproducibility. Data a test depends on
// but cannot regenerate is data that turns a failure into a story about
// a run nobody has any more — so every value here comes out of one
// PRNG, that PRNG comes from one integer, and a run is repeated by
// passing the same integer again.
//
// Two rules follow from that and are worth stating, because breaking
// either quietly costs the property:
//
//   - Nothing here reads the clock. A timestamp comes from a range the
//     caller supplies, because a seed whose output depends on the day it
//     ran is not reproducible.
//   - A Gen is not safe for concurrent use. Two goroutines drawing from
//     one Gen get a valid but unrepeatable interleaving, which is the
//     same thing as no seed at all.
//
// What this is not is a faker library. The name and word lists below
// are short, fixed and English; they exist to make a table look like it
// has data in it, not to stand in for a locale. Pick and Weighted are
// there for when the domain has real values — currencies, statuses,
// SKUs — that no library could guess.

// Gen is a deterministic source of generated values. Construct one with
// [NewGen] and hand it the same seed to get the same run back.
type Gen struct {
	seed uint64
	r    *rand.Rand
	// seq backs the values that have to be distinct across a run —
	// an email landing in a UNIQUE column. It is part of the Gen's
	// state, so it rewinds with Reset and the run stays repeatable.
	seq int64
}

// NewGen returns a Gen drawing from seed. The same seed yields the same
// sequence of values for the same sequence of calls.
func NewGen(seed uint64) *Gen {
	g := &Gen{seed: seed}
	g.Reset()
	return g
}

// Reset rewinds the Gen to its seed, so the next call returns what the
// first call returned. [Seeder.Apply] does this before every run.
func (g *Gen) Reset() {
	// The second PCG word is a fixed odd constant rather than a second
	// caller-supplied number: one integer is what a person can copy out
	// of a failing test and paste into the next run.
	g.r = rand.New(rand.NewPCG(g.seed, 0x9E3779B97F4A7C15))
	g.seq = 0
}

// Seed returns the integer this Gen was built from — the thing to print
// when a seeded test fails.
func (g *Gen) Seed() uint64 { return g.seed }

// Rand exposes the underlying PRNG for a generator this package does
// not have. Drawing from it keeps the run reproducible; drawing from
// anywhere else does not.
func (g *Gen) Rand() *rand.Rand { return g.r }

// next returns the next value of the uniqueness counter.
func (g *Gen) next() int64 {
	g.seq++
	return g.seq
}

// IntN returns a number in [0, n). It panics for n <= 0, as the
// standard library does.
func (g *Gen) IntN(n int) int { return g.r.IntN(n) }

// IntRange returns a number in [lo, hi], both ends included. A reversed
// range is read in the order given, so IntRange(5, 2) is IntRange(2, 5).
func (g *Gen) IntRange(lo, hi int) int {
	if hi < lo {
		lo, hi = hi, lo
	}
	if lo == hi {
		return lo
	}
	return lo + g.r.IntN(hi-lo+1)
}

// Float64Range returns a number in [lo, hi).
func (g *Gen) Float64Range(lo, hi float64) float64 {
	if hi < lo {
		lo, hi = hi, lo
	}
	return lo + g.r.Float64()*(hi-lo)
}

// Bool returns true half the time.
func (g *Gen) Bool() bool { return g.r.IntN(2) == 0 }

// Chance returns true with probability p. p <= 0 is never, p >= 1 is
// always.
func (g *Gen) Chance(p float64) bool {
	if p <= 0 {
		return false
	}
	if p >= 1 {
		return true
	}
	return g.r.Float64() < p
}

// FirstName returns a given name from a short fixed list.
func (g *Gen) FirstName() string { return firstNames[g.r.IntN(len(firstNames))] }

// LastName returns a family name from a short fixed list.
func (g *Gen) LastName() string { return lastNames[g.r.IntN(len(lastNames))] }

// FullName returns "First Last".
func (g *Gen) FullName() string { return g.FirstName() + " " + g.LastName() }

// Email returns an address at one of the reserved example domains,
// distinct from every other address this Gen has produced.
//
// The counter in the local part is not decoration: seed data lands in
// real tables, and a generator that collides on a UNIQUE index is a
// generator whose output cannot be inserted.
func (g *Gen) Email() string {
	return fmt.Sprintf("%s.%s%d@%s",
		strings.ToLower(g.FirstName()),
		strings.ToLower(g.LastName()),
		g.next(),
		emailDomains[g.r.IntN(len(emailDomains))])
}

// Word returns one word from a short fixed list.
func (g *Gen) Word() string { return words[g.r.IntN(len(words))] }

// Words returns n words separated by spaces.
func (g *Gen) Words(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(g.Word())
	}
	return b.String()
}

// Sentence returns a capitalised, full-stopped run of four to twelve
// words.
func (g *Gen) Sentence() string {
	s := g.Words(g.IntRange(4, 12))
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// Paragraph returns n sentences separated by spaces.
func (g *Gen) Paragraph(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = g.Sentence()
	}
	return strings.Join(parts, " ")
}

// TimeBetween returns an instant in [lo, hi), truncated to the
// microsecond PostgreSQL stores — so the value that comes back out of
// the database equals the value that went in, and a test comparing the
// two is testing what it thinks it is.
//
// There is no Now-relative variant on purpose: see the package note
// above on reading the clock.
func (g *Gen) TimeBetween(lo, hi time.Time) time.Time {
	if hi.Before(lo) {
		lo, hi = hi, lo
	}
	d := hi.Sub(lo)
	if d <= 0 {
		return lo.Truncate(time.Microsecond)
	}
	return lo.Add(time.Duration(g.r.Int64N(int64(d)))).Truncate(time.Microsecond)
}

// Pick returns one of the options. It is a free function rather than a
// method because Go does not allow generic methods. Pick(g, xs...)
// spreads a slice.
//
// Picking from a list the caller supplies is how a generated column
// gets domain values — statuses, currencies, plan names — that no
// generator could invent.
func Pick[T any](g *Gen, options ...T) T {
	if len(options) == 0 {
		var zero T
		return zero
	}
	return options[g.r.IntN(len(options))]
}

// Choice is one option of a [Weighted] draw. A non-positive weight is
// never chosen.
type Choice[T any] struct {
	Value  T
	Weight float64
}

// Weighted returns one option with probability proportional to its
// weight — the way a status column is 90% "active" and 10% the rest,
// which is what makes seeded data resemble the table it stands in for.
// Weights need not sum to anything in particular.
func Weighted[T any](g *Gen, choices ...Choice[T]) T {
	var zero T
	total := 0.0
	for _, c := range choices {
		if c.Weight > 0 {
			total += c.Weight
		}
	}
	if total <= 0 {
		return zero
	}
	r := g.r.Float64() * total
	for _, c := range choices {
		if c.Weight <= 0 {
			continue
		}
		r -= c.Weight
		if r < 0 {
			return c.Value
		}
	}
	// Only reachable through floating-point drift at the very top of
	// the range; the last positively weighted option is the answer.
	for i := len(choices) - 1; i >= 0; i-- {
		if choices[i].Weight > 0 {
			return choices[i].Value
		}
	}
	return zero
}

// Maybe returns &v with probability p and nil otherwise — a column that
// is sometimes NULL. The pointer is the point: a nullable column binds
// to a pointer field, so this is the value such a field wants.
//
// v is evaluated by the caller either way, which keeps the number of
// draws taken per call fixed and the run repeatable.
func Maybe[T any](g *Gen, p float64, v T) *T {
	if !g.Chance(p) {
		return nil
	}
	return &v
}

// Shuffle permutes s in place.
func Shuffle[T any](g *Gen, s []T) {
	g.r.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
}

// The lists. Short, fixed, and English — enough to fill a column with
// something readable. See the package note above: this is not localised
// data and should not be used as if it were.

var firstNames = []string{
	"Ada", "Alan", "Amara", "Bo", "Chen", "Dara", "Elif", "Emil",
	"Fatima", "Gustav", "Hana", "Ines", "Ivan", "Juno", "Kai",
	"Lena", "Mateo", "Nadia", "Omar", "Pia", "Quinn", "Rosa",
	"Sami", "Tobias", "Uma", "Viktor", "Wren", "Yara", "Zeno",
}

var lastNames = []string{
	"Adeyemi", "Bauer", "Costa", "Duarte", "Eriksen", "Ferrari",
	"Gallo", "Haas", "Ibarra", "Jensen", "Kovac", "Lindqvist",
	"Moreau", "Novak", "Okafor", "Petrov", "Rossi", "Silva",
	"Tanaka", "Ueda", "Varga", "Weber", "Yilmaz", "Zhang",
}

// Reserved for documentation and testing by RFC 2606 and RFC 6761, so
// a stray mail never reaches anyone.
var emailDomains = []string{"example.com", "example.org", "example.net", "test"}

var words = []string{
	"amber", "anchor", "basin", "beacon", "bramble", "cinder",
	"clover", "compass", "cobalt", "drift", "ember", "fathom",
	"ferrous", "glimmer", "granite", "harbour", "hollow", "indigo",
	"kestrel", "lantern", "lattice", "marble", "meadow", "murmur",
	"nettle", "obsidian", "orchard", "pebble", "quarry", "quill",
	"ravine", "rustle", "saffron", "shale", "sparrow", "thicket",
	"thistle", "tundra", "verdant", "willow", "yonder", "zephyr",
}
