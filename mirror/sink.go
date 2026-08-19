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
type Sink interface {
	// Name identifies the sink in errors and logs.
	Name() string

	// Apply writes a batch. Returning an error causes the batch to
	// be retried in full, so partial application must be safe.
	Apply(ctx context.Context, changes []Change) error
}

// SinkFunc adapts a plain function to [Sink]. Useful for tests and
// for one-off sinks (a metrics counter, a webhook) that need no
// state.
type SinkFunc struct {
	SinkName string
	Fn       func(ctx context.Context, changes []Change) error
}

func (s SinkFunc) Name() string { return s.SinkName }

func (s SinkFunc) Apply(ctx context.Context, changes []Change) error {
	return s.Fn(ctx, changes)
}
