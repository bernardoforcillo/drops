package queues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// PullOptions shapes one [Queue.Pull].
type PullOptions struct {
	// BatchSize is how many messages to ask for, up to
	// [Limits.BatchMessages]. Zero asks for Cloudflare's default.
	BatchSize int

	// VisibilityTimeout is how long the pulled messages stay leased
	// to this consumer before the queue offers them to another.
	// Zero leaves the queue's own setting.
	//
	// Set it to comfortably more than the work takes. Too short and
	// a second consumer starts the same job while the first is still
	// running it, which is the duplicate the handler has to be
	// idempotent against — and the one case where the duplicate was
	// avoidable.
	VisibilityTimeout time.Duration
}

// PulledMessage is one leased message.
type PulledMessage struct {
	// ID identifies the message.
	ID string `json:"id"`

	// Body is the payload as Cloudflare stores it: the text for a
	// text message, the JSON for a JSON one. [PulledMessage.Decode]
	// unmarshals the latter.
	Body string `json:"body"`

	// LeaseID is the handle this consumer holds on the message.
	// Acknowledging or retrying it needs this, not the ID: the lease
	// is what expires, and a stale one must not be able to
	// acknowledge a delivery it no longer owns.
	LeaseID string `json:"lease_id"`

	// Attempts is how many times the message has been delivered,
	// this delivery included. A handler that keeps failing can use
	// it to give up rather than wait for the dead-letter queue.
	Attempts int `json:"attempts"`

	// Metadata is whatever Cloudflare attached.
	Metadata any `json:"metadata"`

	// TimestampMs is when the message was published, in
	// milliseconds since the epoch.
	TimestampMs int64 `json:"timestamp_ms"`
}

// PublishedAt returns when the message was published.
func (m PulledMessage) PublishedAt() time.Time {
	if m.TimestampMs == 0 {
		return time.Time{}
	}
	return time.UnixMilli(m.TimestampMs).UTC()
}

// Decode unmarshals a JSON message's body into v.
func (m PulledMessage) Decode(v any) error {
	if err := json.Unmarshal([]byte(m.Body), v); err != nil {
		return fmt.Errorf("drops/cloudflare/queues: decode message %s: %w", m.ID, err)
	}
	return nil
}

// Batch is one pull's worth of leased messages, and the record of
// what to do with each.
//
// The two-step shape — mark each message, then [Batch.Settle] once —
// is what turns a batch of a hundred decisions into one request
// rather than a hundred. It is also what makes the settlement atomic
// from the consumer's side: either the request lands and every
// decision in it takes effect, or it does not and every message comes
// back when its lease expires.
type Batch struct {
	q *Queue

	// Messages are the leased messages, in the order the queue
	// offered them.
	Messages []PulledMessage

	// BacklogCount is how many unacknowledged messages the queue
	// held when it answered. It is the number to alarm on: a
	// consumer keeping up sees it flat, one falling behind sees it
	// climb.
	BacklogCount int64

	leases  map[string]struct{}
	acks    []string
	retries []retry
}

type retry struct {
	lease string
	delay time.Duration
}

// Settled is what a settlement reports.
type Settled struct {
	// Acked and Retried are how many of each Cloudflare accepted.
	// They can be lower than what was asked for: a lease that
	// expired between the pull and the settlement is refused, and
	// the message it belonged to is already back on the queue.
	Acked   int `json:"ackCount"`
	Retried int `json:"retryCount"`

	// Warnings maps a lease ID to what Cloudflare said about it —
	// which is where an expired lease shows up. A settlement that
	// comes back with warnings is a consumer whose visibility
	// timeout is too short for the work.
	Warnings map[string]string `json:"warnings"`
}

