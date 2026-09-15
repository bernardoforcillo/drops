package workersai_test

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
	"github.com/bernardoforcillo/drops/cloudflare/workersai"
)

type call struct {
	Method  string
	RawPath string
	Body    string
}

type server struct {
	*httptest.Server

	t       *testing.T
	handler func(w http.ResponseWriter, r *http.Request, body []byte)
	calls   []call
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.calls = append(s.calls, call{Method: r.Method, RawPath: r.URL.EscapedPath(), Body: string(raw)})
		s.handler(w, r, raw)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) embedder(model workersai.Model, opts ...workersai.Option) *workersai.Embedder {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return workersai.New(cf, model, opts...)
}

func envelope(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

// embeddings renders n vectors of the given width.
func embeddings(n, dims int) string {
	rows := make([]string, n)
	for i := range rows {
		vals := make([]string, dims)
		for j := range vals {
			vals[j] = fmt.Sprintf("%d.5", i+j)
		}
		rows[i] = "[" + strings.Join(vals, ",") + "]"
	}
	return fmt.Sprintf(`{"shape":[%d,%d],"data":[%s]}`, n, dims, strings.Join(rows, ","))
}

// A model name is "@cf/baai/bge-base-en-v1.5": the slashes are path
// separators and escaping them whole addresses a model that does not
// exist.
func TestModelNameKeepsItsSlashes(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(embeddings(1, 4)))
	}
	if _, err := srv.embedder(workersai.ModelBGEBaseEN).EmbedOne(context.Background(), "hello"); err != nil {
		t.Fatalf("EmbedOne: %v", err)
	}
	// "@" is a legal path character, so it travels as itself; the
	// slashes stay separators. Escaping the name whole would address
	// a model that does not exist.
	want := "/accounts/acct/ai/run/@cf/baai/bge-base-en-v1.5"
	if got := srv.calls[0].RawPath; got != want {
		t.Errorf("path = %s, want %s", got, want)
	}
}

func TestEmbedReturnsOneVectorPerTextInOrder(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(embeddings(3, 8)))
	}

	res, err := srv.embedder(workersai.ModelBGEBaseEN).
		Embed(context.Background(), "one", "two", "three")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(res.Vectors) != 3 {
		t.Fatalf("%d vectors", len(res.Vectors))
	}
	if res.Dimensions != 8 {
		t.Errorf("Dimensions = %d — this is the number the index must be created with", res.Dimensions)
	}
	if res.Requests != 1 {
		t.Errorf("Requests = %d", res.Requests)
	}
	// Order is what makes the result usable: vector i belongs to
	// text i.
	if res.Vectors[0][0] != 0.5 || res.Vectors[1][0] != 1.5 || res.Vectors[2][0] != 2.5 {
		t.Errorf("vectors came back out of order: %v", []float32{res.Vectors[0][0], res.Vectors[1][0], res.Vectors[2][0]})
	}

	var sent struct {
		Text []string `json:"text"`
	}
	if err := json.Unmarshal([]byte(srv.calls[0].Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(sent.Text) != 3 || sent.Text[2] != "three" {
		t.Errorf("body = %s", srv.calls[0].Body)
	}
}

// A slice longer than the ceiling is several requests, and the order
// the caller gets must not depend on where it was cut.
func TestEmbedChunksAndStitches(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, body []byte) {
		var in struct {
			Text []string `json:"text"`
		}
		if err := json.Unmarshal(body, &in); err != nil {
			t.Fatalf("body: %v", err)
		}
		if len(in.Text) > 2 {
			t.Errorf("a chunk of %d exceeded the configured ceiling", len(in.Text))
		}
		fmt.Fprint(w, envelope(embeddings(len(in.Text), 4)))
	}

	texts := []string{"a", "b", "c", "d", "e"}
	res, err := srv.embedder(workersai.ModelBGESmallEN, workersai.WithMaxBatch(2)).
		Embed(context.Background(), texts...)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(res.Vectors) != len(texts) {
		t.Fatalf("%d vectors for %d texts", len(res.Vectors), len(texts))
	}
	if res.Requests != 3 {
		t.Errorf("Requests = %d, want 3 chunks of at most 2", res.Requests)
	}
	if len(srv.calls) != 3 {
		t.Errorf("%d requests", len(srv.calls))
	}
}

// An empty string embeds to a real vector — the model's opinion of
// nothing — which then turns up as somebody's nearest neighbour.
func TestEmbedRefusesEmptyInput(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(embeddings(1, 4)))
	}
	e := srv.embedder(workersai.ModelBGEBaseEN)
	ctx := context.Background()

	if _, err := e.Embed(ctx); !errors.Is(err, workersai.ErrNoText) {
		t.Errorf("err = %v, want ErrNoText", err)
	}
	if _, err := e.Embed(ctx, "fine", "   "); !errors.Is(err, workersai.ErrEmptyText) {
		t.Errorf("err = %v, want ErrEmptyText", err)
	}
	if len(srv.calls) != 0 {
		t.Errorf("%d requests reached the server; the check is local", len(srv.calls))
	}
}

func TestEmbedRefusesAnEmptyModel(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	if _, err := srv.embedder("").EmbedOne(context.Background(), "x"); !errors.Is(err, workersai.ErrNoModel) {
		t.Errorf("err = %v, want ErrNoModel", err)
	}
}

// A reply that does not line up with the request cannot be stitched
// back to the texts that produced it, and guessing would hand the
// caller somebody else's vector.
func TestEmbedRefusesAMismatchedShape(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(embeddings(1, 4)))
	}
	_, err := srv.embedder(workersai.ModelBGEBaseEN).Embed(context.Background(), "a", "b")
	if !errors.Is(err, workersai.ErrShapeMismatch) {
		t.Fatalf("err = %v, want ErrShapeMismatch", err)
	}
	if !strings.Contains(err.Error(), "asked for 2") {
		t.Errorf("error %q does not say what was asked for", err)
	}
}

func TestEmbedRefusesRaggedVectors(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"shape":[2,4],"data":[[1,2,3,4],[1,2,3]]}`))
	}
	_, err := srv.embedder(workersai.ModelBGEBaseEN).Embed(context.Background(), "a", "b")
	if !errors.Is(err, workersai.ErrShapeMismatch) {
		t.Fatalf("err = %v, want ErrShapeMismatch", err)
	}
	if !strings.Contains(err.Error(), "an index cannot hold both") {
		t.Errorf("error %q does not say why it matters", err)
	}
}

// The model and the index have to be chosen together, and this is the
// property that lets one constant name both.
func TestModelNamesMatchVectorizePresets(t *testing.T) {
	pairs := []struct {
		model  workersai.Model
		preset vectorize.Preset
	}{
		{workersai.ModelBGESmallEN, vectorize.PresetBGESmallEN},
		{workersai.ModelBGEBaseEN, vectorize.PresetBGEBaseEN},
		{workersai.ModelBGELargeEN, vectorize.PresetBGELargeEN},
	}
	for _, p := range pairs {
		if string(p.model) != string(p.preset) {
			t.Errorf("model %q and preset %q have drifted apart", p.model, p.preset)
		}
		if !vectorize.Preset(p.model).Valid() {
			t.Errorf("model %q is not a Vectorize preset, so an index cannot be created from it", p.model)
		}
	}
}
