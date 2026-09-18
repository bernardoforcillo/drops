package vectorize

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/vector"
)

// Admin errors.
var (
	// ErrNoDimensions is returned by [Admin.Create] when neither a
	// dimension count nor a preset says how wide the vectors are.
	ErrNoDimensions = errors.New("drops/cloudflare/vectorize: an index needs a dimension count or a preset")

	// ErrAmbiguousConfig is returned by [Admin.Create] given both a
	// preset and an explicit dimension or metric. A preset *is* a
	// dimension and a metric, so supplying both leaves the caller's
	// intent undecidable — and the half that lost would be
	// discovered by the first query that ranked wrongly.
	ErrAmbiguousConfig = errors.New("drops/cloudflare/vectorize: an index takes a preset or an explicit dimension and metric, not both")

	// ErrUnknownPreset is returned for a preset Vectorize does not
	// define.
	ErrUnknownPreset = errors.New("drops/cloudflare/vectorize: unknown preset")

	// ErrUnknownMetric is returned for a distance function Vectorize
	// does not offer.
	ErrUnknownMetric = errors.New("drops/cloudflare/vectorize: unknown metric")
)

// Preset is a named embedding model whose dimension count and metric
// Vectorize already knows.
//
// Creating an index from one is the way to be sure the index agrees
// with whatever produces the vectors: the dimension has to match
// exactly or every write is refused, and the metric has to match or
// the ranking is quietly wrong rather than refused.
type Preset string

// The presets Vectorize defines.
const (
	PresetBGESmallEN    Preset = "@cf/baai/bge-small-en-v1.5"
	PresetBGEBaseEN     Preset = "@cf/baai/bge-base-en-v1.5"
	PresetBGELargeEN    Preset = "@cf/baai/bge-large-en-v1.5"
	PresetOpenAIAda002  Preset = "openai/text-embedding-ada-002"
	PresetCohereMultiV2 Preset = "cohere/embed-multilingual-v2.0"
)

// Valid reports whether p is a preset Vectorize defines.
func (p Preset) Valid() bool {
	switch p {
	case PresetBGESmallEN, PresetBGEBaseEN, PresetBGELargeEN, PresetOpenAIAda002, PresetCohereMultiV2:
		return true
	default:
		return false
	}
}

// Admin manages Vectorize indexes themselves — as opposed to [Index],
// which reads and writes the vectors inside one.
//
// It is a separate type for the reason
// [github.com/bernardoforcillo/drops/cloudflare/d1.Admin] is: a
// service that only queries should not hold the handle that can
// delete the index, and a token scoped to Vectorize:Read cannot
// create one.
//
//	admin := vectorize.NewAdmin(cf)
//	info, err := admin.Create(ctx, vectorize.CreateOptions{
//	    Name:   "product-embeddings",
//	    Preset: vectorize.PresetBGEBaseEN,
//	})
//	store := admin.Index(info.Name)
type Admin struct {
	cf *cloudflare.Client
}

// NewAdmin returns an Admin for the account cf addresses.
//
// The token needs Vectorize:Edit to create or delete, Vectorize:Read
// to list.
func NewAdmin(cf *cloudflare.Client) *Admin { return &Admin{cf: cf} }

// Client returns the underlying Cloudflare API client.
func (a *Admin) Client() *cloudflare.Client { return a.cf }

// Index returns a client for one of this account's indexes, over the
// same Cloudflare client.
//
// Pass [WithMetric] matching what the index was created with: the
// metric decides how a score is read, and this constructor does not
// go and ask.
func (a *Admin) Index(name string, opts ...Option) *Index {
	return New(a.cf, name, opts...)
}

// CreateOptions describes an index to create.
//
// Exactly one of Preset and (Dimensions, Metric) must be set: a
// preset already names both.
type CreateOptions struct {
	// Name is the index's name, unique within the account, and what
	// [New] takes.
	Name string

	// Description is free text shown in the dashboard.
	Description string

	// Dimensions is how many components a vector has. It is fixed at
	// creation and cannot be changed afterwards — an index is
	// rebuilt, not resized — so this is the number to get from the
	// model rather than from memory.
	Dimensions int

	// Metric is the distance function. Fixed at creation too, which
	// is why [WithMetric] on an [Index] is an assertion rather than
	// a request.
	Metric vector.Metric

	// Preset names an embedding model whose dimension and metric
	// Vectorize already knows, and sets both.
	Preset Preset
}

// body renders the create payload, refusing a configuration
// Vectorize would reject or that says two things at once.
func (o CreateOptions) body() (map[string]any, error) {
	if o.Name == "" {
		return nil, ErrNoIndexName
	}
	explicit := o.Dimensions > 0 || o.Metric != ""
	switch {
	case o.Preset != "" && explicit:
		return nil, ErrAmbiguousConfig
	case o.Preset == "" && o.Dimensions <= 0:
		return nil, ErrNoDimensions
	}

	body := map[string]any{"name": o.Name}
	if o.Description != "" {
		body["description"] = o.Description
	}
	if o.Preset != "" {
		if !o.Preset.Valid() {
			return nil, fmt.Errorf("%w: %q", ErrUnknownPreset, o.Preset)
		}
		body["config"] = map[string]any{"preset": string(o.Preset)}
		return body, nil
	}

	m := o.Metric
	if m == "" {
		m = vector.Cosine
	}
	wire, ok := MetricFor(m)
	if !ok {
		return nil, fmt.Errorf("%w: %q — Vectorize offers cosine, euclidean and dot-product", ErrUnknownMetric, m)
	}
	body["config"] = map[string]any{"dimensions": o.Dimensions, "metric": string(wire)}
	return body, nil
}

