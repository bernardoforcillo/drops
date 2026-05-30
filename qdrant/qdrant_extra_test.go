package qdrant_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/qdrant"
)

// --- Hook ------------------------------------------------------------

func TestHookFiresOnEveryRequest(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /collections", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{"collections": []any{}})
	})
	m.handle("DELETE /collections/x", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, true)
	})

	var (
		mu     sync.Mutex
		events []drops.QueryEvent
	)
	hook := func(_ context.Context, e drops.QueryEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	cli, _ := qdrant.NewClient(m.server.URL, qdrant.WithHook(hook))
	ctx := context.Background()
	if _, err := cli.ListCollections(ctx); err != nil {
		t.Fatal(err)
	}
	if err := cli.DeleteCollection(ctx, "x"); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].Kind != "http" || events[0].SQL != "GET /collections" {
		t.Errorf("event[0]: %+v", events[0])
	}
	if events[1].Kind != "http" || events[1].SQL != "DELETE /collections/x" {
		t.Errorf("event[1]: %+v", events[1])
	}
	for i, e := range events {
		if e.Duration <= 0 {
			t.Errorf("event[%d] has zero duration", i)
		}
		if e.Err != nil {
			t.Errorf("event[%d] err = %v", i, e.Err)
		}
	}
}

func TestHookFiresOnError(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /collections/missing", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"status":{"error":"not found"}}`, http.StatusNotFound)
	})

	var got drops.QueryEvent
	cli, _ := qdrant.NewClient(m.server.URL, qdrant.WithHook(func(_ context.Context, e drops.QueryEvent) {
		got = e
	}))
	if _, err := cli.CollectionInfo(context.Background(), "missing"); err == nil {
		t.Fatal("expected an error")
	}
	if got.Err == nil {
		t.Error("hook didn't receive the error")
	}
	if !errors.Is(got.Err, qdrant.ErrCollectionMissing) {
		t.Errorf("hook err: %v", got.Err)
	}
}

func TestWithHookFnReturnsCopy(t *testing.T) {
	base, _ := qdrant.NewClient("http://example.com:6333")
	if base.Hook() != nil {
		t.Fatal("base hook should be nil")
	}
	hooked := base.WithHookFn(func(context.Context, drops.QueryEvent) {})
	if base.Hook() != nil {
		t.Error("WithHookFn mutated original client")
	}
	if hooked.Hook() == nil {
		t.Error("WithHookFn didn't install hook on copy")
	}
}

// --- Ping ------------------------------------------------------------

func TestPingHappyPath(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "healthz check passed")
	})
	var got drops.QueryEvent
	cli, _ := qdrant.NewClient(m.server.URL, qdrant.WithHook(func(_ context.Context, e drops.QueryEvent) {
		got = e
	}))
	if err := cli.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.Kind != "ping" || got.SQL != "/healthz" {
		t.Errorf("ping event: %+v", got)
	}
}

func TestPingPropagatesHTTPError(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	cli, _ := qdrant.NewClient(m.server.URL)
	err := cli.Ping(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	var herr *qdrant.HTTPError
	if !errors.As(err, &herr) || herr.Status != 503 {
		t.Errorf("expected 503 HTTPError, got %v", err)
	}
}

// --- Identifier validation ------------------------------------------

func TestCollectionNameValidation(t *testing.T) {
	cli, _ := qdrant.NewClient("http://example.com:6333")
	ctx := context.Background()
	cases := []struct {
		name string
	}{
		{""},                  // empty
		{"with space"},        // space
		{"slash/here"},        // path separator would break URL
		{"question?mark"},     // query separator
		{"hash#fragment"},     // fragment
		{".dotleader"},        // leading dot
		{"-hyphenleader"},     // leading hyphen
		{"null\x00inside"},    // NUL
	}
	cfg := qdrant.CollectionConfig{
		Vectors: qdrant.VectorParams{Size: 4, Distance: qdrant.DistanceCosine},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.name), func(t *testing.T) {
			err := cli.CreateCollection(ctx, tc.name, cfg)
			if !errors.Is(err, qdrant.ErrInvalidIdentifier) {
				t.Errorf("CreateCollection(%q) err = %v, want ErrInvalidIdentifier", tc.name, err)
			}
		})
	}
}

// --- LoggerHook (shared root) -------------------------------------

func TestRootLoggerHookWorksAgainstQdrant(t *testing.T) {
	m := newMock()
	defer m.close()
	m.handle("GET /collections", func(w http.ResponseWriter, _ *http.Request) {
		envelope(w, map[string]any{"collections": []any{}})
	})
	var lines []string
	logger := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }
	cli, _ := qdrant.NewClient(m.server.URL,
		qdrant.WithHook(drops.LoggerHook(logger, drops.LoggerOptions{
			SlowQuery: 0, // log everything
		})),
	)
	if _, err := cli.ListCollections(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "GET /collections") {
		t.Errorf("log line missing method+path: %s", lines[0])
	}
}

// --- A reasonable timeout default ------------------------------------

func TestDefaultHTTPClientHasTimeout(t *testing.T) {
	cli, _ := qdrant.NewClient("http://example.com:6333")
	if to := cli.HTTPClient().Timeout; to == 0 {
		t.Error("default HTTP client should set a timeout (production safety)")
	} else if to > 5*time.Minute {
		t.Errorf("default timeout %s is too generous", to)
	}
}

// reuse mockServer + envelope from the main test file by importing this
// file alongside it; httptest itself is referenced here only for the
// indirect dependency in `mockServer`.
var _ = httptest.NewServer
