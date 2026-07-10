package otel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/otel"
)

// --- fakes ------------------------------------------------------------

type fakeSpan struct {
	name    string
	started time.Time
	ended   time.Time
	attrs   []otel.Attr
	err     error
}

func (s *fakeSpan) SetAttributes(a ...otel.Attr) { s.attrs = append(s.attrs, a...) }
func (s *fakeSpan) RecordError(err error)         { s.err = err }
func (s *fakeSpan) End(t time.Time)               { s.ended = t }

type fakeTracer struct{ spans []*fakeSpan }

func (t *fakeTracer) Start(ctx context.Context, name string, start time.Time) (context.Context, otel.Span) {
	s := &fakeSpan{name: name, started: start}
	t.spans = append(t.spans, s)
	return ctx, s
}

type record struct {
	value float64
	attrs []otel.Attr
}

type fakeCounter struct{ adds []record }

func (c *fakeCounter) Add(_ context.Context, incr int64, attrs ...otel.Attr) {
	c.adds = append(c.adds, record{float64(incr), attrs})
}

type fakeHistogram struct{ recs []record }

func (h *fakeHistogram) Record(_ context.Context, v float64, attrs ...otel.Attr) {
	h.recs = append(h.recs, record{v, attrs})
}

type fakeMeter struct {
	counters   map[string]*fakeCounter
	histograms map[string]*fakeHistogram
}

func newMeter() *fakeMeter {
	return &fakeMeter{counters: map[string]*fakeCounter{}, histograms: map[string]*fakeHistogram{}}
}
func (m *fakeMeter) Int64Counter(name string) otel.Int64Counter {
	c := &fakeCounter{}
	m.counters[name] = c
	return c
}
func (m *fakeMeter) Float64Histogram(name string) otel.Float64Histogram {
	h := &fakeHistogram{}
	m.histograms[name] = h
	return h
}

func attrVal(attrs []otel.Attr, key string) (any, bool) {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return nil, false
}

// --- tests ------------------------------------------------------------

func TestMetricsRecorded(t *testing.T) {
	m := newMeter()
	inst := otel.New(otel.Config{Meter: m, System: "postgresql"})
	hook := inst.Hook()

	hook(context.Background(), drops.QueryEvent{Kind: "query", Duration: 5 * time.Millisecond})

	calls := m.counters[otel.MetricCalls]
	if len(calls.adds) != 1 || calls.adds[0].value != 1 {
		t.Fatalf("calls counter: %+v", calls.adds)
	}
	if sys, _ := attrVal(calls.adds[0].attrs, otel.AttrSystem); sys != "postgresql" {
		t.Errorf("system attr: %v", sys)
	}
	if op, _ := attrVal(calls.adds[0].attrs, otel.AttrOperation); op != "query" {
		t.Errorf("operation attr: %v", op)
	}
	hist := m.histograms[otel.MetricDuration]
	if len(hist.recs) != 1 || hist.recs[0].value != 0.005 {
		t.Errorf("latency histogram: %+v", hist.recs)
	}
	// No error → error counter stays empty.
	if len(m.counters[otel.MetricErrors].adds) != 0 {
		t.Errorf("error counter should be empty")
	}
}

func TestErrorCounterOnFailure(t *testing.T) {
	m := newMeter()
	hook := otel.New(otel.Config{Meter: m}).Hook()

	hook(context.Background(), drops.QueryEvent{Kind: "exec", Err: errors.New("boom")})

	if len(m.counters[otel.MetricErrors].adds) != 1 {
		t.Errorf("error counter not incremented")
	}
	if errAttr, _ := attrVal(m.counters[otel.MetricCalls].adds[0].attrs, otel.AttrError); errAttr != true {
		t.Errorf("error attr should be true, got %v", errAttr)
	}
}

func TestSpanRetroactiveTimestamps(t *testing.T) {
	tr := &fakeTracer{}
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	inst := otel.New(otel.Config{
		Tracer: tr,
		System: "sqlite",
		Now:    func() time.Time { return fixed },
	})
	inst.Hook()(context.Background(), drops.QueryEvent{
		Kind:     "query",
		Duration: 100 * time.Millisecond,
		Args:     []any{1, 2},
	})

	if len(tr.spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(tr.spans))
	}
	s := tr.spans[0]
	if s.name != "sqlite query" {
		t.Errorf("span name: %q", s.name)
	}
	if !s.ended.Equal(fixed) {
		t.Errorf("span end: %v want %v", s.ended, fixed)
	}
	if want := fixed.Add(-100 * time.Millisecond); !s.started.Equal(want) {
		t.Errorf("span start: %v want %v", s.started, want)
	}
	if n, _ := attrVal(s.attrs, otel.AttrArgsCount); n != int64(2) {
		t.Errorf("args count attr: %v", n)
	}
}

func TestStatementRecordingGated(t *testing.T) {
	tr := &fakeTracer{}
	off := otel.New(otel.Config{Tracer: tr})
	off.Hook()(context.Background(), drops.QueryEvent{Kind: "query", SQL: "SELECT 1"})
	if _, ok := attrVal(tr.spans[0].attrs, otel.AttrStatement); ok {
		t.Error("statement should not be recorded by default")
	}

	tr2 := &fakeTracer{}
	on := otel.New(otel.Config{Tracer: tr2, RecordStatement: true})
	on.Hook()(context.Background(), drops.QueryEvent{Kind: "query", SQL: "SELECT 1"})
	if v, ok := attrVal(tr2.spans[0].attrs, otel.AttrStatement); !ok || v != "SELECT 1" {
		t.Errorf("statement should be recorded when enabled: %v %v", v, ok)
	}
}

func TestSpanRecordsError(t *testing.T) {
	tr := &fakeTracer{}
	sentinel := errors.New("nope")
	otel.New(otel.Config{Tracer: tr}).Hook()(
		context.Background(),
		drops.QueryEvent{Kind: "exec", Err: sentinel},
	)
	if !errors.Is(tr.spans[0].err, sentinel) {
		t.Errorf("span error not recorded: %v", tr.spans[0].err)
	}
}

func TestNoopWhenUnconfigured(t *testing.T) {
	if otel.New(otel.Config{}).Hook() != nil {
		t.Error("Hook() should be nil with neither tracer nor meter")
	}
}

func TestCustomSpanName(t *testing.T) {
	tr := &fakeTracer{}
	inst := otel.New(otel.Config{
		Tracer:   tr,
		SpanName: func(e drops.QueryEvent) string { return "custom:" + e.Kind },
	})
	inst.Hook()(context.Background(), drops.QueryEvent{Kind: "ping"})
	if tr.spans[0].name != "custom:ping" {
		t.Errorf("custom span name: %q", tr.spans[0].name)
	}
}
