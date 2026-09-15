package queues_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/queues"
)

type call struct {
	Method string
	Path   string
	Body   string
}

type server struct {
	*httptest.Server

	t       *testing.T
	handler func(w http.ResponseWriter, r *http.Request, body []byte)

	mu    sync.Mutex
	calls []call
}

func newServer(t *testing.T) *server {
	t.Helper()
	s := &server{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.calls = append(s.calls, call{Method: r.Method, Path: r.URL.Path, Body: string(raw)})
		s.mu.Unlock()
		s.handler(w, r, raw)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) client() *queues.Client {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	return queues.New(cf)
}

func (s *server) recorded() []call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]call(nil), s.calls...)
}

func (s *server) last() call {
	s.t.Helper()
	c := s.recorded()
	if len(c) == 0 {
		s.t.Fatal("no request was made")
	}
	return c[len(c)-1]
}

func envelope(result string) string {
	return `{"success":true,"errors":[],"messages":[],"result":` + result + `}`
}

func TestPublishSendsBodyAndContentType(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }

	type change struct {
		Table string `json:"table"`
		Key   string `json:"key"`
	}
	q := srv.client().Queue("q-1")
	if err := q.Publish(context.Background(), queues.JSON(change{Table: "users", Key: "42"}).After(30*time.Second)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	c := srv.last()
	if want := "/accounts/acct/queues/q-1/messages"; c.Path != want {
		t.Errorf("path = %s, want %s", c.Path, want)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(c.Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if sent["content_type"] != "json" {
		t.Errorf("content_type = %v", sent["content_type"])
	}
	if sent["delay_seconds"] != float64(30) {
		t.Errorf("delay_seconds = %v", sent["delay_seconds"])
	}
	body, _ := sent["body"].(map[string]any)
	if body == nil || body["table"] != "users" {
		t.Errorf("body = %v", sent["body"])
	}
}

func TestPublishBatch(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }

	msgs := []queues.Message{queues.Text("one"), queues.Text("two")}
	if err := srv.client().Queue("q-1").PublishBatch(context.Background(), msgs); err != nil {
		t.Fatalf("PublishBatch: %v", err)
	}
	c := srv.last()
	if !strings.HasSuffix(c.Path, "/messages/batch") {
		t.Errorf("path = %s", c.Path)
	}
	var sent struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(c.Body), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(sent.Messages) != 2 || sent.Messages[1]["body"] != "two" {
		t.Errorf("messages = %v", sent.Messages)
	}
}

// A message over the ceiling is refused before it is sent, with an
// error that says what the shape that fits looks like.
func TestPublishRefusesAnOversizedMessage(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }

	big := strings.Repeat("x", queues.Published.MessageSize)
	err := srv.client().Queue("q-1").Publish(context.Background(), queues.Text(big))
	if !errors.Is(err, queues.ErrMessageTooLarge) {
		t.Fatalf("err = %v, want ErrMessageTooLarge", err)
	}
	if !strings.Contains(err.Error(), "reference to the row") {
		t.Errorf("error %q does not say what to send instead", err)
	}
	if len(srv.recorded()) != 0 {
		t.Errorf("%d requests reached the server", len(srv.recorded()))
	}
}

func TestPublishBatchRefusesTooManyMessages(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }

	msgs := make([]queues.Message, queues.Published.BatchMessages+1)
	for i := range msgs {
		msgs[i] = queues.Text("x")
	}
	err := srv.client().Queue("q-1").PublishBatch(context.Background(), msgs)
	if !errors.Is(err, queues.ErrBatchTooLarge) {
		t.Fatalf("err = %v, want ErrBatchTooLarge", err)
	}
}

func TestTextMessageMustCarryAString(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	err := srv.client().Queue("q-1").Publish(context.Background(),
		queues.Message{Body: 42, ContentType: queues.ContentTypeText})
	if err == nil || !strings.Contains(err.Error(), "want a string") {
		t.Errorf("err = %v", err)
	}
}

const pullReply = `{"message_backlog_count":17,"messages":[
	{"id":"m1","body":"{\"key\":\"1\"}","lease_id":"l1","attempts":1,"timestamp_ms":1789000000000},
	{"id":"m2","body":"{\"key\":\"2\"}","lease_id":"l2","attempts":3}
]}`

