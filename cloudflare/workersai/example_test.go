package workersai_test

import (
	"context"
	"fmt"
	"log"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/vectorize"
	"github.com/bernardoforcillo/drops/cloudflare/workersai"
	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/vector"
)

func ExampleNew() {
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}
	ai := workersai.New(cf, workersai.ModelBGEBaseEN)

	vec, err := ai.EmbedOne(context.Background(), "a tender for street lighting")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("dimensions:", len(vec))
}

// The model and the index are one decision, so they are named from
// one constant.
func Example_indexAndModelTogether() {
	ctx := context.Background()
	cf, err := cloudflare.New("your-account-id", cloudflare.WithAPIToken("your-api-token"))
	if err != nil {
		log.Fatal(err)
	}

	const model = workersai.ModelBGEBaseEN
	ai := workersai.New(cf, model)

	// The preset fixes the index's dimension and metric to the ones
	// this model produces. A dimension mismatch is refused on every
	// write; a model mismatch at the same dimension is not refused at
	// all, and simply returns the wrong neighbours.
	admin := vectorize.NewAdmin(cf)
	info, err := admin.Create(ctx, vectorize.CreateOptions{
		Name:   "tenders",
		Preset: vectorize.Preset(model),
	})
	if err != nil {
		log.Fatal(err)
	}
	index := admin.Index(info.Name)

	res, err := ai.Embed(ctx, "street lighting", "road resurfacing")
	if err != nil {
		log.Fatal(err)
	}
	if _, err := index.Upsert(ctx,
		vectorize.Vector{ID: "t1", Values: res.Vectors[0]},
		vectorize.Vector{ID: "t2", Values: res.Vectors[1]},
	); err != nil {
		log.Fatal(err)
	}
}

// A search is an embedding of the question, run against the index.
func ExampleEmbedder_EmbedOne() {
	var ai *workersai.Embedder
	var index *vectorize.Index
	ctx := context.Background()

	q, err := ai.EmbedOne(ctx, "who resurfaces roads in Lombardy?")
	if err != nil {
		log.Fatal(err)
	}
	hits, err := index.Search(ctx, vector.Search(q).TopK(10).Build())
	if err != nil {
		log.Fatal(err)
	}
	for _, h := range hits.Hits {
		fmt.Println(h.ID, h.Score)
	}
}

// Wiring it to a change stream is one closure — and the closure is
// where the decision this package cannot make lives: which columns
// are the document.
func ExampleEmbedder_Embed_mirror() {
	var ai *workersai.Embedder

	embed := func(ctx context.Context, ch mirror.Change) ([]float32, error) {
		body, _ := ch.Row["body"].(string)
		if body == "" {
			// A nil vector skips the row, which is how a change that
			// does not touch the indexed text says so.
			return nil, nil
		}
		return ai.EmbedOne(ctx, body)
	}
	_ = embed
}
