package vector

import "context"

// Metric is the distance function a search ranks by.
//
// Not every backend serves every metric: pgvector covers all six,
// ClickHouse covers Cosine / L2 / L1 / InnerProduct, and Qdrant fixes
// the metric when the collection is created — there the Metric on a
// Query only tells the adapter how to convert Qdrant's score into a
// distance. A backend asked for a metric it cannot serve returns
// [ErrUnsupportedMetric] rather than quietly ranking by another one.
type Metric string

const (
	// Cosine is the cosine distance, 1 - cos(θ), in [0, 2].
	Cosine Metric = "cosine"
	// L2 is the Euclidean distance.
	L2 Metric = "l2"
	// InnerProduct ranks by the negated inner product, so that — like
	// every other Metric — smaller is closer.
	InnerProduct Metric = "inner_product"
	// L1 is the Manhattan distance.
	L1 Metric = "l1"
	// Hamming is the bit-vector Hamming distance.
	Hamming Metric = "hamming"
	// Jaccard is the bit-vector Jaccard distance.
	Jaccard Metric = "jaccard"
)

// DefaultTopK is the page size a Query gets when it sets none.
const DefaultTopK = 10

// Query is one similarity search. Build it with [Search] rather than
// by hand — the builder is what applies the defaults and keeps the
// cursor opaque.
type Query struct {
	// Vector is the query embedding. Required.
	Vector []float32

	// TopK is the maximum number of hits to return. Zero means
	// [DefaultTopK].
	TopK int

	// Metric is the distance function. Empty means [Cosine].
	Metric Metric

	// Filter narrows the candidate set. The zero Filter means no
	// filtering.
	Filter Filter

	// MaxDistance, when non-nil, drops hits farther than this bound.
	// It is expressed in the Metric's own units — a cosine distance
	// of 0.25, a Euclidean distance of 3.0 — never as a similarity,
	// so the same number means the same thing on every backend.
	MaxDistance *float64

	// IncludeVector asks the backend to return each hit's stored
	// vector. Off by default: it is the expensive part of the payload.
	IncludeVector bool

	// IncludePayload asks the backend to return each hit's payload.
	IncludePayload bool

	// Cursor resumes after a previous page. Empty starts at the first.
	Cursor string

	// Params carries backend-specific tuning that has no portable
	// meaning — Qdrant's hnsw_ef and exact, ClickHouse SETTINGS,
	// pgvector's ivfflat.probes. A backend ignores keys it does not
	// recognise, so a Query stays runnable everywhere.
	Params map[string]any
}

// Store is a searchable collection of vectors. It is the whole
// contract a backend has to satisfy to be usable through this package
// — drops/pg, drops/clickhouse and drops/qdrant each provide one.
type Store interface {
	Search(ctx context.Context, q Query) (*Results, error)
}

// Normalized returns a copy of q with defaults applied: TopK becomes
// [DefaultTopK] when unset, Metric becomes [Cosine] when empty.
// Backends call it first so the defaults are identical across stores.
func (q Query) Normalized() Query {
	if q.TopK <= 0 {
		q.TopK = DefaultTopK
	}
	if q.Metric == "" {
		q.Metric = Cosine
	}
	return q
}

// Validate reports whether the query is runnable at all, independent
// of backend.
func (q Query) Validate() error {
	if len(q.Vector) == 0 {
		return ErrNoVector
	}
	return nil
}

// Param returns the backend-specific parameter under key.
func (q Query) Param(key string) (any, bool) {
	v, ok := q.Params[key]
	return v, ok
}

// QueryBuilder assembles a [Query]. The zero value is not useful;
// start from [Search].
type QueryBuilder struct{ q Query }

// Search starts a query for the given embedding.
//
//	q := vector.Search(emb).TopK(20).Where(vector.Eq("lang", "it")).Build()
func Search(v []float32) *QueryBuilder {
	return &QueryBuilder{q: Query{Vector: v}}
}

// TopK caps the page size. Non-positive values are ignored, leaving
// the default in place.
func (b *QueryBuilder) TopK(n int) *QueryBuilder {
	if n > 0 {
		b.q.TopK = n
	}
	return b
}

// Metric selects the distance function.
func (b *QueryBuilder) Metric(m Metric) *QueryBuilder {
	b.q.Metric = m
	return b
}

// Where sets the filter. Multiple filters are ANDed, so Where can be
// called repeatedly as a query is built up in stages.
func (b *QueryBuilder) Where(filters ...Filter) *QueryBuilder {
	all := append([]Filter{b.q.Filter}, filters...)
	b.q.Filter = And(all...)
	return b
}

// MaxDistance drops hits farther than d, in the metric's own units.
func (b *QueryBuilder) MaxDistance(d float64) *QueryBuilder {
	b.q.MaxDistance = &d
	return b
}

// WithVector asks for each hit's stored vector.
func (b *QueryBuilder) WithVector() *QueryBuilder {
	b.q.IncludeVector = true
	return b
}

// WithPayload asks for each hit's payload.
func (b *QueryBuilder) WithPayload() *QueryBuilder {
	b.q.IncludePayload = true
	return b
}

// After resumes from a cursor returned as [Results.NextCursor]. The
// empty string means "first page", so passing a fresh loop variable
// through works without a special case.
func (b *QueryBuilder) After(cursor string) *QueryBuilder {
	b.q.Cursor = cursor
	return b
}

// Param sets a backend-specific tuning parameter. See [Query.Params].
func (b *QueryBuilder) Param(key string, value any) *QueryBuilder {
	if b.q.Params == nil {
		b.q.Params = map[string]any{}
	}
	b.q.Params[key] = value
	return b
}

// Build returns the assembled query with defaults applied.
func (b *QueryBuilder) Build() Query { return b.q.Normalized() }
