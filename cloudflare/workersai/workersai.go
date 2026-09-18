package workersai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Sentinel errors.
var (
	// ErrNoModel is returned when the model name is empty.
	ErrNoModel = errors.New("drops/cloudflare/workersai: model name is empty")

	// ErrNoText is returned by an embed call with nothing to embed.
	ErrNoText = errors.New("drops/cloudflare/workersai: no text to embed")

	// ErrEmptyText is returned when a text in the batch is empty.
	//
	// It is refused rather than embedded because the vector that
	// comes back is a real vector — it is simply the model's opinion
	// of nothing, and it will be somebody's nearest neighbour.
	ErrEmptyText = errors.New("drops/cloudflare/workersai: cannot embed an empty string")

	// ErrShapeMismatch is returned when the response does not carry
	// one vector per text, or carries vectors of differing widths.
	ErrShapeMismatch = errors.New("drops/cloudflare/workersai: the model returned a shape that does not match the request")
)

// MaxBatch is how many texts Cloudflare accepts in one embedding
// request. [Embedder.Embed] chunks anything longer.
const MaxBatch = 100

// Model is a Workers AI model name.
//
// The values are Cloudflare's own identifiers, slashes and all, and
// they are the same strings
// [github.com/bernardoforcillo/drops/cloudflare/vectorize.Preset]
// uses — so an index and the model that fills it can be named from
// one constant.
type Model string

// The text-embedding models Vectorize also knows as presets, so an
// index created from one of these presets matches this model exactly.
const (
	// ModelBGESmallEN is the small BGE model: the cheapest and the
	// narrowest vector, which is also the fastest to search.
	ModelBGESmallEN Model = "@cf/baai/bge-small-en-v1.5"

	// ModelBGEBaseEN is the middle one, and the usual default.
	ModelBGEBaseEN Model = "@cf/baai/bge-base-en-v1.5"

	// ModelBGELargeEN is the widest, and the most expensive to store
	// and to search.
	ModelBGELargeEN Model = "@cf/baai/bge-large-en-v1.5"
)

// Embedder turns text into vectors with one Workers AI model.
//
// Safe for concurrent use: it holds the client and the model name and
// nothing else.
type Embedder struct {
	cf       *cloudflare.Client
	model    Model
	maxBatch int
}

// Option configures an [Embedder].
type Option func(*Embedder)

// WithMaxBatch changes how many texts travel in one request.
//
// Defaults to [MaxBatch]. It exists because the ceiling is
// Cloudflare's to raise, and because a smaller one is sometimes what
// a caller wants: a request carrying a hundred long passages is
// slower to fail and slower to retry than two carrying fifty.
func WithMaxBatch(n int) Option {
	return func(e *Embedder) {
		if n > 0 {
			e.maxBatch = n
		}
	}
}

// New returns an Embedder for one model.
//
// The token needs "Workers AI:Read", which is the permission that
// runs a model — the naming is Cloudflare's.
func New(cf *cloudflare.Client, model Model, opts ...Option) *Embedder {
	e := &Embedder{cf: cf, model: model, maxBatch: MaxBatch}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Model returns the model this Embedder runs.
func (e *Embedder) Model() Model { return e.model }

// Client returns the underlying Cloudflare API client.
func (e *Embedder) Client() *cloudflare.Client { return e.cf }

// path builds the run path for the model.
//
// A model name is "@cf/baai/bge-base-en-v1.5": it contains slashes
// that are path separators and an "@" that is not a delimiter at all.
// [cloudflare.Client.AccountPath] escapes a part whole, which would
// turn the slashes into %2F and address a model that does not exist,
// so the name is split on "/" and escaped a segment at a time.
func (e *Embedder) path() (string, error) {
	if strings.TrimSpace(string(e.model)) == "" {
		return "", ErrNoModel
	}
	parts := make([]string, 0, 4)
	for _, seg := range strings.Split(string(e.model), "/") {
		if seg != "" {
			parts = append(parts, seg)
		}
	}
	var b strings.Builder
	b.WriteString("/accounts/")
	b.WriteString(url.PathEscape(e.cf.AccountID()))
	b.WriteString("/ai/run")
	for _, seg := range parts {
		b.WriteString("/")
		b.WriteString(url.PathEscape(seg))
	}
	return b.String(), nil
}

// Result is a batch of embeddings.
type Result struct {
	// Vectors are the embeddings, one per input text and in the same
	// order.
	Vectors [][]float32

	// Dimensions is how wide each vector is — the number a Vectorize
	// index has to have been created with.
	Dimensions int

	// Requests is how many HTTP calls the batch took, which is one
	// per [MaxBatch] texts. Worth logging: it is what Workers AI
	// bills on.
	Requests int
}

// EmbedOne returns the embedding of one text.
func (e *Embedder) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	res, err := e.Embed(ctx, text)
	if err != nil {
		return nil, err
	}
	return res.Vectors[0], nil
}

// Embed returns one embedding per text, in order.
//
// A slice longer than the batch ceiling is sent in several requests
// and stitched back together, so the order the caller gets is the
// order it gave regardless of how the batch was cut.
func (e *Embedder) Embed(ctx context.Context, texts ...string) (*Result, error) {
	if len(texts) == 0 {
		return nil, ErrNoText
	}
	for i, t := range texts {
		if strings.TrimSpace(t) == "" {
			return nil, fmt.Errorf("%w: text %d", ErrEmptyText, i)
		}
	}
	p, err := e.path()
	if err != nil {
		return nil, err
	}

	out := &Result{Vectors: make([][]float32, 0, len(texts))}
	for start := 0; start < len(texts); start += e.maxBatch {
		end := min(start+e.maxBatch, len(texts))
		chunk := texts[start:end]

		var reply struct {
			Data  [][]float64 `json:"data"`
			Shape []int       `json:"shape"`
		}
		err := e.cf.Do(ctx, cloudflare.Request{
			Method: http.MethodPost,
			Path:   p,
			Body:   map[string]any{"text": chunk},
			// Running a model is a POST and is safe to repeat: it
			// reads nothing and writes nothing. Retrying costs
			// another inference rather than a duplicated row.
			Idempotent: cloudflare.Idempotently(),
		}, &reply)
		if err != nil {
			return nil, err
		}
		if len(reply.Data) != len(chunk) {
			return nil, fmt.Errorf("%w: asked for %d embeddings, got %d",
				ErrShapeMismatch, len(chunk), len(reply.Data))
		}
		for _, row := range reply.Data {
			if out.Dimensions == 0 {
				out.Dimensions = len(row)
			}
			if len(row) != out.Dimensions {
				return nil, fmt.Errorf("%w: one vector is %d wide and another is %d — an index cannot hold both",
					ErrShapeMismatch, out.Dimensions, len(row))
			}
			vec := make([]float32, len(row))
			for i, v := range row {
				vec[i] = float32(v)
			}
			out.Vectors = append(out.Vectors, vec)
		}
		out.Requests++
	}
	if out.Dimensions == 0 {
		return nil, fmt.Errorf("%w: the model returned no vectors", ErrShapeMismatch)
	}
	return out, nil
}