func TestPullAndSettle(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/messages/pull") {
			fmt.Fprint(w, envelope(pullReply))
			return
		}
		fmt.Fprint(w, envelope(`{"ackCount":1,"retryCount":1,"warnings":{}}`))
	}

	q := srv.client().Queue("q-1")
	batch, err := q.Pull(context.Background(), queues.PullOptions{BatchSize: 10, VisibilityTimeout: time.Minute})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if batch.BacklogCount != 17 {
		t.Errorf("BacklogCount = %d, want 17", batch.BacklogCount)
	}
	if len(batch.Messages) != 2 {
		t.Fatalf("%d messages", len(batch.Messages))
	}
	if batch.Pending() != 2 {
		t.Errorf("Pending = %d before anything is marked", batch.Pending())
	}

	var payload struct {
		Key string `json:"key"`
	}
	if err := batch.Messages[0].Decode(&payload); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if payload.Key != "1" {
		t.Errorf("decoded %+v", payload)
	}
	if got := batch.Messages[0].PublishedAt(); got.IsZero() {
		t.Error("PublishedAt is zero for a message with a timestamp")
	}
	if got := batch.Messages[1].PublishedAt(); !got.IsZero() {
		t.Error("PublishedAt invents a time for a message without one")
	}

	if err := batch.Ack(batch.Messages[0]); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := batch.Retry(batch.Messages[1], 45*time.Second); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if batch.Pending() != 0 {
		t.Errorf("Pending = %d after marking both", batch.Pending())
	}

	settled, err := batch.Settle(context.Background())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if settled.Acked != 1 || settled.Retried != 1 {
		t.Errorf("settled = %+v", settled)
	}

	calls := srv.recorded()
	// One pull, one settlement: the whole point of marking first.
	if len(calls) != 2 {
		t.Fatalf("%d requests, want 2", len(calls))
	}
	var pull map[string]any
	if err := json.Unmarshal([]byte(calls[0].Body), &pull); err != nil {
		t.Fatalf("pull body: %v", err)
	}
	if pull["batch_size"] != float64(10) || pull["visibility_timeout_ms"] != float64(60000) {
		t.Errorf("pull body = %s", calls[0].Body)
	}
	var ack struct {
		Acks    []map[string]any `json:"acks"`
		Retries []map[string]any `json:"retries"`
	}
	if err := json.Unmarshal([]byte(calls[1].Body), &ack); err != nil {
		t.Fatalf("ack body: %v", err)
	}
	if len(ack.Acks) != 1 || ack.Acks[0]["lease_id"] != "l1" {
		t.Errorf("acks = %v", ack.Acks)
	}
	if len(ack.Retries) != 1 || ack.Retries[0]["delay_seconds"] != float64(45) {
		t.Errorf("retries = %v", ack.Retries)
	}
}

// A lease is the handle on a delivery. Acknowledging one that did not
// come from this batch would acknowledge someone else's work.
func TestAckRefusesAForeignLease(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(pullReply)) }
	batch, err := srv.client().Queue("q-1").Pull(context.Background(), queues.PullOptions{})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	other := queues.PulledMessage{ID: "m9", LeaseID: "l9"}
	if err := batch.Ack(other); !errors.Is(err, queues.ErrUnknownLease) {
		t.Errorf("Ack err = %v, want ErrUnknownLease", err)
	}
	if err := batch.Retry(other, 0); !errors.Is(err, queues.ErrUnknownLease) {
		t.Errorf("Retry err = %v, want ErrUnknownLease", err)
	}
}

// A batch with nothing marked must not send a request: every message
// in it simply stays leased until it expires.
func TestSettleWithNothingMarkedSendsNothing(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(pullReply)) }
	batch, err := srv.client().Queue("q-1").Pull(context.Background(), queues.PullOptions{})
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	before := len(srv.recorded())
	settled, err := batch.Settle(context.Background())
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if settled.Acked != 0 || settled.Retried != 0 {
		t.Errorf("settled = %+v", settled)
	}
	if len(srv.recorded()) != before {
		t.Error("an empty settlement made a request")
	}
}

