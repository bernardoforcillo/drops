package sqlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PushOptions tunes how Push applies schema changes.
type PushOptions struct {
	// DryRun returns the statements that would be applied without
	// executing them — useful for previewing changes in CI.
	//
	// A dry run does not refuse. It reports what it found in
	// PushResult.Destructive and leaves the decision to the caller,
	// because a preview that will not show you the plan you would have
	// to permit is no preview at all. The refusal happens on the run
	// that would have written.
	DryRun bool

	// AllowDestructive applies the changes Push otherwise refuses —
	// the ones that lose a column, convert a value, drop an index or
	// leave a trigger broken. It is off by default; see "A change a
	// rebuild cannot carry the data through" in Push's doc comment.
	//
	// It is the same permission `drops push --allow-destructive`
	// carries on PostgreSQL, and it means the same thing: not that the
	// change is safe, but that somebody has looked at what it destroys
	// and said yes. It does not answer a rename question — see
	// PushOptions.Renames — because whether a column is being renamed
	// and whether it may be destroyed are two different questions, and
	// one answer must not stand in for the other.
	AllowDestructive bool

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

	// Destructive is what this push destroys — read out of the diff,
	// because none of it can be read out of Statements. Non-empty only
	// on a DryRun or under AllowDestructive: otherwise Push refuses
	// and these come back inside a *DestructiveChangeError instead.
	Destructive []DestructiveChange
}

// ErrSchemaRequired is returned by Push when schema is nil.
var ErrSchemaRequired = errors.New("drops/sqlite: Push requires a non-nil schema")

// DestructiveChangeError is Push declining to destroy something
// nobody said it could. Nothing was applied.
//
// It carries the findings rather than a summary because a refusal that
// says only "this push is destructive" has told the operator nothing
// they can act on, and the next thing they do is set AllowDestructive
// blind — which is how the option stops meaning anything.
type DestructiveChangeError struct {
	// Changes is everything the push would have destroyed, in the
	// order the diff found it.
	Changes []DestructiveChange
}

func (e *DestructiveChangeError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "drops/sqlite: refusing a push that destroys %d thing(s), and applying nothing:", len(e.Changes))
	for _, c := range e.Changes {
		b.WriteString("\n  [" + c.Rule + "] " + c.Message)
		if c.Suggestion != "" {
			b.WriteString("\n      " + c.Suggestion)
		}
	}
	b.WriteString("\nNone of this is visible in the statements the push would run: on SQLite a schema change is a\n" +
		"table rebuild, and a rebuild that loses a column is spelled exactly like one that does not.\n" +
		"Set PushOptions.AllowDestructive once you have read the list.")
	return b.String()
}

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
//   - Reads the same diff for what it destroys, and refuses unless
//     AllowDestructive says otherwise. See "A change a rebuild cannot
//     carry the data through" below.
//   - If DryRun, returns the statements unexecuted, with what it found.
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
// # A change a rebuild cannot carry the data through
//
// Answering the rename question settles what a change means. It does
// not settle whether the change may run, and on this dialect that
// second question has nowhere else to be asked.
//
// On PostgreSQL a destructive change is a destructive statement, so
// `drops push` can read the plan it is about to run, classify the DDL
// and stop on a DROP COLUMN. Here the plan is a CREATE, an INSERT ...
// SELECT, a DROP TABLE and a RENAME — the same four statements whether
// the rebuild widens the table or loses half of it. The column that is
// going is simply absent from the INSERT's column list. There is no
// statement to classify, so a guard downstream of the SQL cannot exist.
//
// What can is a guard on the diff, which is where the fact lives: this
// column is in the previous snapshot and not in the next one. Push asks
// DestructiveChanges for it and refuses unless AllowDestructive. The
// rules cover what a rebuild costs, which is more than a dropped
// column:
//
//   - drop-column — the column is in the database and not in the
//     schema, so the copy leaves it out and its data goes.
//   - alter-column-type — the column goes from TEXT affinity to a
//     numeric one, and the copy converts as it goes: '007' arrives as
//     the integer 7.
//   - alter-column-set-not-null — the column gains NOT NULL and the
//     rows still hold NULLs, which the copy will not accept.
//   - add-unique-constraint — the column or the column list gains
//     UNIQUE and the rows already hold a value twice.
//   - add-check-constraint — the table gains a CHECK and a row already
//     breaks it.
//
// The last three cost no data: SQLite refuses the copy and the whole
// transaction rolls back. What they buy is a refusal that names the
// constraint and counts the rows in the way, instead of a constraint
// message from halfway through a rebuild. All three are put to the
// data before they are reported, so a tightening the rows already
// satisfy is not stopped for nothing — see confirmAgainstTheRows.
//
//   - rebuild-drops-index and rebuild-stale-trigger — the index that
//     keys a departing column, which the rebuild does not put back,
//     and the trigger that names one, which it puts back broken. Diff
//     writes both into the migration as comments and GenerateMigration
//     surfaces them through AnalyzeMigration; until now Push, which
//     prints no migration for anyone to read, did not.
//
// The refusal returns *DestructiveChangeError naming every finding,
// before any statement runs.
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

	destructive, err := confirmAgainstTheRows(ctx, db,
		DestructiveChanges(owned, desired, DiffOptions{Renames: renames}),
		owned, desired, renames)
	if err != nil {
		return nil, err
	}
	if opt.DryRun {
		return &PushResult{Statements: stmts, Applied: false, Destructive: destructive}, nil
	}
	if len(destructive) > 0 && !opt.AllowDestructive {
		return nil, &DestructiveChangeError{Changes: destructive}
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
	return &PushResult{Statements: stmts, Applied: true, Destructive: destructive}, nil
}

