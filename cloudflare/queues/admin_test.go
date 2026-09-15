package queues_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare/queues"
)

const consumerReply = `{"consumer_id":"c-1","queue_name":"changes","type":"http_pull",
	"dead_letter_queue":"changes-dlq","created_on":"2026-09-15T04:05:06Z",
	"settings":{"batch_size":50,"max_retries":5,"retry_delay":30,"visibility_timeout_ms":120000}}`

func TestCreateConsumerSendsPullTypeAndSettings(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(consumerReply))
	}

	c, err := srv.client().Queue("q-1").CreateConsumer(context.Background(), queues.ConsumerOptions{
		Settings: queues.ConsumerSettings{
			BatchSize:         50,
			VisibilityTimeout: 2 * time.Minute,
			MaxRetries:        5,
			RetryDelay:        30 * time.Second,
		},
		DeadLetterQueue: "changes-dlq",
	})
	if err != nil {
		t.Fatalf("CreateConsumer: %v", err)
	}

	call := srv.last()
	if call.Method != http.MethodPost || !strings.HasSuffix(call.Path, "/queues/q-1/consumers") {
		t.Errorf("%s %s", call.Method, call.Path)
	}
	var sent struct {
		Type     string `json:"type"`
		DLQ      string `json:"dead_letter_queue"`
		Settings struct {
			BatchSize    int   `json:"batch_size"`
			MaxRetries   int   `json:"max_retries"`
			RetryDelay   int   `json:"retry_delay"`
			VisibilityMs int64 `json:"visibility_timeout_ms"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(call.Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent.Type != "http_pull" {
		t.Errorf("type = %q — only the pull consumer is reachable from Go", sent.Type)
	}
	if sent.Settings.VisibilityMs != 120000 || sent.Settings.RetryDelay != 30 {
		t.Errorf("settings = %+v", sent.Settings)
	}
	if sent.DLQ != "changes-dlq" {
		t.Errorf("dead_letter_queue = %q", sent.DLQ)
	}

	if c.ID != "c-1" || c.Type != queues.ConsumerHTTPPull {
		t.Errorf("consumer = %+v", c)
	}
	// Cloudflare reports one of these in seconds and the other in
	// milliseconds; both arrive as durations.
	if got := c.Settings.VisibilityTimeout; got != 2*time.Minute {
		t.Errorf("VisibilityTimeout = %s", got)
	}
	if got := c.Settings.RetryDelay; got != 30*time.Second {
		t.Errorf("RetryDelay = %s", got)
	}
	if c.Settings.BatchSize != 50 || c.Settings.MaxRetries != 5 {
		t.Errorf("settings = %+v", c.Settings)
	}
}

func TestConsumerSettingsAreCheckedAgainstTheLimits(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(consumerReply)) }
	q := srv.client().Queue("q-1")
	ctx := context.Background()

	cases := []struct {
		name string
		opts queues.ConsumerOptions
		want string
	}{
		{"batch", queues.ConsumerOptions{Settings: queues.ConsumerSettings{BatchSize: queues.Published.BatchMessages + 1}}, "batch size"},
		{"visibility", queues.ConsumerOptions{Settings: queues.ConsumerSettings{VisibilityTimeout: queues.Published.VisibilityTimeout + time.Second}}, "visibility timeout"},
		{"retries", queues.ConsumerOptions{Settings: queues.ConsumerSettings{MaxRetries: queues.Published.Retries + 1}}, "retries"},
		{"delay", queues.ConsumerOptions{Settings: queues.ConsumerSettings{RetryDelay: queues.Published.DeliveryDelay + time.Second}}, "retry delay"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := q.CreateConsumer(ctx, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name the %s limit", err, tc.want)
			}
		})
	}
	if len(srv.recorded()) != 0 {
		t.Errorf("%d requests reached the server; the checks are local", len(srv.recorded()))
	}
}

// A queue with no pull consumer answers a pull exactly as an empty
// queue does, so the difference has to be askable.
func TestPullConsumerNamesTheMissingOne(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`[{"consumer_id":"w-1","type":"worker","script_name":"handler"}]`))
	}
	_, err := srv.client().Queue("q-1").PullConsumer(context.Background())
	if !errors.Is(err, queues.ErrNoPullConsumer) {
		t.Fatalf("err = %v, want ErrNoPullConsumer", err)
	}
	if !strings.Contains(err.Error(), "1 consumer") {
		t.Errorf("error %q does not say what is attached instead", err)
	}
}

func TestEnsurePullConsumerCreatesOnlyWhenMissing(t *testing.T) {
	srv := newServer(t)
	consumers := "[]"
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, envelope(consumers))
			return
		}
		consumers = "[" + consumerReply + "]"
		fmt.Fprint(w, envelope(consumerReply))
	}
	q := srv.client().Queue("q-1")
	ctx := context.Background()

	first, err := q.EnsurePullConsumer(ctx, queues.ConsumerOptions{})
	if err != nil {
		t.Fatalf("EnsurePullConsumer: %v", err)
	}
	if first.ID != "c-1" {
		t.Errorf("consumer = %+v", first)
	}
	created := 0
	for _, c := range srv.recorded() {
		if c.Method == http.MethodPost {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d consumers created on the first call, want 1", created)
	}

	// The second call finds one and must not rewrite a running
	// consumer's settings behind its back.
	if _, err := q.EnsurePullConsumer(ctx, queues.ConsumerOptions{
		Settings: queues.ConsumerSettings{BatchSize: 1},
	}); err != nil {
		t.Fatalf("EnsurePullConsumer: %v", err)
	}
	created = 0
	for _, c := range srv.recorded() {
		if c.Method == http.MethodPost {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d POSTs after the second call, want the existing consumer left alone", created)
	}
}

func TestUpdateAndDeleteConsumer(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(consumerReply)) }
	q := srv.client().Queue("q-1")
	ctx := context.Background()

	if _, err := q.UpdateConsumer(ctx, "c-1", queues.ConsumerOptions{
		Settings: queues.ConsumerSettings{BatchSize: 10},
	}); err != nil {
		t.Fatalf("UpdateConsumer: %v", err)
	}
	if c := srv.last(); c.Method != http.MethodPut || !strings.HasSuffix(c.Path, "/consumers/c-1") {
		t.Errorf("%s %s", c.Method, c.Path)
	}

	if err := q.DeleteConsumer(ctx, "c-1"); err != nil {
		t.Fatalf("DeleteConsumer: %v", err)
	}
	if c := srv.last(); c.Method != http.MethodDelete {
		t.Errorf("method = %s", c.Method)
	}

	if _, err := q.UpdateConsumer(ctx, "", queues.ConsumerOptions{}); !errors.Is(err, queues.ErrNoConsumerID) {
		t.Errorf("err = %v, want ErrNoConsumerID", err)
	}
	if err := q.DeleteConsumer(ctx, ""); !errors.Is(err, queues.ErrNoConsumerID) {
		t.Errorf("err = %v, want ErrNoConsumerID", err)
	}
}

// A purge has no undo and no dead-letter hop, so it cannot be spelled
// the same way as an ordinary call.
func TestPurgeNeedsTheConfirmation(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	q := srv.client().Queue("q-1")
	ctx := context.Background()

	if err := q.Purge(ctx, queues.PurgeConfirmation{}); !errors.Is(err, queues.ErrNotPermanent) {
		t.Fatalf("err = %v, want ErrNotPermanent", err)
	}
	if len(srv.recorded()) != 0 {
		t.Fatalf("an unconfirmed purge reached the server")
	}

	if err := q.Purge(ctx, queues.DeleteMessagesPermanently); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	c := srv.last()
	if !strings.HasSuffix(c.Path, "/queues/q-1/purge") {
		t.Errorf("path = %s", c.Path)
	}
	if !strings.Contains(c.Body, `"delete_messages_permanently":true`) {
		t.Errorf("body = %s", c.Body)
	}
}

func TestPurgeStatus(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"completed":"true","started_at":"2026-09-15T04:05:06Z"}`))
	}
	st, err := srv.client().Queue("q-1").PurgeStatus(context.Background())
	if err != nil {
		t.Fatalf("PurgeStatus: %v", err)
	}
	if st.Completed != "true" || st.StartedAt == "" {
		t.Errorf("state = %+v", st)
	}
	if srv.last().Method != http.MethodGet {
		t.Errorf("method = %s", srv.last().Method)
	}
}

