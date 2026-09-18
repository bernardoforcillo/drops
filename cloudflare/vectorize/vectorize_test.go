package vectorize_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/vectorize"
	"github.com/bernardoforcillo/drops/vector"
)

type server struct {
	t      *testing.T
	srv    *httptest.Server
	paths  []string
	bodies []string
	reply  func(n int) string
	n      int
}

func newServer(t *testing.T, reply func(n int) string) *server {
	t.Helper()
	s := &server{t: t, reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.paths = append(s.paths, r.URL.Path)
		s.bodies = append(s.bodies, string(raw))
		s.n++
		fmt.Fprint(w, s.reply(s.n))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *server) client() *cloudflare.Client {
	s.t.Helper()
	c, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.srv.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}))
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return c
}

func (s *server) index(opts ...vectorize.Option) *vectorize.Index {
	return vectorize.New(s.client(), "emb", opts...)
}

func envelope(result string) string {
	return `{"result":` + result + `,"success":true,"errors":[],"messages":[]}`
}

// matches builds a query reply with the given id/score pairs.
func matches(pairs ...any) string {
	parts := make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		parts = append(parts, fmt.Sprintf(`{"id":%q,"score":%v}`, pairs[i], pairs[i+1]))
	}
	return envelope(fmt.Sprintf(`{"count":%d,"matches":[%s]}`, len(parts), strings.Join(parts, ",")))
}

// Filter compilation --------------------------------------------------

func TestCompileFilterConjunction(t *testing.T) {
	f, err := vectorize.CompileFilter(vector.And(
		vector.Eq("lang", "it"),
		vector.Gte("published_at", 1700000000),
	))
	if err != nil {
		t.Fatalf("CompileFilter: %v", err)
	}
	raw, _ := json.Marshal(f)
	var got map[string]map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["lang"]["$eq"] != "it" {
		t.Errorf("lang = %v", got["lang"])
	}
	if got["published_at"]["$gte"] != float64(1700000000) {
		t.Errorf("published_at = %v", got["published_at"])
	}
}

func TestZeroFilterCompilesToNothing(t *testing.T) {
	f, err := vectorize.CompileFilter(vector.Filter{})
	if err != nil {
		t.Fatalf("CompileFilter: %v", err)
	}
	if f != nil {
		t.Errorf("zero filter compiled to %v, want nil", f)
	}
}

// The refusals are the point: each one is an operator Vectorize does
// not have, and guessing at it would return the wrong rows.
func TestUnsupportedOperatorsAreRefused(t *testing.T) {
	cases := []struct {
		name string
		f    vector.Filter
	}{
		{"or", vector.Or(vector.Eq("a", 1), vector.Eq("b", 2))},
		{"not over a conjunction", vector.Not(vector.And(vector.Eq("a", 1), vector.Eq("b", 2)))},
		{"is null", vector.IsNull("a")},
		{"is not null", vector.IsNotNull("a")},
		{"match text", vector.MatchText("body", "hello")},
		{"has id", vector.HasID("1", "2")},
		{"geo within", vector.GeoWithin("loc", vector.GeoBox{})},
		{"empty in", vector.In("a")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := vectorize.CompileFilter(tc.f)
			if !errors.Is(err, vector.ErrUnsupportedOp) {
				t.Errorf("err = %v, want ErrUnsupportedOp", err)
			}
		})
	}
}

// A negated leaf has a real opposite, so rewriting it is exact rather
// than approximate.
func TestNegatedLeavesAreRewritten(t *testing.T) {
	cases := []struct {
		name    string
		f       vector.Filter
		field   string
		wantOp  string
		wantVal any
	}{
		{"not eq", vector.Not(vector.Eq("a", "x")), "a", "$ne", "x"},
		{"not in", vector.Not(vector.In("a", "x", "y")), "a", "$nin", nil},
		{"not gte", vector.Not(vector.Gte("n", 5)), "n", "$lt", 5},
		{"ne directly", vector.Ne("a", "x"), "a", "$ne", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := vectorize.CompileFilter(tc.f)
			if err != nil {
				t.Fatalf("CompileFilter: %v", err)
			}
			cmp, ok := f[tc.field]
			if !ok {
				t.Fatalf("no entry for %q in %v", tc.field, f)
			}
			val, ok := cmp[tc.wantOp]
			if !ok {
				t.Fatalf("want %s on %q, got %v", tc.wantOp, tc.field, cmp)
			}
			if tc.wantVal != nil && val != tc.wantVal {
				t.Errorf("%s = %v, want %v", tc.wantOp, val, tc.wantVal)
			}
		})
	}
}

