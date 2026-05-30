package qdrant_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/bernardoforcillo/drops/qdrant"
)

// exampleServer returns an httptest.Server that mimics a tiny subset
// of the Qdrant API so the example tests stay self-contained and
// deterministic (no real Qdrant instance required).
func exampleServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"result": result, "status": "ok", "time": 0.0,
			})
		}
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/collections/docs":
			respond(true)
		case r.Method == http.MethodPut && r.URL.Path == "/collections/docs/points":
			respond(map[string]any{"operation_id": 1, "status": "completed"})
		case r.Method == http.MethodPost && r.URL.Path == "/collections/docs/points/search":
			respond([]map[string]any{
				{"id": "doc-1", "score": 0.93, "payload": map[string]any{"topic": "go"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

// ExampleNewClient shows the minimal happy-path: connect, create a
// collection, upsert a point, search.
func ExampleNewClient() {
	srv := exampleServer()
	defer srv.Close()

	cli, _ := qdrant.NewClient(srv.URL)
	ctx := context.Background()

	if err := cli.CreateCollection(ctx, "docs", qdrant.CollectionConfig{
		Vectors: qdrant.VectorParams{Size: 3, Distance: qdrant.DistanceCosine},
	}); err != nil {
		fmt.Println("create:", err)
		return
	}
	if err := cli.Upsert(ctx, "docs", []qdrant.Point{
		{ID: "doc-1", Vector: []float32{1, 0, 0},
			Payload: map[string]any{"topic": "go"}},
	}); err != nil {
		fmt.Println("upsert:", err)
		return
	}
	hits, _ := cli.Search(ctx, "docs", qdrant.SearchRequest{
		Vector:      []float32{1, 0, 0},
		Limit:       1,
		WithPayload: true,
	})
	fmt.Printf("found %d hit(s): %v at %.2f\n",
		len(hits), hits[0].ID, hits[0].Score)
	// Output: found 1 hit(s): doc-1 at 0.93
}

// ExampleMust shows the filter DSL: AND-of conditions on a payload.
func ExampleMust() {
	filter := qdrant.Must(
		qdrant.Eq("topic", "go"),
		qdrant.Range("created_at", qdrant.RangeOpts{Gte: qdrant.F(1700000000)}),
	)
	body, _ := json.Marshal(filter)
	fmt.Println(string(body))
	// Output: {"must":[{"key":"topic","match":{"value":"go"}},{"key":"created_at","range":{"gte":1700000000}}]}
}
