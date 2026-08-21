package pg

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
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
	//
	// It is not a read-only operation. Push still asks the server to
	// respell the declared CHECK expressions and index predicates
	// before it can tell whether they changed, and that probe is DDL
	// against the real table — see Push's doc comment for the lock it
	// takes. Skipping the probe would make the preview list changes
	// that are not changes, which is the more misleading of the two.
	DryRun bool

	// DropUnmanagedIndexes lets Push drop an index that exists in the
	// database but appears in no table of the Go schema. It is off by
	// default; see Push's doc comment for why.
	DropUnmanagedIndexes bool

	// DropUnmanagedObjects lets Push drop an enum, sequence, view or
	// policy that exists in the database and appears nowhere in the Go
	// schema, and lets it switch off a row-level security the schema
	// does not declare. It is off by default for the same reason
	// DropUnmanagedIndexes is, with a sharper edge: turning RLS off is
	// not a slow rebuild, it is every row of the table becoming
	// visible to everyone the moment the push commits.
	DropUnmanagedObjects bool

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
	// DryRun, or when the diff was empty).
	Applied bool
	// Notices are the differences Push saw and did not act on. An
	// empty Statements list with a non-empty Notices list means the
	// database and the schema disagree about something Push declined
	// to change.
	Notices []SchemaNotice
}

// SchemaNotice is a difference Push can see but will not act on.
//
// A migration tool that silently skips a change is worse than one that
// says which changes it cannot make, so every such decision surfaces
// here rather than being dropped on the floor.
type SchemaNotice struct {
	// Rule is a stable identifier for the kind of notice —
	// "unmanaged-index", "unmanaged-enum", "unmanaged-sequence",
	// "unmanaged-view", "unmanaged-policy", "unmanaged-rls",
	// "unrepresentable-index", "enum-labels-reordered",
	// "check-not-normalised", "index-predicate-not-normalised",
	// "policy-expression-not-normalised", "view-not-normalised".
	Rule string
	// Table is the table the notice concerns, unqualified.
	Table string
	// Object is the index or constraint name, where one applies.
	Object string
	// Message says what was seen and what was not done about it.
	Message string
	// SQL is the statement Push withheld, empty when the notice
	// describes something with no statement behind it. Run it by hand
	// if the notice is telling you what you wanted.
	SQL string
}

// String renders the notice as "rule: message" with the withheld SQL
// appended when there is one.
func (n SchemaNotice) String() string {
	out := n.Rule + ": " + n.Message
	if n.SQL != "" {
		out += " (" + n.SQL + ")"
	}
	return out
}

