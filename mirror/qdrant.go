package mirror

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bernardoforcillo/drops/qdrant"
)

// Embedder turns a changed row into the vector that represents it.
//
// It is a function rather than a configuration because embedding is
// the one part of this pipeline drops cannot do for you: it is a call
// to whichever model you have chosen, over whichever concatenation of
// fields you consider the document. Returning a nil vector skips the
// row — the natural way to say "this change does not affect the
// index", for a row whose searchable fields did not move.
type Embedder func(ctx context.Context, ch Change) ([]float32, error)

// QdrantSink keeps a Qdrant collection in step with a source table.
type QdrantSink struct {
	cli        *qdrant.Client
	collection string
	embed      Embedder
	payload    func(Change) map[string]any
	wait       bool
}

// QdrantOption configures a [QdrantSink].
type QdrantOption func(*QdrantSink)

// WithPayload chooses what is stored alongside the vector. The
// default is the row itself, which is what makes
// [github.com/bernardoforcillo/drops/vector] filters on payload
// fields work without further mapping. Narrow it when rows carry
// fields that have no business leaving the database.
func WithPayload(fn func(Change) map[string]any) QdrantOption {
	return func(s *QdrantSink) { s.payload = fn }
}

// WithWait makes each write block until Qdrant has the data in its
// local segments. Off by default, matching Qdrant's own default: the
// mirror is asynchronous already, and waiting per batch costs
// throughput without changing the guarantee. Turn it on when a test
// needs to read its own write.
func WithWait(wait bool) QdrantOption {
	return func(s *QdrantSink) { s.wait = wait }
}

// NewQdrantSink returns a [Sink] that upserts a point per changed row
// and deletes the point when the row goes.
//
// Upsert is idempotent by construction — the point id is the source
// primary key — so replaying a batch converges, which is what the
// at-least-once delivery of a durable source requires.
func NewQdrantSink(cli *qdrant.Client, collection string, embed Embedder, opts ...QdrantOption) (*QdrantSink, error) {
	if cli == nil {
		return nil, fmt.Errorf("drops/mirror: NewQdrantSink needs a client")
	}
	if collection == "" {
		return nil, fmt.Errorf("drops/mirror: NewQdrantSink needs a collection name")
	}
	if embed == nil {
		return nil, fmt.Errorf("drops/mirror: NewQdrantSink needs an Embedder — drops cannot guess how your rows become vectors")
	}
	s := &QdrantSink{
		cli:        cli,
		collection: collection,
		embed:      embed,
		payload:    func(ch Change) map[string]any { return ch.Row },
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

func (s *QdrantSink) Name() string { return "qdrant:" + s.collection }

// Apply writes the batch.
//
// Changes are applied in runs of the same kind rather than split into
// "all upserts, then all deletes". A batch that inserts a key and
// then deletes it must end with the point gone, and reordering would
// end with it present.
func (s *QdrantSink) Apply(ctx context.Context, changes []Change) error {
	for i := 0; i < len(changes); {
		j := i
		del := changes[i].IsDelete()
		for j < len(changes) && changes[j].IsDelete() == del {
			j++
		}
		var err error
		if del {
			err = s.deleteRun(ctx, changes[i:j])
		} else {
			err = s.upsertRun(ctx, changes[i:j])
		}
		if err != nil {
			return err
		}
		i = j
	}
	return nil
}

func (s *QdrantSink) upsertRun(ctx context.Context, changes []Change) error {
	points := make([]qdrant.Point, 0, len(changes))
	for _, ch := range changes {
		if err := ch.Validate(); err != nil {
			return err
		}
		vec, err := s.embed(ctx, ch)
		if err != nil {
			return fmt.Errorf("drops/mirror: embed %s: %w", ch.Key, err)
		}
		if len(vec) == 0 {
			// The embedder declined this row; nothing to index.
			continue
		}
		points = append(points, qdrant.Point{
			ID:      PointID(ch.Key),
			Vector:  vec,
			Payload: s.payload(ch),
		})
	}
	if len(points) == 0 {
		return nil
	}
	return s.cli.Upsert(ctx, s.collection, points, qdrant.WriteOptions{Wait: s.wait})
}

func (s *QdrantSink) deleteRun(ctx context.Context, changes []Change) error {
	ids := make([]any, 0, len(changes))
	for _, ch := range changes {
		if err := ch.Validate(); err != nil {
			return err
		}
		ids = append(ids, PointID(ch.Key))
	}
	if len(ids) == 0 {
		return nil
	}
	return s.cli.DeleteByIDs(ctx, s.collection, ids, qdrant.WriteOptions{Wait: s.wait})
}

// PointID converts a source primary key into the form Qdrant accepts
// as a point id.
//
// Qdrant takes an unsigned integer or a UUID and rejects arbitrary
// strings, so a numeric key travels as a number and everything else
// as the string it already is — which works for UUID keys and fails
// loudly at the server for keys that are neither, rather than being
// silently rewritten here into something that would not match on
// delete.
func PointID(key string) any {
	if n, err := strconv.ParseUint(key, 10, 64); err == nil {
		return n
	}
	return key
}
