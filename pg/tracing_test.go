package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// recordingTracer captures every span opened during a test so
// assertions can check name, attributes, error and ordering.
type recordingTracer struct {
	spans []*recordingSpan
}

func (t *recordingTracer) Start(ctx context.Context, name string) (context.Context, pg.Span) {
	s := &recordingSpan{name: name, attrs: map[string]any{}}
	t.spans = append(t.spans, s)
	return ctx, s
}

type recordingSpan struct {
	name   string
	attrs  map[string]any
	err    error
	ended  bool
}

func (s *recordingSpan) SetAttribute(k string, v any) { s.attrs[k] = v }
func (s *recordingSpan) RecordError(err error)        { s.err = err }
func (s *recordingSpan) End()                         { s.ended = true }

func TestTracerStartsSpanPerExec(t *testing.T) {
	tracer := &recordingTracer{}
	db := pg.New(&fakeDriver{}).WithTracer(tracer)
	if _, err := db.Exec(context.Background(), "UPDATE x SET y = 1"); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(tracer.spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tracer.spans))
	}
	s := tracer.spans[0]
	if !s.ended {
		t.Error("span must be ended")
	}
	if s.name != "drops.exec" {
		t.Errorf("span name: %s", s.name)
	}
	if s.attrs[pg.AttrStatement] != "UPDATE x SET y = 1" {
		t.Errorf("statement attr: %v", s.attrs[pg.AttrStatement])
	}
	if s.attrs[pg.AttrSystem] != pg.AttrSystemPG {
		t.Errorf("system attr: %v", s.attrs[pg.AttrSystem])
	}
}

func TestTracerStartsSpanPerQuery(t *testing.T) {
	tracer := &recordingTracer{}
	db := pg.New(&fakeDriver{}).WithTracer(tracer)
	if _, err := db.Query(context.Background(), "SELECT 1"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(tracer.spans) != 1 || tracer.spans[0].name != "drops.query" {
		t.Errorf("expected drops.query span, got %v", tracer.spans)
	}
}

func TestTracerRecordsError(t *testing.T) {
	tracer := &recordingTracer{}
	boom := errors.New("boom")
	db := pg.New(&errDriver{err: boom}).WithTracer(tracer)
	_, _ = db.Exec(context.Background(), "INSERT ...")
	if len(tracer.spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tracer.spans))
	}
	if !errors.Is(tracer.spans[0].err, boom) {
		t.Errorf("error not recorded on span: %v", tracer.spans[0].err)
	}
}

func TestTracerNilIsNoop(t *testing.T) {
	// Without a tracer the hot path should produce no spans and
	// no panics, exec the query as normal.
	db := pg.New(&fakeDriver{})
	if _, err := db.Exec(context.Background(), "SELECT 1"); err != nil {
		t.Errorf("Exec without tracer should work, got %v", err)
	}
}
