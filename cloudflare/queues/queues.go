package queues

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
)

// Sentinel errors.
var (
	// ErrNoQueueID is returned by an operation given an empty queue
	// identifier.
	ErrNoQueueID = errors.New("drops/cloudflare/queues: queue ID is empty")

	// ErrNoQueueName is returned by [Client.Create] without a name.
	ErrNoQueueName = errors.New("drops/cloudflare/queues: queue name is empty")

	// ErrNoMessages is returned by a publish with nothing to send.
	ErrNoMessages = errors.New("drops/cloudflare/queues: no messages to publish")

	// ErrMessageTooLarge is returned before sending a message past
	// [Limits.MessageSize].
	ErrMessageTooLarge = errors.New("drops/cloudflare/queues: message is too large")

	// ErrBatchTooLarge is returned before sending more messages in
	// one request than [Limits.BatchMessages], or more bytes than
	// [Limits.BatchSize].
	ErrBatchTooLarge = errors.New("drops/cloudflare/queues: batch is too large")

	// ErrUnknownLease is returned by [Batch.Ack] or [Batch.Retry]
	// for a message that did not come from that batch.
	ErrUnknownLease = errors.New("drops/cloudflare/queues: message is not from this batch")
)

// Limits are Cloudflare's published Queues limits.
//
// They are values rather than prose because two of them change a
// design rather than a configuration file, and a number a test can
// assert against is worth more than a number in a comment. Cloudflare
// does revise these; treat the struct as this package's record of
// them at the time it was written.
type Limits struct {
	// MessageSize caps one message's body. About a hundred bytes of
	// Cloudflare's own metadata count against it, which is why the
	// check here leaves room rather than allowing the figure
	// exactly.
	//
	// It is the limit that decides what a message carries. A change
	// event holding a row with a large text column will not fit, and
	// the shape that does is the key plus enough to act on it, with
	// the consumer reading the row back.
	MessageSize int

	// BatchMessages caps how many messages one publish or one pull
	// may carry.
	BatchMessages int

	// BatchSize caps the bytes of one batch publish.
	BatchSize int

	// VisibilityTimeout caps how long a pulled message may stay
	// leased.
	VisibilityTimeout time.Duration

	// DeliveryDelay caps how far into the future a message may be
	// deferred.
	DeliveryDelay time.Duration

	// RetentionPeriod caps how long an unconsumed message is kept.
	RetentionPeriod time.Duration

	// Retries is how many times a message may be retried before it
	// goes to the dead-letter queue.
	Retries int
}

// Published are Cloudflare's Queues limits as documented.
var Published = Limits{
	MessageSize:       128 << 10, // 128 KB
	BatchMessages:     100,
	BatchSize:         256 << 10, // 256 KB
	VisibilityTimeout: 12 * time.Hour,
	DeliveryDelay:     24 * time.Hour,
	RetentionPeriod:   14 * 24 * time.Hour,
	Retries:           100,
}

// metadataAllowance is the room left for Cloudflare's own per-message
// metadata, which counts against [Limits.MessageSize]. Documented as
// "approximately 100 bytes"; the check leaves twice that, because
// refusing a message that would have fit costs a caller one chunking
// decision and accepting one that does not costs them a failed
// publish they cannot see coming.
const metadataAllowance = 200

// Client is Cloudflare Queues for one account.
type Client struct {
	cf *cloudflare.Client
}

// New returns a Queues client for the account cf addresses.
//
// The token needs "Queues:Edit" to publish and to pull — a pull
// acknowledges messages, so there is no read-only form of consuming.
func New(cf *cloudflare.Client) *Client { return &Client{cf: cf} }

// Client returns the underlying Cloudflare API client.
func (c *Client) Client() *cloudflare.Client { return c.cf }

// Queue returns a handle for one queue, addressed by its ID — the
// UUID `wrangler queues info <name>` prints, not the queue's name.
func (c *Client) Queue(queueID string) *Queue {
	return &Queue{c: c, id: queueID}
}

