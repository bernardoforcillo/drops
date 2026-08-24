package otel_test

import (
	"context"
	"errors"
	"strings"
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
func (s *fakeSpan) RecordError(err error)        { s.err = err }
func (s *fakeSpan) End(t time.Time)              { s.ended = t }

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

// A panicking adapter — a nil OTel span, an attribute mapper that
// does not recognise a value — must still leave the span closed.
// drops.CallHook recovers the panic on the way out, so a span left
// open here is one the exporter never emits.
type panickingSpan struct {
	fakeSpan
	on string
}

func (s *panickingSpan) SetAttributes(a ...otel.Attr) {
	if s.on == "attrs" {
		panic("adapter blew up mapping an attribute")
	}
	s.fakeSpan.SetAttributes(a...)
}

func (s *panickingSpan) RecordError(err error) { panic("adapter blew up recording an error") }

type panickingTracer struct {
	on   string
	span *panickingSpan
}

func (t *panickingTracer) Start(ctx context.Context, name string, start time.Time) (context.Context, otel.Span) {
	t.span = &panickingSpan{fakeSpan: fakeSpan{name: name, started: start}, on: t.on}
	return ctx, t.span
}

func TestSpanEndedEvenWhenTheAdapterPanics(t *testing.T) {
	fixed := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		on    string
		event drops.QueryEvent
	}{
		{"SetAttributes", "attrs", drops.QueryEvent{Kind: "query"}},
		{"RecordError", "err", drops.QueryEvent{Kind: "query", Err: errors.New("boom")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &panickingTracer{on: tc.on}
			hook := otel.New(otel.Config{Tracer: tr, Now: func() time.Time { return fixed }}).Hook()
			drops.CallHook(hook, context.Background(), tc.event)

			if tr.span == nil {
				t.Fatal("no span started")
			}
			if tr.span.ended.IsZero() {
				t.Error("span was never ended, so the exporter keeps it forever")
			}
		})
	}
}

// Arguments are user data. The span may say how many there were and
// nothing more, whatever the statement setting is.
func TestArgumentValuesNeverReachTheSpan(t *testing.T) {
	tr := &fakeTracer{}
	otel.New(otel.Config{Tracer: tr, RecordStatement: true}).Hook()(
		context.Background(),
		drops.QueryEvent{
			Kind: "query",
			SQL:  "SELECT id FROM users WHERE ssn = $1",
			Args: []any{"078-05-1120"},
		},
	)
	for _, a := range tr.spans[0].attrs {
		if s, ok := a.Value.(string); ok && strings.Contains(s, "078-05-1120") {
			t.Errorf("attribute %q leaked an argument value: %v", a.Key, a.Value)
		}
	}
	if n, _ := attrVal(tr.spans[0].attrs, otel.AttrArgsCount); n != int64(1) {
		t.Errorf("args count attr = %v, want 1", n)
	}
}

// The split is the reason this package can tell an operator whether to
// add connections or fix the query, so it has to reach both the
// histograms and the span.
func TestSplitMetricsRecordedWhenTheDriverMeasuredIt(t *testing.T) {
	m := newMeter()
	tr := &fakeTracer{}
	inst := otel.New(otel.Config{Meter: m, Tracer: tr, System: "postgresql"})

	inst.Hook()(context.Background(), drops.QueryEvent{
		Kind:         "query",
		Duration:     100 * time.Millisecond,
		WaitDuration: 90 * time.Millisecond,
		WaitKnown:    true,
	})

	wait := m.histograms[otel.MetricWaitTime]
	if len(wait.recs) != 1 || wait.recs[0].value != 0.09 {
		t.Fatalf("wait histogram: %+v", wait.recs)
	}
	query := m.histograms[otel.MetricQueryTime]
	if len(query.recs) != 1 || query.recs[0].value != 0.01 {
		t.Fatalf("query histogram: %+v", query.recs)
	}
	// The total keeps meaning the total; splitting it must not
	// silently redefine an instrument dashboards already read.
	total := m.histograms[otel.MetricDuration]
	if len(total.recs) != 1 || total.recs[0].value != 0.1 {
		t.Fatalf("duration histogram: %+v", total.recs)
	}

	if len(tr.spans) != 1 {
		t.Fatalf("expected one span, got %d", len(tr.spans))
	}
	if v, ok := attrVal(tr.spans[0].attrs, otel.AttrWaitTime); !ok || v != 0.09 {
		t.Errorf("span wait attr: %v ok=%v", v, ok)
	}
	if v, ok := attrVal(tr.spans[0].attrs, otel.AttrQueryTime); !ok || v != 0.01 {
		t.Errorf("span query attr: %v ok=%v", v, ok)
	}
}

// A zero recorded for every unmeasured statement would make an
// exhausted pool and an uninstrumented driver produce the same
// histogram, which is the one failure mode worth preventing.
func TestSplitMetricsAbsentWhenTheDriverDidNot(t *testing.T) {
	m := newMeter()
	tr := &fakeTracer{}
	inst := otel.New(otel.Config{Meter: m, Tracer: tr, System: "postgresql"})

	inst.Hook()(context.Background(), drops.QueryEvent{
		Kind: "query", Duration: 100 * time.Millisecond,
	})

	if recs := m.histograms[otel.MetricWaitTime].recs; len(recs) != 0 {
		t.Errorf("wait histogram should be empty, got %+v", recs)
	}
	if recs := m.histograms[otel.MetricQueryTime].recs; len(recs) != 0 {
		t.Errorf("query histogram should be empty, got %+v", recs)
	}
	if len(m.histograms[otel.MetricDuration].recs) != 1 {
		t.Error("the total is always recorded")
	}
	if _, ok := attrVal(tr.spans[0].attrs, otel.AttrWaitTime); ok {
		t.Error("span carries a wait attribute nobody measured")
	}
}