// Create makes an index and returns its configuration.
//
// Neither the dimension nor the metric can be changed afterwards, so
// this call is where both are decided: a vector of the wrong length
// is refused on every write, and a mismatched metric is worse — it
// ranks, and it ranks wrongly.
func (a *Admin) Create(ctx context.Context, opts CreateOptions) (*Info, error) {
	body, err := opts.body()
	if err != nil {
		return nil, err
	}
	var info Info
	if err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   a.cf.AccountPath("/vectorize/v2/indexes"),
		Body:   body,
	}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Get returns one index's configuration. It is [Index.Describe]
// reached from the admin handle, for the code that has a name rather
// than an [Index].
func (a *Admin) Get(ctx context.Context, name string) (*Info, error) {
	if name == "" {
		return nil, ErrNoIndexName
	}
	var info Info
	if err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   a.cf.AccountPath("/vectorize/v2/indexes", name),
	}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// List returns the account's indexes.
func (a *Admin) List(ctx context.Context) ([]Info, error) {
	var out []Info
	if err := a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   a.cf.AccountPath("/vectorize/v2/indexes"),
	}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// FindByName returns the index with the given name, or
// [cloudflare.ErrNotFound].
func (a *Admin) FindByName(ctx context.Context, name string) (*Info, error) {
	if name == "" {
		return nil, ErrNoIndexName
	}
	all, err := a.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("drops/cloudflare/vectorize: no index named %q: %w", name, cloudflare.ErrNotFound)
}

// Delete removes an index and every vector in it. There is no undo
// and no export: the vectors are derived data, and regenerating them
// is the recovery path.
func (a *Admin) Delete(ctx context.Context, name string) error {
	if name == "" {
		return ErrNoIndexName
	}
	return a.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   a.cf.AccountPath("/vectorize/v2/indexes", name),
	}, nil)
}

// ListVectorsOptions pages [Index.ListVectors].
type ListVectorsOptions struct {
	// Count is how many identifiers to return. Zero leaves
	// Vectorize's default.
	Count int

	// Cursor continues from where the previous page stopped.
	Cursor string
}

// VectorPage is one page of vector identifiers.
type VectorPage struct {
	// IDs are the identifiers in this page, and only the
	// identifiers: the endpoint does not return values or metadata,
	// which is what makes it cheap enough to walk an index with.
	// [Index.GetByIDs] fetches the records themselves.
	IDs []string

	// Count is how many identifiers this page holds.
	Count int64

	// TotalCount is how many vectors the index holds altogether.
	TotalCount int64

	// Cursor continues the listing, and is empty at the end of it.
	Cursor string

	// CursorExpires is when the cursor stops working. A listing
	// resumed after it has to start again — which is why a sweep
	// over a large index should do its work as it goes rather than
	// collecting every identifier first.
	CursorExpires time.Time

	// Truncated reports whether Vectorize had more to give.
	Truncated bool
}

// ListVectors returns a page of the index's vector identifiers.
//
// It is the enumeration a delete-by-filter would need and Vectorize
// does not have — see [Index.DeleteWhere] for why that operation is
// refused rather than approximated. Walking the index is the honest
// version of it, and it is honest about the cost: identifiers only,
// a page at a time, with a cursor that expires.
func (i *Index) ListVectors(ctx context.Context, opts ListVectorsOptions) (*VectorPage, error) {
	p, err := i.path("list")
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.Count > 0 {
		q.Set("count", strconv.Itoa(opts.Count))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}

	var raw struct {
		Count                     int64  `json:"count"`
		TotalCount                int64  `json:"totalCount"`
		IsTruncated               bool   `json:"isTruncated"`
		NextCursor                string `json:"nextCursor"`
		CursorExpirationTimestamp string `json:"cursorExpirationTimestamp"`
		Vectors                   []struct {
			ID string `json:"id"`
		} `json:"vectors"`
	}
	if err := i.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   p,
		Query:  q,
	}, &raw); err != nil {
		return nil, err
	}

	page := &VectorPage{
		Count:      raw.Count,
		TotalCount: raw.TotalCount,
		Cursor:     raw.NextCursor,
		Truncated:  raw.IsTruncated,
		IDs:        make([]string, len(raw.Vectors)),
	}
	for n, v := range raw.Vectors {
		page.IDs[n] = v.ID
	}
	if raw.CursorExpirationTimestamp != "" {
		if t, parseErr := time.Parse(time.RFC3339, raw.CursorExpirationTimestamp); parseErr == nil {
			page.CursorExpires = t
		}
	}
	return page, nil
}