// QueueInfo is one queue as the API describes it.
type QueueInfo struct {
	// ID is the queue's identifier, which is what [Client.Queue]
	// takes.
	ID string `json:"queue_id"`

	// Name is the queue's name, unique within the account.
	Name string `json:"queue_name"`

	// CreatedOn and ModifiedOn are timestamps as the API reports
	// them.
	CreatedOn  string `json:"created_on"`
	ModifiedOn string `json:"modified_on"`

	// ConsumersTotalCount is how many consumers are attached. A
	// queue with none is a queue whose messages are piling up, and
	// a pull consumer counts here — so zero is the first thing to
	// check when [Queue.Pull] comes back empty from a queue that
	// should not be.
	ConsumersTotalCount int `json:"consumers_total_count"`

	// ProducersTotalCount is how many Workers bind it as a
	// producer. It does not count HTTP publishes, which need no
	// binding.
	ProducersTotalCount int `json:"producers_total_count"`

	// Settings are the queue's delivery settings.
	Settings struct {
		DeliveryDelay          float64 `json:"delivery_delay"`
		DeliveryPaused         bool    `json:"delivery_paused"`
		MessageRetentionPeriod float64 `json:"message_retention_period"`
	} `json:"settings"`
}

// Create makes a queue.
func (c *Client) Create(ctx context.Context, name string) (*QueueInfo, error) {
	if name == "" {
		return nil, ErrNoQueueName
	}
	var out QueueInfo
	err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   c.cf.AccountPath("/queues"),
		Body:   map[string]any{"queue_name": name},
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Get returns one queue's metadata.
func (c *Client) Get(ctx context.Context, queueID string) (*QueueInfo, error) {
	if queueID == "" {
		return nil, ErrNoQueueID
	}
	var out QueueInfo
	err := c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodGet,
		Path:   c.cf.AccountPath("/queues/", queueID),
	}, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes a queue and every message still in it.
func (c *Client) Delete(ctx context.Context, queueID string) error {
	if queueID == "" {
		return ErrNoQueueID
	}
	return c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodDelete,
		Path:   c.cf.AccountPath("/queues/", queueID),
	}, nil)
}

// maxListPages caps how far [Client.List] will walk.
const maxListPages = 200

// List returns the account's queues, walking every page.
func (c *Client) List(ctx context.Context) ([]QueueInfo, error) {
	var out []QueueInfo
	for page := 1; page <= maxListPages; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "100")
		env, err := c.cf.DoEnvelope(ctx, cloudflare.Request{
			Method: http.MethodGet,
			Path:   c.cf.AccountPath("/queues"),
			Query:  q,
		})
		if err != nil {
			return nil, err
		}
		var batch []QueueInfo
		if err := json.Unmarshal(env.Result, &batch); err != nil {
			return nil, fmt.Errorf("drops/cloudflare/queues: decode result: %w", err)
		}
		out = append(out, batch...)
		if len(batch) < 100 {
			return out, nil
		}
	}
	return out, fmt.Errorf("drops/cloudflare/queues: stopped listing queues after %d pages", maxListPages)
}

// FindByName returns the queue with the given name, or
// [cloudflare.ErrNotFound].
func (c *Client) FindByName(ctx context.Context, name string) (*QueueInfo, error) {
	if name == "" {
		return nil, ErrNoQueueName
	}
	all, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("drops/cloudflare/queues: no queue named %q: %w", name, cloudflare.ErrNotFound)
}

// Queue publishes to and consumes from one queue.
//
// Safe for concurrent use: it holds nothing but the identifier, and
// every operation is one request.
type Queue struct {
	c  *Client
	id string
}

// ID returns the queue's identifier.
func (q *Queue) ID() string { return q.id }

// Message is something to publish.
//
// Build one with [JSON] or [Text] rather than by hand: the content
// type has to match what Body is, and Cloudflare decides how the
// consumer sees the message from it.
type Message struct {
	// Body is the payload. With ContentTypeJSON it is marshalled;
	// with ContentTypeText it must be a string.
	Body any `json:"body"`

	// ContentType tells Cloudflare how to carry the body.
	ContentType ContentType `json:"content_type,omitempty"`

	// Delay defers delivery of this message. Zero means the queue's
	// own delay setting applies.
	Delay time.Duration `json:"-"`
}

// ContentType is how Cloudflare carries a message body.
type ContentType string

// The content types Cloudflare Queues accepts over HTTP.
const (
	ContentTypeJSON ContentType = "json"
	ContentTypeText ContentType = "text"
)

// JSON returns a message carrying v as JSON.
func JSON(v any) Message {
	return Message{Body: v, ContentType: ContentTypeJSON}
}

