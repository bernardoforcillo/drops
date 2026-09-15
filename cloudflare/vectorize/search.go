package vectorize

import (
	"context"
	"fmt"
	"net/http"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/vector"
)

var _ vector.Store = (*Index)(nil)

// QueryRequest is the body of a Vectorize query.
type QueryRequest struct {
	Vector         []float32      `json:"vector"`
	TopK           int            `json:"topK,omitempty"`
	ReturnValues   bool           `json:"returnValues,omitempty"`
	ReturnMetadata ReturnMetadata `json:"returnMetadata,omitempty"`
	Filter         Filter         `json:"filter,omitempty"`
	Namespace      string         `json:"namespace,omitempty"`
}

// QueryResponse is what a Vectorize query answers with.
type QueryResponse struct {
	Count   int     `json:"count"`
	Matches []Match `json:"matches"`
}

// Query is the native door onto the query endpoint, for callers that
// want Vectorize's own vocabulary rather than the portable one.
func (i *Index) Query(ctx context.Context, req QueryRequest) (*QueryResponse, error) {
	p, err := i.path("query")
	if err != nil {
		return nil, err
	}
	if req.Namespace == "" {
		req.Namespace = i.ns
	}
	var out QueryResponse
	err = i.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   p,
		Body:   req,
		// A query reads. Cloudflare models it as a POST because it
		// carries a body, not because it has an effect.
		Idempotent: cloudflare.Idempotently(),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TopKCeiling returns the default topK ceiling for a query with these
// return options — [MaxTopKWithValues] once values or full metadata
// are asked for, [MaxTopK] otherwise.
//
// [Index.TopKCeiling] is the one a query actually goes through; it
// honours [WithTopKCeilings].
func TopKCeiling(returnValues bool, metadata ReturnMetadata) int {
	if returnValues || metadata == MetadataAll {
		return MaxTopKWithValues
	}
	return MaxTopK
}

// Search implements
// [github.com/bernardoforcillo/drops/vector.Store].
//
// Pagination deserves a word, because Vectorize offers none. The
// query endpoint takes topK and returns the topK nearest — there is
// no offset, no keyset, nothing to resume from. What the cursor
// carries is therefore an offset this adapter applies itself: it asks
// for offset+TopK+1 hits and slices. That is a real page, with no
// duplicates and no gaps, for as long as offset+TopK+1 stays under
// the ceiling [TopKCeiling] reports — and past it the answer is
// [ErrPageBeyondTopK] rather than a page that quietly repeats the
// last one.
//
// So paging works, shallowly, and says so when it stops. At the
// ceiling HasMore goes false, which is the honest reading of it: the
// vector package defines HasMore as "a further page exists", and past
// topK no further page exists through this API however many vectors
// are in the index. A result set that needs deeper paging than that
// wants a different store, or a narrower filter.
func (i *Index) Search(ctx context.Context, q vector.Query) (*vector.Results, error) {
	q = q.Normalized()
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if i.name == "" {
		return nil, ErrNoIndexName
	}
	if q.Metric != i.metric {
		return nil, fmt.Errorf("%w: index %q is %s, query asked for %s",
			vector.ErrUnsupportedMetric, i.name, i.metric, q.Metric)
	}
	if _, ok := MetricFor(q.Metric); !ok {
		return nil, fmt.Errorf("%w: Vectorize has no %s distance", vector.ErrUnsupportedMetric, q.Metric)
	}

	filter, err := CompileFilter(q.Filter)
	if err != nil {
		return nil, err
	}

	cur, _, err := vector.DecodeCursor(q.Cursor, BackendVectorize)
	if err != nil {
		return nil, err
	}

	metadata := MetadataNone
	if q.IncludePayload {
		// "indexed" returns the filterable fields and does not lower
		// the topK ceiling; "all" returns everything and drops it to
		// 20. Which one the caller wants is a genuine choice, so it
		// is a Param rather than a guess — and the cheaper one is
		// the default.
		metadata = MetadataIndexed
		if raw, ok := q.Param("returnMetadata"); ok {
			if s, isString := raw.(string); isString {
				metadata = ReturnMetadata(s)
			}
		}
	}

	// One extra hit is what tells HasMore from "exactly a full page",
	// and it costs nothing: it is the same request.
	want := cur.Offset + q.TopK + 1
	ceiling := i.TopKCeiling(q.IncludeVector, metadata)
	if cur.Offset >= ceiling {
		return nil, fmt.Errorf("%w: page starts at %d, and Vectorize returns at most %d hits",
			ErrPageBeyondTopK, cur.Offset, ceiling)
	}
	if want > ceiling {
		want = ceiling
	}

	req := QueryRequest{
		Vector:         q.Vector,
		TopK:           want,
		ReturnValues:   q.IncludeVector,
		ReturnMetadata: metadata,
		Filter:         filter,
		Namespace:      i.ns,
	}
	if ns, ok := q.Param("namespace"); ok {
		if s, isString := ns.(string); isString {
			req.Namespace = s
		}
	}

	resp, err := i.Query(ctx, req)
	if err != nil {
		return nil, err
	}

	matches := resp.Matches
	if cur.Offset > 0 {
		if cur.Offset >= len(matches) {
			// The offset ran past the end of the result set. That is
			// an empty last page, not an error.
			return &vector.Results{}, nil
		}
		matches = matches[cur.Offset:]
	}

	res := &vector.Results{}
	res.HasMore = len(matches) > q.TopK
	if res.HasMore {
		matches = matches[:q.TopK]
	}
	res.Hits = make([]vector.Hit, 0, len(matches))
	for _, m := range matches {
		d := distanceFromScore(q.Metric, m.Score)
		if q.MaxDistance != nil && d > *q.MaxDistance {
			// Vectorize has no score threshold in the request, so
			// the ceiling is applied here. Hits come back nearest
			// first, so everything after the first one over the
			// bound is over it too.
			res.HasMore = false
			break
		}
		res.Hits = append(res.Hits, vector.Hit{
			ID:       m.ID,
			Distance: d,
			Score:    vector.ScoreFromDistance(q.Metric, d),
			Vector:   m.Values,
			Payload:  m.Metadata,
		})
	}

	if res.HasMore {
		next, cErr := vector.OffsetCursor(BackendVectorize, cur.Offset+len(res.Hits)).Encode()
		if cErr != nil {
			return nil, cErr
		}
		res.NextCursor = next
	}
	return res, nil
}

// BackendVectorize stamps the cursors this store issues, so replaying
// one against a pgvector or Qdrant store fails with
// [github.com/bernardoforcillo/drops/vector.ErrCursorMismatch] rather
// than returning the wrong page.
const BackendVectorize vector.Backend = "vectorize"

// Vectorize's score is not one thing. For cosine and dot-product it
// is a similarity (higher is better); for euclidean it is the
// distance itself (lower is better). These two functions are the only
// place that asymmetry is written down — everything downstream works
// in the portable convention where Distance is always "lower is
// closer".

func distanceFromScore(m vector.Metric, score float64) float64 {
	switch m {
	case vector.Cosine:
		return 1 - score
	case vector.InnerProduct:
		return -score
	default: // euclidean: the score already is the distance.
		return score
	}
}