// Pausing keeps everything and stops delivery, which means the false
// that resumes has to be sent rather than omitted as a zero value.
func TestPauseAndResume(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"queue_id":"q-1","queue_name":"changes","settings":{"delivery_paused":true}}`))
	}
	c := srv.client()
	ctx := context.Background()

	if _, err := c.Pause(ctx, "q-1"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if !strings.Contains(srv.last().Body, `"delivery_paused":true`) {
		t.Errorf("body = %s", srv.last().Body)
	}
	if _, err := c.Resume(ctx, "q-1"); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !strings.Contains(srv.last().Body, `"delivery_paused":false`) {
		t.Errorf("resume body = %s, want the false sent rather than omitted", srv.last().Body)
	}
}

func TestUpdateSettings(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"queue_id":"q-1","queue_name":"renamed"}`))
	}
	c := srv.client()
	ctx := context.Background()

	if _, err := c.UpdateSettings(ctx, "q-1", queues.Settings{
		Name:            "renamed",
		DeliveryDelay:   10 * time.Second,
		RetentionPeriod: 48 * time.Hour,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	body := srv.last().Body
	for _, want := range []string{`"queue_name":"renamed"`, `"delivery_delay":10`, `"message_retention_period":172800`} {
		if !strings.Contains(body, want) {
			t.Errorf("body %s is missing %s", body, want)
		}
	}

	if _, err := c.UpdateSettings(ctx, "q-1", queues.Settings{
		RetentionPeriod: queues.Published.RetentionPeriod + time.Hour,
	}); err == nil || !strings.Contains(err.Error(), "retention period") {
		t.Errorf("err = %v, want the retention limit named", err)
	}
}

func TestConsumerOperationsRefuseAnEmptyQueueID(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	q := srv.client().Queue("")
	ctx := context.Background()

	if _, err := q.Consumers(ctx); !errors.Is(err, queues.ErrNoQueueID) {
		t.Errorf("Consumers err = %v", err)
	}
	if _, err := q.CreateConsumer(ctx, queues.ConsumerOptions{}); !errors.Is(err, queues.ErrNoQueueID) {
		t.Errorf("CreateConsumer err = %v", err)
	}
	if err := q.Purge(ctx, queues.DeleteMessagesPermanently); !errors.Is(err, queues.ErrNoQueueID) {
		t.Errorf("Purge err = %v", err)
	}
	if _, err := srv.client().Pause(ctx, ""); !errors.Is(err, queues.ErrNoQueueID) {
		t.Errorf("Pause err = %v", err)
	}
	if len(srv.recorded()) != 0 {
		t.Errorf("%d requests reached the server", len(srv.recorded()))
	}
}

// Reading a consumer and writing it back with one field changed is
// the natural edit, and it only works if both sides speak the same
// type.
func TestConsumerSettingsRoundTrip(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(consumerReply))
	}
	q := srv.client().Queue("q-1")
	ctx := context.Background()

	got, err := q.PullConsumer(ctx)
	if err != nil {
		// PullConsumer lists; the canned reply is a single consumer
		// object, so fetch it the other way for this check.
		got, err = q.CreateConsumer(ctx, queues.ConsumerOptions{})
		if err != nil {
			t.Fatalf("CreateConsumer: %v", err)
		}
	}

	settings := got.Settings
	settings.BatchSize = 25
	if _, err := q.UpdateConsumer(ctx, got.ID, queues.ConsumerOptions{
		Settings:        settings,
		DeadLetterQueue: got.DeadLetterQueue,
	}); err != nil {
		t.Fatalf("UpdateConsumer: %v", err)
	}

	var sent struct {
		Settings struct {
			BatchSize    int   `json:"batch_size"`
			RetryDelay   int   `json:"retry_delay"`
			VisibilityMs int64 `json:"visibility_timeout_ms"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(srv.last().Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent.Settings.BatchSize != 25 {
		t.Errorf("batch size = %d", sent.Settings.BatchSize)
	}
	// The untouched fields must survive the round trip in the units
	// Cloudflare wants them back in.
	if sent.Settings.RetryDelay != 30 || sent.Settings.VisibilityMs != 120000 {
		t.Errorf("round-tripped settings = %+v", sent.Settings)
	}
}
