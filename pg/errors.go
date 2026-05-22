package pg

import "errors"

// Package-level sentinel errors. Callers may check them with errors.Is
// to branch on a specific failure mode without inspecting the message.
//
// The decorating wrappers used by builders use fmt.Errorf("...: %w", err)
// so the chain stays intact when extra context is added.
var (
	// ErrReturningRequired is returned by *Builder.All / *Builder.One on
	// INSERT/UPDATE/DELETE statements that were not paired with a
	// Returning(...) clause.
	ErrReturningRequired = errors.New("drops/pg: builder.All/One requires Returning(...)")

	// ErrNoRowsToInsert is returned by InsertBuilder.Exec when no row has
	// been added via Row / Rows.
	ErrNoRowsToInsert = errors.New("drops/pg: INSERT with no rows")

	// ErrNoUpdateAssignments is returned by UpdateBuilder.Exec when no
	// Set assignment has been added.
	ErrNoUpdateAssignments = errors.New("drops/pg: UPDATE with no Set assignments")

	// ErrSchemaRequired is returned by Push when schema is nil.
	ErrSchemaRequired = errors.New("drops/pg: Schema is required")

	// ErrInvalidIdentifier is returned when a SQL identifier (table,
	// column, schema) contains characters that would break out of
	// double-quoted identifier safety (NUL, embedded double quotes).
	ErrInvalidIdentifier = errors.New("drops/pg: invalid SQL identifier")
)
