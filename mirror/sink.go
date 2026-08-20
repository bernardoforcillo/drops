package mirror

import "context"

// Sink is a store that keeps a mirrored copy of a source table.
//
// Apply must be idempotent. Every [Source] this package supports
// delivers at-least-once — that is what makes it durable — so a sink
// will see the same change twice whenever a batch is retried after a
// partial failure, and applying it twice has to leave the mirror in
// the state applying it once would have.
//
// Apply receives changes in source order and should preserve it
// within a key. Batches may interleave keys freely.
//
// A plain Sink is not required to look at [Change.Version]: the
// [Pump] hands it changes in the order the source committed them, so
// last-write-wins is already the right answer. What that costs is
// that such a sink has no defence against a write that reaches it out
// of order from somewhere else — which is why [NewFillReseeder],
// the one path in this package that writes to a sink outside the
// ordered stream, takes only sinks that implement [VersionAwareSink].
// It also assumes one pump: the outbox drain in drops/pg uses SKIP
// LOCKED so that workers can share a queue, so two pumps on one
// outbox can hold two changes to one key at the same time and apply
// them in either order, which a version-blind sink cannot notice.
type Sink interface {
	// Name identifies the sink in errors and logs.
	Name() string

	// Apply writes a batch. Returning an error causes the batch to
	// be retried in full, so partial application must be safe.
	Apply(ctx context.Context, changes []Change) error
}

// VersionAwareSink is a [Sink] that decides between two writes to one
// key by [Change.Version] rather than by the order they arrive in.
//
// This is a property of the store, not a promise a wrapper can make
// on its behalf. [ClickHouseSink] has it because ReplacingMergeTree
// does the comparison in the engine; [QdrantSink] does not, because
// Qdrant's upsert cannot be made conditional on what the point
// already holds.
//
// It exists so that a caller who is about to write outside the
// ordered stream can be refused at construction rather than corrupt
// the mirror at 3am. Announcing it is an assertion about behaviour:
// a sink that returns true and then lets an older change overwrite a
// newer one breaks the argument every fill-mode reseed rests on.
type VersionAwareSink interface {
	Sink

	// VersionAware reports whether a change whose version is below
	// the one this sink already holds for that key leaves the stored
	// row alone.
	VersionAware() bool
}

// sinkHonoursVersion reports whether s can be trusted to lose a race
// it should lose. A sink that does not implement [VersionAwareSink]
// answers no, which is the safe reading of silence.
func sinkHonoursVersion(s Sink) bool {
	v, ok := s.(VersionAwareSink)
	return ok && v.VersionAware()
}

// SinkFunc adapts a plain function to [Sink]. Useful for tests and
// for one-off sinks (a metrics counter, a webhook) that need no
// state.
type SinkFunc struct {
	SinkName string
	Fn       func(ctx context.Context, changes []Change) error

	// Versioned makes this sink a [VersionAwareSink]. Set it only if
	// Fn really does drop a change whose [Change.Version] is below
	// the one already stored for that key — it is what lets the
	// function be used as a fill-mode reseed target.
	Versioned bool
}

func (s SinkFunc) Name() string { return s.SinkName }

func (s SinkFunc) Apply(ctx context.Context, changes []Change) error {
	return s.Fn(ctx, changes)
}

func (s SinkFunc) VersionAware() bool { return s.Versioned }
