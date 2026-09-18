// Package workersai turns text into the vectors
// [github.com/bernardoforcillo/drops/vector] searches over.
//
// It is the piece that was missing from the Cloudflare vector story.
// [github.com/bernardoforcillo/drops/cloudflare/vectorize] stores
// embeddings and searches them; [github.com/bernardoforcillo/drops/mirror]
// keeps them in step with a table. Neither produces one — an
// [github.com/bernardoforcillo/drops/mirror.Embedder] has always been
// a function the caller supplies, because drops cannot guess how a
// row becomes a vector. This is Cloudflare's answer to that question,
// on the same account and the same API token as the index it feeds.
//
//	ai := workersai.New(cf, workersai.ModelBGEBaseEN)
//	vec, err := ai.EmbedOne(ctx, "a tender for street lighting")
//
// # It has to agree with the index
//
// An embedding is only comparable with embeddings from the same
// model. A Vectorize index fixes its dimension and metric at
// creation, so the model and the index have to be chosen together,
// and this package's model names are the same strings
// [github.com/bernardoforcillo/drops/cloudflare/vectorize.Preset]
// uses for exactly that reason:
//
//	admin.Create(ctx, vectorize.CreateOptions{
//	    Name:   "tenders",
//	    Preset: vectorize.Preset(workersai.ModelBGEBaseEN),
//	})
//
// Getting it wrong is not symmetrical. A dimension mismatch is
// refused by every write, loudly and immediately. A *model* mismatch
// at the same dimension is accepted, and the search simply returns
// the wrong neighbours — which is why the pairing is worth making
// once, in code, rather than in two configuration files.
//
// # Feeding a mirror
//
// [github.com/bernardoforcillo/drops/mirror.Embedder] is a function,
// so wiring this to a change stream is one closure — and the closure
// is where the decision lives that this package cannot make: which
// columns are the document.
//
//	embed := func(ctx context.Context, ch mirror.Change) ([]float32, error) {
//	    body, _ := ch.Row["body"].(string)
//	    if body == "" {
//	        return nil, nil // a nil vector skips the row
//	    }
//	    return ai.EmbedOne(ctx, body)
//	}
//	sink, err := mirror.NewQdrantSink(cli, "tenders", embed)
//
// # Limits
//
// A hundred texts per request and about 512 tokens each, for the BGE
// models. [Embedder.Embed] chunks a longer slice for you and says so
// in [Result.Requests]; the token ceiling it cannot help with, because
// truncation happens at the model and is not reported — a document
// longer than the window is embedded from its beginning, and the
// tail simply does not influence the vector. Split long documents
// into passages and embed each, which is what makes a search return
// the paragraph rather than the file.
package workersai