// Text returns a message carrying s as text.
func Text(s string) Message {
	return Message{Body: s, ContentType: ContentTypeText}
}

// After returns a copy of m whose delivery is deferred by d.
//
// It is the backoff a failed side effect wants when it has not been
// pulled yet — for one that has, [Batch.Retry] carries the same
// delay.
func (m Message) After(d time.Duration) Message {
	m.Delay = d
	return m
}

// wire renders a message as the API's message object.
func (m Message) wire() (map[string]any, error) {
	ct := m.ContentType
	if ct == "" {
		ct = ContentTypeJSON
	}
	if ct == ContentTypeText {
		if _, ok := m.Body.(string); !ok {
			return nil, fmt.Errorf("drops/cloudflare/queues: a text message's body is %T, want a string", m.Body)
		}
	}
	out := map[string]any{"body": m.Body, "content_type": string(ct)}
	if m.Delay > 0 {
		if m.Delay > Published.DeliveryDelay {
			return nil, fmt.Errorf("drops/cloudflare/queues: a delay of %s exceeds the %s Cloudflare allows", m.Delay, Published.DeliveryDelay)
		}
		out["delay_seconds"] = int(m.Delay.Seconds())
	}
	return out, nil
}

// Publish sends one message.
func (q *Queue) Publish(ctx context.Context, m Message) error {
	if q.id == "" {
		return ErrNoQueueID
	}
	body, err := m.wire()
	if err != nil {
		return err
	}
	if err := checkMessageSize(body); err != nil {
		return err
	}
	// A publish is not idempotent: repeating one that may have
	// arrived delivers the payload twice, and a consumer that has to
	// tolerate a duplicate anyway should not be handed extra ones by
	// the client.
	return q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/messages",
		Body:   body,
	}, nil)
}

// PublishBatch sends several messages in one request.
//
// The batch is not atomic: Cloudflare accepts it as a unit, but a
// failure part-way through can leave some of it delivered. A consumer
// has to tolerate that for the same reason it has to tolerate a
// redelivery — which it does already, because the queue is
// at-least-once.
func (q *Queue) PublishBatch(ctx context.Context, msgs []Message) error {
	if q.id == "" {
		return ErrNoQueueID
	}
	if len(msgs) == 0 {
		return ErrNoMessages
	}
	if len(msgs) > Published.BatchMessages {
		return fmt.Errorf("%w: %d messages exceeds Cloudflare's limit of %d per request — send them in chunks",
			ErrBatchTooLarge, len(msgs), Published.BatchMessages)
	}
	wire := make([]map[string]any, len(msgs))
	for i, m := range msgs {
		w, err := m.wire()
		if err != nil {
			return fmt.Errorf("drops/cloudflare/queues: message %d: %w", i, err)
		}
		if err := checkMessageSize(w); err != nil {
			return fmt.Errorf("drops/cloudflare/queues: message %d: %w", i, err)
		}
		wire[i] = w
	}
	body := map[string]any{"messages": wire}
	if size, err := jsonSize(body); err != nil {
		return err
	} else if size > Published.BatchSize {
		return fmt.Errorf("%w: %d bytes exceeds Cloudflare's limit of %d per batch — send fewer messages per request",
			ErrBatchTooLarge, size, Published.BatchSize)
	}
	return q.c.cf.Do(ctx, cloudflare.Request{
		Method: http.MethodPost,
		Path:   q.c.cf.AccountPath("/queues/", q.id) + "/messages/batch",
		Body:   body,
	}, nil)
}

// checkMessageSize refuses a message Cloudflare would reject, locally
// and without a round trip — and, more to the point, with an error
// that says what the limit is and what to do about it.
func checkMessageSize(body map[string]any) error {
	size, err := jsonSize(body)
	if err != nil {
		return err
	}
	if size > Published.MessageSize-metadataAllowance {
		return fmt.Errorf("%w: %d bytes exceeds Cloudflare's %d (less about %d of its own metadata) — send a reference to the row rather than the row",
			ErrMessageTooLarge, size, Published.MessageSize, metadataAllowance)
	}
	return nil
}

// jsonSize measures a body as it will travel.
func jsonSize(v any) (int, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, fmt.Errorf("drops/cloudflare/queues: encode message: %w", err)
	}
	return len(raw), nil
}