func TestConsumeAcksSuccessesAndRetriesFailures(t *testing.T) {
	srv := newServer(t)
	var pulls atomic.Int64
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/messages/pull") {
			if pulls.Add(1) == 1 {
				fmt.Fprint(w, envelope(pullReply))
				return
			}
			fmt.Fprint(w, envelope(`{"message_backlog_count":0,"messages":[]}`))
			return
		}
		fmt.Fprint(w, envelope(`{"ackCount":1,"retryCount":1,"warnings":{}}`))
	}

	// Consume ends only on a context or transport failure, so the
	// test bounds it: one batch of work, then the deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	var seen []string
	err := srv.client().Queue("q-1").Consume(ctx,
		func(_ context.Context, m queues.PulledMessage) error {
			seen = append(seen, m.ID)
			if m.ID == "m2" {
				return errors.New("downstream is down")
			}
			return nil
		},
		queues.ConsumeOptions{
			Pull:    queues.PullOptions{BatchSize: 2},
			Idle:    time.Millisecond,
			Backoff: func(attempts int) time.Duration { return time.Duration(attempts) * time.Second },
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Consume err = %v, want the context's", err)
	}

	if len(seen) != 2 || seen[0] != "m1" || seen[1] != "m2" {
		t.Fatalf("handler saw %v, want both messages exactly once", seen)
	}

	var ack struct {
		Acks    []map[string]any `json:"acks"`
		Retries []map[string]any `json:"retries"`
	}
	found := false
	for _, c := range srv.recorded() {
		if strings.HasSuffix(c.Path, "/messages/ack") {
			if err := json.Unmarshal([]byte(c.Body), &ack); err != nil {
				t.Fatalf("ack body: %v", err)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("the batch was never settled")
	}
	if len(ack.Acks) != 1 || ack.Acks[0]["lease_id"] != "l1" {
		t.Errorf("acks = %v, want only the message that succeeded", ack.Acks)
	}
	// m2 arrived having been attempted three times, so the backoff
	// was asked about three rather than about one.
	if len(ack.Retries) != 1 || ack.Retries[0]["delay_seconds"] != float64(3) {
		t.Errorf("retries = %v, want the failure back with its backoff", ack.Retries)
	}
}

func TestConsumeStopsOnErrStop(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if strings.HasSuffix(r.URL.Path, "/messages/pull") {
			fmt.Fprint(w, envelope(pullReply))
			return
		}
		fmt.Fprint(w, envelope(`{"ackCount":1,"retryCount":0,"warnings":{}}`))
	}

	var seen int
	err := srv.client().Queue("q-1").Consume(context.Background(),
		func(_ context.Context, _ queues.PulledMessage) error {
			seen++
			return queues.ErrStop
		},
		queues.ConsumeOptions{Idle: time.Millisecond})
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if seen != 1 {
		t.Errorf("handler ran %d times, want 1 — ErrStop ends the loop", seen)
	}
	// The message that said stop is acknowledged; the rest of the
	// batch is left unmarked so the queue offers it again.
	var ack struct {
		Acks []map[string]any `json:"acks"`
	}
	if err := json.Unmarshal([]byte(srv.last().Body), &ack); err != nil {
		t.Fatalf("ack body: %v", err)
	}
	if len(ack.Acks) != 1 {
		t.Errorf("acks = %v, want only the message that stopped the loop", ack.Acks)
	}
}

func TestConsumeStopsWhenTheContextIsDone(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		fmt.Fprint(w, envelope(`{"message_backlog_count":0,"messages":[]}`))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := srv.client().Queue("q-1").Consume(ctx,
		func(context.Context, queues.PulledMessage) error { return nil },
		queues.ConsumeOptions{Idle: time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's", err)
	}
}

func TestDefaultBackoffIsCapped(t *testing.T) {
	if got := queues.DefaultBackoff(1); got != 10*time.Second {
		t.Errorf("first retry waits %s", got)
	}
	if got := queues.DefaultBackoff(3); got != 40*time.Second {
		t.Errorf("third retry waits %s", got)
	}
	// Uncapped, attempt 20 would be about 60 days — well past the
	// fortnight a message is even kept.
	if got := queues.DefaultBackoff(20); got != time.Hour {
		t.Errorf("twentieth retry waits %s, want the cap", got)
	}
	if got := queues.DefaultBackoff(20); got > queues.Published.RetentionPeriod {
		t.Errorf("a backoff of %s outlives the %s a message is kept", got, queues.Published.RetentionPeriod)
	}
}

func TestQueueLifecycle(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, r *http.Request, _ []byte) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/queues") {
			fmt.Fprint(w, envelope(`[{"queue_id":"q-1","queue_name":"changes","consumers_total_count":1}]`))
			return
		}
		fmt.Fprint(w, envelope(`{"queue_id":"q-1","queue_name":"changes"}`))
	}
	c := srv.client()
	ctx := context.Background()

	made, err := c.Create(ctx, "changes")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if made.ID != "q-1" {
		t.Errorf("queue = %+v", made)
	}

	found, err := c.FindByName(ctx, "changes")
	if err != nil {
		t.Fatalf("FindByName: %v", err)
	}
	if found.ConsumersTotalCount != 1 {
		t.Errorf("consumers = %d", found.ConsumersTotalCount)
	}
	if _, err := c.FindByName(ctx, "absent"); !errors.Is(err, cloudflare.ErrNotFound) {
		t.Errorf("FindByName err = %v, want ErrNotFound", err)
	}

	if err := c.Delete(ctx, "q-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestEmptyIdentifiersAreRefusedLocally(t *testing.T) {
	srv := newServer(t)
	srv.handler = func(w http.ResponseWriter, _ *http.Request, _ []byte) { fmt.Fprint(w, envelope(`{}`)) }
	c := srv.client()
	ctx := context.Background()

	if _, err := c.Create(ctx, ""); !errors.Is(err, queues.ErrNoQueueName) {
		t.Errorf("Create err = %v", err)
	}
	if err := c.Queue("").Publish(ctx, queues.Text("x")); !errors.Is(err, queues.ErrNoQueueID) {
		t.Errorf("Publish err = %v", err)
	}
	if _, err := c.Queue("").Pull(ctx, queues.PullOptions{}); !errors.Is(err, queues.ErrNoQueueID) {
		t.Errorf("Pull err = %v", err)
	}
	if err := c.Queue("q").PublishBatch(ctx, nil); !errors.Is(err, queues.ErrNoMessages) {
		t.Errorf("PublishBatch err = %v", err)
	}
	if len(srv.recorded()) != 0 {
		t.Errorf("%d requests reached the server", len(srv.recorded()))
	}
}
