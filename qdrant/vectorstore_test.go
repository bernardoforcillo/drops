package qdrant_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/qdrant"
	"github.com/bernardoforcillo/drops/vector"
)

// compiledJSON renders a portable filter through the adapter and back
// out as JSON, which is the form the assertions care about.
func compiledJSON(t *testing.T, f vector.Filter) string {
	t.Helper()
	out, err := qdrant.CompileFilter(f)
	if err != nil {
		t.Fatalf("CompileFilter: %v", err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func TestCompileFilterZeroIsNil(t *testing.T) {
	out, err := qdrant.CompileFilter(vector.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		// An empty filter object is not the same as no filter:
		// Qdrant rejects the former.
		t.Errorf("zero filter should compile to nil, got %+v", out)
	}
}

func TestCompileFilterBlocks(t *testing.T) {
	got := compiledJSON(t, vector.And(
		vector.Eq("lang", "it"),
		vector.Or(vector.In("topic", "go", "rust"), vector.Gte("year", 2020)),
	))
	for _, want := range []string{
		`"must"`, `"should"`,
		`"key":"lang"`, `"value":"it"`,
		`"any":["go","rust"]`,
		`"key":"year"`, `"gte":2020`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("compiled filter missing %s\ngot %s", want, got)
		}
	}
}

// Qdrant has no NOT operator, only a must_not block — every negative
// form has to route through it.
func TestCompileFilterNegationsUseMustNot(t *testing.T) {
	for name, f := range map[string]vector.Filter{
		"Not":       vector.Not(vector.Eq("lang", "it")),
		"Ne":        vector.Ne("lang", "it"),
		"IsNotNull": vector.IsNotNull("author"),
	} {
		got := compiledJSON(t, f)
		if !strings.Contains(got, `"must_not"`) {
			t.Errorf("%s: expected a must_not block, got %s", name, got)
		}
	}
}

// The portable IsNull means "no usable value", which in Qdrant's
// vocabulary is either a missing key (is_empty) or a JSON null
// (is_null).
func TestCompileFilterIsNullCoversBothQdrantForms(t *testing.T) {
	got := compiledJSON(t, vector.IsNull("author"))
	if !strings.Contains(got, `"is_null"`) || !strings.Contains(got, `"is_empty"`) {
		t.Errorf("IsNull should cover is_null and is_empty, got %s", got)
	}
}

func TestCompileFilterRangeRejectsNonNumeric(t *testing.T) {
	_, err := qdrant.CompileFilter(vector.Gte("year", "recently"))
	if !errors.Is(err, vector.ErrNotNumeric) {
		t.Errorf("err = %v want ErrNotNumeric", err)
	}
}

func TestCompileFilterGeo(t *testing.T) {
	got := compiledJSON(t, vector.GeoWithin("loc", vector.GeoBox{
		TopLeft:     vector.GeoPoint{Lat: 45.5, Lon: 9.1},
		BottomRight: vector.GeoPoint{Lat: 45.4, Lon: 9.3},
	}))
	if !strings.Contains(got, `"geo_bounding_box"`) {
		t.Errorf("expected a geo_bounding_box, got %s", got)
	}
}

// --- Search ---------------------------------------------------------

// searchStub replies to the search endpoint with n hits of descending
// score and records the decoded request bodies.
func searchStub(t *testing.T, m *mockServer, scores []float32) *[]qdrant.SearchRequest {
	t.Helper()
	var seen []qdrant.SearchRequest
	m.handle("POST /collections/docs/points/search", func(w http.ResponseWriter, r *http.Request) {
		var req qdrant.SearchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		seen = append(seen, req)

		hits := make([]map[string]any, 0, len(scores))
		for i, s := range scores {
			hits = append(hits, map[string]any{"id": i + 1, "score": s})
		}
		if req.Limit < len(hits) {
			hits = hits[:req.Limit]
		}
		envelope(w, hits)
	})
	return &seen
}

func TestStoreSearchRequestShape(t *testing.T) {
	m := newMock()
	defer m.close()
	seen := searchStub(t, m, []float32{0.9, 0.8})

	cli, _ := qdrant.NewClient(m.server.URL)
	store := cli.Store("docs")

	q := vector.Search([]float32{1, 0, 0}).
		TopK(5).
		Where(vector.Eq("lang", "it")).
		MaxDistance(0.4).
		WithPayload().
		Build()

	if _, err := store.Search(context.Background(), q); err != nil {
		t.Fatalf("Search: %v", err)
	}

	req := (*seen)[0]
	// TopK+1 is what makes HasMore exact without a second request.
	if req.Limit != 6 {
		t.Errorf("Limit = %d, want TopK+1 = 6", req.Limit)
	}
	if !req.WithPayload {
		t.Error("WithPayload not forwarded")
	}
	if req.Filter == nil || len(req.Filter.Must) == 0 {
		t.Errorf("filter not forwarded: %+v", req.Filter)
	}
	// MaxDistance 0.4 on cosine is a score threshold of 0.6.
	if req.ScoreThreshold == nil || math.Abs(float64(*req.ScoreThreshold)-0.6) > 1e-6 {
		t.Errorf("ScoreThreshold = %v, want 0.6", req.ScoreThreshold)
	}
}

func TestStoreSearchPaginatesByOffset(t *testing.T) {
	m := newMock()
	defer m.close()
	// Three hits available, page size two: the first page must report
	// more and hand back a cursor.
	seen := searchStub(t, m, []float32{0.9, 0.8, 0.7})

	cli, _ := qdrant.NewClient(m.server.URL)
	store := cli.Store("docs")
	ctx := context.Background()

	first, err := store.Search(ctx, vector.Search([]float32{1}).TopK(2).Build())
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Hits) != 2 {
		t.Fatalf("first page has %d hits, want 2", len(first.Hits))
	}
	if !first.HasMore || first.NextCursor == "" {
		t.Fatalf("expected a further page, got HasMore=%v cursor=%q", first.HasMore, first.NextCursor)
	}

	if _, err := store.Search(ctx, vector.Search([]float32{1}).TopK(2).After(first.NextCursor).Build()); err != nil {
		t.Fatalf("second page: %v", err)
	}
	if got := (*seen)[1].Offset; got != 2 {
		t.Errorf("second request Offset = %d, want 2", got)
	}
}

func TestStoreSearchLastPageHasNoCursor(t *testing.T) {
	m := newMock()
	defer m.close()
	searchStub(t, m, []float32{0.9, 0.8})

	cli, _ := qdrant.NewClient(m.server.URL)
	res, err := cli.Store("docs").Search(context.Background(), vector.Search([]float32{1}).TopK(5).Build())
	if err != nil {
		t.Fatal(err)
	}
	if res.HasMore || res.NextCursor != "" {
		t.Errorf("HasMore=%v cursor=%q, want a terminal page", res.HasMore, res.NextCursor)
	}
}

// Qdrant's score means different things per metric — a similarity for
// Cosine, the distance itself for Euclid. The adapter is the only
// place that asymmetry lives, so it is worth pinning down.
func TestStoreSearchScoreConversion(t *testing.T) {
	cases := []struct {
		metric   vector.Metric
		score    float32
		wantDist float64
	}{
		{vector.Cosine, 0.75, 0.25},
		{vector.L2, 1.5, 1.5},
		{vector.InnerProduct, 2.0, -2.0},
	}
	for _, c := range cases {
		t.Run(string(c.metric), func(t *testing.T) {
			m := newMock()
			defer m.close()
			searchStub(t, m, []float32{c.score})

			cli, _ := qdrant.NewClient(m.server.URL)
			store := cli.Store("docs", qdrant.WithMetric(c.metric))
			res, err := store.Search(context.Background(),
				vector.Search([]float32{1}).Metric(c.metric).TopK(3).Build())
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Hits) != 1 {
				t.Fatalf("got %d hits", len(res.Hits))
			}
			hit := res.Hits[0]
			if math.Abs(hit.Distance-c.wantDist) > 1e-6 {
				t.Errorf("Distance = %v want %v", hit.Distance, c.wantDist)
			}
			// Score must agree with the shared convention whatever
			// the backend reported.
			if want := vector.ScoreFromDistance(c.metric, c.wantDist); math.Abs(hit.Score-want) > 1e-6 {
				t.Errorf("Score = %v want %v", hit.Score, want)
			}
		})
	}
}

