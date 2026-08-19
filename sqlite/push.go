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
//   - Diffs the two, over the tables schema declares — a table drops was
//     never told about belongs to somebody else and is left alone.
//   - If DryRun, returns the statements unexecuted.
//   - Otherwise applies them in a single transaction; any failure rolls
//     the whole push back.
//
// Push never creates or drops an index or a trigger. The Go schema DSL
// cannot declare either, so every one in the database is undeclared by
// construction and dropping the undeclared ones would drop all of them.
// Where a change forces a table rebuild, the indexes and triggers that
// rebuild destroys are put back — see Diff.
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

	stmts := Diff(ownedBy(current, desired), desired)
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

// ownedBy narrows a live introspection to the tables the Schema
// declares.
//
// A SQLite file is often shared: another migration tool keeps its
// bookkeeping there, an unrelated service keeps a table there, and a
// drops migration history under a name Migrator.WithTable chose is not
// the default one Introspect skips. To Diff, every one of those looks
// like a table that used to exist and no longer should, and it emits a
// DROP for it — so a push against a database drops did not create alone
// deletes someone else's data.
//
// A hard-coded list of foreign table names cannot work; the Schema is
// the only statement of ownership drops has, so a table it never names
// is left alone. The cost is that dropping a table now means writing
// the DROP into a migration rather than deleting the Go declaration and
// pushing, which is the reviewable path anyway.
func ownedBy(live, declared *Snapshot) *Snapshot {
	out := &Snapshot{
		ID:      live.ID,
		PrevID:  live.PrevID,
		Version: live.Version,
		Dialect: live.Dialect,
		Tables:  make(map[string]*TableSnapshot, len(live.Tables)),
	}
	for name, ts := range live.Tables {
		if _, ok := declared.Tables[name]; ok {
			out.Tables[name] = ts
		}
	}
	return out
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