// confirmAgainstTheRows puts the three findings that are not settled by
// the schemas alone to the database, and keeps only the ones the rows
// bear out.
//
// Every other rule is settled by the two schemas: a column either is in
// both or is not, a declared type either changes affinity or does not.
// These three are half a fact each. "Gains NOT NULL", "gains UNIQUE",
// "gains a CHECK" are facts about the schemas; "and there are NULLs to
// carry", "and a value is already held twice", "and a row already
// breaks it" are facts about the rows, and only the second halves cost
// anything.
//
// Refusing without asking would be the more conservative choice and the
// worse one. Tightening a column that was never actually null, or
// declaring one unique that has held distinct values all along, is an
// ordinary, frequent change, and a guard that demands AllowDestructive
// for it teaches the reflex of setting AllowDestructive — after which
// the option is no longer protecting the drop-column case it exists
// for.
//
// A probe that cannot be answered is never an acquittal, but the two
// kinds of failure are not the same. The NOT NULL probe is a lookup of
// one column that both snapshots agree exists, so a query that will not
// run there is a fault rather than an awkward schema, and it comes back
// as an error. The other two carry a column list and an expression the
// schema wrote, and an expression SQLite will not evaluate outside a
// CHECK is an ordinary thing to be handed — so the finding is kept and
// says it is unconfirmed. Discarding it would mean the failure this
// guard exists to prevent happening anyway, silently.
func confirmAgainstTheRows(ctx context.Context, db *DB, changes []DestructiveChange, live, desired *Snapshot, renames []Rename) ([]DestructiveChange, error) {
	out := make([]DestructiveChange, 0, len(changes))
	for _, c := range changes {
		switch c.Rule {
		case "alter-column-set-not-null":
			table, column := liveNames(renames, c.Table, c.Object)
			n, err := countRows(ctx, db,
				"SELECT count(*) FROM "+quoteIdent(table)+" WHERE "+quoteIdent(column)+" IS NULL")
			if err != nil {
				return nil, fmt.Errorf("drops/sqlite: counting NULLs in %s.%s: %w", table, column, err)
			}
			if n == 0 {
				continue
			}
			c.Message = fmt.Sprintf("column %s of %s gains NOT NULL and %d row(s) hold NULL there; "+
				"the rebuild's INSERT ... SELECT carries those NULLs into a column that rejects them, so the push fails partway rather than converging.",
				quoteIdent(c.Object), quoteIdent(c.Table), n)
		case "add-unique-constraint":
			source, columns := probeSource(live, desired, renames, c.Table), uniqueColumns(desired, c.Table, c.Object)
			// Every column IS NOT NULL: SQLite counts NULLs as
			// distinct from one another in a unique index, so a group
			// of them is not a duplicate and grouping them would
			// refuse a push nothing is wrong with.
			var conds, keys []string
			for _, col := range columns {
				conds = append(conds, quoteIdent(col)+" IS NOT NULL")
				keys = append(keys, quoteIdent(col))
			}
			n, err := countRows(ctx, db, "SELECT count(*) FROM (SELECT 1 FROM "+source+
				" WHERE "+strings.Join(conds, " AND ")+" GROUP BY "+strings.Join(keys, ", ")+" HAVING count(*) > 1)")
			if err != nil {
				c.Message += " The rows could not be checked for duplicates (" + err.Error() + "), so this is reported unconfirmed."
				break
			}
			if n == 0 {
				continue
			}
			c.Message = fmt.Sprintf("UNIQUE %s on %s is new over %s, and %d group(s) of rows already share a value there; "+
				"the rebuild's INSERT ... SELECT carries them into a constraint that rejects them, so the push fails partway rather than converging.",
				quoteIdent(c.Object), quoteIdent(c.Table), strings.Join(quoteIdentList(columns), ", "), n)
		case "add-check-constraint":
			source, expr := probeSource(live, desired, renames, c.Table), checkExpression(desired, c.Table, c.Object)
			// NOT (expr) rather than "expr is not true": SQLite fails
			// a CHECK only on a value of false, so a row the
			// expression is NULL for passes, and NOT (NULL) is NULL,
			// which the WHERE also drops. The two agree by
			// construction.
			n, err := countRows(ctx, db, "SELECT count(*) FROM "+source+" WHERE NOT ("+expr+")")
			if err != nil {
				c.Message += " The rows could not be checked against it (" + err.Error() + "), so this is reported unconfirmed."
				break
			}
			if n == 0 {
				continue
			}
			c.Message = fmt.Sprintf("CHECK %s on %s is new and %d row(s) already break it; "+
				"the rebuild's INSERT ... SELECT carries them into a constraint that rejects them, so the push fails partway rather than converging.",
				quoteIdent(c.Object), quoteIdent(c.Table), n)
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// uniqueColumns returns the columns a new UNIQUE spans, under the names
// the new schema gives them. A single-column one is named by its
// column and renders inline; a multi-column one is named by the
// constraint.
func uniqueColumns(desired *Snapshot, table, object string) []string {
	if t, ok := desired.Tables[table]; ok {
		if u, ok := t.UniqueConstraints[object]; ok {
			return u.Columns
		}
	}
	return []string{object}
}

// checkExpression returns the SQL inside a new CHECK.
func checkExpression(desired *Snapshot, table, object string) string {
	if t, ok := desired.Tables[table]; ok {
		if c, ok := t.CheckConstraints[object]; ok {
			return c.Value
		}
	}
	return "1"
}

// probeSource renders what the row probes select from: the live table,
// presented under the names the new schema uses.
//
// Querying the live table directly is wrong twice over, and neither
// mistake announces itself. A column this push renames is still there
// under its old name, and one it adds is not there at all — and SQLite
// re-reads a double-quoted identifier matching no column as a string
// literal rather than failing. A GROUP BY over such a "column" puts
// every row in one group and reports duplication that is not there; a
// CHECK over one compares a constant and reports none that is. The
// second is the dangerous direction: a silent acquittal, and the push
// then dies on the constraint the probe was asked about.
//
// So the probes select from a projection that supplies both — the
// renamed columns under their new names, and the added ones at the
// value the rebuild's INSERT ... SELECT will leave them at, which is
// their default or NULL.
func probeSource(live, desired *Snapshot, renames []Rename, table string) string {
	// The column argument is empty because only the table's own name
	// is being unwound here; liveNames maps the two independently.
	liveName, _ := liveNames(renames, table, "")
	liveT, ok := live.Tables[liveName]
	if !ok {
		return quoteIdent(liveName)
	}
	curT, ok := desired.Tables[table]
	if !ok {
		return quoteIdent(liveName)
	}
	var extras []string
	renamed := map[string]bool{}
	for _, r := range renames {
		if r.Kind != RenameColumn || r.Table != table {
			continue
		}
		if _, clash := liveT.Columns[r.To]; clash {
			continue
		}
		if _, from := liveT.Columns[r.From]; !from {
			continue
		}
		renamed[r.To] = true
		extras = append(extras, quoteIdent(r.From)+" AS "+quoteIdent(r.To))
	}
	for _, k := range sortedMapKeys(curT.Columns) {
		if _, live := liveT.Columns[k]; live || renamed[k] {
			continue
		}
		value := "NULL"
		if d := curT.Columns[k].Default; d != nil && *d != "" {
			value = *d
		}
		extras = append(extras, value+" AS "+quoteIdent(k))
	}
	if len(extras) == 0 {
		return quoteIdent(liveName)
	}
	return "(SELECT *, " + strings.Join(extras, ", ") + " FROM " + quoteIdent(liveName) + ")"
}

// countRows runs a one-row, one-column count query.
func countRows(ctx context.Context, db *DB, query string) (int64, error) {
	rows, err := db.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

// liveNames maps a table and column named as the diff names them —
// after the renames — back to the names the database still has.
//
// Table renames run before column renames, so a column rename states
// its table by the table's new name; unwinding therefore goes the other
// way, column first and then table.
func liveNames(renames []Rename, table, column string) (string, string) {
	liveTable, liveColumn := table, column
	for _, r := range renames {
		if r.Kind == RenameColumn && r.Table == table && r.To == column {
			liveColumn = r.From
		}
	}
	for _, r := range renames {
		if r.Kind == RenameTable && r.To == table {
			liveTable = r.From
		}
	}
	return liveTable, liveColumn
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