// Push introspects the live database, diffs it against the supplied Go
// schema, and applies the changes — drops's equivalent of drizzle-kit's
// `push` command.
//
// Behaviour:
//   - Reads the current state of the configured schema via Introspect.
//   - Builds a target snapshot from `schema`.
//   - Settles the rename questions the change raises, or refuses,
//     before anything below touches the server. See "A change that
//     could be a rename".
//   - Asks the server to respell the declared CHECK expressions,
//     partial-index predicates, policy expressions and view bodies, so
//     the two sides of the diff are written in the same dialect of
//     PostgreSQL's own deparser. See "What the probe costs" below:
//     this happens on a DryRun too.
//   - Diffs the two using DiffOptions{Safe: opts.Safe} and the renames
//     it settled.
//   - If DryRun, returns the statements unexecuted.
//   - Otherwise applies them inside a single transaction; any failure
//     rolls back the whole push. CREATE INDEX CONCURRENTLY is the one
//     exception — PostgreSQL refuses it inside a transaction block
//     (SQLSTATE 25001) — so those statements run afterwards, once the
//     rest has committed.
//
// # What the probe costs
//
// Respelling a declared expression means handing it to the server
// under a throwaway name and reading back what the deparser makes of
// it. Each kind of expression is handed over as the kind of object it
// is: a CHECK body and an index predicate as a CHECK constraint added
// NOT VALID, a policy's USING and WITH CHECK as a policy, a view body
// as a view — the last two because a policy may hold a subquery and a
// view is one, and a CHECK constraint may not. Reusing one probe for
// all of them would have reported every such policy as unparseable.
//
// NOT VALID skips the table scan, a materialised view is probed WITH
// NO DATA, and the whole thing is rolled back, but ALTER TABLE ADD
// CONSTRAINT and CREATE POLICY still take an ACCESS EXCLUSIVE lock on
// the table, and PostgreSQL holds a lock until the transaction ends —
// not until the savepoint rolls back. So a push against a schema with
// CHECK constraints, partial indexes or policies briefly locks every
// table carrying one, all at once, and blocks readers of those tables
// for the length of the probe. It happens on a DryRun as well, because
// there is no way to preview a change to an expression without first
// learning how the server spells it.
//
// Set lock_timeout on the connection if a push must never wait behind
// a long reader. A probe that fails for an operational reason — a lock
// timeout, a deadlock, a cancelled statement — fails the whole push
// rather than being reported as an unparseable expression, so a
// change is never quietly left unapplied.
//
// # An object the schema never declared
//
// Push does not drop one. Diff does, and is right to: it compares two
// declarations, so an index missing from the newer one was removed.
// Push's "previous" side is a database, where an index missing from
// the Go schema was very likely never declared there in the first
// place — added by a DBA under load, by an extension, or by another
// migration tool. Dropping it is a rebuild on a table big enough to
// have needed the index, at whatever hour the push happens to run.
// Set DropUnmanagedIndexes to take the other side of that trade; the
// withheld statements are reported as notices either way, ready to run
// by hand.
//
// The same reasoning covers the objects Introspect learned to read
// after indexes did — an enum, a sequence, a view, a policy, and a
// row-level security somebody switched on by hand — under
// DropUnmanagedObjects. The RLS case is the one worth reading twice: a
// table with RLS enabled in the database and no EnableRLS in the Go
// schema is one ALTER TABLE away from serving every row to every
// caller, and a migration tool that performs that silently is worse
// than one that will not perform it at all. Push withholds it and
// reports an "unmanaged-rls" notice.
//
// What an extension owns is not withheld, it is never seen: Introspect
// leaves those out of the snapshot entirely, so no drop is proposed for
// PostGIS's views or for the sequence behind a serial column.
//
// # A change that could be a rename
//
// A column the database has and the schema does not, paired with one
// the schema has and the database does not, is either a rename or a
// drop-and-add, and the two differ by the whole contents of the
// column. Push cannot tell them apart any more than Diff can, so it
// asks the same question GenerateMigration asks and returns
// *RenameAmbiguityError rather than guessing — before any statement
// runs, and whatever else the push was going to do.
//
// Answer it in the schema with (*Col[T]).RenamedFrom or
// (*Table).RenamedFrom, which is durable and travels to every database
// the schema is pushed to, or for one run with PushOptions.Renames.
// A refusal is not something Safe or a destructive-statement
// permission overrides: whether a change is a rename and whether its
// consequences may be applied are two different questions, and one
// answer should not stand in for the other.
//
// # What Push cannot see
//
// Introspect reads back most, not all, of what the schema layer can
// declare, and Diff only compares what both sides carry. Push will
// therefore not notice a change to:
//
//   - an index's operator class, WITH storage parameters, column
//     ordering (ASC/DESC, NULLS FIRST/LAST) or NULLS NOT DISTINCT —
//     none of them reach the snapshot from either side;
//   - an index with any element that is an expression rather than a
//     column, such as lower(name): one such element makes the whole
//     index unrepresentable, so Push neither creates it nor compares
//     it against one the database already has. It is reported as an
//     "unrepresentable-index" notice; emit pg.CreateIndex for it;
//   - a multi-column FOREIGN KEY, which Introspect skips;
//   - where a sequence has got to. Its declared attributes are
//     compared and an ALTER SEQUENCE states the ones that moved, but
//     the value it is handing out next is not part of the declaration
//     and no push restarts a live sequence. So a declaration that
//     raises MINVALUE above that value, or lowers MAXVALUE below it,
//     is not a push drops can apply: PostgreSQL refuses the ALTER
//     (SQLSTATE 22023) rather than move the sequence, and the push
//     rolls back whole. Where the sequence should resume is a
//     question only the operator can answer — ALTER SEQUENCE ...
//     RESTART it by hand, then push;
//   - which columns a view reads, so a push that drops or retypes a
//     column takes down every view the schema declares and builds it
//     again, whether that view was in the way or not. See Diff. A view
//     the Go schema does not declare is left standing instead: it will
//     refuse the column change itself (SQLSTATE 2BP01), and if it
//     selects from a view that is being rebuilt Push refuses the whole
//     plan up front and names it. Declare it with Schema.AddView so it
//     is rebuilt too, or set DropUnmanagedObjects to have it removed;
//   - a view body that resolves against the table it selects from,
//     SELECT * above all. The probe respells the declared body before
//     the table DDL runs, so the * is expanded against the shape the
//     table has now; a migration that drops a column from under such a
//     view fails on the rebuilt CREATE VIEW naming the column that has
//     just gone. Name the columns.
//   - an enum label that was removed or reordered. PostgreSQL cannot
//     drop a label at all, and can only reorder one by rewriting every
//     column of the type, so Diff appends new labels and leaves the
//     rest alone. A reorder is at least reported, as an
//     "enum-labels-reordered" notice: the order is part of the type,
//     so leaving it unmentioned would be claiming the database matches
//     the schema when it does not;
//   - a CHECK expression, index predicate, policy clause or view body
//     the server refused to respell, reported as a "not-normalised"
//     notice and left alone rather than churned;
//   - a partial index whose predicate binds a value with no literal
//     spelling, which the snapshot records as no predicate at all —
//     the same declaration pg.CreateIndex cannot render either.
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

	// Before the probe, not after it. A push that is going to refuse
	// should not first take an ACCESS EXCLUSIVE lock on every table
	// carrying a CHECK constraint or a policy to answer a question it
	// is about to throw away. Renames are settled from column names and
	// types, which the probe does not touch.
	renames, err := resolvePushRenames(current, desired,
		mergeDecisions(DeclaredRenames(schema), opt.Renames))
	if err != nil {
		return nil, err
	}

	notices, err := renormaliseExpressions(ctx, db, current, desired)
	if err != nil {
		return nil, fmt.Errorf("drops/pg: normalise declared expressions: %w", err)
	}
	notices = append(notices, unrepresentableIndexNotices(desired)...)
	notices = append(notices, enumOrderNotices(current, desired)...)

	stmts := Diff(current, desired, DiffOptions{Safe: opt.Safe, Renames: renames})
	if !opt.DropUnmanagedIndexes {
		var withheld []SchemaNotice
		stmts, withheld = withhold(stmts, unmanagedIndexDrops(current, desired, opt.Safe))
		notices = append(notices, withheld...)
	}
	if !opt.DropUnmanagedObjects {
		var withheld []SchemaNotice
		stmts, withheld = withhold(stmts, unmanagedObjectDrops(current, desired, opt.Safe))
		notices = append(notices, withheld...)
		// Before anything runs, and on a DryRun too: a view Push may
		// not drop, standing on one it is about to rebuild, is a plan
		// the server will refuse halfway through.
		if err := checkUndeclaredViewDependents(current, desired); err != nil {
			return nil, err
		}
	}
	sortNotices(notices)

	if len(stmts) == 0 {
		return &PushResult{Statements: nil, Applied: false, Notices: notices}, nil
	}
	if opt.DryRun {
		return &PushResult{Statements: stmts, Applied: false, Notices: notices}, nil
	}

	var deferred []string
	if err := db.InTx(ctx, func(tx *DB) error {
		for _, s := range stmts {
			if needsOwnTransaction(s) {
				deferred = append(deferred, s)
				continue
			}
			if _, err := tx.Exec(ctx, s); err != nil {
				return fmt.Errorf("applying %q: %w", excerptSQL(s), err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, s := range deferred {
		if _, err := db.Exec(ctx, s); err != nil {
			return nil, fmt.Errorf("applying %q: %w", excerptSQL(s), err)
		}
	}
	return &PushResult{Statements: stmts, Applied: true, Notices: notices}, nil
}

// checkUndeclaredViewDependents refuses a push that would walk into
// SQLSTATE 2BP01 on a view.
//
// A migration that drops or retypes a column takes every view the Go
// schema declares down around it and builds them again — see Diff. The
// drops are plain, not CASCADE, so a view the database holds and the
// schema does not, selecting from one of those, stops the drop dead.
// PostgreSQL's message names the view it could not drop, which is the
// declared one, and says only that "other objects depend on it": the
// object worth naming is the one nobody declared, and Push is the half
// that knows which views those are.
//
// It does not run under DropUnmanagedObjects, where such a view is
// dropped along with the rest and there is nothing left to block.
func checkUndeclaredViewDependents(current, desired *Snapshot) error {
	if !columnWorkAViewCanBlock(current, desired) {
		return nil
	}
	for _, key := range sortedKeys(current.Views) {
		if _, declared := desired.Views[key]; !declared {
			continue
		}
		rebuilt := current.Views[key]
		for _, other := range sortedKeys(current.Views) {
			if other == key {
				continue
			}
			if _, declared := desired.Views[other]; declared {
				continue // coming down too, in dependency order
			}
			dependent := current.Views[other]
			if !mentionsIdent(dependent.Definition, rebuilt.Name) {
				continue
			}
			return fmt.Errorf("drops/pg: view %q selects from %q and is declared by no Schema.AddView. "+
				"This push changes a column PostgreSQL will not change while a view reads it, so %[2]q has to be "+
				"dropped and rebuilt around the change — which %[1]q blocks (SQLSTATE 2BP01). Declare %[1]q with "+
				"Schema.AddView so it is rebuilt too, drop it by hand, or set PushOptions.DropUnmanagedObjects",
				dependent.Name, rebuilt.Name)
		}
	}
	return nil
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
// exists to close — and the quieter of the two doors, because a push
// that goes through executes immediately with no migration file for
// anybody to read first.
//
// The answers come from the schema (DeclaredRenames) and from the call
// (PushOptions.Renames), the call winning, merged by the caller. There
// is no rename log here: a push has no migration directory, so the
// advice on the refusal points at the schema instead.
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
	"    pg.Add(Users, pg.Text(\"emailAddress\").RenamedFrom(\"email\"))\n" +
	"    pg.NewTable(\"people\").RenamedFrom(\"users\")\n" +
	"stated there it answers every database the schema is pushed to, and it goes inert once the rename has happened.\n" +
	"For one run instead, or to say the column really is being dropped rather than renamed:\n" +
	"    drops push --rename-column users.email=emailAddress   (or --drop-column users.email)\n" +
	"    PushOptions.Renames, for a caller that is not the CLI: a RenameDecision naming the pair\n" +
	"    renames it, one naming only the object that is going declines."

// needsOwnTransaction reports whether a statement has to run outside
// the push transaction. Only CONCURRENTLY index builds do: PostgreSQL
// rejects them inside a transaction block with SQLSTATE 25001. They
// come last in the diff anyway, so deferring them past the commit
// preserves the order the diff asked for.
func needsOwnTransaction(stmt string) bool {
	return strings.Contains(stmt, "INDEX CONCURRENTLY ")
}

// ----------------------------------------------------------------------
// Expression normalisation
// ----------------------------------------------------------------------

// errProbeDone unwinds the probe transaction. The probes are DDL and
// exist only to be read back, so the transaction is always rolled
// back; returning an error is how InTx is told to do that.
var errProbeDone = errors.New("drops/pg: probe complete")

// exprProbe is one expression to be respelled by the server.
type exprProbe struct {
	name     string // table name for the notice, empty for a schema-level object
	object   string
	rule     string
	expr     string  // the declared expression, as the Go schema wrote it
	target   *string // where the server's spelling is written back
	fallback *string // what to use instead when the probe fails
	// respell creates the throwaway object named probe, reads the
	// deparsed form back out of the catalogue, and leaves the caller
	// to roll the savepoint back.
	respell func(ctx context.Context, tx *DB, probe string) (string, error)
}

// renormaliseExpressions rewrites the expression-valued fields of the
// desired snapshot into the spelling PostgreSQL itself would report,
// so Diff can compare them as text.
//
// This is the one thing only a server can do. pg_get_expr,
// pg_get_constraintdef and pg_get_viewdef do not echo an expression
// back, they print a parse tree: `"age" >= 0` comes back `(age >= 0)`,
// `status = 'x'` comes back `(status = 'x'::text)`, `IN (...)` comes
// back `= ANY (ARRAY[...])`, and a view's SELECT comes back with its
// columns expanded and its table aliases rewritten. No amount of
// string tidying in Go reproduces that, and guessing produces a diff
// that either churns forever or misses a real change. So the declared
// expression is handed to the server, read back through the same
// deparser Introspect reads, and rolled back.
//
// Each kind goes over as the kind of object it is — a CHECK body and
// an index predicate as a NOT VALID CHECK constraint, a policy's
// clauses as a policy, a view's body as a view. A policy may hold a
// subquery and a view is one; a CHECK constraint may not hold either,
// so probing everything as a CHECK would have reported the whole class
// as unparseable and silently frozen it at whatever the database held.
//
// Only expressions already present on both sides are probed: a
// constraint, index, policy or view the database does not have yet is
// created from the declared spelling, and the server stores its own on
// the way in.
//
// A probe that fails leaves the desired side carrying the database's
// value, so the push does not churn, and returns a notice saying the
// expression went unchecked.
func renormaliseExpressions(ctx context.Context, db *DB, current, desired *Snapshot) ([]SchemaNotice, error) {
	var probes []*exprProbe
	for _, key := range sortedKeys(desired.Tables) {
		dt := desired.Tables[key]
		ct, ok := current.Tables[key]
		if !ok {
			continue
		}
		qualified := qualifiedTableSQL(dt)
		for _, name := range sortedKeys(dt.CheckConstraints) {
			cc, ok := ct.CheckConstraints[name]
			if !ok {
				continue
			}
			dc := dt.CheckConstraints[name]
			probes = append(probes, &exprProbe{
				name:     dt.Name,
				object:   name,
				rule:     "check-not-normalised",
				expr:     dc.Value,
				target:   &dc.Value,
				fallback: &cc.Value,
				respell:  checkProbe(qualified, dc.Value),
			})
		}
		for _, name := range sortedKeys(dt.Indexes) {
			di := dt.Indexes[name]
			ci, ok := ct.Indexes[name]
			if !ok || di.Where == "" {
				continue
			}
			probes = append(probes, &exprProbe{
				name:     dt.Name,
				object:   name,
				rule:     "index-predicate-not-normalised",
				expr:     di.Where,
				target:   &di.Where,
				fallback: &ci.Where,
				respell:  checkProbe(qualified, di.Where),
			})
		}
		for _, name := range sortedKeys(dt.Policies) {
			cp, ok := ct.Policies[name]
			if !ok {
				continue
			}
			dp := dt.Policies[name]
			for _, clause := range []struct {
				withCheck        bool
				declared, actual *string
			}{
				{false, &dp.Using, &cp.Using},
				{true, &dp.WithCheck, &cp.WithCheck},
			} {
				if *clause.declared == "" {
					continue
				}
				probes = append(probes, &exprProbe{
					name:     dt.Name,
					object:   name,
					rule:     "policy-expression-not-normalised",
					expr:     *clause.declared,
					target:   clause.declared,
					fallback: clause.actual,
					respell:  policyProbe(qualified, *clause.declared, clause.withCheck),
				})
			}
		}
	}
	for _, key := range sortedKeys(desired.Views) {
		dv := desired.Views[key]
		cv, ok := current.Views[key]
		if !ok || dv.Definition == "" {
			continue
		}
		probes = append(probes, &exprProbe{
			object:   dv.Name,
			rule:     "view-not-normalised",
			expr:     dv.Definition,
			target:   &dv.Definition,
			fallback: &cv.Definition,
			// The probe goes into the namespace the view was read
			// from, so the catalogue lookup that follows cannot land
			// on a same-named view in another schema of the path.
			respell: viewProbe(cv.Schema, dv.Definition, dv.Materialized),
		})
	}
	if len(probes) == 0 {
		return nil, nil
	}

	var notices []SchemaNotice
	err := db.InTx(ctx, func(tx *DB) error {
		// InTx may be retrying; start the report over rather than
		// reporting the first attempt's findings twice.
		notices = nil
		for i, p := range probes {
			savepoint := fmt.Sprintf("drops_probe_%d", i)
			if _, err := tx.Exec(ctx, "SAVEPOINT "+savepoint); err != nil {
				return err
			}
			def, perr := p.respell(ctx, tx, savepoint)
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
				return err
			}
			if perr != nil {
				if !probeRefusedExpression(perr) {
					return fmt.Errorf("probing %s on %q: %w", p.object, p.name, perr)
				}
				*p.target = *p.fallback
				notices = append(notices, SchemaNotice{
					Rule:    p.rule,
					Table:   p.name,
					Object:  p.object,
					Message: fmt.Sprintf("the server would not parse the declared expression %s (%v), so it was compared as the database already has it and any change to it will not be applied", p.expr, perr),
				})
				continue
			}
			*p.target = def
		}
		return errProbeDone
	})
	if err != nil && !errors.Is(err, errProbeDone) {
		return nil, err
	}
	return notices, nil
}

// checkProbe adds the declared expression as a NOT VALID CHECK
// constraint and reads the spelling the server gives it back.
func checkProbe(table, expr string) func(context.Context, *DB, string) (string, error) {
	return func(ctx context.Context, tx *DB, name string) (string, error) {
		stmt := fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID`,
			table, quoteIdent(name), expr)
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return "", err
		}
		// Scoped to the table, not just the name: a probe is
		// short-lived but the catalogue is global, and another schema
		// is entitled to a constraint called the same thing.
		def, err := probeScalar(ctx, tx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint
			 WHERE conrelid = $1::regclass AND conname = $2`, table, name)
		if err != nil {
			return "", err
		}
		return checkExprOf(def), nil
	}
}

// policyProbe adds the declared expression as a policy on the table and
// reads back pg_get_expr's spelling of it — the same column and the
// same function readIntrospectPolicies reads.
//
// The probe policy is created without a FOR clause whatever command the
// real one names: PostgreSQL rejects USING on a FOR INSERT policy and
// WITH CHECK on a FOR SELECT one, and the deparsing does not depend on
// which command the expression was attached to.
func policyProbe(table, expr string, withCheck bool) func(context.Context, *DB, string) (string, error) {
	clause, column := "USING", "polqual"
	if withCheck {
		clause, column = "WITH CHECK", "polwithcheck"
	}
	return func(ctx context.Context, tx *DB, name string) (string, error) {
		stmt := fmt.Sprintf(`CREATE POLICY %s ON %s %s (%s)`,
			quoteIdent(name), table, clause, expr)
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return "", err
		}
		return probeScalar(ctx, tx, fmt.Sprintf(
			`SELECT coalesce(pg_get_expr(%s, polrelid), '') FROM pg_policy
			 WHERE polrelid = $1::regclass AND polname = $2`, column), table, name)
	}
}

// viewProbe creates the declared body as a view and reads back
// pg_get_viewdef's spelling of it.
//
// A materialised view is probed as one, WITH NO DATA so the query is
// planned and rewritten but never run — a matview accepts query shapes
// a plain view does not, and the point of the probe is to find out
// what the server makes of this query, not of one like it.
func viewProbe(schema, def string, materialized bool) func(context.Context, *DB, string) (string, error) {
	return func(ctx context.Context, tx *DB, name string) (string, error) {
		qualified := quoteIdent(schema) + "." + quoteIdent(name)
		stmt := fmt.Sprintf(`CREATE VIEW %s AS %s`, qualified, def)
		if materialized {
			stmt = fmt.Sprintf(`CREATE MATERIALIZED VIEW %s AS %s WITH NO DATA`, qualified, def)
		}
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return "", err
		}
		body, err := probeScalar(ctx, tx,
			`SELECT pg_get_viewdef(oid, true) FROM pg_class WHERE oid = $1::regclass`, qualified)
		if err != nil {
			return "", err
		}
		return viewBodyOf(body), nil
	}
}

// probeScalar runs a one-row, one-column catalogue query and returns
// the value. A missing row means the object the probe just created is
// not where it was looked for, which is a bug in the probe rather than
// a verdict on the expression.
func probeScalar(ctx context.Context, tx *DB, query string, args ...any) (string, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", errors.New("the probe object was not found in the catalogue")
	}
	var out string
	if err := rows.Scan(&out); err != nil {
		return "", err
	}
	return out, rows.Err()
}

