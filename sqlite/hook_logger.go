package sqlite

import "github.com/bernardoforcillo/drops"

// LoggerFunc / LoggerOptions / LoggerHook are aliases for the
// dialect-neutral versions in the root drops package, kept here so a
// codebase can install a logging hook symmetrically across dialects
// (sqlite.LoggerHook, pg.LoggerHook, clickhouse.LoggerHook).
//
// New code may prefer drops.LoggerHook + drops.LoggerOptions directly —
// the same hook works against sqlite.DB, pg.DB, clickhouse.DB and
// qdrant.Client without modification.
type (
	LoggerFunc    = drops.LoggerFunc
	LoggerOptions = drops.LoggerOptions
)

// LoggerHook re-exports drops.LoggerHook. See its documentation for
// behaviour and tuning options. Attach it with db.WithHook.
func LoggerHook(log LoggerFunc, opts ...LoggerOptions) drops.Hook {
	return drops.LoggerHook(log, opts...)
}
