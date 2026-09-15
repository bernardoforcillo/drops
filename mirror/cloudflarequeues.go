package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare/queues"
)

// QueuesSink publishes each change to a Cloudflare Queue.
//
// It is the sink for the mirror that is not a store. [ClickHouseSink]
// and [QdrantSink] keep a copy of the table; this one hands the
// change to whatever is on the other side of the queue — a Worker
// that invalidates a cache, a job that re-indexes a document, a
// webhook to a system drops knows nothing about. The pump, the
// outbox and the ordering are the same; only the far end differs.
//
//	q := queues.New(cf).Queue(queueID)
//	sink, err := mirror.NewQueuesSink(q)
//	pump := mirror.NewPump(source, sink)
//
// # What this sink cannot promise
//
// It is not a [VersionAwareSink], and the reason is structural rather
// than an omission. Version-awareness means a store can be asked to
// ignore a write older than what it already holds — ClickHouse can,
// because ReplacingMergeTree does the comparison in the engine. A
// queue holds nothing to compare against. Every message it is given
// is delivered, in whatever order the consumer gets to it.
//
// That has two consequences worth being plain about.
//
// A fill-mode reseed will refuse this sink, because it writes outside
// the ordered stream and there is nothing here to lose the race
// against.
//
// And the consumer must be idempotent, more strictly than the [Sink]
// contract already requires. The pump delivers at-least-once, so a
// retried batch republishes the changes that already went; the queue
// itself is at-least-once, so a message may be delivered twice
// anyway. [Change.Key] and [Change.Version] travel in every message
// precisely so the consumer can drop what it has already applied:
// together they identify one change exactly, and a consumer that
// keeps the highest version it has seen per key is both deduplicated
// and protected against a redelivery arriving late.
type QueuesSink struct {
	q      *queues.Queue
	name   string
	encode func(Change) (any, error)
	delay  time.Duration
}

// QueuesOption configures a [QueuesSink].
type QueuesOption func(*QueuesSink)

// WithQueuesName sets the sink's name in errors and logs. Defaults to
// "cloudflare-queues".
func WithQueuesName(name string) QueuesOption {
	return func(s *QueuesSink) {
		if name != "" {
			s.name = name
		}
	}
}

// WithQueuesEncoder replaces what a change becomes on the wire.
//
// The default is [ChangeEvent], which carries the whole row. Replace
// it when the row is bigger than a message may be — Cloudflare's
// ceiling is 128 KB — or when it holds columns that have no business
// leaving the database. The shape that always fits is the key plus
// enough to act on it, with the consumer reading the row back.
func WithQueuesEncoder(fn func(Change) (any, error)) QueuesOption {
	return func(s *QueuesSink) {
		if fn != nil {
			s.encode = fn
		}
	}
}

// WithQueuesDelay defers delivery of every message by d, up to the
// twenty-four hours Cloudflare allows.
//
// It buys the consumer time to lose a race it would otherwise lose
// silently: a cache invalidation that arrives before the replica it
// is meant to invalidate has caught up does nothing, and a short
// delay is cheaper than the read-repair that would be needed to
// notice.
func WithQueuesDelay(d time.Duration) QueuesOption {
	return func(s *QueuesSink) { s.delay = d }
}

// ChangeEvent is the default wire shape of a mirrored change.
//
// The field names are the JSON a consumer will read, so they are
// chosen to be obvious from the other side rather than to match the
// Go field names: a Worker author reading `{"op":"update","key":"42"}`
// should not have to find this file.
type ChangeEvent struct {
	// Op is "insert", "update" or "delete".
	Op string `json:"op"`

	// Key is the source row's primary key.
	Key string `json:"key"`

	// Row is the row's columns, absent for a delete.
	Row map[string]any `json:"row,omitempty"`

	// Version orders competing changes to one key. A consumer that
	// keeps the highest it has seen per key is deduplicated against
	// both the pump's retries and the queue's redeliveries.
	Version uint64 `json:"version"`

	// At is when the mutation happened at the source, in RFC 3339.
	At string `json:"at,omitempty"`
}

