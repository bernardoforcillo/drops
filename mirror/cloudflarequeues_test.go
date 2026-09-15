package mirror_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops/cloudflare"
	"github.com/bernardoforcillo/drops/cloudflare/queues"
	"github.com/bernardoforcillo/drops/mirror"
)

// queuesServer records the publish bodies a sink sends.
type queuesServer struct {
	*httptest.Server

	t      *testing.T
	bodies []string
	paths  []string
	reply  func(n int) (int, string)
}

func newQueuesServer(t *testing.T) *queuesServer {
	t.Helper()
	s := &queuesServer{t: t}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		s.bodies = append(s.bodies, string(raw))
		s.paths = append(s.paths, r.URL.Path)
		status, body := 200, `{"success":true,"errors":[],"messages":[],"result":{}}`
		if s.reply != nil {
			status, body = s.reply(len(s.bodies))
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *queuesServer) sink(opts ...mirror.QueuesOption) *mirror.QueuesSink {
	s.t.Helper()
	cf, err := cloudflare.New("acct",
		cloudflare.WithAPIToken("tok"),
		cloudflare.WithBaseURL(s.URL),
		cloudflare.WithRetryPolicy(cloudflare.RetryPolicy{}),
	)
	if err != nil {
		s.t.Fatalf("cloudflare.New: %v", err)
	}
	sink, err := mirror.NewQueuesSink(queues.New(cf).Queue("q-1"), opts...)
	if err != nil {
		s.t.Fatalf("NewQueuesSink: %v", err)
	}
	return sink
}

// decodeBatch pulls the change events out of a batch publish body.
func decodeBatch(t *testing.T, body string) []mirror.ChangeEvent {
	t.Helper()
	var sent struct {
		Messages []struct {
			Body        mirror.ChangeEvent `json:"body"`
			ContentType string             `json:"content_type"`
			Delay       int                `json:"delay_seconds"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("body is not a batch publish: %v (%s)", err, body)
	}
	out := make([]mirror.ChangeEvent, len(sent.Messages))
	for i, m := range sent.Messages {
		if m.ContentType != "json" {
			t.Errorf("message %d content_type = %q", i, m.ContentType)
		}
		out[i] = m.Body
	}
	return out
}

func TestQueuesSinkPublishesTheChangeEvent(t *testing.T) {
	srv := newQueuesServer(t)
	at := time.Date(2026, 9, 15, 4, 5, 6, 0, time.UTC)

	err := srv.sink().Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpUpdate, Key: "42", Row: map[string]any{"name": "ada"}, Version: 7, At: at},
		{Op: mirror.OpDelete, Key: "43", Version: 8, At: at},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(srv.bodies) != 1 {
		t.Fatalf("%d requests, want 1 batch", len(srv.bodies))
	}
	if !strings.HasSuffix(srv.paths[0], "/messages/batch") {
		t.Errorf("path = %s", srv.paths[0])
	}

	events := decodeBatch(t, srv.bodies[0])
	if len(events) != 2 {
		t.Fatalf("%d events", len(events))
	}
	if events[0].Op != "update" || events[0].Key != "42" || events[0].Version != 7 {
		t.Errorf("event = %+v", events[0])
	}
	if events[0].Row["name"] != "ada" {
		t.Errorf("row = %v", events[0].Row)
	}
	if events[0].At != at.Format(time.RFC3339Nano) {
		t.Errorf("at = %q", events[0].At)
	}
	// A delete carries no row: the row is gone at the source, and the
	// consumer needs only the key to act on it.
	if events[1].Op != "delete" || events[1].Row != nil {
		t.Errorf("delete event = %+v", events[1])
	}
}

// Cloudflare takes a hundred messages per request, and a mirror batch
// is whatever the pump handed over.
func TestQueuesSinkChunksLargeBatches(t *testing.T) {
	srv := newQueuesServer(t)
	n := queues.Published.BatchMessages + 5
	changes := make([]mirror.Change, n)
	for i := range changes {
		changes[i] = mirror.Change{Op: mirror.OpInsert, Key: fmt.Sprint(i), Version: uint64(i + 1)}
	}

	if err := srv.sink().Apply(context.Background(), changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(srv.bodies) != 2 {
		t.Fatalf("%d requests, want 2 chunks", len(srv.bodies))
	}
	if got := len(decodeBatch(t, srv.bodies[0])); got != queues.Published.BatchMessages {
		t.Errorf("first chunk had %d messages", got)
	}
	if got := len(decodeBatch(t, srv.bodies[1])); got != 5 {
		t.Errorf("second chunk had %d messages", got)
	}
}

// The byte ceiling cannot be predicted from the change count, so a
// chunk that is too many bytes is split rather than failed: a sink
// that gave up here would stop the mirror on a wide table.
func TestQueuesSinkFallsBackToOneMessageAtATime(t *testing.T) {
	srv := newQueuesServer(t)
	// Each row is comfortably under the message ceiling, but four of
	// them together pass the batch ceiling.
	big := strings.Repeat("x", queues.Published.BatchSize/3)
	changes := make([]mirror.Change, 4)
	for i := range changes {
		changes[i] = mirror.Change{
			Op:      mirror.OpInsert,
			Key:     fmt.Sprint(i),
			Row:     map[string]any{"blob": big},
			Version: uint64(i + 1),
		}
	}

	if err := srv.sink().Apply(context.Background(), changes); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// One refused batch never reaches the server, then four single
	// publishes do.
	if len(srv.bodies) != 4 {
		t.Fatalf("%d requests, want 4 single publishes", len(srv.bodies))
	}
	for i, p := range srv.paths {
		if strings.HasSuffix(p, "/messages/batch") {
			t.Errorf("request %d went to the batch endpoint", i)
		}
	}
}

// A message no encoder can shrink names the key it belongs to, because
// "message too large" in a batch of a hundred is otherwise a needle.
func TestQueuesSinkNamesTheChangeThatFailed(t *testing.T) {
	srv := newQueuesServer(t)
	huge := strings.Repeat("x", queues.Published.MessageSize)
	err := srv.sink().Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpInsert, Key: "the-big-one", Row: map[string]any{"doc": huge}, Version: 1},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "the-big-one") {
		t.Errorf("error %q does not name the change", err)
	}
}

// An encoder is where a caller drops what the consumer does not care
// about, and where a row too wide for a message becomes a reference.
func TestQueuesSinkEncoderCanReshapeAndDrop(t *testing.T) {
	srv := newQueuesServer(t)
	sink := srv.sink(
		mirror.WithQueuesName("cache-invalidation"),
		mirror.WithQueuesDelay(5*time.Second),
		mirror.WithQueuesEncoder(func(ch mirror.Change) (any, error) {
			if ch.Op == mirror.OpDelete {
				return nil, nil
			}
			return map[string]any{"invalidate": "user:" + ch.Key}, nil
		}),
	)
	if sink.Name() != "cache-invalidation" {
		t.Errorf("Name = %q", sink.Name())
	}

	err := sink.Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpUpdate, Key: "42", Row: map[string]any{"secret": "s"}, Version: 1},
		{Op: mirror.OpDelete, Key: "43", Version: 2},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var sent struct {
		Messages []struct {
			Body  map[string]any `json:"body"`
			Delay int            `json:"delay_seconds"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(srv.bodies[0]), &sent); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(sent.Messages) != 1 {
		t.Fatalf("%d messages, want the delete dropped", len(sent.Messages))
	}
	if sent.Messages[0].Body["invalidate"] != "user:42" {
		t.Errorf("body = %v", sent.Messages[0].Body)
	}
	if _, leaked := sent.Messages[0].Body["secret"]; leaked {
		t.Error("the encoder's shape was ignored and the row was sent")
	}
	if sent.Messages[0].Delay != 5 {
		t.Errorf("delay_seconds = %d", sent.Messages[0].Delay)
	}
}

// A queue holds nothing to compare a version against, so the sink
// must not claim it can lose a race — which is what keeps a fill-mode
// reseed from accepting it.
func TestQueuesSinkIsNotVersionAware(t *testing.T) {
	srv := newQueuesServer(t)
	var s mirror.Sink = srv.sink()
	if va, ok := s.(mirror.VersionAwareSink); ok && va.VersionAware() {
		t.Error("QueuesSink claims to be version-aware; a queue stores nothing to compare against")
	}
}

func TestQueuesSinkEmptyBatchSendsNothing(t *testing.T) {
	srv := newQueuesServer(t)
	if err := srv.sink().Apply(context.Background(), nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(srv.bodies) != 0 {
		t.Errorf("%d requests for an empty batch", len(srv.bodies))
	}
}

func TestNewQueuesSinkNeedsAQueue(t *testing.T) {
	if _, err := mirror.NewQueuesSink(nil); err == nil {
		t.Error("NewQueuesSink(nil) succeeded")
	}
}
