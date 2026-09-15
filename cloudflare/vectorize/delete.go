package vectorize

import (
	"context"
	"errors"
	"fmt"

	"github.com/bernardoforcillo/drops/vector"
)

// ErrDeleteByFilterUnbounded is returned by [Index.DeleteWhere] when
// the matching set may be larger than one query can see. See that
// method for what to do instead.
var ErrDeleteByFilterUnbounded = errors.New("drops/cloudflare/vectorize: delete-by-filter would exceed the topK ceiling")

// DeleteWhere deletes the vectors matching a portable filter.
//
// Vectorize has no delete-by-filter. What it has is delete-by-ID, so
// this finds the IDs with a query and then deletes them — and that
// composition is bounded in a way a real delete-by-filter is not.
// A query returns at most [Index.TopKCeiling] hits, so a filter
// matching more than that cannot be fully resolved, and this method
// refuses with [ErrDeleteByFilterUnbounded] rather than deleting an
// arbitrary prefix of the set and reporting success.
//
// So it is a convenience for a bounded delete — one document's
// chunks, one tenant's few hundred vectors — and not a substitute for
// the operation Vectorize lacks.
//
// For an unbounded delete, do not reach for this. Derive the IDs
// instead: make them a deterministic function of what you would have
// filtered on, so the set can be regenerated without asking the index
// what is in it.
//
//	// Instead of deleting where document_id = X:
//	func chunkID(docID string, n int) string {
//	    return fmt.Sprintf("%s:%d", docID, n)
//	}
//	// … then delete chunkID(doc, 0..count-1) directly.
//
// That is the shape that scales, because it needs no query at all —
// and it is worth adopting before the collection grows past the
// ceiling, not after.
//
// probe is the vector to search around. Vectorize has no "match
// everything" query — every search is a nearest-neighbour search — so
// a filtered delete still needs a point to search from. Any vector of
// the right dimension will do when the filter is what selects the
// set; the hits are taken for their IDs, not their ranking.
func (i *Index) DeleteWhere(ctx context.Context, probe []float32, filter vector.Filter) (*Mutation, error) {
	if len(probe) == 0 {
		return nil, vector.ErrNoVector
	}
	compiled, err := CompileFilter(filter)
	if err != nil {
		return nil, err
	}
	if len(compiled) == 0 {
		return nil, errors.New("drops/cloudflare/vectorize: DeleteWhere needs a filter; an empty one would delete whatever the probe happened to be near")
	}

	ceiling := i.TopKCeiling(false, MetadataNone)
	resp, err := i.Query(ctx, QueryRequest{
		Vector:         probe,
		TopK:           ceiling,
		ReturnMetadata: MetadataNone,
		Filter:         compiled,
		Namespace:      i.ns,
	})
	if err != nil {
		return nil, err
	}
	if len(resp.Matches) == 0 {
		return nil, nil
	}
	// A full page means there may be more behind it, and there is no
	// way to ask. Deleting what was seen and returning success would
	// leave the caller believing the filter had been applied.
	if len(resp.Matches) >= ceiling {
		return nil, fmt.Errorf(
			"%w: the filter matched at least %d vectors and a query sees at most that many — derive deterministic IDs and delete those instead (see Index.DeleteWhere)",
			ErrDeleteByFilterUnbounded, ceiling)
	}

	ids := make([]string, len(resp.Matches))
	for n, m := range resp.Matches {
		ids[n] = m.ID
	}
	return i.DeleteByIDs(ctx, ids...)
}