// Pull leases a batch of messages.
//
// An empty batch is not an error: it means the queue had nothing, or
// that the queue has no pull consumer configured — which is a
// configuration a client cannot distinguish from an empty queue, and
// the first thing to check when messages are being published and none
// arrive. [QueueInfo.ConsumersTotalCount] is where that shows.
func (q *Queue) Pull(ctx context.Context, opts PullOptions) (*Batch, error) {
	if q.id == "" {
		return nil, ErrNoQueueID
	}
	if opts.BatchSize > Published.BatchMessages {
		return nil, fmt.Errorf("%w: a batch of %d exceeds Cloudflare's limit of %d per pull",
			ErrBatchTooLarge, opts.BatchSize, Published.BatchMessages)
	}
	if opts.VisibilityTimeout > Published.VisibilityTimeout {
		return nil, fmt.Errorf("drops/cloudflare/queues: a visibility timeout of %s exceeds the %s Cloudflare allows",
			opts.VisibilityTimeout, Published.VisibilityTimeout)
	}

	body := map[string]any{}
	if opts.BatchSize > 0 {
		body["batch_size"] = opts.BatchSize
	}
	if opts.VisibilityTimeout > 0 {
		body["visibility_timeout_ms"] = opts.VisibilityTimeout.Milliseconds()
	}

	var out struct {
		MessageBacklogCount json.Number     `json:"message_backlog_count"`
		Messages            []PulledMessage `json:"messages"`
	}
	err := q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/messages/pull",
		Body:   body,
		// A pull mutates the queue — it takes leases — so it is not
		// safe to repeat: a retried pull leases a second batch and
		// abandons the first until its timeout expires.
	}, &out)
	if err != nil {
		return nil, err
	}

	backlog, _ := out.MessageBacklogCount.Int64()
	b := &Batch{
		q:            q,
		Messages:     out.Messages,
		BacklogCount: backlog,
		leases:       make(map[string]struct{}, len(out.Messages)),
	}
	for _, m := range out.Messages {
		b.leases[m.LeaseID] = struct{}{}
	}
	return b, nil
}

// Ack marks a message done. It takes effect at [Batch.Settle].
func (b *Batch) Ack(m PulledMessage) error {
	if _, ok := b.leases[m.LeaseID]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownLease, m.ID)
	}
	b.acks = append(b.acks, m.LeaseID)
	return nil
}

// Retry returns a message to the queue after delay, so another
// attempt can have it. It takes effect at [Batch.Settle].
//
// A delay of zero makes the message available immediately, which for
// a failure that will fail again is a way to spend the retry budget
// in a fraction of a second. Give it the backoff the failure
// deserves.
func (b *Batch) Retry(m PulledMessage, delay time.Duration) error {
	if _, ok := b.leases[m.LeaseID]; !ok {
		return fmt.Errorf("%w: %s", ErrUnknownLease, m.ID)
	}
	if delay > Published.DeliveryDelay {
		return fmt.Errorf("drops/cloudflare/queues: a retry delay of %s exceeds the %s Cloudflare allows", delay, Published.DeliveryDelay)
	}
	b.retries = append(b.retries, retry{lease: m.LeaseID, delay: delay})
	return nil
}

// Pending reports how many of the batch's messages have been neither
// acknowledged nor retried.
//
// Those are the ones that will come back when their lease expires,
// which is right for a consumer that crashed and wrong for one that
// simply forgot: a loop that leaves messages pending every pass
// re-reads them forever.
func (b *Batch) Pending() int {
	return len(b.Messages) - len(b.acks) - len(b.retries)
}

// Settle sends the batch's acknowledgements and retries as one
// request.
//
// A batch with nothing marked settles without a request and reports
// nothing done — every message in it stays leased until it expires.
func (b *Batch) Settle(ctx context.Context) (*Settled, error) {
	if len(b.acks) == 0 && len(b.retries) == 0 {
		return &Settled{}, nil
	}
	body := map[string]any{}
	if len(b.acks) > 0 {
		acks := make([]map[string]any, len(b.acks))
		for i, lease := range b.acks {
			acks[i] = map[string]any{"lease_id": lease}
		}
		body["acks"] = acks
	}
	if len(b.retries) > 0 {
		rs := make([]map[string]any, len(b.retries))
		for i, r := range b.retries {
			entry := map[string]any{"lease_id": r.lease}
			if r.delay > 0 {
				entry["delay_seconds"] = int(r.delay.Seconds())
			}
			rs[i] = entry
		}
		body["retries"] = rs
	}

	var out Settled
	err := b.q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   b.q.c.cf.AccountPath("/queues/", b.q.id) + "/messages/ack",
		Body:   body,
		// Acknowledging the same lease twice is harmless — the
		// second is refused as a warning, not an error — so a
		// settlement that may not have arrived is worth repeating.
		Idempotent: cloudflare.Idempotently(),
	}, &out)
	if err != nil {
		return nil, err
	}

	// Settled decisions must not be re-sent if the caller settles
	// again; what stays is only what was never marked.
	b.acks = nil
	b.retries = nil
	return &out, nil
}