// An empty NotIn matches everything, which is no constraint — and
// that is expressible.
func TestEmptyNotInIsNoConstraint(t *testing.T) {
	f, err := vectorize.CompileFilter(vector.NotIn("a"))
	if err != nil {
		t.Fatalf("CompileFilter: %v", err)
	}
	if len(f) != 0 {
		t.Errorf("empty NotIn compiled to %v, want no constraint", f)
	}
}

// Two comparisons on the same field with the same operator would
// collapse into one, so they are reported instead.
func TestContradictoryConjunctionIsReported(t *testing.T) {
	_, err := vectorize.CompileFilter(vector.And(vector.Eq("a", 1), vector.Eq("a", 2)))
	if err == nil {
		t.Fatal("expected an error for two $eq on one field")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("err = %v, want it to say the field is constrained twice", err)
	}
}

// Between is two bounds on one field with different operators, which
// is fine.
func TestBetweenCompiles(t *testing.T) {
	f, err := vectorize.CompileFilter(vector.Between("n", 1, 10))
	if err != nil {
		t.Fatalf("CompileFilter: %v", err)
	}
	if f["n"]["$gte"] != 1 || f["n"]["$lte"] != 10 {
		t.Errorf("n = %v, want $gte 1 and $lte 10", f["n"])
	}
}

func TestRangeNeedsANumber(t *testing.T) {
	_, err := vectorize.CompileFilter(vector.Gt("n", "five"))
	if !errors.Is(err, vector.ErrNotNumeric) {
		t.Errorf("err = %v, want ErrNotNumeric", err)
	}
}

// Search ---------------------------------------------------------------

func TestSearchMapsScoresToDistances(t *testing.T) {
	s := newServer(t, func(int) string { return matches("a", 0.9, "b", 0.5) })
	res, err := s.index().Search(context.Background(),
		vector.Search([]float32{1, 0}).TopK(10).Build())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("%d hits, want 2", len(res.Hits))
	}
	// Cosine: Vectorize's score is a similarity, so distance is 1-s.
	if got := res.Hits[0].Distance; got != 0.09999999999999998 && got != 0.1 {
		t.Errorf("distance = %v, want ~0.1", got)
	}
	if res.Hits[0].Score <= res.Hits[1].Score {
		t.Error("scores are not ordered nearest-first")
	}
	if res.HasMore {
		t.Error("HasMore = true with fewer hits than TopK")
	}
}

func TestEuclideanScoreIsAlreadyADistance(t *testing.T) {
	s := newServer(t, func(int) string { return matches("a", 2.5) })
	res, err := s.index(vectorize.WithMetric(vector.L2)).Search(context.Background(),
		vector.Search([]float32{1, 0}).Metric(vector.L2).Build())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Hits[0].Distance != 2.5 {
		t.Errorf("distance = %v, want 2.5 — euclidean scores are distances", res.Hits[0].Distance)
	}
}

// The metric is fixed at index creation, so a query asking for
// another one is a mistake worth reporting rather than a ranking by
// the wrong function.
func TestMetricMismatchIsRefused(t *testing.T) {
	s := newServer(t, func(int) string { return matches() })
	_, err := s.index(vectorize.WithMetric(vector.Cosine)).Search(context.Background(),
		vector.Search([]float32{1}).Metric(vector.L2).Build())
	if !errors.Is(err, vector.ErrUnsupportedMetric) {
		t.Errorf("err = %v, want ErrUnsupportedMetric", err)
	}
	if s.n != 0 {
		t.Errorf("%d request(s) — the mismatch should be caught locally", s.n)
	}
}

