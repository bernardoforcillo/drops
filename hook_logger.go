package drops

import (
	"context"
	"strings"
	"time"
)

// LoggerFunc is the minimal logger contract used by LoggerHook. Most
// production logger types — log/slog's printf-style helpers, zerolog,
// zap.SugaredLogger, even fmt.Printf — satisfy it directly or via a
// one-line adapter, so the hook itself takes no dependency on any
// specific logging library.
type LoggerFunc func(format string, args ...any)

// LoggerOptions tunes LoggerHook behaviour. The zero value enables
// query logging for every event.
type LoggerOptions struct {
	// SlowQuery, if non-zero, limits logging to operations that took at
	// least this long. Errors always log, regardless of this threshold.
	SlowQuery time.Duration

	// LogArgs controls whether bound parameters are included in log
	// lines. Default false because args may contain secrets.
	LogArgs bool

	// MaxSQLLength truncates the SQL fragment to this many characters.
	// 0 means no truncation.
	MaxSQLLength int

	// Redact, if non-nil, is applied to a copy of the args slice before
	// logging. Use it to strip passwords, tokens, PII, etc. when
	// LogArgs is true. Ignored when LogArgs is false (no args are
	// logged at all).
	Redact func(args []any) []any
}

// LoggerHook returns a Hook that writes one line per operation to log.
// It works with any DB that accepts a drops.Hook — pg, clickhouse,
// qdrant — so a single function shape covers every dialect.
//
//	db := pg.New(stdlib.New(sqlDB)).WithHook(
//	    drops.LoggerHook(log.Printf,
//	        drops.LoggerOptions{SlowQuery: 100 * time.Millisecond}),
//	)
//
// Pair with ChainHooks to combine logging with metrics or tracing.
func LoggerHook(log LoggerFunc, opts ...LoggerOptions) Hook {
	var o LoggerOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	return func(_ context.Context, e QueryEvent) {
		if o.SlowQuery > 0 && e.Duration < o.SlowQuery && e.Err == nil {
			return
		}
		sql := e.SQL
		if o.MaxSQLLength > 0 && len(sql) > o.MaxSQLLength {
			sql = sql[:o.MaxSQLLength] + "…"
		}
		// Collapse multi-line SQL so a log line stays single-line.
		sql = strings.Join(strings.Fields(sql), " ")
		status := "ok"
		if e.Err != nil {
			status = "err=" + e.Err.Error()
		}
		args := e.Args
		if o.LogArgs && o.Redact != nil && len(args) > 0 {
			cp := append([]any(nil), args...)
			args = o.Redact(cp)
		}
		switch {
		case sql == "" && o.LogArgs:
			log("[drops] %s %s in %s args=%v", e.Kind, status, e.Duration, args)
		case sql == "":
			log("[drops] %s %s in %s", e.Kind, status, e.Duration)
		case o.LogArgs:
			log("[drops] %s %s in %s sql=%q args=%v", e.Kind, status, e.Duration, sql, args)
		default:
			log("[drops] %s %s in %s sql=%q", e.Kind, status, e.Duration, sql)
		}
	}
}
