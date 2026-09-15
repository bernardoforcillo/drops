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

// Consumer management errors.
var (
	// ErrNoConsumerID is returned by an operation given an empty
	// consumer identifier.
	ErrNoConsumerID = errors.New("drops/cloudflare/queues: consumer ID is empty")

	// ErrNoPullConsumer is returned by [Queue.PullConsumer] when the
	// queue has none.
	//
	// It is the answer to the question a [Queue.Pull] returning an
	// empty batch cannot distinguish: a queue with no pull consumer
	// answers exactly like an empty one.
	ErrNoPullConsumer = errors.New("drops/cloudflare/queues: the queue has no HTTP pull consumer")

	// ErrNotPermanent is returned by [Queue.Purge] without the
	// confirmation. See [DeleteMessagesPermanently].
	ErrNotPermanent = errors.New("drops/cloudflare/queues: a purge must be confirmed with queues.DeleteMessagesPermanently")
)

// ConsumerType is how a queue is consumed.
type ConsumerType string

// The consumer types Cloudflare defines.
const (
	// ConsumerWorker is a Worker Cloudflare pushes batches into. It
	// cannot be driven from a Go process, which is why this package
	// only creates the other kind — but it is reported by
	// [Queue.Consumers], because a queue that already has one is a
	// queue whose messages are going somewhere else.
	ConsumerWorker ConsumerType = "worker"

	// ConsumerHTTPPull is the consumer [Queue.Pull] speaks to.
	ConsumerHTTPPull ConsumerType = "http_pull"
)

// ConsumerSettings are the delivery rules a consumer is created with.
//
// They are the queue's side of the same numbers [PullOptions] carries
// per request: a pull asks for a batch size and a visibility timeout,
// and these are the defaults when it does not.
type ConsumerSettings struct {
	// BatchSize is how many messages a pull returns by default, up
	// to [Limits.BatchMessages].
	BatchSize int

	// VisibilityTimeout is how long a pulled message stays leased by
	// default, up to [Limits.VisibilityTimeout].
	VisibilityTimeout time.Duration

	// MaxRetries is how many times a message may be retried before
	// it goes to the dead-letter queue, up to [Limits.Retries].
	//
	// Without a dead-letter queue it is how many times a message is
	// retried before it is dropped, which is worth saying plainly:
	// the message is gone, and nothing reports that it was.
	MaxRetries int

	// RetryDelay is how long a retried message waits before it is
	// offered again, when the consumer does not say.
	// [Batch.Retry] overrides it per message, which is what
	// [DefaultBackoff] is for.
	RetryDelay time.Duration
}

// Consumer is one consumer attached to a queue.
type Consumer struct {
	// ID identifies the consumer.
	ID string `json:"consumer_id"`

	// QueueName is the queue it consumes.
	QueueName string `json:"queue_name"`

	// Type is whether Cloudflare pushes to a Worker or this
	// consumer pulls over HTTP.
	Type ConsumerType `json:"type"`

	// ScriptName is the Worker, for a push consumer. Empty for a
	// pull consumer, which is not a Worker.
	ScriptName string `json:"script_name"`

	// DeadLetterQueue is where a message goes once it has been
	// retried [ConsumerSettings.MaxRetries] times. Empty means
	// there is none, and such a message is dropped.
	DeadLetterQueue string `json:"dead_letter_queue"`

	// CreatedOn is when it was attached.
	CreatedOn time.Time `json:"created_on"`

	// Settings are its delivery rules, in the same type
	// [ConsumerOptions] takes — so reading a consumer, changing one
	// field and writing it back is one expression rather than a
	// transcription between two shapes.
	Settings ConsumerSettings `json:"settings"`
}