// NewQueuesSink returns a [Sink] that publishes one message per
// change.
func NewQueuesSink(q *queues.Queue, opts ...QueuesOption) (*QueuesSink, error) {
	if q == nil {
		return nil, errors.New("drops/mirror: NewQueuesSink needs a queue")
	}
	s := &QueuesSink{
		q:      q,
		name:   "cloudflare-queues",
		encode: defaultChangeEvent,
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// defaultChangeEvent renders a change as a [ChangeEvent].
func defaultChangeEvent(ch Change) (any, error) {
	ev := ChangeEvent{
		Op:      string(ch.Op),
		Key:     ch.Key,
		Row:     ch.Row,
		Version: ch.Version,
	}
	if !ch.At.IsZero() {
		ev.At = ch.At.UTC().Format(time.RFC3339Nano)
	}
	return ev, nil
}

// Name implements [Sink].
func (s *QueuesSink) Name() string { return s.name }

// Queue returns the queue the sink publishes to.
func (s *QueuesSink) Queue() *queues.Queue { return s.q }

// Apply implements [Sink]. It publishes the batch in as few requests
// as Cloudflare's per-request ceiling allows.
//
// A batch that fails part-way through is retried in full by the pump,
// so some changes are published twice. That is the at-least-once
// delivery the [Sink] contract already names, and the reason
// [ChangeEvent] carries a version.
func (s *QueuesSink) Apply(ctx context.Context, changes []Change) error {
	if len(changes) == 0 {
		return nil
	}
	msgs := make([]queues.Message, 0, len(changes))
	for _, ch := range changes {
		payload, err := s.encode(ch)
		if err != nil {
			return fmt.Errorf("drops/mirror: %s: encode change for key %q: %w", s.name, ch.Key, err)
		}
		if payload == nil {
			// An encoder may drop a change deliberately — a column
			// nobody downstream cares about changing, a tenant that
			// is not mirrored.
			continue
		}
		m := queues.JSON(payload)
		if s.delay > 0 {
			m = m.After(s.delay)
		}
		msgs = append(msgs, m)
	}
	if len(msgs) == 0 {
		return nil
	}

	for start := 0; start < len(msgs); start += queues.Published.BatchMessages {
		end := min(start+queues.Published.BatchMessages, len(msgs))
		if err := s.publish(ctx, msgs[start:end], changes, start); err != nil {
			return err
		}
	}
	return nil
}

// publish sends one chunk, falling back to one message at a time when
// the chunk is too many bytes for a single request.
//
// The fallback exists because the byte ceiling cannot be predicted
// from the change count: a hundred small rows fit in one request and
// twenty large ones do not, and a sink that failed on the second case
// would fail on a table rather than on a row. Splitting costs
// requests, which is the right thing to spend to keep the mirror
// moving.
func (s *QueuesSink) publish(ctx context.Context, msgs []queues.Message, changes []Change, offset int) error {
	err := s.q.PublishBatch(ctx, msgs)
	if err == nil {
		return nil
	}
	if !errors.Is(err, queues.ErrBatchTooLarge) || len(msgs) == 1 {
		return s.wrap(err, changes, offset, len(msgs))
	}
	for i, m := range msgs {
		if pubErr := s.q.Publish(ctx, m); pubErr != nil {
			return s.wrap(pubErr, changes, offset+i, 1)
		}
	}
	return nil
}

// wrap names the change a failure belongs to, because "message too
// large" without a key is a needle in a batch of a hundred.
func (s *QueuesSink) wrap(err error, changes []Change, offset, count int) error {
	if count == 1 && offset < len(changes) {
		return fmt.Errorf("drops/mirror: %s: publishing the change to key %q: %w", s.name, changes[offset].Key, err)
	}
	return fmt.Errorf("drops/mirror: %s: publishing %d changes from key %q: %w",
		s.name, count, keyAt(changes, offset), err)
}

// keyAt names the first change in a chunk, for an error message.
func keyAt(changes []Change, i int) string {
	if i < len(changes) {
		return changes[i].Key
	}
	return ""
}
