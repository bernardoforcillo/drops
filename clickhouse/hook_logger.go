package clickhouse

import "github.com/bernardoforcillo/drops"

// LoggerFunc / LoggerOptions / LoggerHook re-export the dialect-neutral
// helpers from the root drops package. A single Hook function works
// against pg.DB, clickhouse.DB and qdrant.Client unchanged.
type (
	LoggerFunc    = drops.LoggerFunc
	LoggerOptions = drops.LoggerOptions
)

// LoggerHook re-exports drops.LoggerHook. See its documentation for
// behaviour and tuning options.
func LoggerHook(log LoggerFunc, opts ...LoggerOptions) drops.Hook {
	return drops.LoggerHook(log, opts...)
}
