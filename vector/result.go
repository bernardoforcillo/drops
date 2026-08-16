package vector

// Hit is one search result.
//
// Distance and Score are two views of the same number, both always
// populated, because backends disagree about which one they return
// natively and callers disagree about which one they want:
//
//   - Distance is in the [Metric]'s own units, and smaller is closer.
//     Compare it against a threshold, or against another hit from the
//     same query.
//   - Score is a ranking value where larger is better. Sort by it
//     without caring which metric produced it.
//
// [ScoreFromDistance] and [DistanceFromScore] are the single
// conversion between them; every adapter goes through those two
// functions rather than re-deriving the sign convention.
type Hit struct {
	// ID is the record's identifier — an integer or a string,
	// whichever the store holds.
	ID any

	// Distance is the metric distance to the query vector. Lower is
	// closer.
	Distance float64

	// Score is the similarity. Higher is better.
	Score float64

	// Vector is the stored embedding, present only when the query set
	// [Query.IncludeVector].
	Vector []float32

	// Payload is the record's metadata, present only when the query
	// set [Query.IncludePayload].
	Payload map[string]any
}

// Results is one page of hits.
type Results struct {
	// Hits are the matches, nearest first.
	Hits []Hit

	// NextCursor resumes the search after the last hit. Empty when
	// there is no further page.
	NextCursor string

	// HasMore reports whether a further page exists. It is decided by
	// asking the backend for TopK+1 hits and trimming, so it costs no
	// extra round trip and never lies about a page that turns out to
	// be empty.
	HasMore bool
}

// ScoreFromDistance converts a metric distance into a
// larger-is-better score.
//
// For [Cosine] the natural similarity is 1-d, which lands in [-1, 1]
// and reads the way people expect a cosine similarity to read. For
// [InnerProduct] the distance is already the negated product, so the
// score is its negation — the raw inner product. Every other metric
// (L2, L1, Hamming, Jaccard) has no bounded similarity to map onto, so
// the score is simply -d: unbounded, but correctly ordered, which is
// all a score has to be.
func ScoreFromDistance(m Metric, d float64) float64 {
	if m == Cosine {
		return 1 - d
	}
	return -d
}

// DistanceFromScore is the inverse of [ScoreFromDistance]. Adapters
// over stores that return a score natively — Qdrant — use it to fill
// in [Hit.Distance].
func DistanceFromScore(m Metric, s float64) float64 {
	if m == Cosine {
		return 1 - s
	}
	return -s
}