// probeRefusedExpression reports whether err is the server refusing the
// expression, as opposed to refusing to do the work just now.
//
// The probe is real DDL against a real table, so it fails for reasons
// that say nothing about the expression: it waits on ACCESS EXCLUSIVE,
// so a lock timeout or a deadlock is the ordinary outcome on a busy
// table. Counting those as "unparseable" silently downgraded the
// comparison to whatever the database already held — the tightened
// CHECK did not ship, the push reported success, and the notice
// blamed the declared expression. Only the SQLSTATE classes that
// describe the statement itself qualify: 42 syntax error or access
// rule violation, 22 data exception, 0A feature not supported.
// Anything else aborts the push and is the caller's to retry.
func probeRefusedExpression(err error) bool {
	var pgErr *PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "42", "22", "0A":
		return true
	}
	return false
}

// qualifiedTableSQL renders a table snapshot's name for use in DDL,
// schema-qualified when it carries one.
func qualifiedTableSQL(t *TableSnapshot) string {
	if t.Schema == "" || t.Schema == "public" {
		return quoteIdent(t.Name)
	}
	return quoteIdent(t.Schema) + "." + quoteIdent(t.Name)
}

// ----------------------------------------------------------------------
// Notices
// ----------------------------------------------------------------------

// unmanagedIndexDrops returns, keyed by the exact statement Diff would
// emit, a notice for every DROP INDEX that targets an index the
// database has and the Go schema never declared.
//
// The statements are matched by text rather than re-derived by the
// caller, because the text is what Diff produced from the same two
// snapshots a moment earlier — anything that does not match is a drop
// Diff emitted for a different reason (an index declared on both sides
// whose shape changed) and has to go through.
func unmanagedIndexDrops(current, desired *Snapshot, safe bool) map[string]SchemaNotice {
	// Index names are unique across a schema, not per table, so the
	// question is whether the Go schema declares the name anywhere —
	// not whether it declares it on the table the database put it on.
	declared := map[string]bool{}
	for _, dt := range desired.Tables {
		for name := range dt.Indexes {
			declared[name] = true
		}
	}
	withheld := map[string]SchemaNotice{}
	for _, key := range sortedKeys(current.Tables) {
		ct := current.Tables[key]
		for _, name := range sortedKeys(ct.Indexes) {
			if declared[name] {
				continue
			}
			sql := dropIndexSQL(name, safe)
			withheld[sql] = SchemaNotice{
				Rule:    "unmanaged-index",
				Table:   ct.Name,
				Object:  name,
				Message: fmt.Sprintf("index %q on %q exists in the database and is declared by no table in the Go schema; Push left it alone", name, ct.Name),
				SQL:     sql,
			}
		}
	}
	return withheld
}

