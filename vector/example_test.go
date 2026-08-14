package vector_test

import (
	"context"
	"fmt"
	"math"
	"sort"

	"github.com/bernardoforcillo/drops/vector"
)

// memStore is a toy in-memory vector.Store. A real one is
// pg.NewVectorStore, clickhouse.NewVectorStore or (*qdrant.Client).Store
// — this one exists so the examples run without a database.
type memStore struct {
	ids     []string
	vectors [][]float32
}

func (m *memStore) Search(_ context.Context, q vector.Query) (*vector.Results, error) {
	q = q.Normalized()
	if err := q.Validate(); err != nil {
		return nil, err
	}
	hits := make([]vector.Hit, 0, len(m.ids))
	for i, v := range m.vectors {
		d := cosineDistance(q.Vector, v)
		if q.MaxDistance != nil && d > *q.MaxDistance {
			continue
		}
		hits = append(hits, vector.Hit{
			ID:       m.ids[i],
			Distance: d,
			Score:    vector.ScoreFromDistance(q.Metric, d),
		})
	}
	sort.Slice(hits, func(a, b int) bool { return hits[a].Distance < hits[b].Distance })

	// Resume after the cursor, then trim to the page — the same
	// TopK+1 probe every real backend uses.
	cur, ok, err := vector.DecodeCursor(q.Cursor, vector.BackendPostgres)
	if err != nil {
		return nil, err
	}
	if ok && cur.Distance != nil {
		for len(hits) > 0 && hits[0].Distance <= *cur.Distance {
			hits = hits[1:]
		}
	}
	res := &vector.Results{HasMore: len(hits) > q.TopK}
	if res.HasMore {
		hits = hits[:q.TopK]
		next, kErr := vector.KeysetCursor(vector.BackendPostgres, hits[len(hits)-1])
		if kErr != nil {
			return nil, kErr
		}
		if res.NextCursor, err = next.Encode(); err != nil {
			return nil, err
		}
	}
	res.Hits = hits
	return res, nil
}

func cosineDistance(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// The point of the package: search is written once against
// vector.Store, so the same function runs against pgvector, ClickHouse
// or Qdrant depending only on what is passed in.
func ExampleStore() {
	store := &memStore{
		ids:     []string{"go", "rust", "cooking"},
		vectors: [][]float32{{1, 0, 0}, {0.9, 0.1, 0}, {0, 0, 1}},
	}

	q := vector.Search([]float32{1, 0, 0}).
		TopK(2).
		Metric(vector.Cosine).
		Where(vector.And(
			vector.Eq("lang", "en"),
			vector.Gte("year", 2020),
		)).
		WithPayload().
		Build()

	res, err := store.Search(context.Background(), q)
	if err != nil {
		panic(err)
	}
	for _, h := range res.Hits {
		fmt.Printf("%s (distance %.2f)\n", h.ID, h.Distance)
	}
	// Output:
	// go (distance 0.00)
	// rust (distance 0.01)
}

// Paging is the same three lines whichever store is underneath: pass
// the previous page's cursor back, stop when HasMore goes false.
func ExampleResults_pagination() {
	store := &memStore{
		ids:     []string{"a", "b", "c", "d"},
		vectors: [][]float32{{1, 0, 0}, {0.9, 0.1, 0}, {0.5, 0.5, 0}, {0, 1, 0}},
	}

	ctx := context.Background()
	for cursor, page := "", 1; ; page++ {
		res, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).TopK(2).After(cursor).Build())
		if err != nil {
			panic(err)
		}
		for _, h := range res.Hits {
			fmt.Printf("page %d: %s\n", page, h.ID)
		}
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
	}
	// Output:
	// page 1: a
	// page 1: b
	// page 2: c
	// page 2: d
}
