package sqlite

import (
	"context"
	"errors"
	"fmt"
)

// PushOptions tunes how Push applies schema changes.
type PushOptions struct {
	// DryRun returns the statements that would be applied without
	// executing them — useful for previewing changes in CI.
	DryRun bool
}

// PushResult is the outcome of a Push call.
type PushResult struct {
	// Statements is the ordered SQL diff between the live database and
	// the supplied Go schema.
	Statements []string
	// Applied is true when the statements were executed (false for
	// DryRun or an empty diff).
	Applied bool
}

// ErrSchemaRequired is returned by Push when schema is nil.
var ErrSchemaRequired = errors.New("drops/sqlite: Push requires a non-nil schema")

// Push introspects the live database, diffs it against the supplied Go
// schema, and applies the changes — the drops equivalent of drizzle-kit
// `push`.
//
//   - Reads the current state via Introspect.
//   - Builds the target snapshot from schema.
//   - Diffs the two.
//   - If DryRun, returns the statements unexecuted.
//   - Otherwise applies them in a single transaction; any failure rolls
//     the whole push back.
//
// Push is convenient for development but skips migration history; prefer
// the Migrator for production, so changes are reviewable and reproducible.
func Push(ctx context.Context, db *DB, schema *Schema, opts ...PushOptions) (*PushResult, error) {
	if schema == nil {
		return nil, ErrSchemaRequired
	}
	var opt PushOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	current, err := Introspect(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("drops/sqlite: introspect: %w", err)
	}
	desired := BuildSnapshot(schema)

	stmts := Diff(current, desired)
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

// excerptSQL trims a statement to a short single-line form for error
// messages.
func excerptSQL(s string) string {
	const max = 80
	out := s
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}