func TestMetricVectorizeCannotServeIsRefused(t *testing.T) {
	s := newServer(t, func(int) string { return matches() })
	_, err := s.index(vectorize.WithMetric(vector.Hamming)).Search(context.Background(),
		vector.Search([]float32{1}).Metric(vector.Hamming).Build())
	if !errors.Is(err, vector.ErrUnsupportedMetric) {
		t.Errorf("err = %v, want ErrUnsupportedMetric for Hamming", err)
	}
}

// One extra hit is what distinguishes a full page from a last page,
// and it must not be handed to the caller.
func TestTopKPlusOneProbesForAFurtherPage(t *testing.T) {
	s := newServer(t, func(int) string { return matches("a", 0.9, "b", 0.8, "c", 0.7) })
	res, err := s.index().Search(context.Background(),
		vector.Search([]float32{1}).TopK(2).Build())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	var body vectorize.QueryRequest
	if err := json.Unmarshal([]byte(s.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.TopK != 3 {
		t.Errorf("requested topK = %d, want 3 (TopK+1)", body.TopK)
	}
	if len(res.Hits) != 2 {
		t.Errorf("%d hits, want 2 — the probe hit must be trimmed", len(res.Hits))
	}
	if !res.HasMore || res.NextCursor == "" {
		t.Error("HasMore/NextCursor should be set when a further page exists")
	}
}

func TestCursorResumesByOffset(t *testing.T) {
	s := newServer(t, func(int) string {
		return matches("a", 0.9, "b", 0.8, "c", 0.7, "d", 0.6)
	})
	idx := s.index()
	first, err := idx.Search(context.Background(), vector.Search([]float32{1}).TopK(2).Build())
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	second, err := idx.Search(context.Background(),
		vector.Search([]float32{1}).TopK(2).After(first.NextCursor).Build())
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Hits) != 2 {
		t.Fatalf("%d hits on page 2, want 2", len(second.Hits))
	}
	if second.Hits[0].ID != "c" {
		t.Errorf("page 2 starts at %v, want c — the offset was not applied", second.Hits[0].ID)
	}
	// The second request must ask for offset+TopK+1.
	var body vectorize.QueryRequest
	if err := json.Unmarshal([]byte(s.bodies[1]), &body); err != nil {
		t.Fatal(err)
	}
	if body.TopK != 5 {
		t.Errorf("second request topK = %d, want 5 (offset 2 + TopK 2 + 1)", body.TopK)
	}
}

// Past the ceiling the page cannot be fetched at all, and saying so
// beats returning the previous page again.
func TestPageBeyondTopKCeilingIsRefused(t *testing.T) {
	s := newServer(t, func(int) string { return matches() })
	deep, err := vector.OffsetCursor(vectorize.BackendVectorize, 150).Encode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.index().Search(context.Background(),
		vector.Search([]float32{1}).TopK(10).After(deep).Build())
	if !errors.Is(err, vectorize.ErrPageBeyondTopK) {
		t.Errorf("err = %v, want ErrPageBeyondTopK", err)
	}
}

// A cursor from another store must not be read as an offset here.
func TestForeignCursorIsRejected(t *testing.T) {
	s := newServer(t, func(int) string { return matches() })
	foreign, err := vector.OffsetCursor(vector.BackendQdrant, 10).Encode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.index().Search(context.Background(),
		vector.Search([]float32{1}).After(foreign).Build())
	if !errors.Is(err, vector.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

func TestMaxDistanceTrimsTheTail(t *testing.T) {
	s := newServer(t, func(int) string { return matches("a", 0.95, "b", 0.5) })
	res, err := s.index().Search(context.Background(),
		vector.Search([]float32{1}).TopK(10).MaxDistance(0.1).Build())
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("%d hits, want 1 — b is 0.5 away, past the 0.1 bound", len(res.Hits))
	}
	if res.Hits[0].ID != "a" {
		t.Errorf("kept %v, want a", res.Hits[0].ID)
	}
}

// Asking for the payload must not silently drop the topK ceiling to
// 20: "indexed" metadata is the cheaper mode and the default.
func TestPayloadDefaultsToIndexedMetadata(t *testing.T) {
	s := newServer(t, func(int) string { return matches("a", 0.9) })
	if _, err := s.index().Search(context.Background(),
		vector.Search([]float32{1}).TopK(50).WithPayload().Build()); err != nil {
		t.Fatalf("Search: %v", err)
	}
	var body vectorize.QueryRequest
	if err := json.Unmarshal([]byte(s.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.ReturnMetadata != vectorize.MetadataIndexed {
		t.Errorf("returnMetadata = %q, want %q", body.ReturnMetadata, vectorize.MetadataIndexed)
	}
	if body.TopK != 51 {
		t.Errorf("topK = %d, want 51 — indexed metadata does not lower the ceiling", body.TopK)
	}
}

func TestReturnMetadataAllLowersTheCeiling(t *testing.T) {
	s := newServer(t, func(int) string { return matches("a", 0.9) })
	if _, err := s.index().Search(context.Background(),
		vector.Search([]float32{1}).TopK(50).WithPayload().
			Param("returnMetadata", "all").Build()); err != nil {
		t.Fatalf("Search: %v", err)
	}
	var body vectorize.QueryRequest
	if err := json.Unmarshal([]byte(s.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.TopK != vectorize.MaxTopKWithValues {
		t.Errorf("topK = %d, want %d", body.TopK, vectorize.MaxTopKWithValues)
	}
}

func TestTopKCeiling(t *testing.T) {
	if got := vectorize.TopKCeiling(false, vectorize.MetadataIndexed); got != vectorize.MaxTopK {
		t.Errorf("ceiling without values = %d, want %d", got, vectorize.MaxTopK)
	}
	if got := vectorize.TopKCeiling(true, vectorize.MetadataNone); got != vectorize.MaxTopKWithValues {
		t.Errorf("ceiling with values = %d, want %d", got, vectorize.MaxTopKWithValues)
	}
}

// Writes ---------------------------------------------------------------

func TestUpsertSendsNDJSON(t *testing.T) {
	s := newServer(t, func(int) string { return envelope(`{"mutationId":"m-1"}`) })
	m, err := s.index().Upsert(context.Background(),
		vectorize.Vector{ID: "a", Values: []float32{1, 2}, Metadata: map[string]any{"lang": "it"}},
		vectorize.Vector{ID: "b", Values: []float32{3, 4}},
	)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if m.MutationID != "m-1" {
		t.Errorf("MutationID = %q", m.MutationID)
	}
	lines := strings.Split(strings.TrimSpace(s.bodies[0]), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d NDJSON line(s), want 2:\n%s", len(lines), s.bodies[0])
	}
	var first vectorize.Vector
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 is not JSON: %v", err)
	}
	if first.ID != "a" || first.Metadata["lang"] != "it" {
		t.Errorf("line 1 = %+v", first)
	}
	if !strings.HasSuffix(s.paths[0], "/vectorize/v2/indexes/emb/upsert") {
		t.Errorf("path = %q", s.paths[0])
	}
}

// Vectorize would reject a ragged batch; catching it here says which
// vector is the odd one.
func TestRaggedBatchIsRefusedLocally(t *testing.T) {
	s := newServer(t, func(int) string { return envelope(`{"mutationId":"m"}`) })
	_, err := s.index().Upsert(context.Background(),
		vectorize.Vector{ID: "a", Values: []float32{1, 2}},
		vectorize.Vector{ID: "b", Values: []float32{3}},
	)
	if !errors.Is(err, vectorize.ErrDimensionMismatch) {
		t.Fatalf("err = %v, want ErrDimensionMismatch", err)
	}
	if !strings.Contains(err.Error(), `"b"`) {
		t.Errorf("err = %v, want it to name the odd vector", err)
	}
	if s.n != 0 {
		t.Errorf("%d request(s), want 0", s.n)
	}
}

func TestUpsertRefusesAVectorWithoutAnID(t *testing.T) {
	s := newServer(t, func(int) string { return envelope(`{"mutationId":"m"}`) })
	_, err := s.index().Upsert(context.Background(), vectorize.Vector{Values: []float32{1}})
	if err == nil {
		t.Fatal("expected an error for a vector with no ID")
	}
}

func TestEmptyUpsertIsAnError(t *testing.T) {
	s := newServer(t, func(int) string { return envelope(`{}`) })
	if _, err := s.index().Upsert(context.Background()); !errors.Is(err, vectorize.ErrNoVectors) {
		t.Errorf("err = %v, want ErrNoVectors", err)
	}
}

func TestNamespaceIsAppliedToWritesAndQueries(t *testing.T) {
	s := newServer(t, func(n int) string {
		if n == 1 {
			return envelope(`{"mutationId":"m"}`)
		}
		return matches("a", 0.9)
	})
	idx := s.index(vectorize.WithNamespace("tenant-7"))
	if _, err := idx.Upsert(context.Background(), vectorize.Vector{ID: "a", Values: []float32{1}}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var wrote vectorize.Vector
	if err := json.Unmarshal([]byte(strings.TrimSpace(s.bodies[0])), &wrote); err != nil {
		t.Fatal(err)
	}
	if wrote.Namespace != "tenant-7" {
		t.Errorf("written namespace = %q, want tenant-7", wrote.Namespace)
	}

	if _, err := idx.Search(context.Background(), vector.Search([]float32{1}).Build()); err != nil {
		t.Fatalf("Search: %v", err)
	}
	var q vectorize.QueryRequest
	if err := json.Unmarshal([]byte(s.bodies[1]), &q); err != nil {
		t.Fatal(err)
	}
	if q.Namespace != "tenant-7" {
		t.Errorf("query namespace = %q, want tenant-7", q.Namespace)
	}
}

func TestNDJSONRoundTrip(t *testing.T) {
	in := []vectorize.Vector{
		{ID: "a", Values: []float32{1, 2}, Metadata: map[string]any{"n": float64(1)}},
		{ID: "b", Values: []float32{3, 4}},
	}
	raw, err := vectorize.NDJSON(in)
	if err != nil {
		t.Fatalf("NDJSON: %v", err)
	}
	out, err := vectorize.ParseNDJSON(raw)
	if err != nil {
		t.Fatalf("ParseNDJSON: %v", err)
	}
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Errorf("round trip = %+v", out)
	}
	if out[0].Metadata["n"] != float64(1) {
		t.Errorf("metadata lost: %+v", out[0].Metadata)
	}
}

// Index management -------------------------------------------------------

func TestMetadataIndexLifecycle(t *testing.T) {
	s := newServer(t, func(n int) string {
		if n == 1 {
			return envelope(`{"mutationId":"m-1"}`)
		}
		return envelope(`{"metadataIndexes":[{"propertyName":"lang","indexType":"string"}]}`)
	})
	idx := s.index()
	if _, err := idx.CreateMetadataIndex(context.Background(), "lang", vectorize.MetadataString); err != nil {
		t.Fatalf("CreateMetadataIndex: %v", err)
	}
	list, err := idx.ListMetadataIndexes(context.Background())
	if err != nil {
		t.Fatalf("ListMetadataIndexes: %v", err)
	}
	if len(list) != 1 || list[0].PropertyName != "lang" || list[0].IndexType != vectorize.MetadataString {
		t.Errorf("indexes = %+v", list)
	}
}

func TestGetByIDsAndDeleteByIDs(t *testing.T) {
	s := newServer(t, func(n int) string {
		if n == 1 {
			return envelope(`[{"id":"a","values":[1,2],"metadata":{"lang":"it"}}]`)
		}
		return envelope(`{"mutationId":"m-2"}`)
	})
	idx := s.index()
	got, err := idx.GetByIDs(context.Background(), "a")
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(got) != 1 || got[0].ID != "a" || got[0].Metadata["lang"] != "it" {
		t.Errorf("got = %+v", got)
	}
	m, err := idx.DeleteByIDs(context.Background(), "a")
	if err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}
	if m.MutationID != "m-2" {
		t.Errorf("MutationID = %q", m.MutationID)
	}
}

func TestEmptyIndexNameIsRefused(t *testing.T) {
	s := newServer(t, func(int) string { return envelope(`{}`) })
	if _, err := vectorize.New(s.client(), "").Describe(context.Background()); !errors.Is(err, vectorize.ErrNoIndexName) {
		t.Errorf("err = %v, want ErrNoIndexName", err)
	}
}

func TestMetricFor(t *testing.T) {
	for _, tc := range []struct {
		in   vector.Metric
		want vectorize.Metric
		ok   bool
	}{
		{vector.Cosine, vectorize.MetricCosine, true},
		{vector.L2, vectorize.MetricEuclidean, true},
		{vector.InnerProduct, vectorize.MetricDotProduct, true},
		{vector.L1, "", false},
		{vector.Jaccard, "", false},
	} {
		got, ok := vectorize.MetricFor(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("MetricFor(%s) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

var _ vector.Store = (*vectorize.Index)(nil)

// Delete-by-filter ------------------------------------------------------

// Vectorize has no delete-by-filter; the composition that stands in
// for it is bounded, and must say so rather than delete a prefix.
func TestDeleteWhereRefusesAnUnboundedSet(t *testing.T) {
	full := make([]string, vectorize.MaxTopK)
	for i := range full {
		full[i] = fmt.Sprintf(`{"id":"v%d","score":0.9}`, i)
	}
	s := newServer(t, func(int) string {
		return envelope(fmt.Sprintf(`{"count":%d,"matches":[%s]}`, len(full), strings.Join(full, ",")))
	})
	_, err := s.index().DeleteWhere(context.Background(), []float32{1}, vector.Eq("doc", "x"))
	if !errors.Is(err, vectorize.ErrDeleteByFilterUnbounded) {
		t.Fatalf("err = %v, want ErrDeleteByFilterUnbounded", err)
	}
	if s.n != 1 {
		t.Errorf("%d request(s) — nothing should have been deleted", s.n)
	}
}

func TestDeleteWhereDeletesABoundedSet(t *testing.T) {
	s := newServer(t, func(n int) string {
		if n == 1 {
			return matches("a", 0.9, "b", 0.8)
		}
		return envelope(`{"mutationId":"m-9"}`)
	})
	m, err := s.index().DeleteWhere(context.Background(), []float32{1}, vector.Eq("doc", "x"))
	if err != nil {
		t.Fatalf("DeleteWhere: %v", err)
	}
	if m.MutationID != "m-9" {
		t.Errorf("MutationID = %q", m.MutationID)
	}
	var deleted struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal([]byte(s.bodies[1]), &deleted); err != nil {
		t.Fatal(err)
	}
	if len(deleted.IDs) != 2 || deleted.IDs[0] != "a" {
		t.Errorf("deleted = %v, want [a b]", deleted.IDs)
	}
}

func TestDeleteWhereRefusesAnEmptyFilter(t *testing.T) {
	s := newServer(t, func(int) string { return matches() })
	if _, err := s.index().DeleteWhere(context.Background(), []float32{1}, vector.Filter{}); err == nil {
		t.Error("an empty filter should be refused, not applied to whatever the probe was near")
	}
}

func TestTopKCeilingsAreOverridable(t *testing.T) {
	s := newServer(t, func(int) string { return matches("a", 0.9) })
	idx := s.index(vectorize.WithTopKCeilings(600, 50))
	if got := idx.TopKCeiling(false, vectorize.MetadataIndexed); got != 600 {
		t.Errorf("ceiling = %d, want the override 600", got)
	}
	if got := idx.TopKCeiling(true, vectorize.MetadataNone); got != 50 {
		t.Errorf("ceiling with values = %d, want the override 50", got)
	}
	if _, err := idx.Search(context.Background(),
		vector.Search([]float32{1}).TopK(500).Build()); err != nil {
		t.Fatalf("Search at a raised ceiling: %v", err)
	}
	var body vectorize.QueryRequest
	if err := json.Unmarshal([]byte(s.bodies[0]), &body); err != nil {
		t.Fatal(err)
	}
	if body.TopK != 501 {
		t.Errorf("topK = %d, want 501 — the raised ceiling should not clamp it", body.TopK)
	}
}
