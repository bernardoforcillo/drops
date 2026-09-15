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

type adminCall struct {
	Method string
	Path   string
	Query  string
	Body   string
}

type adminServer struct {
	*httptest.Server

	t       *testing.T
	handler func(w http.ResponseWriter, r *http.Request)
	calls   []adminCall
}

func newAdminServer(t *testing.T) *adminServer {
	t.Helper()
	s := &adminServer{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, adminCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(raw)})
		s.handler(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *adminServer) admin() *vectorize.Admin {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return vectorize.NewAdmin(cf)
}

func (s *adminServer) last() adminCall {
	s.t.Helper()
	if len(s.calls) == 0 {
		s.t.Fatal("no request was made")
	}
	return s.calls[len(s.calls)-1]
}

func adminEnvelope(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

func TestCreateIndexWithExplicitDimensions(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, adminEnvelope(`{"name":"docs","description":"d",
			"config":{"dimensions":768,"metric":"euclidean"},
			"created_on":"2026-09-15T04:05:06Z"}`))
	}

	info, err := srv.admin().Create(context.Background(), vectorize.CreateOptions{
		Name:        "docs",
		Description: "d",
		Dimensions:  768,
		Metric:      vector.L2,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	c := srv.last()
	if c.Method != http.MethodPost || c.Path != "/accounts/acct/vectorize/v2/indexes" {
		t.Errorf("%s %s", c.Method, c.Path)
	}
	var sent struct {
		Name   string `json:"name"`
		Config struct {
			Dimensions int    `json:"dimensions"`
			Metric     string `json:"metric"`
			Preset     string `json:"preset"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(c.Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent.Config.Dimensions != 768 || sent.Config.Metric != "euclidean" {
		t.Errorf("config = %+v", sent.Config)
	}
	if sent.Config.Preset != "" {
		t.Errorf("a preset was sent alongside an explicit config: %s", c.Body)
	}
	if info.Config.Dimensions != 768 {
		t.Errorf("info = %+v", info)
	}
}

// A preset is a dimension and a metric, which is the pair that has to
// agree with whatever produces the vectors.
func TestCreateIndexFromAPreset(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, adminEnvelope(`{"name":"docs","config":{"dimensions":768,"metric":"cosine"}}`))
	}
	if _, err := srv.admin().Create(context.Background(), vectorize.CreateOptions{
		Name:   "docs",
		Preset: vectorize.PresetBGEBaseEN,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.Contains(srv.last().Body, `"preset":"@cf/baai/bge-base-en-v1.5"`) {
		t.Errorf("body = %s", srv.last().Body)
	}
	if strings.Contains(srv.last().Body, "dimensions") {
		t.Errorf("a preset must not carry a dimension too: %s", srv.last().Body)
	}
}

// Neither half of the configuration can be changed after creation, so
// a request that says two different things must be refused rather
// than half-applied.
func TestCreateIndexRefusesAnUndecidableConfig(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, adminEnvelope(`{}`)) }
	a := srv.admin()
	ctx := context.Background()

	cases := []struct {
		name string
		opts vectorize.CreateOptions
		want error
	}{
		{"no name", vectorize.CreateOptions{Dimensions: 8}, vectorize.ErrNoIndexName},
		{"nothing to size it by", vectorize.CreateOptions{Name: "x"}, vectorize.ErrNoDimensions},
		{"preset and dimensions", vectorize.CreateOptions{Name: "x", Preset: vectorize.PresetBGEBaseEN, Dimensions: 8}, vectorize.ErrAmbiguousConfig},
		{"preset and metric", vectorize.CreateOptions{Name: "x", Preset: vectorize.PresetBGEBaseEN, Metric: vector.Cosine}, vectorize.ErrAmbiguousConfig},
		{"unknown preset", vectorize.CreateOptions{Name: "x", Preset: "@cf/nope"}, vectorize.ErrUnknownPreset},
		{"unknown metric", vectorize.CreateOptions{Name: "x", Dimensions: 8, Metric: "jaccard"}, vectorize.ErrUnknownMetric},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.Create(ctx, tc.opts); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server; the checks are local", len(srv.calls))
	}
}

// An unset metric takes the package default rather than leaving
// Vectorize to guess.
func TestCreateIndexDefaultsTheMetric(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, adminEnvelope(`{"name":"x"}`)) }
	if _, err := srv.admin().Create(context.Background(), vectorize.CreateOptions{Name: "x", Dimensions: 4}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.Contains(srv.last().Body, `"metric":"cosine"`) {
		t.Errorf("body = %s", srv.last().Body)
	}
}

func TestListGetDeleteAndFindByName(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/indexes") {
			fmt.Fprint(w, adminEnvelope(`[{"name":"docs","config":{"dimensions":768,"metric":"cosine"}},
				{"name":"products","config":{"dimensions":1536,"metric":"dot-product"}}]`))
			return
		}
		fmt.Fprint(w, adminEnvelope(`{"name":"docs","config":{"dimensions":768,"metric":"cosine"}}`))
	}
	a := srv.admin()
	ctx := context.Background()

	all, err := a.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 || all[1].Config.Dimensions != 1536 {
		t.Fatalf("indexes = %+v", all)
	}

	found, err := a.FindByName(ctx, "products")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if found.Config.Metric != vectorize.MetricDotProduct {
		t.Errorf("found = %+v", found)
	}
	if _, err := a.FindByName(ctx, "absent"); !errors.Is(err, cloudflare.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	got, err := a.Get(ctx, "docs")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "docs" {
		t.Errorf("Get = %+v", got)
	}
	if p := srv.last().Path; p != "/accounts/acct/vectorize/v2/indexes/docs" {
		t.Errorf("path = %s", p)
	}

	if err := a.Delete(ctx, "docs"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if srv.last().Method != http.MethodDelete {
		t.Errorf("method = %s", srv.last().Method)
	}
}

func TestAdminRefusesAnEmptyIndexName(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, adminEnvelope(`{}`)) }
	a := srv.admin()
	ctx := context.Background()

	if _, err := a.Get(ctx, ""); !errors.Is(err, vectorize.ErrNoIndexName) {
		t.Errorf("Get err = %v", err)
	}
	if err := a.Delete(ctx, ""); !errors.Is(err, vectorize.ErrNoIndexName) {
		t.Errorf("Delete err = %v", err)
	}
	if _, err := a.FindByName(ctx, ""); !errors.Is(err, vectorize.ErrNoIndexName) {
		t.Errorf("FindByName err = %v", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server", len(srv.calls))
	}
}

// Walking the index is what stands in for the delete-by-filter
// Vectorize does not have, so the cursor and the totals have to
// survive the decode.
func TestListVectorsPages(t *testing.T) {
	srv := newAdminServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, adminEnvelope(`{"count":2,"totalCount":57,"isTruncated":true,
			"nextCursor":"abc","cursorExpirationTimestamp":"2026-09-15T05:00:00Z",
			"vectors":[{"id":"v1"},{"id":"v2"}]}`))
	}
	idx := srv.admin().Index("docs")

	page, err := idx.ListVectors(context.Background(), vectorize.ListVectorsOptions{Count: 2, Cursor: "prev"})
	if err != nil {
		t.Fatalf("ListVectors: %v", err)
	}
	c := srv.last()
	if !strings.HasSuffix(c.Path, "/indexes/docs/list") {
		t.Errorf("path = %s", c.Path)
	}
	if !strings.Contains(c.Query, "count=2") || !strings.Contains(c.Query, "cursor=prev") {
		t.Errorf("query = %s", c.Query)
	}
	if len(page.IDs) != 2 || page.IDs[1] != "v2" {
		t.Errorf("ids = %v", page.IDs)
	}
	if page.TotalCount != 57 || !page.Truncated || page.Cursor != "abc" {
		t.Errorf("page = %+v", page)
	}
	if page.CursorExpires.IsZero() {
		t.Error("the cursor expiry was not parsed; a sweep needs to know it exists")
	}
}
