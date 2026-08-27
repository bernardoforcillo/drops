package pg

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
)

// Knowing how selective a predicate is, without asking the planner.
//
// Most bad plans are not bad choices — they are good choices made
// from a wrong row estimate. PostgreSQL keeps statistics for its own
// planner but does not offer them in a form an application can reason
// with, and the questions an application has are answerable more
// cheaply than a full histogram:
//
//   - Is "status = 'pending'" worth an index, or does it match a
//     third of the table? (An index scan for a third of a table is
//     slower than reading it.)
//   - Is this tenant one of the handful that hold most of the rows?
//   - Which join should drive, given what this filter actually
//     matches?
//
// A [Sketch] is a Count-Min sketch over one column: a fixed-size
// table of counters that answers "how many rows have this value" in
// constant space, whatever the cardinality. InstantDB keeps one per
// attribute and uses it to order joins; the structure transfers
// directly, and here it feeds [WithPlanHints] and the decision of
// whether an index is worth creating at all.
//
//	s, err := pg.SketchColumn(ctx, db, "orders", "status", pg.SketchOptions{})
//	if s.Selectivity("pending") > 0.2 {
//	    // A fifth of the table. An index on status will not help.
//	}
//
// # The one thing to know about the estimate
//
// A Count-Min sketch never *under*counts. Hash collisions add other
// values' counts to yours, so an estimate is an upper bound: exact
// for the values that matter and inflated for the rare ones.
//
// That asymmetry is why it is the right structure here rather than a
// convenient one. Over-estimating selectivity makes a caller decline
// an index it might have wanted — a missed optimisation, visible in a
// latency graph. Under-estimating would make it declare a predicate
// selective when it is not, which is the mistake that produces the
// bad plan in the first place. The error only ever falls on the safe
// side.

// Sketch is a Count-Min sketch: depth independent hash functions over
// a table of width counters each.
type Sketch struct {
	width uint32
	depth uint32
	bins  []uint64
	total uint64
	// distinct counts the values that were seen for the first time
	// as far as the sketch can tell — an estimate, and an
	// under-estimate, since a value whose counters were already
	// raised by a collision looks familiar.
	distinct uint64
}

// SketchOptions configures a sketch.
type SketchOptions struct {
	// Width is the number of counters per row. Larger is more
	// accurate; the expected over-count is the row total divided by
	// Width. Default 2048, which holds the error under a tenth of a
	// percent of the table for a table of any size.
	Width uint32

	// Depth is the number of independent hash rows. Each one is
	// another chance for the minimum to be uncontaminated. Default
	// 5; beyond about 8 the returns are not worth the memory.
	Depth uint32

	// Sample, when between 0 and 1, reads only that fraction of the
	// table using TABLESAMPLE SYSTEM and scales the counts back up.
	//
	// It turns a full scan of a large table into a bounded read, at
	// the cost of the sampling error — which lands on the rare
	// values, since a value present in a tenth of a percent of rows
	// may not appear in a 1% sample at all. Sample when the question
	// is "is this predicate selective", not when it is "does this
	// value exist".
	Sample float64
}

func (o SketchOptions) width() uint32 {
	if o.Width == 0 {
		return 2048
	}
	return o.Width
}

func (o SketchOptions) depth() uint32 {
	if o.Depth == 0 {
		return 5
	}
	return o.Depth
}

// NewSketch returns an empty sketch.
func NewSketch(opts SketchOptions) *Sketch {
	w, d := opts.width(), opts.depth()
	return &Sketch{width: w, depth: d, bins: make([]uint64, uint64(w)*uint64(d))}
}

// Add records n occurrences of value.
func (s *Sketch) Add(value any, n uint64) {
	if n == 0 {
		return
	}
	key := sketchKey(value)
	fresh := true
	for row := uint32(0); row < s.depth; row++ {
		i := s.index(key, row)
		if s.bins[i] != 0 {
			fresh = false
		}
		s.bins[i] += n
	}
	s.total += n
	if fresh {
		s.distinct++
	}
}

