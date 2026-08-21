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

	// Renames answers the rename questions this push raises, for one
	// run. They are merged over what the schema itself declares — see
	// DeclaredRenames — with these winning.
	//
	// This is the per-call answer, the one a command line carries. The
	// durable one is (*Col[T]).RenamedFrom in the schema, and it is
	// the better answer for anything that will be pushed more than
	// once: a flag answers the database in front of you, a declaration
	// answers every database the schema reaches.
	//
	// A candidate neither of them answers is not guessed at: Push
	// returns *RenameAmbiguityError and executes nothing.
	Renames []RenameDecision
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
//   - Settles the rename questions the change raises, or refuses. See
//     "A change that could be a rename" below.
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
// # A change that could be a rename
//
// A column the database has and the schema does not, paired with one
// the schema has and the database does not, is either a rename or a
// drop-and-add, and the two differ by the whole contents of the
// column. Push cannot tell them apart any more than Diff can, so it
// asks the same question GenerateMigration asks and returns
// *RenameAmbiguityError rather than guessing — before any statement
// runs.
//
// Nowhere does this matter more than here. A dropped column on SQLite
// is not a DROP COLUMN anybody can read in the statement list: the
// rebuild copies the columns both sides name and leaves the rest out,
// so an unstated rename takes the column's data with nothing at all in
// the SQL to say so, and a guard that reads the statements for
// destruction has nothing to catch.
//
// Answer it in the schema with (*Col[T]).RenamedFrom or
// (*Table).RenamedFrom, which is durable and travels to every database
// the schema is pushed to, or for one run with PushOptions.Renames.
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

	answers := mergeDecisions(DeclaredRenames(schema), opt.Renames)
	owned := ownedBy(current, desired, renamedAwayTables(current, answers)...)
	renames, err := resolvePushRenames(owned, desired, answers)
	if err != nil {
		return nil, err
	}

	stmts := Diff(owned, desired, DiffOptions{Renames: renames})
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

// resolvePushRenames settles the rename questions this push raises, or
// refuses.
//
// It is the same reasoning GenerateMigration applies, against the same
// detector, for the same reason: a column gone from the database and
// one arrived in the schema is either a rename or a drop-and-add, the
// two differ by the whole contents of the column, and nothing in
// either side says which. Push reaching that comparison without asking
// the question was a second door into the data loss the question
// exists to close, and on SQLite the quietest of them all — the
// rebuild loses the column with no DROP COLUMN anywhere in the
// statement list.
//
// The answers come from the schema (DeclaredRenames) and from the
// call (PushOptions.Renames), the call winning; the caller merges the
// two because it needs the merged set before this point as well — see
// renamedAwayTables. There is no rename log here: a push has no
// migration directory, so the advice on the refusal points at the
// schema instead.
func resolvePushRenames(current, desired *Snapshot, answers []RenameDecision) ([]Rename, error) {
	renames, unresolved := ResolveRenames(current, desired, answers)
	if len(unresolved) > 0 {
		return nil, &RenameAmbiguityError{Candidates: unresolved, Advice: pushRenameAdvice}
	}
	if err := validateRenames(current, desired, renames); err != nil {
		return nil, err
	}
	return renames, nil
}

// pushRenameAdvice closes a push's refusal, in place of the flags and
// the rename log the generator points at — a push has neither.
//
// The schema declaration comes first because it is the answer that
// lasts: an operator answering at a terminal answers for the one
// database in front of them, and the next database the schema is
// pushed to asks again. The per-run answer is named second, and it is
// the only way to say the other thing, that the column really is being
// dropped — which is not a fact about the schema at all. Once such a
// drop has been pushed the old column is gone and the question never
// comes back, so there is nothing lasting to record.
const pushRenameAdvice = "a rename is a fact about the schema's history and belongs with the schema:\n" +
	"    sqlite.Add(Users, sqlite.Text(\"emailAddress\").RenamedFrom(\"email\"))\n" +
	"    sqlite.NewTable(\"people\").RenamedFrom(\"users\")\n" +
	"stated there it answers every database the schema is pushed to, and it goes inert once the rename has happened.\n" +
	"For one run instead, or to say the column really is being dropped, use PushOptions.Renames: a\n" +
	"RenameDecision naming the pair renames it, one naming only the object that is going declines."

// renamedAwayTables names the tables a rename answer says a declared
// table used to be called, while the rename is still ahead of this
// database.
//
// The schema does not name them any more, which is precisely the
// problem: ownedBy would file them under "somebody else's" and drop
// them from the previous side a moment before the rename would have
// named one — leaving the push to create the new table empty beside
// the old one, with the rename it was told about applied to nothing. A
// schema that says "people used to be users" has claimed users, and
// this is what says so.
//
// The claim expires. A declaration is meant to be left in the source
// until every deployment has caught up, so it outlives the rename; what
// it must not outlive is its hold on the old name. Once live carries
// the new name the rename has happened here, and whatever answers to
// the old one now is somebody else's table — which is the case ownedBy
// exists for. Keeping the hole open past that point offered that table
// to the diff as the previous life of a table the schema does declare:
// the push either asked an unanswerable question pairing its columns
// with the wrong table's, or, once told the old name really was going,
// dropped it.
func renamedAwayTables(live *Snapshot, answers []RenameDecision) []string {
	var out []string
	for _, d := range answers {
		if d.Kind != RenameTable || !d.IsRename || d.From == "" {
			continue
		}
		if live.Tables[d.To] != nil {
			continue
		}
		out = append(out, d.From)
	}
	return out
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
//
// alsoKeep names tables to keep whatever the Schema says now — see
// renamedAwayTables.
func ownedBy(live, declared *Snapshot, alsoKeep ...string) *Snapshot {
	keep := make(map[string]bool, len(alsoKeep))
	for _, n := range alsoKeep {
		keep[n] = true
	}
	out := &Snapshot{
		ID:      live.ID,
		PrevID:  live.PrevID,
		Version: live.Version,
		Dialect: live.Dialect,
		Tables:  make(map[string]*TableSnapshot, len(live.Tables)),
	}
	for name, ts := range live.Tables {
		if _, ok := declared.Tables[name]; ok || keep[ts.Name] {
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
