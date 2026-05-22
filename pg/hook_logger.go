package pg

import "github.com/bernardoforcillo/drops"

// LoggerFunc / LoggerOptions / LoggerHook are aliases for the
// dialect-neutral versions in the root drops package. They are kept
// here so existing pg-only call sites compile unchanged.
//
// New code should prefer drops.LoggerHook + drops.LoggerOptions
// directly — the same hook works against pg.DB, clickhouse.DB and
// qdrant.Client without modification.
type (
	LoggerFunc    = drops.LoggerFunc
	LoggerOptions = drops.LoggerOptions
)

// LoggerHook re-exports drops.LoggerHook. See its documentation for
// behaviour and tuning options.
func LoggerHook(log LoggerFunc, opts ...LoggerOptions) drops.Hook {
	return drops.LoggerHook(log, opts...)
}
