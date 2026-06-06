package pg

import (
	"context"
)

// Tracer is the minimal contract drops needs to emit a distributed
// trace span around every Exec / Query. It deliberately mirrors a
// subset of OpenTelemetry's trace.Tracer without importing it, so
// drops carries no external tracing dependency. Wire it up with
// db.WithTracer; pass an adapter that bridges to your real
// tracer:
//
//	type otelAdapter struct{ tracer trace.Tracer }
//
//	func (a otelAdapter) Start(ctx context.Context, name string) (context.Context, pg.Span) {
//	    ctx, span := a.tracer.Start(ctx, name)
//	    return ctx, otelSpan{span}
//	}
//
//	db := pg.New(drv).WithTracer(otelAdapter{tracer: otel.Tracer("myapp")})
//
// The Span surface mirrors the methods drops needs: SetAttribute
// for query metadata, RecordError to mark failures, End to close
// the span. Implementations are expected to be safe for
// concurrent use — drops calls them from whichever goroutine
// happens to run the query.
type Tracer interface {
	Start(ctx context.Context, name string) (context.Context, Span)
}

// Span is the per-query handle returned by Tracer.Start.
type Span interface {
	SetAttribute(key string, value any)
	RecordError(err error)
	End()
}

// WithTracer attaches t. Returns a shallow copy of db so global
// instances stay unaffected — mirrors the WithHook pattern.
// Pass nil to clear.
func (db *DB) WithTracer(t Tracer) *DB {
	cp := *db
	cp.tracer = t
	return &cp
}

// Tracer returns the configured tracer, or nil.
func (db *DB) Tracer() Tracer { return db.tracer }

// startSpan opens a span if a tracer is configured; otherwise
// returns a no-op span so callers don't have to nil-check.
func (db *DB) startSpan(ctx context.Context, name string) (context.Context, Span) {
	if db.tracer == nil {
		return ctx, noopSpan{}
	}
	return db.tracer.Start(ctx, name)
}

// noopSpan is the inert implementation used when no tracer is
// configured. Every method is a no-op; the value is zero-sized so
// returning it from the hot path is free.
type noopSpan struct{}

func (noopSpan) SetAttribute(string, any) {}
func (noopSpan) RecordError(error)        {}
func (noopSpan) End()                     {}

// attribute keys used by drops spans — kept as named constants so
// downstream alerts / dashboards can match them reliably.
const (
	AttrStatement = "db.statement"
	AttrArgsCount = "db.args.count"
	AttrSystem    = "db.system"
	AttrOperation = "db.operation"
	AttrSystemPG  = "postgresql"
)