// A collection's metric is fixed at creation and a search cannot
// override it, so a mismatch is a caller error rather than something
// to silently reinterpret.
func TestStoreSearchRejectsMetricMismatch(t *testing.T) {
	cli, _ := qdrant.NewClient("http://127.0.0.1:1")
	store := cli.Store("docs", qdrant.WithMetric(vector.Cosine))
	_, err := store.Search(context.Background(), vector.Search([]float32{1}).Metric(vector.L2).Build())
	if !errors.Is(err, vector.ErrUnsupportedMetric) {
		t.Errorf("err = %v want ErrUnsupportedMetric", err)
	}
}

func TestStoreSearchRejectsUnsupportedMetric(t *testing.T) {
	cli, _ := qdrant.NewClient("http://127.0.0.1:1")
	store := cli.Store("docs", qdrant.WithMetric(vector.Hamming))
	_, err := store.Search(context.Background(), vector.Search([]float32{1}).Metric(vector.Hamming).Build())
	if !errors.Is(err, vector.ErrUnsupportedMetric) {
		t.Errorf("err = %v want ErrUnsupportedMetric", err)
	}
}

func TestStoreSearchRejectsForeignCursor(t *testing.T) {
	enc, err := vector.OffsetCursor(vector.BackendPostgres, 10).Encode()
	if err != nil {
		t.Fatal(err)
	}
	cli, _ := qdrant.NewClient("http://127.0.0.1:1")
	_, err = cli.Store("docs").Search(context.Background(),
		vector.Search([]float32{1}).After(enc).Build())
	if !errors.Is(err, vector.ErrCursorMismatch) {
		t.Errorf("err = %v want ErrCursorMismatch", err)
	}
}

