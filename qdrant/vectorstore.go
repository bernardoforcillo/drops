package qdrant

import (
	"context"
	"fmt"

	"github.com/bernardoforcillo/drops/vector"
)

// Store adapts a Qdrant collection to
// [github.com/bernardoforcillo/drops/vector.Store], so the same
// portable query that runs against a pgvector table runs here.
//
// Build one with [Client.Store]. The Client's own typed methods
// (Search, Scroll, Recommend) stay available and lose nothing — Store
// is a second, portable door onto the same collection, not a
// replacement for the first.
type Store struct {
	c          *Client
	collection string
	metric     vector.Metric
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithMetric declares the distance function the collection was
// created with.
//
// This matters more than it looks. Qdrant fixes the metric at
// collection-creation time and a search cannot override it, so the
// Metric on a [vector.Query] is not a request — it is an assertion
// about the collection. Store checks the two agree and returns
// [vector.ErrUnsupportedMetric] when they don't, rather than ranking
// by cosine while the caller believes it asked for L2.
//
// It is also what tells the adapter how to read Qdrant's score, whose
// meaning changes with the metric: a similarity for Cosine and Dot, a
// raw distance for Euclid and Manhattan.
//
// Defaults to [vector.Cosine], matching Qdrant's most common setup.
func WithMetric(m vector.Metric) StoreOption {
	return func(s *Store) { s.metric = m }
}

// Store returns a portable [vector.Store] view of a collection.
//
//	store := cli.Store("embeddings", qdrant.WithMetric(vector.Cosine))
//	res, err := store.Search(ctx, q)
func (c *Client) Store(collection string, opts ...StoreOption) *Store {
	s := &Store{c: c, collection: collection, metric: vector.Cosine}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Collection returns the collection name the store reads from.
func (s *Store) Collection() string { return s.collection }

// Metric returns the collection's declared distance function.
func (s *Store) Metric() vector.Metric { return s.metric }

// MetricDistance maps a portable [vector.Metric] onto the Qdrant
// Distance constant that names the same function. The second result
// is false for metrics Qdrant has no equivalent for (Hamming,
// Jaccard).
func MetricDistance(m vector.Metric) (Distance, bool) {
	switch m {
	case vector.Cosine:
		return DistanceCosine, true
	case vector.L2:
		return DistanceEuclid, true
	case vector.InnerProduct:
		return DistanceDot, true
	case vector.L1:
		return DistanceManhattan, true
	default:
		return "", false
	}
}

// Search implements [vector.Store].
//
// Pagination is by offset, because Qdrant's search endpoint offers no
// keyset: the cursor carries the number of hits to skip. The adapter
// asks for TopK+1 hits and trims, so HasMore is exact and costs no
// second request.
func (s *Store) Search(ctx context.Context, q vector.Query) (*vector.Results, error) {
	q = q.Normalized()
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if err := validateName("collection", s.collection); err != nil {
		return nil, err
	}
	if q.Metric != s.metric {
		return nil, fmt.Errorf("%w: collection %q is %s, query asked for %s",
			vector.ErrUnsupportedMetric, s.collection, s.metric, q.Metric)
	}
	if _, ok := MetricDistance(q.Metric); !ok {
		return nil, fmt.Errorf("%w: qdrant has no %s distance", vector.ErrUnsupportedMetric, q.Metric)
	}

	filter, err := CompileFilter(q.Filter)
	if err != nil {
		return nil, err
	}

	cur, _, err := vector.DecodeCursor(q.Cursor, vector.BackendQdrant)
	if err != nil {
		return nil, err
	}

	req := SearchRequest{
		Vector:      q.Vector,
		Limit:       q.TopK + 1, // +1 probes for a further page
		Offset:      cur.Offset,
		Filter:      filter,
		WithVector:  q.IncludeVector,
		WithPayload: q.IncludePayload,
		Params:      searchParams(q),
	}
	if q.MaxDistance != nil {
		t := float32(scoreFromDistance(q.Metric, *q.MaxDistance))
		req.ScoreThreshold = &t
	}

	hits, err := s.c.Search(ctx, s.collection, req)
	if err != nil {
		return nil, err
	}

	res := &vector.Results{}
	res.HasMore = len(hits) > q.TopK
	if res.HasMore {
		hits = hits[:q.TopK]
	}
	res.Hits = make([]vector.Hit, 0, len(hits))
	for _, h := range hits {
		d := distanceFromScore(q.Metric, float64(h.Score))
		res.Hits = append(res.Hits, vector.Hit{
			ID:       h.ID,
			Distance: d,
			Score:    vector.ScoreFromDistance(q.Metric, d),
			Vector:   h.Vector,
			Payload:  h.Payload,
		})
	}
	if res.HasMore {
		next, cErr := vector.OffsetCursor(vector.BackendQdrant, cur.Offset+len(res.Hits)).Encode()
		if cErr != nil {
			return nil, cErr
		}
		res.NextCursor = next
	}
	return res, nil
}

// searchParams forwards the Qdrant-specific knobs from
// [vector.Query.Params] — hnsw_ef, exact, indexed_only — and drops
// keys meant for other backends, so one Query stays runnable
// everywhere.
func searchParams(q vector.Query) map[string]any {
	var out map[string]any
	for _, k := range []string{"hnsw_ef", "exact", "indexed_only", "quantization"} {
		if v, ok := q.Params[k]; ok {
			if out == nil {
				out = map[string]any{}
			}
			out[k] = v
		}
	}
	return out
}

// Qdrant's score is not one thing. For Cosine and Dot it is a
// similarity (higher is better); for Euclid and Manhattan it is the
// distance itself (lower is better). These two functions are the only
// place that asymmetry is encoded — everything downstream works in
// the portable convention where Distance is always "lower is closer".

func distanceFromScore(m vector.Metric, score float64) float64 {
	switch m {
	case vector.Cosine:
		return 1 - score
	case vector.InnerProduct:
		return -score
	default: // Euclid / Manhattan: the score already is the distance.
		return score
	}
}

func scoreFromDistance(m vector.Metric, distance float64) float64 {
	switch m {
	case vector.Cosine:
		return 1 - distance
	case vector.InnerProduct:
		return -distance
	default:
		return distance
	}
}

// CompileFilter turns a portable [vector.Filter] into the Qdrant
// filter object. The zero Filter compiles to nil — no filter at all
// rather than an empty one, which Qdrant would reject.
//
// Exported because it is useful on its own: it lets code that already
// builds portable filters drive the Client's own Scroll, Recommend and
// DeleteByFilter methods.
func CompileFilter(f vector.Filter) (*Filter, error) {
	cond, ok, err := vector.Compile[Condition](f, filterVisitor{})
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	// A top-level Condition has to sit in a block; Must with a single
	// member is the one that means "this condition holds".
	return &Filter{Must: []Condition{cond}}, nil
}

// filterVisitor compiles portable filter nodes into Qdrant
// Conditions.
//
// Qdrant has no general negation operator — only a MustNot block — so
// every negative form (Ne, NotIn, IsNotNull, Not) is expressed as a
// nested filter whose MustNot holds the positive form.
type filterVisitor struct{}

func (filterVisitor) And(parts []Condition) (Condition, error) {
	return Condition{Nested: &Filter{Must: parts}}, nil
}

func (filterVisitor) Or(parts []Condition) (Condition, error) {
	return Condition{Nested: &Filter{Should: parts}}, nil
}

func (filterVisitor) Not(part Condition) (Condition, error) {
	return Condition{Nested: &Filter{MustNot: []Condition{part}}}, nil
}

func (v filterVisitor) Compare(op vector.Op, field string, value any) (Condition, error) {
	switch op {
	case vector.OpEq:
		return Eq(field, value), nil
	case vector.OpNe:
		return v.Not(Eq(field, value))
	case vector.OpLt, vector.OpLte, vector.OpGt, vector.OpGte:
		f, err := toFloat(value)
		if err != nil {
			return Condition{}, fmt.Errorf("qdrant: %s on %q: %w", op, field, err)
		}
		var opts RangeOpts
		switch op {
		case vector.OpLt:
			opts.Lt = &f
		case vector.OpLte:
			opts.Lte = &f
		case vector.OpGt:
			opts.Gt = &f
		case vector.OpGte:
			opts.Gte = &f
		}
		return Range(field, opts), nil
	default:
		return Condition{}, fmt.Errorf("%w: qdrant compare %q", vector.ErrUnsupportedOp, op)
	}
}

func (v filterVisitor) Set(op vector.Op, field string, values []any) (Condition, error) {
	switch op {
	case vector.OpIn:
		return In(field, values...), nil
	case vector.OpNotIn:
		return NotIn(field, values...), nil
	default:
		return Condition{}, fmt.Errorf("%w: qdrant set %q", vector.ErrUnsupportedOp, op)
	}
}

func (v filterVisitor) Null(op vector.Op, field string) (Condition, error) {
	switch op {
	case vector.OpIsNull:
		// Qdrant separates "the key is missing" (is_empty) from "the
		// value is JSON null" (is_null). The portable IsNull means
		// "no usable value", so it has to cover both.
		return Condition{Nested: &Filter{
			Should: []Condition{IsNull(field), IsEmpty(field)},
		}}, nil
	case vector.OpIsNotNull:
		null, err := v.Null(vector.OpIsNull, field)
		if err != nil {
			return Condition{}, err
		}
		return v.Not(null)
	default:
		return Condition{}, fmt.Errorf("%w: qdrant null %q", vector.ErrUnsupportedOp, op)
	}
}

func (filterVisitor) MatchText(field, text string) (Condition, error) {
	return MatchText(field, text), nil
}

func (filterVisitor) HasID(ids []any) (Condition, error) {
	return HasID(ids...), nil
}

func (filterVisitor) GeoWithin(field string, box vector.GeoBox) (Condition, error) {
	return GeoIn(field,
		GeoPoint{Lat: box.TopLeft.Lat, Lon: box.TopLeft.Lon},
		GeoPoint{Lat: box.BottomRight.Lat, Lon: box.BottomRight.Lon},
	), nil
}

// toFloat converts a Go numeric to the float64 Qdrant's range filter
// speaks. Non-numeric values are a caller error, not something to
// coerce.
func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int8:
		return float64(n), nil
	case int16:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case uint:
		return float64(n), nil
	case uint8:
		return float64(n), nil
	case uint16:
		return float64(n), nil
	case uint32:
		return float64(n), nil
	case uint64:
		return float64(n), nil
	case float32:
		return float64(n), nil
	case float64:
		return n, nil
	default:
		return 0, fmt.Errorf("%w: got %T", vector.ErrNotNumeric, v)
	}
}

// compile-time check that Store satisfies the portable interface.
var _ vector.Store = (*Store)(nil)