// unmanagedObjectDrops is unmanagedIndexDrops for the rest of what
// Introspect reads: enums, sequences, views, policies, and the two
// row-level security flags.
//
// The RLS entries are the odd ones out in that they are not drops of
// anything. They are here for the same reason: the database has a
// protection the Go schema does not mention, and the difference between
// "the schema removed it" and "the schema never knew about it" is not
// one a snapshot can tell — so the statement that would remove it is
// withheld and named rather than run.
//
// Only a table the Go schema declares is examined. A table absent from
// it is being dropped whole, and Diff's DROP TABLE takes its policies
// with it; withholding an RLS statement for a table that will not
// exist would report a difference nobody can act on.
func unmanagedObjectDrops(current, desired *Snapshot, safe bool) map[string]SchemaNotice {
	withheld := map[string]SchemaNotice{}
	add := func(sql string, n SchemaNotice) {
		n.SQL = sql
		withheld[sql] = n
	}
	for _, key := range sortedKeys(current.Enums) {
		if _, ok := desired.Enums[key]; ok {
			continue
		}
		add(dropEnumSQL(key, safe), SchemaNotice{
			Rule:   "unmanaged-enum",
			Object: key,
			Message: fmt.Sprintf(
				"enum type %q exists in the database and is declared by no Schema.AddEnum; Push left it alone", key),
		})
	}
	for _, key := range sortedKeys(current.Sequences) {
		if _, ok := desired.Sequences[key]; ok {
			continue
		}
		seq := current.Sequences[key]
		add(dropSequenceSQL(seq.Name, safe), SchemaNotice{
			Rule:   "unmanaged-sequence",
			Object: seq.Name,
			Message: fmt.Sprintf(
				"sequence %q exists in the database and is declared by no Schema.AddSequence; Push left it alone", seq.Name),
		})
	}
	for _, key := range sortedKeys(current.Views) {
		if _, ok := desired.Views[key]; ok {
			continue
		}
		view := current.Views[key]
		add(dropViewSQL(view, safe), SchemaNotice{
			Rule:   "unmanaged-view",
			Object: view.Name,
			Message: fmt.Sprintf(
				"view %q exists in the database and is declared by no Schema.AddView; Push left it alone", view.Name),
		})
	}
	for _, key := range sortedKeys(current.Tables) {
		ct := current.Tables[key]
		dt, ok := desired.Tables[key]
		if !ok {
			continue
		}
		for _, name := range sortedKeys(ct.Policies) {
			if _, ok := dt.Policies[name]; ok {
				continue
			}
			add(dropPolicySQL(dt.Name, name, safe), SchemaNotice{
				Rule:   "unmanaged-policy",
				Table:  dt.Name,
				Object: name,
				Message: fmt.Sprintf(
					"policy %q on %q exists in the database and is declared by no Table.AddPolicy; Push left it alone", name, dt.Name),
			})
		}
		if ct.IsRLSEnabled && !dt.IsRLSEnabled {
			add(fmt.Sprintf(`ALTER TABLE %s DISABLE ROW LEVEL SECURITY;`, quoteIdent(dt.Name)), SchemaNotice{
				Rule:   "unmanaged-rls",
				Table:  dt.Name,
				Object: dt.Name,
				Message: fmt.Sprintf(
					"row-level security is enabled on %q in the database and no EnableRLS declares it; Push did not switch it off, which would have made every row readable by every caller", dt.Name),
			})
		}
		if ct.IsRLSForced && !dt.IsRLSForced {
			add(fmt.Sprintf(`ALTER TABLE %s NO FORCE ROW LEVEL SECURITY;`, quoteIdent(dt.Name)), SchemaNotice{
				Rule:   "unmanaged-rls",
				Table:  dt.Name,
				Object: dt.Name,
				Message: fmt.Sprintf(
					"row-level security is forced on %q in the database and no ForceRLS declares it; Push did not lift it, which would have exempted the table's owner from its own policies", dt.Name),
			})
		}
	}
	return withheld
}

