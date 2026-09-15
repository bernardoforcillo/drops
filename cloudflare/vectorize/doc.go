// Package vectorize is the Cloudflare Vectorize backend for
// [github.com/bernardoforcillo/drops/vector].
//
// It supplies a [github.com/bernardoforcillo/drops/vector.Store], so
// the portable query that runs against a pgvector table or a Qdrant
// collection runs here too:
//
//	cf, _ := cloudflare.New(accountID, cloudflare.WithAPIToken(token))
//	store := vectorize.New(cf, "product-embeddings", vectorize.WithMetric(vector.Cosine))
//
//	res, err := store.Search(ctx, vector.Search(embedding).
//	    TopK(20).
//	    Where(vector.Eq("lang", "it"), vector.Gte("published_at", cutoff)).
//	    WithPayload().
//	    Build())
//
// Alongside the portable door, [Index] is the typed one: create and
// describe indexes, upsert and insert vectors, fetch and delete by ID,
// and declare the metadata indexes without which a filter cannot
// narrow anything.
//
// # Three constraints that change what you can ask for
//
// Vectorize is a smaller query language than the other vector stores
// drops speaks to, and the gaps are not ones an adapter can paper
// over. Each one fails loudly rather than quietly returning the wrong
// page.
//
// First, the filter language is conjunctive. Vectorize has $eq, $ne,
// $in, $nin, $lt, $lte, $gt and $gte, combined by listing them —
// which means AND — and it has no $or and no $not. So
// [vector.Or] and [vector.Not] compile to
// [vector.ErrUnsupportedOp], as do [vector.MatchText],
// [vector.IsNull], [vector.HasID] and [vector.GeoWithin]. A negated
// leaf that has a direct opposite is rewritten into it (Not(Eq) is
// Ne, Not(In) is NotIn); anything else is refused.
//
// Second, a metadata field can only be filtered on once a metadata
// index exists for it, and creating one only affects vectors written
// afterwards. A filter over an unindexed field is not an error at
// Vectorize — it simply matches nothing, which is the worst way for
// this to go wrong. [Index.CreateMetadataIndex] declares one and
// [Index.ListMetadataIndexes] shows what is declared; check the
// second before concluding a filter is broken.
//
// Third, there is no pagination. The query endpoint takes topK and
// nothing else, and topK is capped — at [MaxTopK], and at
// [MaxTopKWithValues] once values or full metadata are returned. The
// [vector.Cursor] contract is honoured by over-fetching and slicing
// client-side, which works exactly as far as that ceiling and then
// returns [ErrPageBeyondTopK] rather than a page that silently
// repeats rows.
//
// # Scores
//
// Vectorize's score means different things per metric — a similarity
// for cosine and dot-product, a distance for euclidean — the same
// asymmetry Qdrant has. The metric is fixed when the index is
// created, so [WithMetric] is an assertion about the index rather
// than a request: it is what tells the adapter how to read the score,
// and a query asking for a different one is
// [vector.ErrUnsupportedMetric] rather than a ranking by the wrong
// function.
package vectorize