// UnmarshalJSON decodes a consumer, converting Cloudflare's seconds
// and milliseconds into durations.
//
// The conversion is here rather than at each call site because the
// two units sit side by side in the same object — retry_delay is
// seconds and visibility_timeout_ms is milliseconds — and a caller
// reading them raw gets one of them wrong eventually.
func (c *Consumer) UnmarshalJSON(data []byte) error {
	var w struct {
		ID              string       `json:"consumer_id"`
		QueueName       string       `json:"queue_name"`
		Type            ConsumerType `json:"type"`
		ScriptName      string       `json:"script_name"`
		DeadLetterQueue string       `json:"dead_letter_queue"`
		CreatedOn       time.Time    `json:"created_on"`
		Settings        struct {
			BatchSize           int     `json:"batch_size"`
			MaxRetries          int     `json:"max_retries"`
			RetryDelay          float64 `json:"retry_delay"`
			VisibilityTimeoutMs float64 `json:"visibility_timeout_ms"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return fmt.Errorf("drops/cloudflare/queues: decode consumer: %w", err)
	}
	*c = Consumer{
		ID:              w.ID,
		QueueName:       w.QueueName,
		Type:            w.Type,
		ScriptName:      w.ScriptName,
		DeadLetterQueue: w.DeadLetterQueue,
		CreatedOn:       w.CreatedOn,
		Settings: ConsumerSettings{
			BatchSize:         w.Settings.BatchSize,
			MaxRetries:        w.Settings.MaxRetries,
			RetryDelay:        time.Duration(w.Settings.RetryDelay) * time.Second,
			VisibilityTimeout: time.Duration(w.Settings.VisibilityTimeoutMs) * time.Millisecond,
		},
	}
	return nil
}

// ConsumerOptions describes a pull consumer to create or replace.
type ConsumerOptions struct {
	// Settings are the delivery rules. The zero value leaves
	// Cloudflare's defaults.
	Settings ConsumerSettings

	// DeadLetterQueue is the queue a message goes to once it has
	// exhausted its retries. It must already exist.
	//
	// Leaving it empty is a decision, not a default: a message that
	// runs out of retries with no dead-letter queue is discarded,
	// and nothing anywhere records that it was.
	DeadLetterQueue string
}

// body renders the create or replace payload.
func (o ConsumerOptions) body() (map[string]any, error) {
	settings := map[string]any{}
	s := o.Settings
	if s.BatchSize > 0 {
		if s.BatchSize > Published.BatchMessages {
			return nil, fmt.Errorf("%w: a batch size of %d exceeds Cloudflare's limit of %d",
				ErrBatchTooLarge, s.BatchSize, Published.BatchMessages)
		}
		settings["batch_size"] = s.BatchSize
	}
	if s.VisibilityTimeout > 0 {
		if s.VisibilityTimeout > Published.VisibilityTimeout {
			return nil, fmt.Errorf("drops/cloudflare/queues: a visibility timeout of %s exceeds the %s Cloudflare allows",
				s.VisibilityTimeout, Published.VisibilityTimeout)
		}
		settings["visibility_timeout_ms"] = s.VisibilityTimeout.Milliseconds()
	}
	if s.MaxRetries > 0 {
		if s.MaxRetries > Published.Retries {
			return nil, fmt.Errorf("drops/cloudflare/queues: %d retries exceeds the %d Cloudflare allows",
				s.MaxRetries, Published.Retries)
		}
		settings["max_retries"] = s.MaxRetries
	}
	if s.RetryDelay > 0 {
		if s.RetryDelay > Published.DeliveryDelay {
			return nil, fmt.Errorf("drops/cloudflare/queues: a retry delay of %s exceeds the %s Cloudflare allows",
				s.RetryDelay, Published.DeliveryDelay)
		}
		settings["retry_delay"] = int(s.RetryDelay.Seconds())
	}

	body := map[string]any{"type": string(ConsumerHTTPPull)}
	if len(settings) > 0 {
		body["settings"] = settings
	}
	if o.DeadLetterQueue != "" {
		body["dead_letter_queue"] = o.DeadLetterQueue
	}
	return body, nil
}

// Consumers returns the queue's consumers, of either kind.
func (q *Queue) Consumers(ctx context.Context) ([]Consumer, error) {
	if q.id == "" {
		return nil, ErrNoQueueID
	}
	var out []Consumer
	err := q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/consumers",
	}, &out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PullConsumer returns the queue's HTTP pull consumer, or
// [ErrNoPullConsumer].
//
// This is the check to run when [Queue.Pull] keeps coming back empty
// from a queue that is definitely being written to: without a pull
// consumer the queue answers a pull exactly as an empty queue does,
// and no error says which it was.
func (q *Queue) PullConsumer(ctx context.Context) (*Consumer, error) {
	all, err := q.Consumers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Type == ConsumerHTTPPull {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %d consumer(s) attached, none of them http_pull", ErrNoPullConsumer, len(all))
}

// CreateConsumer attaches an HTTP pull consumer to the queue.
//
// A queue has to have one before [Queue.Pull] returns anything, and
// before this existed that was a step outside the library — wrangler
// or the dashboard — which meant a queue could be created in code and
// then silently not be consumable.
func (q *Queue) CreateConsumer(ctx context.Context, opts ConsumerOptions) (*Consumer, error) {
	if q.id == "" {
		return nil, ErrNoQueueID
	}
	body, err := opts.body()
	if err != nil {
		return nil, err
	}
	var out Consumer
	if err := q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/consumers",
		Body:   body,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateConsumer replaces a consumer's settings, keeping its ID.
func (q *Queue) UpdateConsumer(ctx context.Context, consumerID string, opts ConsumerOptions) (*Consumer, error) {
	if q.id == "" {
		return nil, ErrNoQueueID
	}
	if consumerID == "" {
		return nil, ErrNoConsumerID
	}
	body, err := opts.body()
	if err != nil {
		return nil, err
	}
	var out Consumer
	if err := q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPut,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/consumers/" + consumerID,
		Body:   body,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteConsumer detaches a consumer. Messages already leased to it
// come back when their leases expire.
func (q *Queue) DeleteConsumer(ctx context.Context, consumerID string) error {
	if q.id == "" {
		return ErrNoQueueID
	}
	if consumerID == "" {
		return ErrNoConsumerID
	}
	return q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/consumers/" + consumerID,
	}, nil)
}

// EnsurePullConsumer returns the queue's HTTP pull consumer, creating
// one with opts if there is none.
//
// It is the call a service makes at boot: the queue is consumable
// afterwards or the error says why, rather than the first pull coming
// back empty and looking like an idle queue. It does not change an
// existing consumer's settings — a running consumer's visibility
// timeout is not something a restarting peer should quietly rewrite.
// Use [Queue.UpdateConsumer] for that, deliberately.
func (q *Queue) EnsurePullConsumer(ctx context.Context, opts ConsumerOptions) (*Consumer, error) {
	existing, err := q.PullConsumer(ctx)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, ErrNoPullConsumer):
		return nil, err
	}
	return q.CreateConsumer(ctx, opts)
}

// PurgeConfirmation is the acknowledgement [Queue.Purge] requires.
type PurgeConfirmation struct{ confirmed bool }

// DeleteMessagesPermanently is the only value [Queue.Purge] accepts.
//
// It is a parameter rather than a second method because a purge
// deletes every message in the queue with no undo and no dead-letter
// hop, and Cloudflare asks the caller to say so explicitly. Making
// the Go signature carry it puts that sentence at the call site,
// where a reviewer reads it, instead of in a doc comment nobody opens
// at three in the morning.
var DeleteMessagesPermanently = PurgeConfirmation{confirmed: true}

// Purge deletes every message in the queue.
//
// There is no undo, nothing is moved to the dead-letter queue, and
// messages already leased to a consumer are deleted too — a consumer
// mid-batch will find its settlement rejected.
func (q *Queue) Purge(ctx context.Context, confirm PurgeConfirmation) error {
	if q.id == "" {
		return ErrNoQueueID
	}
	if !confirm.confirmed {
		return ErrNotPermanent
	}
	return q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/purge",
		Body:   map[string]any{"delete_messages_permanently": true},
	}, nil)
}

// PurgeState is what Cloudflare reports about the last purge.
type PurgeState struct {
	// Completed is Cloudflare's own word for whether the last purge
	// finished. It is a string rather than a bool because that is
	// what the API returns, and inventing a bool would have to guess
	// what an unrecognised value meant.
	Completed string `json:"completed"`

	// StartedAt is when the last purge began.
	StartedAt string `json:"started_at"`
}

// PurgeStatus reports on the last purge of this queue.
func (q *Queue) PurgeStatus(ctx context.Context) (*PurgeState, error) {
	if q.id == "" {
		return nil, ErrNoQueueID
	}
	var out PurgeState
	if err := q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/purge",
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Settings are a queue's own delivery rules, as opposed to a
// consumer's.
type Settings struct {
	// Name renames the queue. Empty leaves the name alone.
	//
	// It lives here rather than beside the queue ID in the argument
	// list because two adjacent strings transpose silently, and the
	// failure — renaming a queue to its own identifier — is not one
	// the API refuses.
	Name string

	// DeliveryDelay defers every message on the queue by this much.
	// A message's own [Message.Delay] is added to it.
	DeliveryDelay time.Duration

	// DeliveryPaused stops the queue handing messages to consumers
	// without dropping anything: publishes still succeed and the
	// backlog grows. It is the lever to pull when a consumer is
	// doing damage and the messages are worth keeping.
	DeliveryPaused bool

	// RetentionPeriod is how long an unconsumed message is kept, up
	// to [Limits.RetentionPeriod].
	RetentionPeriod time.Duration
}

// UpdateSettings changes a queue's delivery rules, and optionally its
// name.
//
// Every zero field leaves that setting alone, except DeliveryPaused,
// which is a boolean and is therefore always sent — pausing and
// resuming are the two things this call exists to do, and a false
// that did nothing would make resuming impossible.
func (c *Client) UpdateSettings(ctx context.Context, queueID string, s Settings) (*QueueInfo, error) {
	if queueID == "" {
		return nil, ErrNoQueueID
	}
	if s.RetentionPeriod > Published.RetentionPeriod {
		return nil, fmt.Errorf("drops/cloudflare/queues: a retention period of %s exceeds the %s Cloudflare allows",
			s.RetentionPeriod, Published.RetentionPeriod)
	}
	if s.DeliveryDelay > Published.DeliveryDelay {
		return nil, fmt.Errorf("drops/cloudflare/queues: a delivery delay of %s exceeds the %s Cloudflare allows",
			s.DeliveryDelay, Published.DeliveryDelay)
	}

	settings := map[string]any{"delivery_paused": s.DeliveryPaused}
	if s.DeliveryDelay > 0 {
		settings["delivery_delay"] = int(s.DeliveryDelay.Seconds())
	}
	if s.RetentionPeriod > 0 {
		settings["message_retention_period"] = int(s.RetentionPeriod.Seconds())
	}
	body := map[string]any{"settings": settings}
	if s.Name != "" {
		body["queue_name"] = s.Name
	}

	var out QueueInfo
	if err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPut,
		Path:   c.cf.AccountPath("/queues/", queueID),
		Body:   body,
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Pause stops the queue delivering to its consumers, keeping
// everything already in it.
func (c *Client) Pause(ctx context.Context, queueID string) (*QueueInfo, error) {
	return c.setPaused(ctx, queueID, true)
}

// Resume undoes [Client.Pause].
func (c *Client) Resume(ctx context.Context, queueID string) (*QueueInfo, error) {
	return c.setPaused(ctx, queueID, false)
}

// setPaused flips delivery_paused without touching anything else the
// queue is configured with.
func (c *Client) setPaused(ctx context.Context, queueID string, paused bool) (*QueueInfo, error) {
	if queueID == "" {
		return nil, ErrNoQueueID
	}
	var out QueueInfo
	if err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPut,
		Path:   c.cf.AccountPath("/queues/", queueID),
		Body:   map[string]any{"settings": map[string]any{"delivery_paused": paused}},
	}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
