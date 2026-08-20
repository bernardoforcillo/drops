package qdrant_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/qdrant"
	"github.com/bernardoforcillo/drops/vector"
)

// conditionKeys is Qdrant's whole condition vocabulary: the keys of a
// leaf condition, plus the three block keys a sub-filter is written
// with. Qdrant's Filter is declared deny_unknown_fields and its
// Condition is an untagged union, so an object carrying anything else
// matches no variant and the request is refused — which is why the
// walk below is worth doing on rendered JSON rather than trusting a
// substring.
var conditionKeys = map[string]bool{
	"key": true, "match": true, "range": true, "has_id": true,
	"is_empty": true, "is_null": true, "geo_bounding_box": true,
	"must": true, "should": true, "must_not": true,
}

// checkFilterGrammar walks a rendered filter object and reports every
// key Qdrant would not recognise, with the path it sits at.
func checkFilterGrammar(t *testing.T, path string, raw json.RawMessage) {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("%s: not an object: %v", path, err)
	}
	for _, block := range []string{"must", "should", "must_not"} {
		body, ok := f[block]
		if !ok {
			continue
		}
		var conds []json.RawMessage
		if err := json.Unmarshal(body, &conds); err != nil {
			t.Fatalf("%s.%s: not an array: %v", path, block, err)
		}
		for i, c := range conds {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(c, &obj); err != nil {
				t.Fatalf("%s.%s[%d]: not an object: %v", path, block, i, err)
			}
			at := fmt.Sprintf("%s.%s[%d]", path, block, i)
			for k := range obj {
				if !conditionKeys[k] {
					t.Errorf("%s carries key %q, which is in no Qdrant condition variant: %s", at, k, c)
				}
			}
			checkFilterGrammar(t, at, c)
		}
	}
}

// Every portable connective and negative form compiles to a sub-filter,
// and Qdrant writes a sub-filter bare inside the enclosing block —
// {"must_not":[{"must":[…]}]}. Wrapping it under a "filter" key makes
// the condition match no variant of Qdrant's untagged union and the
// whole search comes back 400.
func TestCompiledFilterUsesQdrantConditionGrammar(t *testing.T) {
	cases := map[string]vector.Filter{
		"And":       vector.And(vector.Eq("lang", "it"), vector.Eq("topic", "go")),
		"Or":        vector.Or(vector.Eq("lang", "it"), vector.Eq("lang", "en")),
		"Not":       vector.Not(vector.Eq("lang", "it")),
		"Ne":        vector.Ne("lang", "it"),
		"IsNull":    vector.IsNull("author"),
		"IsNotNull": vector.IsNotNull("author"),
		"nested": vector.And(
			vector.Eq("lang", "it"),
			vector.Or(vector.In("topic", "go", "rust"), vector.Not(vector.Gte("year", 2020))),
		),
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			raw := json.RawMessage(compiledJSON(t, f))
			checkFilterGrammar(t, "filter", raw)
		})
	}
}

func TestConditionRoundTripsThroughJSON(t *testing.T) {
	f := qdrant.Must(
		qdrant.Eq("topic", "go"),
		qdrant.Nest(qdrant.MustNot(qdrant.IsEmpty("deleted_at"))),
	)
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var back qdrant.Filter
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	if len(back.Must) != 2 {
		t.Fatalf("must block has %d conditions, want 2: %+v", len(back.Must), back)
	}
	if back.Must[0].Key != "topic" {
		t.Errorf("leaf condition lost its key: %+v", back.Must[0])
	}
	sub := back.Must[1].Nested
	if sub == nil || len(sub.MustNot) != 1 || sub.MustNot[0].IsEmpty == nil {
		t.Errorf("sub-filter did not survive the round trip: %+v", back.Must[1])
	}
}

// An empty operand list is what an unset request parameter compiles to,
// and the portable layer fixes its meaning. Qdrant's match object has
// no empty form to express either answer — every variant has a required
// key — so the adapter has to reach for a filter term instead.
func TestCompileFilterEmptyOperandLists(t *testing.T) {
	cases := []struct {
		name    string
		filter  vector.Filter
		matches bool // what the compiled term must select
	}{
		{"In", vector.In("lang"), false},
		{"NotIn", vector.NotIn("lang"), true},
		{"HasID", vector.HasID(), false},
		{"In inside And", vector.And(vector.Eq("topic", "go"), vector.In("lang")), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compiledJSON(t, tc.filter)
			checkFilterGrammar(t, "filter", json.RawMessage(got))
			if strings.Contains(got, `"match":{}`) || strings.Contains(got, `{"key":"lang"}`) {
				t.Errorf("empty operand list rendered an empty condition: %s", got)
			}
			// "Match nothing" is a must_not over the term that matches
			// everything; "match everything" is that term on its own.
			if hasNothing := strings.Contains(got, `"must_not":[{}]`); hasNothing == tc.matches {
				t.Errorf("compiled to %s, want it to match %v", got, tc.matches)
			}
		})
	}
}

