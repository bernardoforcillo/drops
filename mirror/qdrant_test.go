package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/mirror"
	"github.com/bernardoforcillo/drops/qdrant"
)

type qreq struct {
	Path string
	Body string
}

func qdrantStub(t *testing.T) (*qdrant.Client, *[]qreq) {
	t.Helper()
	var seen []qreq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, qreq{Path: r.URL.Path, Body: string(body)})
		_ = json.NewEncoder(w).Encode(map[string]any{"result": true, "status": "ok"})
	}))
	t.Cleanup(srv.Close)
	cli, err := qdrant.NewClient(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return cli, &seen
}

func constEmbedder(v ...float32) mirror.Embedder {
	return func(context.Context, mirror.Change) ([]float32, error) { return v, nil }
}

func TestQdrantSinkUpsertsAndDeletes(t *testing.T) {
	cli, seen := qdrantStub(t)
	s, err := mirror.NewQdrantSink(cli, "docs", constEmbedder(1, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	err = s.Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpInsert, Key: "1", Row: map[string]any{"title": "a"}},
		{Op: mirror.OpDelete, Key: "2"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(*seen) != 2 {
		t.Fatalf("expected an upsert and a delete, got %d requests", len(*seen))
	}
	if !strings.HasSuffix((*seen)[0].Path, "/points") {
		t.Errorf("first request should be the upsert, got %s", (*seen)[0].Path)
	}
	if !strings.Contains((*seen)[0].Body, `"title":"a"`) {
		t.Errorf("payload not forwarded: %s", (*seen)[0].Body)
	}
	if !strings.HasSuffix((*seen)[1].Path, "/points/delete") {
		t.Errorf("second request should be the delete, got %s", (*seen)[1].Path)
	}
}

// A batch that creates a key and then removes it must end with the
// point gone. Grouping all upserts before all deletes would be a
// reordering that inverts that.
func TestQdrantSinkPreservesOrderAcrossKinds(t *testing.T) {
	cli, seen := qdrantStub(t)
	s, _ := mirror.NewQdrantSink(cli, "docs", constEmbedder(1))
	err := s.Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpInsert, Key: "1"},
		{Op: mirror.OpDelete, Key: "1"},
		{Op: mirror.OpInsert, Key: "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 3 {
		t.Fatalf("expected three runs (upsert, delete, upsert), got %d", len(*seen))
	}
	kinds := []string{"/points", "/points/delete", "/points"}
	for i, want := range kinds {
		if !strings.HasSuffix((*seen)[i].Path, want) {
			t.Errorf("request %d = %s, want suffix %s", i, (*seen)[i].Path, want)
		}
	}
}

// Consecutive changes of the same kind should batch into one request
// — that is the difference between one round trip and N.
func TestQdrantSinkBatchesRuns(t *testing.T) {
	cli, seen := qdrantStub(t)
	s, _ := mirror.NewQdrantSink(cli, "docs", constEmbedder(1))
	err := s.Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpInsert, Key: "1"},
		{Op: mirror.OpUpdate, Key: "2"},
		{Op: mirror.OpInsert, Key: "3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 1 {
		t.Fatalf("three upserts should be one request, got %d", len(*seen))
	}
}

// A nil vector is how an embedder says "this change does not affect
// the index".
func TestQdrantSinkSkipsDeclinedRows(t *testing.T) {
	cli, seen := qdrantStub(t)
	s, _ := mirror.NewQdrantSink(cli, "docs",
		func(_ context.Context, ch mirror.Change) ([]float32, error) {
			if ch.Key == "skip" {
				return nil, nil
			}
			return []float32{1}, nil
		})
	if err := s.Apply(context.Background(), []mirror.Change{{Op: mirror.OpInsert, Key: "skip"}}); err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 0 {
		t.Errorf("a declined row should issue no request, got %d", len(*seen))
	}
}

func TestQdrantSinkSurfacesEmbedFailure(t *testing.T) {
	cli, _ := qdrantStub(t)
	boom := errors.New("model unavailable")
	s, _ := mirror.NewQdrantSink(cli, "docs",
		func(context.Context, mirror.Change) ([]float32, error) { return nil, boom })
	err := s.Apply(context.Background(), []mirror.Change{{Op: mirror.OpInsert, Key: "1"}})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want the embedder's error", err)
	}
}

func TestQdrantSinkCustomPayload(t *testing.T) {
	cli, seen := qdrantStub(t)
	s, _ := mirror.NewQdrantSink(cli, "docs", constEmbedder(1),
		mirror.WithPayload(func(ch mirror.Change) map[string]any {
			return map[string]any{"lang": ch.Row["lang"]}
		}))
	err := s.Apply(context.Background(), []mirror.Change{
		{Op: mirror.OpInsert, Key: "1", Row: map[string]any{"lang": "it", "secret": "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := (*seen)[0].Body
	if !strings.Contains(body, `"lang":"it"`) {
		t.Errorf("payload missing the mapped field: %s", body)
	}
	if strings.Contains(body, "secret") {
		t.Errorf("payload mapper was bypassed, secret leaked: %s", body)
	}
}

// Qdrant takes unsigned integers or UUIDs, not arbitrary strings.
func TestPointID(t *testing.T) {
	if got := mirror.PointID("42"); got != uint64(42) {
		t.Errorf("numeric key = %v (%T), want uint64(42)", got, got)
	}
	uuid := "6f1a1b6e-0f4e-4a0b-9e1e-2a3b4c5d6e7f"
	if got := mirror.PointID(uuid); got != uuid {
		t.Errorf("uuid key = %v", got)
	}
	// A negative number is not a valid Qdrant id; it stays a string
	// so the server rejects it rather than this silently rewriting
	// it into something a later delete would not match.
	if got := mirror.PointID("-1"); got != "-1" {
		t.Errorf("negative key = %v, want the string unchanged", got)
	}
}

func TestNewQdrantSinkValidates(t *testing.T) {
	cli, _ := qdrantStub(t)
	if _, err := mirror.NewQdrantSink(nil, "docs", constEmbedder(1)); err == nil {
		t.Error("expected an error for a nil client")
	}
	if _, err := mirror.NewQdrantSink(cli, "", constEmbedder(1)); err == nil {
		t.Error("expected an error for an empty collection")
	}
	if _, err := mirror.NewQdrantSink(cli, "docs", nil); err == nil {
		t.Error("expected an error for a missing embedder")
	}
}

func TestQdrantSinkRejectsKeylessChange(t *testing.T) {
	cli, _ := qdrantStub(t)
	s, _ := mirror.NewQdrantSink(cli, "docs", constEmbedder(1))
	if err := s.Apply(context.Background(), []mirror.Change{{Op: mirror.OpInsert}}); !errors.Is(err, mirror.ErrNoKey) {
		t.Errorf("err = %v want ErrNoKey", err)
	}
}
