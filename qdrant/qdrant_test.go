package qdrant_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops/qdrant"
)

// mockServer is a minimal Qdrant-shaped HTTP stub used by the tests.
// It records every request and replies with whatever the matching
// handler decides — keeping the package's I/O surface verifiable
// without spinning up a real Qdrant instance.
type mockServer struct {
	mu       sync.Mutex
	requests []recordedReq
	handlers map[string]http.HandlerFunc
	server   *httptest.Server
}

type recordedReq struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

func newMock() *mockServer {
	m := &mockServer{handlers: map[string]http.HandlerFunc{}}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.requests = append(m.requests, recordedReq{
			Method: r.Method,
			Path:   r.URL.RequestURI(),
			Header: r.Header.Clone(),
			Body:   string(body),
		})
		h := m.handlers[r.Method+" "+r.URL.Path]
		if h == nil {
			h = m.handlers[r.Method+" *"]
		}
		m.mu.Unlock()
		if h == nil {
			http.Error(w, `{"status": "not found"}`, http.StatusNotFound)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		h(w, r)
	}))
	return m
}

func (m *mockServer) handle(methodPath string, h http.HandlerFunc) { m.handlers[methodPath] = h }

func (m *mockServer) reqs() []recordedReq {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedReq, len(m.requests))
	copy(out, m.requests)
	return out
}

func (m *mockServer) close() { m.server.Close() }

// envelope writes Qdrant's `{ "result": ..., "status": "ok" }` shape.
func envelope(w http.ResponseWriter, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"result": result,
		"status": "ok",
		"time":   0.0,
	})
}

// --- Client / options -----------------------------------------------

func TestNewClientRejectsBadURL(t *testing.T) {
	cases := []string{"", "no-scheme.example.com", "://broken"}
	for _, c := range cases {
		if _, err := qdrant.NewClient(c); err == nil {
			t.Errorf("NewClient(%q) should fail", c)
		}
	}
}

func TestNewClientTrimsTrailingSlash(t *testing.T) {
	cli, err := qdrant.NewClient("http://example.com:6333/")
	if err != nil {
		t.Fatal(err)
	}
	if got := cli.BaseURL(); got != "http://example.com:6333" {
		t.Errorf("BaseURL = %q", got)
	}
}

func TestAPIKeyHeadersSet(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /collections", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{"collections": []any{}})
	})
	cli, _ := qdrant.NewClient(m.server.URL, qdrant.WithAPIKey("secret"))
	if _, err := cli.ListCollections(context.Background()); err != nil {
		t.Fatal(err)
	}
	r := m.reqs()[0]
	if r.Header.Get("api-key") != "secret" {
		t.Errorf("api-key header missing")
	}
	if r.Header.Get("Authorization") != "Bearer secret" {
		t.Errorf("Authorization header missing")
	}
}

// --- Collections ----------------------------------------------------

func TestCreateAndListCollections(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("PUT /collections/embeddings", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, true)
	})
	m.handle("GET /collections", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{
			"collections": []map[string]string{{"name": "embeddings"}, {"name": "logs"}},
		})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	ctx := context.Background()
	if err := cli.CreateCollection(ctx, "embeddings", qdrant.CollectionConfig{
		Vectors: qdrant.VectorParams{Size: 384, Distance: qdrant.DistanceCosine},
	}); err != nil {
		t.Fatal(err)
	}
	names, err := cli.ListCollections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "embeddings" || names[1] != "logs" {
		t.Errorf("ListCollections: %v", names)
	}
	// Body of the create call must have the vector size + distance.
	body := m.reqs()[0].Body
	if !strings.Contains(body, `"size":384`) || !strings.Contains(body, `"distance":"Cosine"`) {
		t.Errorf("create body missing vector cfg: %s", body)
	}
}

