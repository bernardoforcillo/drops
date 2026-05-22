// Package qdrant is a focused HTTP client for the Qdrant vector
// database (https://qdrant.tech).
//
// Unlike drops/pg and drops/clickhouse, Qdrant doesn't speak SQL — it
// has a JSON API for collection management and vector search. This
// package exposes that API as plain Go types and methods, with no
// dependency on the upstream Go SDK (net/http + encoding/json only).
//
// Quick start:
//
//	cli, _ := qdrant.NewClient("http://localhost:6333",
//	    qdrant.WithAPIKey(os.Getenv("QDRANT_API_KEY")))
//
//	_ = cli.CreateCollection(ctx, "embeddings", qdrant.CollectionConfig{
//	    Vectors: qdrant.VectorParams{Size: 384, Distance: qdrant.DistanceCosine},
//	})
//
//	_ = cli.Upsert(ctx, "embeddings", []qdrant.Point{
//	    {ID: "doc-1", Vector: vec1, Payload: map[string]any{"topic": "go"}},
//	    {ID: "doc-2", Vector: vec2, Payload: map[string]any{"topic": "rust"}},
//	})
//
//	hits, _ := cli.Search(ctx, "embeddings", qdrant.SearchRequest{
//	    Vector:      query,
//	    Limit:       10,
//	    Filter:      qdrant.Must(qdrant.Eq("topic", "go")),
//	    WithPayload: true,
//	})
//
// Surface (current):
//
//   - Collection ops: CreateCollection, DeleteCollection,
//     CollectionExists, CollectionInfo, ListCollections
//   - Point ops: Upsert, Delete, Retrieve, Count
//   - Search ops: Search, Scroll, Recommend
//   - Filter DSL: Must / Should / MustNot blocks with Eq / In / Range /
//     IsEmpty / HasID / GeoBoundingBox conditions
//
// Out of scope (drop down to raw HTTP via Do for these): gRPC,
// snapshots, sharding management, payload indexes, cluster topology.
package qdrant