// Estimate returns the upper bound on how many rows carry value.
func (s *Sketch) Estimate(value any) uint64 {
	if s == nil || s.total == 0 {
		return 0
	}
	key := sketchKey(value)
	best := uint64(math.MaxUint64)
	for row := uint32(0); row < s.depth; row++ {
		if v := s.bins[s.index(key, row)]; v < best {
			best = v
		}
	}
	if best == math.MaxUint64 {
		return 0
	}
	return best
}

// Selectivity returns the estimated fraction of rows carrying value,
// in [0,1].
//
// This is the number to branch on. Below roughly 0.05 an index scan
// is usually worth it; above 0.2 PostgreSQL will read the table
// whatever indexes exist, and it is right to.
func (s *Sketch) Selectivity(value any) float64 {
	if s == nil || s.total == 0 {
		return 0
	}
	f := float64(s.Estimate(value)) / float64(s.total)
	if f > 1 {
		return 1
	}
	return f
}

// Total returns how many rows the sketch was built from.
func (s *Sketch) Total() uint64 {
	if s == nil {
		return 0
	}
	return s.total
}

// DistinctEstimate returns a lower bound on the number of distinct
// values, which is what makes it useful in the direction that
// matters: a sketch reporting a thousand distinct values has at least
// that many, so the column is worth an index. A sketch reporting five
// may have more.
func (s *Sketch) DistinctEstimate() uint64 {
	if s == nil {
		return 0
	}
	return s.distinct
}

// ExpectedError returns the over-count the sketch's dimensions admit
// — total divided by width. An estimate within this of zero is noise
// rather than evidence.
func (s *Sketch) ExpectedError() float64 {
	if s == nil || s.width == 0 {
		return 0
	}
	return float64(s.total) / float64(s.width)
}

// Merge folds other into s. The two must have the same dimensions.
//
// It is what makes a sketch computable in parallel: sketch each
// shard, or each chunk of a partitioned table, and merge. Count-Min
// merges exactly — the counters simply add — so a merged sketch is
// identical to one built from the whole in a single pass.
func (s *Sketch) Merge(other *Sketch) error {
	if other == nil {
		return nil
	}
	if s.width != other.width || s.depth != other.depth {
		return fmt.Errorf("drops/pg: cannot merge a %dx%d sketch into a %dx%d one",
			other.width, other.depth, s.width, s.depth)
	}
	for i := range s.bins {
		s.bins[i] += other.bins[i]
	}
	s.total += other.total
	if other.distinct > s.distinct {
		s.distinct = other.distinct
	}
	return nil
}

// MarshalBinary encodes the sketch so it can be stored next to the
// table it describes and reloaded without a rescan.
func (s *Sketch) MarshalBinary() ([]byte, error) {
	if s == nil {
		return nil, errors.New("drops/pg: cannot marshal a nil sketch")
	}
	out := make([]byte, 0, 24+len(s.bins)*8)
	var hdr [24]byte
	binary.BigEndian.PutUint32(hdr[0:], s.width)
	binary.BigEndian.PutUint32(hdr[4:], s.depth)
	binary.BigEndian.PutUint64(hdr[8:], s.total)
	binary.BigEndian.PutUint64(hdr[16:], s.distinct)
	out = append(out, hdr[:]...)
	var b [8]byte
	for _, v := range s.bins {
		binary.BigEndian.PutUint64(b[:], v)
		out = append(out, b[:]...)
	}
	return out, nil
}

