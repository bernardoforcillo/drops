package pg

import (
	"context"
	"fmt"
)

// PushOptions tunes how Push applies schema changes.
type PushOptions struct {
	// Schema restricts introspection to one PostgreSQL schema. Empty
	// defaults to "public".
	Schema string

	// Safe wraps every destructive or creative DDL in IF [NOT] EXISTS
	// so the apply is idempotent and safe to retry.
	Safe bool

	// DryRun returns the statements that would be applied without
	// executing them. Useful for previewing changes in CI.
	DryRun bool
}

// PushResult is the outcome of a Push call.
type PushResult struct {
	// Statements is the ordered SQL diff between the live database and
	// the supplied Go schema.
	Statements []string
	// Applied is true when the statements were executed (false for
	// DryRun, or when the diff was empty).
	Applied bool
}

// Push introspects the live database, diffs it against the supplied Go
// schema, and applies the changes — drops's equivalent of drizzle-kit's
// `push` command.
//
// Behaviour:
//   - Reads the current state of the configured schema via Introspect.
//   - Builds a target snapshot from `schema`.
//   - Diffs the two using DiffOptions{Safe: opts.Safe}.
//   - If DryRun, returns the statements unexecuted.
//   - Otherwise applies them inside a single transaction; any failure
//     rolls back the whole push.
//
// Push is convenient for development but skips migration history. For
// production use, prefer GenerateMigration + DrizzleMigrator so changes
// are reviewable and reproducible.
func Push(ctx context.Context, db *DB, schema *Schema, opts ...PushOptions) (*PushResult, error) {
	if schema == nil {
		return nil, ErrSchemaRequired
	}
	var opt PushOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	schemaName := opt.Schema
	if schemaName == "" {
		schemaName = "public"
	}

	current, err := Introspect(ctx, db, IntrospectOptions{Schemas: []string{schemaName}})
	if err != nil {
		return nil, fmt.Errorf("drops/pg: introspect: %w", err)
	}
	desired := BuildSnapshot(schema)

	stmts := Diff(current, desired, DiffOptions{Safe: opt.Safe})
	if len(stmts) == 0 {
		return &PushResult{Statements: nil, Applied: false}, nil
	}
	if opt.DryRun {
		return &PushResult{Statements: stmts, Applied: false}, nil
	}

	if err := db.InTx(ctx, func(tx *DB) error {
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return fmt.Errorf("applying %q: %w", excerptSQL(s), err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return &PushResult{Statements: stmts, Applied: true}, nil
}
