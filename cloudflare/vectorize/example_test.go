package vectorize_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/vectorize"
	"github.com/bernardoforcillo/drops/vector"
)

func ExampleNew() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	index := vectorize.New(cf, "product-embeddings", vectorize.WithMetric(vector.Cosine))
	_ = index
}

// The portable query: the same value runs against pgvector, Qdrant
// and Vectorize.
func ExampleIndex_Search() {
	var index *vectorize.Index
	ctx := context.Background()

	res, err := index.Search(ctx, vector.Search(embedding()).
		TopK(20).
		Where(
			vector.Eq("lang", "it"),
			vector.Gte("published_at", 1700000000),
		).
		WithPayload().
		Build())
	if err != nil {
		log.Fatal(err)
	}
	for _, hit := range res.Hits {
		fmt.Printf("%v at distance %.3f\n", hit.ID, hit.Distance)
	}
}

// Vectorize's filter language is conjunctive: an Or has no
// equivalent, and approximating it would change what topK means.
func ExampleCompileFilter_unsupported() {
	_, err := vectorize.CompileFilter(vector.Or(
		vector.Eq("lang", "it"),
		vector.Eq("lang", "en"),
	))
	if errors.Is(err, vector.ErrUnsupportedOp) {
		// Index the disjunction as its own property instead — a
		// "lang_group" that is already "it_or_en" at write time —
		// and filter on that with Eq.
		fmt.Println("no $or; move the disjunction into the metadata")
	}
	// Output: no $or; move the disjunction into the metadata
}

// A filter can only narrow on a property that has a metadata index,
// and the index only covers vectors written after it existed. A
// filter over an unindexed property is not an error at Vectorize — it
// just matches nothing, which is the worst way for this to fail.
func ExampleIndex_CreateMetadataIndex() {
	var index *vectorize.Index
	ctx := context.Background()

	for property, kind := range map[string]vectorize.MetadataIndexType{
		"lang":         vectorize.MetadataString,
		"published_at": vectorize.MetadataNumber,
		"is_active":    vectorize.MetadataBoolean,
	} {
		if _, err := index.CreateMetadataIndex(ctx, property, kind); err != nil {
			log.Fatal(err)
		}
	}
	// Then write the vectors. Not the other way round.
}

// Vectorize has no delete-by-filter. For a bounded set this composes
// one out of a query and a delete-by-ID — and refuses when the set
// may be larger than a query can see.
func ExampleIndex_DeleteWhere() {
	var index *vectorize.Index
	ctx := context.Background()

	_, err := index.DeleteWhere(ctx, embedding(), vector.Eq("document_id", "doc-1"))
	if errors.Is(err, vectorize.ErrDeleteByFilterUnbounded) {
		// The way that scales: derive the IDs from what you would
		// have filtered on, so the set can be regenerated without
		// asking the index what is in it.
		ids := make([]string, 0, 64)
		for n := 0; n < 64; n++ {
			ids = append(ids, fmt.Sprintf("doc-1:%d", n))
		}
		if _, err := index.DeleteByIDs(ctx, ids...); err != nil {
			log.Fatal(err)
		}
	}
}

func ExampleIndex_Upsert() {
	var index *vectorize.Index
	ctx := context.Background()

	m, err := index.Upsert(ctx,
		vectorize.Vector{
			ID:       "doc-1:0",
			Values:   embedding(),
			Metadata: map[string]any{"lang": "it", "document_id": "doc-1"},
		},
		vectorize.Vector{
			ID:       "doc-1:1",
			Values:   embedding(),
			Metadata: map[string]any{"lang": "it", "document_id": "doc-1"},
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	// The write is applied asynchronously: a query issued now may
	// not see it yet.
	fmt.Println("queued as", m.MutationID)
}

func embedding() []float32 { return make([]float32, 768) }
