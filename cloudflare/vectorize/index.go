package vectorize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/vector"
)

// Query ceilings Vectorize imposes on topK.
//
// Cloudflare has moved these, and documents them per plan, so they
// are defaults rather than facts: [WithTopKCeilings] overrides them
// on an index without waiting for a release here. What does not
// change is the shape — asking for values or for full metadata
// lowers the ceiling, because the payload per hit is bigger.
const (
	// MaxTopK is the largest topK a plain query may ask for.
	MaxTopK = 100

	// MaxTopKWithValues is the ceiling once the query asks for the
	// stored vectors or for full metadata.
	MaxTopKWithValues = 20
)

// Sentinel errors.
var (
	// ErrNoIndexName is returned when the index name is empty.
	ErrNoIndexName = errors.New("drops/cloudflare/vectorize: index name is empty")

	// ErrPageBeyondTopK is returned when a cursor asks for a page
	// past what Vectorize will return at all. See the package
	// comment: there is no offset in the query API, so pagination is
	// over-fetch-and-slice and stops where topK does.
	ErrPageBeyondTopK = errors.New("drops/cloudflare/vectorize: page is past the topK ceiling; Vectorize has no offset, so pagination cannot reach it")

	// ErrNoVectors is returned by Upsert / Insert with nothing to
	// write.
	ErrNoVectors = errors.New("drops/cloudflare/vectorize: no vectors")

	// ErrDimensionMismatch is returned when the vectors handed to
	// one Upsert do not all have the same length. Vectorize would
	// reject the batch; catching it here says which vector is the
	// odd one.
	ErrDimensionMismatch = errors.New("drops/cloudflare/vectorize: vectors have differing dimensions")
)

// Metric is a Vectorize distance function, fixed when an index is
// created.
type Metric string

// The metrics Vectorize offers.
const (
	MetricCosine     Metric = "cosine"
	MetricEuclidean  Metric = "euclidean"
	MetricDotProduct Metric = "dot-product"
)

// MetadataIndexType is the type a metadata index is declared over.
type MetadataIndexType string

// The metadata index types Vectorize offers.
const (
	MetadataString  MetadataIndexType = "string"
	MetadataNumber  MetadataIndexType = "number"
	MetadataBoolean MetadataIndexType = "boolean"
)

// ReturnMetadata says how much of a vector's metadata a query asks
// for. "indexed" returns only the fields that have a metadata index,
// and is the cheaper option that does not lower the topK ceiling.
type ReturnMetadata string

// The metadata modes a query may ask for.
const (
	MetadataNone    ReturnMetadata = "none"
	MetadataIndexed ReturnMetadata = "indexed"
	MetadataAll     ReturnMetadata = "all"
)

// Vector is one record in an index.
type Vector struct {
	// ID is the record's identifier. Vectorize IDs are strings.
	ID string `json:"id"`

	// Values is the embedding.
	Values []float32 `json:"values"`

	// Metadata is the record's filterable payload. Only fields with
	// a metadata index can be filtered on — see
	// [Index.CreateMetadataIndex].
	Metadata map[string]any `json:"metadata,omitempty"`

	// Namespace partitions the index. A query restricted to a
	// namespace searches only within it.
	Namespace string `json:"namespace,omitempty"`
}