func TestStoreSearchRejectsEmptyVector(t *testing.T) {
	cli, _ := qdrant.NewClient("http://127.0.0.1:1")
	_, err := cli.Store("docs").Search(context.Background(), vector.Query{})
	if !errors.Is(err, vector.ErrNoVector) {
		t.Errorf("err = %v want ErrNoVector", err)
	}
}

func TestStoreForwardsQdrantParamsOnly(t *testing.T) {
	m := newMock()
	defer m.close()
	seen := searchStub(t, m, []float32{0.9})

	cli, _ := qdrant.NewClient(m.server.URL)
	q := vector.Search([]float32{1}).
		Param("hnsw_ef", 256).
		Param("clickhouse.max_threads", 4). // meant for another backend
		Build()
	if _, err := cli.Store("docs").Search(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	params := (*seen)[0].Params
	if params["hnsw_ef"] != float64(256) {
		t.Errorf("hnsw_ef not forwarded: %v", params)
	}
	if _, ok := params["clickhouse.max_threads"]; ok {
		t.Errorf("another backend's param leaked through: %v", params)
	}
}

func TestMetricDistanceMapping(t *testing.T) {
	cases := map[vector.Metric]qdrant.Distance{
		vector.Cosine:       qdrant.DistanceCosine,
		vector.L2:           qdrant.DistanceEuclid,
		vector.InnerProduct: qdrant.DistanceDot,
		vector.L1:           qdrant.DistanceManhattan,
	}
	for m, want := range cases {
		got, ok := qdrant.MetricDistance(m)
		if !ok || got != want {
			t.Errorf("MetricDistance(%s) = %q ok=%v, want %q", m, got, ok, want)
		}
	}
	if _, ok := qdrant.MetricDistance(vector.Jaccard); ok {
		t.Error("Jaccard should have no Qdrant equivalent")
	}
}