// --- Point IDs ------------------------------------------------------

// bigID is the smallest u64 that a float64 cannot hold: it decodes to
// 9007199254740992, an ID that addresses a different point.
const bigID = uint64(9007199254740993)

func TestPointIDsDecodeExactly(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("POST /collections/v/points/search", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, []map[string]any{{"id": json.RawMessage("9007199254740993"), "score": 0.5}})
	})
	m.handle("POST /collections/v/points", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, []map[string]any{{"id": json.RawMessage("9007199254740993")}})
	})
	m.handle("POST /collections/v/points/scroll", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{
			"points":           []map[string]any{{"id": json.RawMessage("9007199254740993")}},
			"next_page_offset": json.RawMessage("9007199254740993"),
		})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	ctx := context.Background()

	hits, err := cli.Search(ctx, "v", qdrant.SearchRequest{Vector: []float32{1}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != any(bigID) {
		t.Errorf("Search hit ID = %#v, want %#v", hits[0].ID, bigID)
	}

	pts, err := cli.Retrieve(ctx, "v", []any{bigID})
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 1 || pts[0].ID != any(bigID) {
		t.Errorf("Retrieve point ID = %#v, want %#v", pts[0].ID, bigID)
	}

	page, err := cli.Scroll(ctx, "v", qdrant.ScrollRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextPageOffset != any(bigID) {
		t.Errorf("NextPageOffset = %#v, want %#v", page.NextPageOffset, bigID)
	}
}

// A scroll only terminates, and only visits each point once, if the
// offset it hands back is the one it was given.
func TestScrollOffsetIsResentVerbatim(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("POST /collections/v/points/scroll", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Offset json.RawMessage `json:"offset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if len(req.Offset) == 0 {
			envelope(w, map[string]any{
				"points":           []map[string]any{{"id": json.RawMessage("9007199254740993")}},
				"next_page_offset": json.RawMessage("9007199254740993"),
			})
			return
		}
		if got := string(req.Offset); got != "9007199254740993" {
			t.Errorf("second page asked for offset %s, want 9007199254740993", got)
		}
		envelope(w, map[string]any{"points": []map[string]any{}, "next_page_offset": nil})
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	ctx := context.Background()

	first, err := cli.Scroll(ctx, "v", qdrant.ScrollRequest{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := cli.Scroll(ctx, "v", qdrant.ScrollRequest{Limit: 1, Offset: first.NextPageOffset})
	if err != nil {
		t.Fatal(err)
	}
	if second.NextPageOffset != nil {
		t.Errorf("walk did not terminate: %#v", second.NextPageOffset)
	}
}

// --- Identifier validation reaches every endpoint --------------------

// A collection name is routinely a tenant identifier, and every method
// splices it straight into the request path. Only the collection-level
// calls used to check it, so a name carrying a separator retargeted the
// request at a neighbouring endpoint: the one below sends a Scroll to
// POST /collections/docs/points/delete, with the rest of the path
// swallowed by the query string.
func TestEveryEndpointValidatesTheCollectionName(t *testing.T) {
	m := newMock()
	defer m.close()
	cli, _ := qdrant.NewClient(m.server.URL)
	ctx := context.Background()
	const bad = "docs/points/delete?"

	cases := map[string]func() error{
		"Upsert": func() error {
			return cli.Upsert(ctx, bad, []qdrant.Point{{ID: "a", Vector: []float32{1}}})
		},
		"DeleteByIDs":    func() error { return cli.DeleteByIDs(ctx, bad, []any{"a"}) },
		"DeleteByFilter": func() error { return cli.DeleteByFilter(ctx, bad, qdrant.Must(qdrant.Eq("k", "v"))) },
		"Retrieve": func() error {
			_, err := cli.Retrieve(ctx, bad, []any{"a"})
			return err
		},
		"Count": func() error {
			_, err := cli.Count(ctx, bad, nil, true)
			return err
		},
		"Search": func() error {
			_, err := cli.Search(ctx, bad, qdrant.SearchRequest{Vector: []float32{1}})
			return err
		},
		"Recommend": func() error {
			_, err := cli.Recommend(ctx, bad, qdrant.RecommendRequest{Positive: []any{"a"}})
			return err
		},
		"Scroll": func() error {
			_, err := cli.Scroll(ctx, bad, qdrant.ScrollRequest{})
			return err
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			if err := call(); !errors.Is(err, qdrant.ErrInvalidIdentifier) {
				t.Errorf("err = %v, want ErrInvalidIdentifier", err)
			}
		})
	}
	if n := len(m.reqs()); n != 0 {
		t.Errorf("%d request(s) reached the server: %+v", n, m.reqs())
	}
}