// Match is one hit from a query.
type Match struct {
	ID        string         `json:"id"`
	Score     float64        `json:"score"`
	Values    []float32      `json:"values,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	Namespace string         `json:"namespace,omitempty"`
}

// Info describes an index.
type Info struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Config      struct {
		Dimensions int    `json:"dimensions"`
		Metric     Metric `json:"metric"`
	} `json:"config"`
	CreatedOn  string `json:"created_on"`
	ModifiedOn string `json:"modified_on"`
}

// MetadataIndex is one declared metadata index.
type MetadataIndex struct {
	PropertyName string            `json:"propertyName"`
	IndexType    MetadataIndexType `json:"indexType"`
}

// Mutation is what a write returns: the identifier of the async
// mutation Vectorize queued.
//
// Writes are not immediately visible to queries — Vectorize applies
// them asynchronously — so a test that upserts and immediately
// searches will not find the vector. That is the service's behaviour,
// not this client's, and there is no flag to turn it off.
type Mutation struct {
	MutationID string `json:"mutationId"`
}

// Index is a typed client for one Vectorize index.
//
// It is also the [github.com/bernardoforcillo/drops/vector.Store]
// implementation — Search is defined on it in search.go — so one
// value serves both the portable and the native door.
type Index struct {
	cf             *cloudflare.Client
	name           string
	metric         vector.Metric
	ns             string
	topK           int
	topKWithValues int
}

// Option configures an [Index].
type Option func(*Index)

// WithMetric declares the distance function the index was created
// with.
//
// It is an assertion, not a request. Vectorize fixes the metric at
// creation and a query cannot override it, so this is what tells the
// adapter how to read a score — which means a similarity for cosine
// and dot-product and a distance for euclidean. A
// [github.com/bernardoforcillo/drops/vector.Query] asking for a
// different metric is refused with
// [github.com/bernardoforcillo/drops/vector.ErrUnsupportedMetric]
// rather than ranked by the wrong function.
//
// Defaults to [github.com/bernardoforcillo/drops/vector.Cosine].
func WithMetric(m vector.Metric) Option {
	return func(i *Index) { i.metric = m }
}

// WithNamespace restricts every operation to one namespace of the
// index.
func WithNamespace(ns string) Option {
	return func(i *Index) { i.ns = ns }
}

// WithTopKCeilings overrides the topK limits [MaxTopK] and
// [MaxTopKWithValues] name.
//
// They are Cloudflare's to change and they differ by plan, so an
// index that is allowed more should be told so rather than held to
// this package's release date. Non-positive values leave the
// corresponding default in place.
func WithTopKCeilings(plain, withValues int) Option {
	return func(i *Index) {
		if plain > 0 {
			i.topK = plain
		}
		if withValues > 0 {
			i.topKWithValues = withValues
		}
	}
}

// New returns a client for the named Vectorize index.
func New(cf *cloudflare.Client, name string, opts ...Option) *Index {
	i := &Index{
		cf:             cf,
		name:           name,
		metric:         vector.Cosine,
		topK:           MaxTopK,
		topKWithValues: MaxTopKWithValues,
	}
	for _, o := range opts {
		o(i)
	}
	return i
}

// TopKCeiling returns the largest topK this index will serve for a
// query with these return options.
func (i *Index) TopKCeiling(returnValues bool, metadata ReturnMetadata) int {
	if returnValues || metadata == MetadataAll {
		return i.topKWithValues
	}
	return i.topK
}

// Name returns the index name.
func (i *Index) Name() string { return i.name }

// Metric returns the declared distance function.
func (i *Index) Metric() vector.Metric { return i.metric }

// Namespace returns the namespace operations are restricted to, if
// any.
func (i *Index) Namespace() string { return i.ns }

// Client returns the underlying Cloudflare API client.
func (i *Index) Client() *cloudflare.Client { return i.cf }

// path builds a path under this index.
func (i *Index) path(suffix ...string) (string, error) {
	if i.name == "" {
		return "", ErrNoIndexName
	}
	return i.cf.AccountPath("/vectorize/v2/indexes", append([]string{i.name}, suffix...)...), nil
}

// Describe returns the index's configuration — dimensions and
// metric, which is the pair a caller most often wants to check
// against what it is about to write.
func (i *Index) Describe(ctx context.Context) (*Info, error) {
	p, err := i.path()
	if err != nil {
		return nil, err
	}
	var info Info
	if err := i.cf.Do(ctx, cloudflare.Request{Method: http.MethodGet, Path: p}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// CreateIndexRequest describes an index to create.
type CreateIndexRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Config      struct {
		Dimensions int    `json:"dimensions"`
		Metric     Metric `json:"metric"`
	} `json:"config"`
}

// CreateIndex creates an index on the account.
//
// Dimensions and metric are fixed for the life of the index: there is
// no ALTER here, and changing either means creating a new index and
// re-embedding into it.
func CreateIndex(ctx context.Context, cf *cloudflare.Client, name string, dimensions int, metric Metric, description string) (*Info, error) {
	if name == "" {
		return nil, ErrNoIndexName
	}
	if dimensions <= 0 {
		return nil, fmt.Errorf("drops/cloudflare/vectorize: dimensions must be positive, got %d", dimensions)
	}
	req := CreateIndexRequest{Name: name, Description: description}
	req.Config.Dimensions = dimensions
	req.Config.Metric = metric

	var info Info
	err := cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   cf.AccountPath("/vectorize/v2/indexes"),
		Body:   req,
	}, &info)
	if err != nil {
		return nil, err
	}
	return &info, nil
}

// ListIndexes returns every Vectorize index on the account.
func ListIndexes(ctx context.Context, cf *cloudflare.Client) ([]Info, error) {
	var out []Info
	err := cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   cf.AccountPath("/vectorize/v2/indexes"),
	}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteIndex removes an index and everything in it.
func DeleteIndex(ctx context.Context, cf *cloudflare.Client, name string) error {
	if name == "" {
		return ErrNoIndexName
	}
	return cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   cf.AccountPath("/vectorize/v2/indexes", name),
	}, nil)
}

// Upsert writes vectors, replacing any that already exist under the
// same ID.
//
// The body is NDJSON — one vector per line — which is what the
// endpoint takes. The write is applied asynchronously: see
// [Mutation].
func (i *Index) Upsert(ctx context.Context, vectors ...Vector) (*Mutation, error) {
	return i.write(ctx, "upsert", vectors)
}

// Insert writes vectors, failing rather than replacing when an ID is
// already present.
func (i *Index) Insert(ctx context.Context, vectors ...Vector) (*Mutation, error) {
	return i.write(ctx, "insert", vectors)
}

func (i *Index) write(ctx context.Context, op string, vectors []Vector) (*Mutation, error) {
	if len(vectors) == 0 {
		return nil, ErrNoVectors
	}
	dim := len(vectors[0].Values)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for n, v := range vectors {
		if v.ID == "" {
			return nil, fmt.Errorf("drops/cloudflare/vectorize: vector %d has no ID", n)
		}
		if len(v.Values) != dim {
			return nil, fmt.Errorf("%w: vector %d (%q) has %d, the first has %d",
				ErrDimensionMismatch, n, v.ID, len(v.Values), dim)
		}
		if v.Namespace == "" {
			v.Namespace = i.ns
		}
		if err := enc.Encode(v); err != nil {
			return nil, fmt.Errorf("drops/cloudflare/vectorize: encode vector %q: %w", v.ID, err)
		}
	}
	p, err := i.path(op)
	if err != nil {
		return nil, err
	}

	var m Mutation
	err = i.cf.Do(ctx, cloudflare.Request{
		Method:      http.MethodPost,
		Path:        p,
		Raw:         buf.Bytes(),
		ContentType: "application/x-ndjson",
	}, &m)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// GetByIDs fetches vectors by ID, values and metadata included.
func (i *Index) GetByIDs(ctx context.Context, ids ...string) ([]Vector, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	p, err := i.path("get_by_ids")
	if err != nil {
		return nil, err
	}
	var out []Vector
	err = i.cf.Do(ctx, cloudflare.Request{
		Method:     http.MethodPost,
		Path:       p,
		Body:       map[string]any{"ids": ids},
		Idempotent: cloudflare.Idempotently(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteByIDs removes vectors by ID. Like a write, it is applied
// asynchronously.
func (i *Index) DeleteByIDs(ctx context.Context, ids ...string) (*Mutation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	p, err := i.path("delete_by_ids")
	if err != nil {
		return nil, err
	}
	var m Mutation
	err = i.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   p,
		Body:   map[string]any{"ids": ids},
	}, &m)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// CreateMetadataIndex declares that a metadata property can be
// filtered on.
//
// Read this before wondering why a filter matches nothing: Vectorize
// filters only on indexed properties, and an index only covers
// vectors written after it was created. A filter over an unindexed
// property is not an error there — it simply returns no matches,
// which is the failure mode this method exists to prevent. Declare
// the indexes before the first write.
func (i *Index) CreateMetadataIndex(ctx context.Context, property string, kind MetadataIndexType) (*Mutation, error) {
	if property == "" {
		return nil, errors.New("drops/cloudflare/vectorize: metadata property name is empty")
	}
	p, err := i.path("metadata_index", "create")
	if err != nil {
		return nil, err
	}
	var m Mutation
	err = i.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   p,
		Body:   map[string]any{"propertyName": property, "indexType": string(kind)},
	}, &m)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// DeleteMetadataIndex removes a metadata index.
func (i *Index) DeleteMetadataIndex(ctx context.Context, property string) (*Mutation, error) {
	p, err := i.path("metadata_index", "delete")
	if err != nil {
		return nil, err
	}
	var m Mutation
	err = i.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   p,
		Body:   map[string]any{"propertyName": property},
	}, &m)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// ListMetadataIndexes returns the declared metadata indexes — the
// list to check when a filter is returning nothing.
func (i *Index) ListMetadataIndexes(ctx context.Context) ([]MetadataIndex, error) {
	p, err := i.path("metadata_index", "list")
	if err != nil {
		return nil, err
	}
	var out struct {
		MetadataIndexes []MetadataIndex `json:"metadataIndexes"`
	}
	if err := i.cf.Do(ctx, cloudflare.Request{Method: http.MethodGet, Path: p}, &out); err != nil {
		return nil, err
	}
	return out.MetadataIndexes, nil
}

// MetricFor maps a portable
// [github.com/bernardoforcillo/drops/vector.Metric] onto the
// Vectorize name for the same function. The second result is false
// for metrics Vectorize has no equivalent of — L1, Hamming, Jaccard.
func MetricFor(m vector.Metric) (Metric, bool) {
	switch m {
	case vector.Cosine:
		return MetricCosine, true
	case vector.L2:
		return MetricEuclidean, true
	case vector.InnerProduct:
		return MetricDotProduct, true
	default:
		return "", false
	}
}

// NDJSON renders vectors in the line-delimited form the write
// endpoints take. Exported for tests and for callers that want to
// stage a batch to a file first.
func NDJSON(vectors []Vector) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, v := range vectors {
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ParseNDJSON is the inverse of [NDJSON].
func ParseNDJSON(raw []byte) ([]Vector, error) {
	var out []Vector
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var v Vector
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
}
