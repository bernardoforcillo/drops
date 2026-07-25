// Package otel turns the drops.Hook / drops.QueryEvent stream into
// OpenTelemetry spans and metrics — without importing OpenTelemetry.
//
// Every drops DB and cache backend accepts a drops.Hook that fires after
// each operation. This package builds a Hook that, for each event, opens
// a span sized to the operation and records the RED metrics (rate,
// errors, duration). It talks to small OTel-shaped interfaces (Tracer,
// Span, Meter, Int64Counter, Float64Histogram) rather than the real SDK,
// so drops carries no external dependency — exactly as drops/pg's Tracer
// does. Bridging to the real go.opentelemetry.io/otel SDK is a handful of
// adapter lines (see the package example).
//
// Because it is driven purely by the Hook, one Instrumentation works for
// every dialect (pg, sqlite, clickhouse) and every cache backend
// (memory, redis, memcached, tiered):
//
//	inst := otel.New(otel.Config{Tracer: tr, Meter: mt, System: "postgresql"})
//	db := pg.New(drv).WithHook(inst.Hook())
//
// The hook fires after the operation completes, so spans are created
// retroactively with explicit start/end timestamps (start = observed end
// − event Duration). For live, actively-wrapped spans on PostgreSQL,
// combine this with pg.WithTracer; the two compose.
package otel

import (
	"context"
	"time"

	"github.com/bernardoforcillo/drops"
)

// Attr is a single span/metric attribute. Value is typically a string,
// int64, float64 or bool; the adapter maps it to the real OTel
// attribute.KeyValue.
type Attr struct {
	Key   string
	Value any
}

// Tracer mirrors the subset of go.opentelemetry.io/otel/trace.Tracer that
// drops needs. Start opens a span named name that began at startTime;
// pass the equivalent of trace.WithTimestamp(startTime) in the adapter.
type Tracer interface {
	Start(ctx context.Context, name string, startTime time.Time) (context.Context, Span)
}

// Span is the per-operation handle returned by Tracer.Start. End closes
// the span at endTime (trace.WithTimestamp(endTime) in the adapter).
type Span interface {
	SetAttributes(attrs ...Attr)
	RecordError(err error)
	End(endTime time.Time)
}

// Meter mirrors the subset of go.opentelemetry.io/otel/metric.Meter that
// drops needs. The two instrument constructors are called once at New.
type Meter interface {
	Int64Counter(name string) Int64Counter
	Float64Histogram(name string) Float64Histogram
}

// Int64Counter mirrors metric.Int64Counter.
type Int64Counter interface {
	Add(ctx context.Context, incr int64, attrs ...Attr)
}

// Float64Histogram mirrors metric.Float64Histogram.
type Float64Histogram interface {
	Record(ctx context.Context, value float64, attrs ...Attr)
}

// Semantic-convention attribute keys used on drops spans and metrics.
// They follow the OpenTelemetry database conventions so downstream
// dashboards and alerts can match on stable keys.
const (
	AttrSystem    = "db.system"
	AttrOperation = "db.operation"
	AttrStatement = "db.statement"
	AttrArgsCount = "db.args.count"
	AttrError     = "error"
)

// Instrument names. Duration is recorded in seconds, per the OTel
// database-client conventions.
const (
	MetricDuration = "db.client.operation.duration"
	MetricCalls    = "db.client.operations"
	MetricErrors   = "db.client.errors"
)

// Config configures an Instrumentation. Tracer and Meter are each
// optional: supply a Tracer for spans, a Meter for metrics, or both.
type Config struct {
	// Tracer, if set, receives one span per operation. Nil disables
	// span emission.
	Tracer Tracer

	// Meter, if set, receives the RED metrics. Nil disables metrics.
	Meter Meter

	// System is the db.system attribute value, e.g. "postgresql",
	// "sqlite", "clickhouse", or "redis" for a cache. Optional.
	System string

	// RecordStatement attaches the rendered SQL text as the db.statement
	// span attribute. Off by default because statements can embed
	// sensitive literals in non-parameterised fragments; enable only when
	// the statement text is safe to export.
	RecordStatement bool

	// SpanName overrides the span name derived from an event. The default
	// is "<System> <Kind>" (e.g. "postgresql query"), or just the Kind
	// when System is empty.
	SpanName func(e drops.QueryEvent) string

	// Now is injectable for tests; defaults to time.Now. It is used to
	// timestamp the (retroactive) span end.
	Now func() time.Time
}