// withhold removes from stmts every statement the map has a notice
// for, returning what is left and the notices for what was held back.
func withhold(stmts []string, unmanaged map[string]SchemaNotice) ([]string, []SchemaNotice) {
	if len(unmanaged) == 0 {
		return stmts, nil
	}
	kept := make([]string, 0, len(stmts))
	var notices []SchemaNotice
	for _, s := range stmts {
		if n, ok := unmanaged[s]; ok {
			notices = append(notices, n)
			continue
		}
		kept = append(kept, s)
	}
	return kept, notices
}

// enumOrderNotices reports an enum whose labels the Go schema declares
// in a different order from the one the database holds.
//
// Label order is part of the type — it is the ordering `<` uses and the
// one ORDER BY follows — and PostgreSQL offers no way to change it in
// place. Diff appends the new labels and stops there, so the reorder
// would otherwise be a difference Push saw, could not act on, and did
// not mention: a push that reports success against a database whose
// enum sorts differently from the declared one.
//
// Only the labels both sides carry are compared. A label the schema
// adds has not been placed yet, and one the database has and the
// schema does not cannot be removed at all.
func enumOrderNotices(current, desired *Snapshot) []SchemaNotice {
	var out []SchemaNotice
	for _, key := range sortedKeys(desired.Enums) {
		ce, ok := current.Enums[key]
		if !ok {
			continue
		}
		de := desired.Enums[key]
		shared := commonLabels(ce.Values, de.Values)
		if sameStrings(shared, commonLabels(de.Values, ce.Values)) {
			continue
		}
		out = append(out, SchemaNotice{
			Rule:   "enum-labels-reordered",
			Object: key,
			Message: fmt.Sprintf(
				"enum type %q holds its labels as %v and the Go schema declares them as %v; PostgreSQL cannot reorder them and Push did not try",
				key, ce.Values, de.Values),
		})
	}
	return out
}