// UnmarshalBinary restores a sketch encoded by [Sketch.MarshalBinary].
func (s *Sketch) UnmarshalBinary(data []byte) error {
	if len(data) < 24 {
		return errors.New("drops/pg: sketch payload is too short")
	}
	width := binary.BigEndian.Uint32(data[0:])
	depth := binary.BigEndian.Uint32(data[4:])
	want := uint64(width) * uint64(depth)
	if want == 0 || uint64(len(data)-24) != want*8 {
		return fmt.Errorf("drops/pg: sketch payload does not match its %dx%d header", width, depth)
	}
	s.width, s.depth = width, depth
	s.total = binary.BigEndian.Uint64(data[8:])
	s.distinct = binary.BigEndian.Uint64(data[16:])
	s.bins = make([]uint64, want)
	for i := range s.bins {
		s.bins[i] = binary.BigEndian.Uint64(data[24+i*8:])
	}
	return nil
}

// index picks the counter for key in the given row.
func (s *Sketch) index(key uint64, row uint32) uint64 {
	// One 64-bit hash split into two halves and combined per row —
	// the standard Kirsch-Mitzenmacher construction, which gives
	// depth independent-enough hashes from one.
	h1 := uint32(key >> 32)
	h2 := uint32(key)
	col := (h1 + row*h2) % s.width
	return uint64(row)*uint64(s.width) + uint64(col)
}

// sketchKey hashes a value into the space the sketch indexes.
//
// The text form is hashed rather than the Go value, so an int64 7 and
// a string "7" are the same value. That matches what the caller is
// asking — a column holds one type and a comparison against it is
// against that type — and it means a sketch built from a driver that
// scans int32 answers a query written with int.
func sketchKey(v any) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%v", v)
	return h.Sum64()
}

// SketchColumn builds a sketch of a column by reading it.
//
// It is a full scan unless [SketchOptions.Sample] is set, so it
// belongs on a schedule or in a maintenance job rather than on a
// request path. Store the result with [Sketch.MarshalBinary] and load
// it at start-up.
//
// NULLs are skipped: a predicate matches them only through IS NULL,
// which needs no estimate.
func SketchColumn(ctx context.Context, db *DB, table, column string, opts SketchOptions) (*Sketch, error) {
	// Table and column arrive as strings rather than as declared
	// schema objects, so unlike the rest of the package this one
	// cannot rely on the declaration having been checked.
	if err := validateIdent("table", table); err != nil {
		return nil, err
	}
	if err := validateIdent("column", column); err != nil {
		return nil, err
	}
	sample := ""
	scale := uint64(1)
	if opts.Sample > 0 && opts.Sample < 1 {
		sample = fmt.Sprintf(" TABLESAMPLE SYSTEM (%g)", opts.Sample*100)
		scale = uint64(math.Round(1 / opts.Sample))
		if scale == 0 {
			scale = 1
		}
	}

	// Counting per value in the database rather than streaming every
	// row keeps the transfer proportional to the cardinality instead
	// of to the table.
	rows, err := db.Query(ctx, fmt.Sprintf(
		`SELECT %[1]s::text, count(*)::bigint FROM %[2]s%[3]s WHERE %[1]s IS NOT NULL GROUP BY 1`,
		quoteIdent(column), quoteIdent(table), sample))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	s := NewSketch(opts)
	for rows.Next() {
		var value string
		var n int64
		if err := rows.Scan(&value, &n); err != nil {
			return nil, err
		}
		if n > 0 {
			s.Add(value, uint64(n)*scale)
		}
	}
	return s, rows.Err()
}

// TopValues returns the count-estimate of each of the supplied
// candidate values, highest first.
//
// A Count-Min sketch cannot enumerate what it holds — it stores
// counters, not values — so the candidates have to come from
// somewhere else: the enum's labels, the tenants on record, the
// statuses the application defines. That is usually exactly the list
// a caller has.
func (s *Sketch) TopValues(candidates ...any) []SketchCount {
	out := make([]SketchCount, 0, len(candidates))
	for _, v := range candidates {
		out = append(out, SketchCount{
			Value:       v,
			Estimate:    s.Estimate(v),
			Selectivity: s.Selectivity(v),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Estimate > out[j].Estimate })
	return out
}

// SketchCount is one candidate value's estimate.
type SketchCount struct {
	Value       any
	Estimate    uint64
	Selectivity float64
}