func TestCreateCollectionValidates(t *testing.T) {
	cli, _ := qdrant.NewClient("http://localhost:6333")
	if err := cli.CreateCollection(context.Background(), "", qdrant.CollectionConfig{}); err == nil {
		t.Error("empty name should fail")
	}
	if err := cli.CreateCollection(context.Background(), "x",
		qdrant.CollectionConfig{Vectors: qdrant.VectorParams{Size: 0}}); err == nil {
		t.Error("size 0 should fail")
	}
}

func TestCollectionExistsHandlesNotFound(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /collections/missing", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"status":{"error":"Collection 'missing' not found"}}`,
			http.StatusNotFound)
	})
	m.handle("GET /collections/present", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{"status": "green", "vectors_count": 12})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	ctx := context.Background()

	ok, err := cli.CollectionExists(ctx, "missing")
	if err != nil {
		t.Fatalf("CollectionExists(missing): %v", err)
	}
	if ok {
		t.Error("missing should return false")
	}
	ok, err = cli.CollectionExists(ctx, "present")
	if err != nil {
		t.Fatalf("CollectionExists(present): %v", err)
	}
	if !ok {
		t.Error("present should return true")
	}
}

func TestErrCollectionMissingIsWrapped(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /collections/missing", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"status":{"error":"collection not found"}}`, http.StatusNotFound)
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	_, err := cli.CollectionInfo(context.Background(), "missing")
	if !errors.Is(err, qdrant.ErrCollectionMissing) {
		t.Errorf("expected ErrCollectionMissing, got %v", err)
	}
}

// --- Points ---------------------------------------------------------