// commonLabels returns the members of a that also appear in b, in a's
// order.
func commonLabels(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, v := range b {
		in[v] = true
	}
	out := make([]string, 0, len(a))
	for _, v := range a {
		if in[v] {
			out = append(out, v)
		}
	}
	return out
}

// unrepresentableIndexNotices reports declared indexes the snapshot
// cannot describe: at least one element was an expression rather than
// a column reference, which empties Columns, so nothing is left to
// compare or to render.
func unrepresentableIndexNotices(desired *Snapshot) []SchemaNotice {
	var out []SchemaNotice
	for _, key := range sortedKeys(desired.Tables) {
		dt := desired.Tables[key]
		for _, name := range sortedKeys(dt.Indexes) {
			if len(dt.Indexes[name].Columns) > 0 {
				continue
			}
			out = append(out, SchemaNotice{
				Rule:   "unrepresentable-index",
				Table:  dt.Name,
				Object: name,
				Message: fmt.Sprintf(
					"index %q on %q is declared over at least one expression rather than a plain column; the snapshot cannot describe it, so Push neither creates nor compares it — emit pg.CreateIndex for it yourself",
					name, dt.Name),
			})
		}
	}
	return out
}

// sortNotices puts notices in a stable order so two pushes of the same
// pair of schemas report them identically.
func sortNotices(n []SchemaNotice) {
	sort.SliceStable(n, func(i, j int) bool {
		if n[i].Table != n[j].Table {
			return n[i].Table < n[j].Table
		}
		if n[i].Rule != n[j].Rule {
			return n[i].Rule < n[j].Rule
		}
		return n[i].Object < n[j].Object
	})
}