// Instrumentation holds the pre-created metric instruments and the
// configuration. Build it once and share the Hook it produces.
type Instrumentation struct {
	cfg     Config
	now     func() time.Time
	calls   Int64Counter
	errors  Int64Counter
	latency Float64Histogram
}

// New builds an Instrumentation. Metric instruments are created eagerly
// from the Meter (if any) so the hot path only records.
func New(cfg Config) *Instrumentation {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	i := &Instrumentation{cfg: cfg, now: cfg.Now}
	if cfg.Meter != nil {
		i.calls = cfg.Meter.Int64Counter(MetricCalls)
		i.errors = cfg.Meter.Int64Counter(MetricErrors)
		i.latency = cfg.Meter.Float64Histogram(MetricDuration)
	}
	return i
}

// Hook returns a drops.Hook that emits spans and metrics for every
// operation. When neither a Tracer nor a Meter is configured it returns
// nil, which drops treats as "no hook" — zero overhead.
func (i *Instrumentation) Hook() drops.Hook {
	if i.cfg.Tracer == nil && i.cfg.Meter == nil {
		return nil
	}
	return func(ctx context.Context, e drops.QueryEvent) {
		attrs := i.baseAttrs(e)
		i.record(ctx, e, attrs)
		i.span(ctx, e, attrs)
	}
}

// baseAttrs builds the attribute set shared by the span and the metrics.
func (i *Instrumentation) baseAttrs(e drops.QueryEvent) []Attr {
	attrs := make([]Attr, 0, 3)
	if i.cfg.System != "" {
		attrs = append(attrs, Attr{AttrSystem, i.cfg.System})
	}
	if e.Kind != "" {
		attrs = append(attrs, Attr{AttrOperation, e.Kind})
	}
	attrs = append(attrs, Attr{AttrError, e.Err != nil})
	return attrs
}

func (i *Instrumentation) record(ctx context.Context, e drops.QueryEvent, attrs []Attr) {
	if i.cfg.Meter == nil {
		return
	}
	if i.calls != nil {
		i.calls.Add(ctx, 1, attrs...)
	}
	if i.latency != nil {
		i.latency.Record(ctx, e.Duration.Seconds(), attrs...)
	}
	if e.Err != nil && i.errors != nil {
		i.errors.Add(ctx, 1, attrs...)
	}
}

func (i *Instrumentation) span(ctx context.Context, e drops.QueryEvent, attrs []Attr) {
	if i.cfg.Tracer == nil {
		return
	}
	end := i.now()
	start := end.Add(-e.Duration)
	_, span := i.cfg.Tracer.Start(ctx, i.spanName(e), start)

	spanAttrs := attrs
	if i.cfg.RecordStatement && e.SQL != "" {
		spanAttrs = append(append([]Attr(nil), attrs...), Attr{AttrStatement, e.SQL})
	}
	if len(e.Args) > 0 {
		spanAttrs = append(append([]Attr(nil), spanAttrs...), Attr{AttrArgsCount, int64(len(e.Args))})
	}
	span.SetAttributes(spanAttrs...)
	if e.Err != nil {
		span.RecordError(e.Err)
	}
	span.End(end)
}

func (i *Instrumentation) spanName(e drops.QueryEvent) string {
	if i.cfg.SpanName != nil {
		return i.cfg.SpanName(e)
	}
	if i.cfg.System == "" {
		return e.Kind
	}
	if e.Kind == "" {
		return i.cfg.System
	}
	return i.cfg.System + " " + e.Kind
}