func TestUpsertSendsBatch(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("PUT /collections/v/points", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{"operation_id": 1, "status": "completed"})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	err := cli.Upsert(context.Background(), "v", []qdrant.Point{
		{ID: "a", Vector: []float32{1, 2, 3}, Payload: map[string]any{"k": 1}},
		{ID: "b", Vector: []float32{4, 5, 6}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := m.reqs()[0].Body
	if !strings.Contains(body, `"id":"a"`) || !strings.Contains(body, `"vector":[1,2,3]`) {
		t.Errorf("upsert body: %s", body)
	}
}

func TestUpsertEmptyReturnsErrNoPoints(t *testing.T) {
	cli, _ := qdrant.NewClient("http://localhost:6333")
	if err := cli.Upsert(context.Background(), "v", nil); !errors.Is(err, qdrant.ErrNoPoints) {
		t.Errorf("got %v", err)
	}
}

func TestDeleteByIDsAndFilter(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("POST /collections/v/points/delete", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{"status": "completed"})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	ctx := context.Background()

	if err := cli.DeleteByIDs(ctx, "v", []any{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if err := cli.DeleteByFilter(ctx, "v", qdrant.Must(qdrant.Eq("kind", "click"))); err != nil {
		t.Fatal(err)
	}
	if len(m.reqs()) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(m.reqs()))
	}
	if !strings.Contains(m.reqs()[0].Body, `"points":["a","b"]`) {
		t.Errorf("delete-by-ids body: %s", m.reqs()[0].Body)
	}
	if !strings.Contains(m.reqs()[1].Body, `"filter"`) {
		t.Errorf("delete-by-filter body: %s", m.reqs()[1].Body)
	}
}

func TestRetrieveAndCount(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("POST /collections/v/points", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, []map[string]any{
			{"id": "a", "vector": []float32{1, 2}, "payload": map[string]any{"k": "v"}},
		})
	})
	m.handle("POST /collections/v/points/count", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{"count": 7})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	pts, err := cli.Retrieve(context.Background(), "v", []any{"a"},
		qdrant.RetrieveOptions{WithVector: true, WithPayload: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].ID != "a" {
		t.Errorf("Retrieve: %+v", pts)
	}
	n, err := cli.Count(context.Background(), "v", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("Count = %d, want 7", n)
	}
}

// --- Search / Filter / Scroll --------------------------------------

func TestSearchEncodesFilter(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("POST /collections/v/points/search", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, []map[string]any{
			{"id": "x", "score": 0.93, "payload": map[string]any{"topic": "go"}},
		})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	threshold := float32(0.5)
	hits, err := cli.Search(context.Background(), "v", qdrant.SearchRequest{
		Vector:         []float32{1, 0, 0},
		Limit:          5,
		WithPayload:    true,
		ScoreThreshold: &threshold,
		Filter: qdrant.Must(
			qdrant.Eq("topic", "go"),
			qdrant.Range("score", qdrant.RangeOpts{Gte: qdrant.F(0.5)}),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "x" || hits[0].Score != 0.93 {
		t.Errorf("hits: %+v", hits)
	}

	body := m.reqs()[0].Body
	for _, want := range []string{
		`"vector":[1,0,0]`,
		`"limit":5`,
		`"score_threshold":0.5`,
		`"with_payload":true`,
		`"must"`,
		`"key":"topic"`,
		`"value":"go"`,
		`"key":"score"`,
		`"gte":0.5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

func TestScrollAndRecommend(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("POST /collections/v/points/scroll", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{
			"points": []map[string]any{{"id": "a"}, {"id": "b"}},
			"next_page_offset": "c",
		})
	})
	m.handle("POST /collections/v/points/recommend", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, []map[string]any{{"id": "z", "score": 0.7}})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	page, err := cli.Scroll(context.Background(), "v", qdrant.ScrollRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Points) != 2 || page.NextPageOffset != "c" {
		t.Errorf("scroll: %+v", page)
	}
	rec, err := cli.Recommend(context.Background(), "v", qdrant.RecommendRequest{
		Positive: []any{"a"}, Negative: []any{"b"}, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec) != 1 || rec[0].ID != "z" {
		t.Errorf("recommend: %+v", rec)
	}
}

func TestHTTPErrorCaptured(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("PUT /collections/x", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"status":{"error":"bad"}}`, http.StatusBadRequest)
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	err := cli.CreateCollection(context.Background(), "x",
		qdrant.CollectionConfig{Vectors: qdrant.VectorParams{Size: 8, Distance: qdrant.DistanceCosine}})
	if err == nil {
		t.Fatal("expected error")
	}
	var httpErr *qdrant.HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("expected *qdrant.HTTPError, got %T (%v)", err, err)
	}
	if httpErr.Status != 400 {
		t.Errorf("status = %d, want 400", httpErr.Status)
	}
}

// --- Filter DSL renders to expected JSON ---------------------------

func TestFilterDSLEncoding(t *testing.T) {
	cases := []struct {
		name string
		f    *qdrant.Filter
		want []string // fragments that must appear
	}{
		{
			"must eq + in",
			qdrant.Must(qdrant.Eq("topic", "go"), qdrant.In("status", "active", "trial")),
			[]string{`"must"`, `"value":"go"`, `"any":["active","trial"]`},
		},
		{
			"should + must_not",
			&qdrant.Filter{
				Should:  []qdrant.Condition{qdrant.Eq("color", "red")},
				MustNot: []qdrant.Condition{qdrant.IsEmpty("deleted_at")},
			},
			[]string{`"should"`, `"must_not"`, `"is_empty"`, `"key":"deleted_at"`},
		},
		{
			"range with bounds",
			qdrant.Must(qdrant.Range("age", qdrant.RangeOpts{Gte: qdrant.F(18), Lt: qdrant.F(65)})),
			[]string{`"gte":18`, `"lt":65`},
		},
		{
			"has_id + geo",
			qdrant.Must(
				qdrant.HasID(1, 2, 3),
				qdrant.GeoIn("location",
					qdrant.GeoPoint{Lat: 45.5, Lon: 9.2},
					qdrant.GeoPoint{Lat: 45.4, Lon: 9.3}),
			),
			[]string{`"has_id":[1,2,3]`, `"geo_bounding_box"`, `"top_left"`, `"bottom_right"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.f)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(got), want) {
					t.Errorf("missing %q in:\n%s", want, got)
				}
			}
		})
	}
}
