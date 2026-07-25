package otel_test

import (
	"context"
	"fmt"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/otel"
)

// Example shows the wiring shape. In real code the Tracer and Meter are
// thin adapters over the go.opentelemetry.io/otel SDK, for example:
//
//	type tracerAdapter struct{ t trace.Tracer }
//
//	func (a tracerAdapter) Start(ctx context.Context, name string, start time.Time) (context.Context, otel.Span) {
//	    ctx, s := a.t.Start(ctx, name, trace.WithTimestamp(start))
//	    return ctx, spanAdapter{s}
//	}
//
//	type spanAdapter struct{ s trace.Span }
//
//	func (a spanAdapter) SetAttributes(attrs ...otel.Attr) {
//	    kv := make([]attribute.KeyValue, len(attrs))
//	    for i, at := range attrs { kv[i] = toKV(at) }
//	    a.s.SetAttributes(kv...)
//	}
//	func (a spanAdapter) RecordError(err error) { a.s.RecordError(err) }
//	func (a spanAdapter) End(end time.Time)     { a.s.End(trace.WithTimestamp(end)) }
//
// then:
//
//	inst := otel.New(otel.Config{Tracer: tracerAdapter{tr}, Meter: meterAdapter{mt}, System: "postgresql"})
//	db := pg.New(drv).WithHook(inst.Hook())
//
// Here we use in-memory fakes so the example is self-contained.
func Example() {
	m := newMeter()
	inst := otel.New(otel.Config{Meter: m, System: "postgresql"})
	hook := inst.Hook()

	// Simulate what a drops DB emits after two operations.
	hook(context.Background(), drops.QueryEvent{Kind: "query", Duration: 2 * time.Millisecond})
	hook(context.Background(), drops.QueryEvent{Kind: "exec", Duration: 1 * time.Millisecond})

	fmt.Println("operations recorded:", len(m.counters[otel.MetricCalls].adds))
	fmt.Println("latency samples:", len(m.histograms[otel.MetricDuration].recs))
	// Output:
	// operations recorded: 2
	// latency samples: 2
}