// Handler processes one message.
//
// Returning nil acknowledges it. Returning an error returns it to the
// queue, after the backoff [ConsumeOptions.Backoff] chooses.
// Returning [ErrStop] acknowledges the message and ends the loop, which
// is how a consumer drains cleanly on a shutdown signal.
type Handler func(ctx context.Context, m PulledMessage) error

// ErrStop ends a [Queue.Consume] loop after acknowledging the
// message that returned it — the clean way out of a consumer on a
// shutdown signal, as opposed to cancelling the context, which
// abandons the batch mid-flight.
var ErrStop = errors.New("drops/cloudflare/queues: stop consuming")

// ConsumeOptions configures [Queue.Consume].
type ConsumeOptions struct {
	// Pull shapes each pull.
	Pull PullOptions

	// Idle is how long to wait after an empty pull before asking
	// again. Defaults to one second.
	//
	// It is a poll interval, not a long poll: Cloudflare's pull
	// endpoint answers immediately with whatever is there, so a
	// consumer with no wait here spends requests on an idle queue as
	// fast as the network allows.
	Idle time.Duration

	// Backoff chooses how long a failed message waits before it is
	// offered again, from the attempt count it has already had.
	// Defaults to an exponential backoff from ten seconds to an
	// hour.
	Backoff func(attempts int) time.Duration
}

// Consume pulls and processes messages until the context is done, a
// handler returns [Stop], or a pull fails.
//
// It is [Queue.Pull], the loop over the batch and [Batch.Settle]
// written once, for the common case where the handler's error is the
// only decision. A consumer that needs to settle differently — to
// acknowledge a poison message rather than retry it forever, to batch
// the work rather than take it one at a time — should use those three
// directly; this is not the shape to bend.
//
// A handler's error is not returned: it sends the message back for
// another attempt, which is the whole point of a queue. Only a
// failure to talk to Cloudflare ends the loop.
func (q *Queue) Consume(ctx context.Context, h Handler, opts ConsumeOptions) error {
	if h == nil {
		return errors.New("drops/cloudflare/queues: Consume needs a handler")
	}
	idle := opts.Idle
	if idle <= 0 {
		idle = time.Second
	}
	backoff := opts.Backoff
	if backoff == nil {
		backoff = DefaultBackoff
	}
	pull := opts.Pull
	if pull.BatchSize == 0 {
		pull.BatchSize = Published.BatchMessages
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		batch, err := q.Pull(ctx, pull)
		if err != nil {
			return err
		}
		if len(batch.Messages) == 0 {
			if err := sleep(ctx, idle); err != nil {
				return err
			}
			continue
		}

		stop := false
		for _, m := range batch.Messages {
			if stop {
				// Everything after the message that said stop stays
				// unmarked, so it goes back to the queue when its
				// lease expires rather than being dropped.
				break
			}
			switch err := h(ctx, m); {
			case err == nil:
				_ = batch.Ack(m)
			case errors.Is(err, ErrStop):
				_ = batch.Ack(m)
				stop = true
			default:
				_ = batch.Retry(m, backoff(m.Attempts))
			}
		}

		if _, err := batch.Settle(ctx); err != nil {
			return err
		}
		if stop {
			return nil
		}
	}
}

// DefaultBackoff doubles from ten seconds, capped at an hour.
//
// The cap matters more than the curve: a message that has failed
// twenty times is not going to succeed because it waited twelve days,
// and an uncapped exponential puts it past
// [Limits.RetentionPeriod] long before it runs out of retries.
func DefaultBackoff(attempts int) time.Duration {
	const base = 10 * time.Second
	const ceiling = time.Hour
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 10 {
		return ceiling
	}
	d := base << (attempts - 1)
	if d > ceiling {
		return ceiling
	}
	return d
}

// sleep waits, or returns early when the context is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
