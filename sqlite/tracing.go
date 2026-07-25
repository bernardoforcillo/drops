package sqlite

import "context"

// Tracer is the minimal contract drops needs to emit a span around every
// Exec / Query. It mirrors a subset of OpenTelemetry's trace.Tracer
// without importing it, so drops carries no external tracing dependency.
// Wire it up with db.WithTracer, passing an adapter that bridges to your
// real tracer. Implementations must be safe for concurrent use.
type Tracer interface {
	Start(ctx context.Context, name string) (context.Context, Span)
}

// Span is the per-query handle returned by Tracer.Start.
type Span interface {
	SetAttribute(key string, value any)
	RecordError(err error)
	End()
}

// WithTracer attaches t, returning a shallow copy of db (mirrors
// WithHook). Pass nil to clear.
func (db *DB) WithTracer(t Tracer) *DB {
	cp := *db
	cp.tracer = t
	return &cp
}

// Tracer returns the configured tracer, or nil.
func (db *DB) Tracer() Tracer { return db.tracer }

// startSpan opens a span when a tracer is configured, otherwise returns
// a no-op span so callers never nil-check.
func (db *DB) startSpan(ctx context.Context, name string) (context.Context, Span) {
	if db.tracer == nil {
		return ctx, noopSpan{}
	}
	return db.tracer.Start(ctx, name)
}

// annotateSpan sets the standard drops attributes on span. Cheap and
// safe against the no-op span (its setters do nothing).
func (db *DB) annotateSpan(span Span, op, sql string, args []any) {
	span.SetAttribute(AttrSystem, AttrSystemSQLite)
	span.SetAttribute(AttrOperation, op)
	span.SetAttribute(AttrStatement, sql)
	span.SetAttribute(AttrArgsCount, len(args))
}

// noopSpan is the inert implementation used when no tracer is set. Every
// method is a no-op; the value is zero-sized so returning it from the
// hot path is free.
type noopSpan struct{}

func (noopSpan) SetAttribute(string, any) {}
func (noopSpan) RecordError(error)        {}
func (noopSpan) End()                     {}

// Attribute keys used by drops spans — named constants so downstream
// dashboards can match them reliably.
const (
	AttrStatement    = "db.statement"
	AttrArgsCount    = "db.args.count"
	AttrSystem       = "db.system"
	AttrOperation    = "db.operation"
	AttrSystemSQLite = "sqlite"
)
