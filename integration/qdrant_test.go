package integration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/qdrant"
	"github.com/bernardoforcillo/drops/vector"
)

func openQdrant(t *testing.T) *qdrant.Client {
	t.Helper()
	url := integration.DSN(t, integration.EnvQdrant)
	cli, err := qdrant.NewClient(url)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := cli.Ping(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", url, err)
	}
	return cli
}

// collection creates one for the test and removes it afterwards.
func collection(t *testing.T, cli *qdrant.Client, size int, dist qdrant.Distance) string {
	t.Helper()
	name := integration.UniqueName(t, "c")
	ctx := context.Background()
	_ = cli.DeleteCollection(ctx, name)
	err := cli.CreateCollection(ctx, name, qdrant.CollectionConfig{
		Vectors: qdrant.VectorParams{Size: size, Distance: dist},
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	t.Cleanup(func() { _ = cli.DeleteCollection(context.Background(), name) })
	return name
}

// The filter DSL is compiled into JSON that only Qdrant can judge. A
// condition it rejects, or silently ignores, is invisible to a test
// that marshals and string-compares.
func TestQdrantFilterDSLIsAcceptedByTheServer(t *testing.T) {
	cli := openQdrant(t)
	ctx := context.Background()
	name := collection(t, cli, 3, qdrant.DistanceCosine)

	points := []qdrant.Point{
		{ID: 1, Vector: []float32{1, 0, 0}, Payload: map[string]any{"lang": "it", "year": 2021, "tags": []any{"go"}}},
		{ID: 2, Vector: []float32{0.9, 0.1, 0}, Payload: map[string]any{"lang": "it", "year": 2019}},
		{ID: 3, Vector: []float32{0, 0, 1}, Payload: map[string]any{"lang": "en", "year": 2023}},
	}
	if err := cli.Upsert(ctx, name, points, qdrant.WriteOptions{Wait: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	cases := map[string]struct {
		filter vector.Filter
		want   int
	}{
		"eq":        {vector.Eq("lang", "it"), 2},
		"ne":        {vector.Ne("lang", "it"), 1},
		"in":        {vector.In("lang", "it", "en"), 3},
		"notIn":     {vector.NotIn("lang", "en"), 2},
		"gte":       {vector.Gte("year", 2021), 2},
		"between":   {vector.Between("year", 2020, 2022), 1},
		"and":       {vector.And(vector.Eq("lang", "it"), vector.Gte("year", 2021)), 1},
		"or":        {vector.Or(vector.Eq("lang", "en"), vector.Lt("year", 2020)), 2},
		"not":       {vector.Not(vector.Eq("lang", "it")), 1},
		"isNull":    {vector.IsNull("tags"), 2},
		"isNotNull": {vector.IsNotNull("tags"), 1},
		"hasID":     {vector.HasID(1, 3), 2},
	}

	store := cli.Store(name, qdrant.WithMetric(vector.Cosine))
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			res, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).
				TopK(10).Where(c.filter).WithPayload().Build())
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(res.Hits) != c.want {
				ids := make([]any, len(res.Hits))
				for i, h := range res.Hits {
					ids[i] = h.ID
				}
				t.Errorf("matched %d points %v, want %d", len(res.Hits), ids, c.want)
			}
		})
	}
}

// Qdrant's score means different things per metric. The adapter
// normalises it; only a server can confirm the normalisation is right
// way round.
func TestQdrantDistanceNormalisation(t *testing.T) {
	cli := openQdrant(t)
	ctx := context.Background()

	for _, c := range []struct {
		metric vector.Metric
		dist   qdrant.Distance
	}{
		{vector.Cosine, qdrant.DistanceCosine},
		{vector.L2, qdrant.DistanceEuclid},
		{vector.InnerProduct, qdrant.DistanceDot},
	} {
		t.Run(string(c.metric), func(t *testing.T) {
			name := collection(t, cli, 3, c.dist)
			err := cli.Upsert(ctx, name, []qdrant.Point{
				{ID: 1, Vector: []float32{1, 0, 0}},
				{ID: 2, Vector: []float32{0, 1, 0}},
			}, qdrant.WriteOptions{Wait: true})
			if err != nil {
				t.Fatalf("Upsert: %v", err)
			}

			store := cli.Store(name, qdrant.WithMetric(c.metric))
			res, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).TopK(2).Metric(c.metric).Build())
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(res.Hits) != 2 {
				t.Fatalf("got %d hits", len(res.Hits))
			}
			// The identical vector must be first, and its distance
			// must be the smaller of the two — whichever metric.
			if res.Hits[0].ID != uint64(1) && res.Hits[0].ID != float64(1) {
				t.Errorf("nearest hit is %v, want point 1", res.Hits[0].ID)
			}
			if res.Hits[0].Distance > res.Hits[1].Distance {
				t.Errorf("distances are inverted: %v then %v", res.Hits[0].Distance, res.Hits[1].Distance)
			}
			if res.Hits[0].Score < res.Hits[1].Score {
				t.Errorf("scores are inverted: %v then %v", res.Hits[0].Score, res.Hits[1].Score)
			}
		})
	}
}

// Offset pagination through the portable cursor.
func TestQdrantPaginationWalksEveryPointOnce(t *testing.T) {
	cli := openQdrant(t)
	ctx := context.Background()
	name := collection(t, cli, 3, qdrant.DistanceCosine)

	const total = 7
	pts := make([]qdrant.Point, 0, total)
	for i := 0; i < total; i++ {
		pts = append(pts, qdrant.Point{ID: i + 1, Vector: []float32{1, float32(i) / 10, 0}})
	}
	if err := cli.Upsert(ctx, name, pts, qdrant.WriteOptions{Wait: true}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	store := cli.Store(name, qdrant.WithMetric(vector.Cosine))
	seen := map[string]int{}
	for cursor, page := "", 0; ; page++ {
		res, err := store.Search(ctx, vector.Search([]float32{1, 0, 0}).TopK(3).After(cursor).Build())
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, h := range res.Hits {
			seen[toKey(h.ID)]++
		}
		if !res.HasMore {
			break
		}
		cursor = res.NextCursor
		if page > 5 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Errorf("walked %d distinct points, want %d", len(seen), total)
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("point %s returned %d times", k, n)
		}
	}
}

// A cursor from another backend must be refused rather than
// misinterpreted.
func TestQdrantRefusesAForeignCursor(t *testing.T) {
	cli := openQdrant(t)
	name := collection(t, cli, 3, qdrant.DistanceCosine)
	enc, err := vector.OffsetCursor(vector.BackendPostgres, 5).Encode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = cli.Store(name).Search(context.Background(),
		vector.Search([]float32{1, 0, 0}).After(enc).Build())
	if !errors.Is(err, vector.ErrCursorMismatch) {
		t.Errorf("err = %v, want ErrCursorMismatch", err)
	}
}

func toKey(id any) string {
	switch v := id.(type) {
	case string:
		return v
	default:
		return fmtID(v)
	}
}

func fmtID(v any) string {
	switch n := v.(type) {
	case float64:
		return string(rune('0' + int(n)))
	case uint64:
		return string(rune('0' + int(n)))
	case int64:
		return string(rune('0' + int(n)))
	}
	return "?"
}
