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
	DryRun bool

	// DropUnmanagedIndexes lets Push drop an index that exists in the
	// database but appears in no table of the Go schema. It is off by
	// default; see Push's doc comment for why.
	DropUnmanagedIndexes bool
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
	// "unmanaged-index", "unrepresentable-index",
	// "check-not-normalised", "index-predicate-not-normalised".
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
//   - Asks the server to respell the declared CHECK expressions and
//     partial-index predicates, so the two sides of the diff are
//     written in the same dialect of PostgreSQL's own deparser.
//   - Diffs the two using DiffOptions{Safe: opts.Safe}.
//   - If DryRun, returns the statements unexecuted.
//   - Otherwise applies them inside a single transaction; any failure
//     rolls back the whole push. CREATE INDEX CONCURRENTLY is the one
//     exception — PostgreSQL refuses it inside a transaction block
//     (SQLSTATE 25001) — so those statements run afterwards, once the
//     rest has committed.
//
// # An index the schema never declared
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
// # What Push cannot see
//
// Introspect reads back most, not all, of what the schema layer can
// declare, and Diff only compares what both sides carry. Push will
// therefore not notice a change to:
//
//   - an index's operator class, WITH storage parameters, column
//     ordering (ASC/DESC, NULLS FIRST/LAST) or NULLS NOT DISTINCT —
//     none of them reach the snapshot from either side;
//   - an index element that is an expression rather than a column,
//     such as lower(name): the snapshot records nothing for it, so
//     Push neither creates the index nor compares it against one the
//     database already has. It is reported as an
//     "unrepresentable-index" notice; emit pg.CreateIndex for it;
//   - a multi-column FOREIGN KEY, which Introspect skips;
//   - enums, sequences, views, RLS and policies, which Introspect does
//     not read at all — Diff sees them as new on every push;
//   - a CHECK expression or index predicate the server refused to
//     respell, reported as a "not-normalised" notice and left alone
//     rather than churned;
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

	notices, err := renormaliseExpressions(ctx, db, current, desired)
	if err != nil {
		return nil, fmt.Errorf("drops/pg: normalise declared expressions: %w", err)
	}
	notices = append(notices, unrepresentableIndexNotices(desired)...)

	stmts := Diff(current, desired, DiffOptions{Safe: opt.Safe})
	if !opt.DropUnmanagedIndexes {
		var withheld []SchemaNotice
		stmts, withheld = withholdUnmanagedIndexDrops(stmts, current, desired, opt.Safe)
		notices = append(notices, withheld...)
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
	table    string // qualified, as written into the ALTER TABLE
	name     string // table name for the notice
	object   string // index or constraint name
	rule     string
	expr     string  // the declared expression, as the Go schema wrote it
	target   *string // where the server's spelling is written back
	fallback *string // what to use instead when the probe fails
}

// renormaliseExpressions rewrites the expression-valued fields of the
// desired snapshot into the spelling PostgreSQL itself would report,
// so Diff can compare them as text.
//
// This is the one thing only a server can do. pg_get_expr and
// pg_get_constraintdef do not echo an expression back, they print a
// parse tree: `"age" >= 0` comes back `(age >= 0)`, `status = 'x'`
// comes back `(status = 'x'::text)`, and `IN (...)` comes back
// `= ANY (ARRAY[...])`. No amount of string tidying in Go reproduces
// that, and guessing produces a diff that either churns forever or
// misses a real change. So the declared expression is handed to the
// server as a CHECK constraint that is added NOT VALID — which skips
// the table scan — read back through the same deparser Introspect
// reads, and rolled back.
//
// Only expressions already present on both sides are probed: a
// constraint or index the database does not have yet is created from
// the declared spelling, and the server stores its own on the way in.
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
				table:    qualified,
				name:     dt.Name,
				object:   name,
				rule:     "check-not-normalised",
				expr:     dc.Value,
				target:   &dc.Value,
				fallback: &cc.Value,
			})
		}
		for _, name := range sortedKeys(dt.Indexes) {
			di := dt.Indexes[name]
			ci, ok := ct.Indexes[name]
			if !ok || di.Where == "" {
				continue
			}
			probes = append(probes, &exprProbe{
				table:    qualified,
				name:     dt.Name,
				object:   name,
				rule:     "index-predicate-not-normalised",
				expr:     di.Where,
				target:   &di.Where,
				fallback: &ci.Where,
			})
		}
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
			def, perr := probeConstraintDef(ctx, tx, p, savepoint)
			if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT "+savepoint); err != nil {
				return err
			}
			if perr != nil {
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

// probeConstraintDef adds the declared expression as a NOT VALID CHECK
// constraint, reads the spelling the server gives it back, and leaves
// the caller to roll the savepoint back.
func probeConstraintDef(ctx context.Context, tx *DB, p *exprProbe, name string) (string, error) {
	stmt := fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID`,
		p.table, quoteIdent(name), p.expr)
	if _, err := tx.Exec(ctx, stmt); err != nil {
		return "", err
	}
	// Scoped to the table, not just the name: a probe is short-lived
	// but the catalogue is global, and another schema is entitled to a
	// constraint called the same thing.
	rows, err := tx.Query(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		 WHERE conrelid = $1::regclass AND conname = $2`, p.table, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", errors.New("the probe constraint was not found in pg_constraint")
	}
	var def string
	if err := rows.Scan(&def); err != nil {
		return "", err
	}
	return checkExprOf(def), rows.Err()
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

// withholdUnmanagedIndexDrops removes from stmts every DROP INDEX that
// targets an index the database has and the Go schema never declared,
// returning what is left and a notice for each one held back.
//
// The statements are matched by text rather than re-derived, because
// the text is what Diff produced from the same two snapshots a moment
// earlier — anything that does not match is a drop Diff emitted for a
// different reason (an index declared on both sides whose shape
// changed) and has to go through.
func withholdUnmanagedIndexDrops(stmts []string, current, desired *Snapshot, safe bool) ([]string, []SchemaNotice) {
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
	if len(withheld) == 0 {
		return stmts, nil
	}
	kept := make([]string, 0, len(stmts))
	var notices []SchemaNotice
	for _, s := range stmts {
		if n, ok := withheld[s]; ok {
			notices = append(notices, n)
			continue
		}
		kept = append(kept, s)
	}
	return kept, notices
}

// unrepresentableIndexNotices reports declared indexes the snapshot
// cannot describe: every element was an expression rather than a
// column reference, so nothing is left to compare or to render.
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
					"index %q on %q is declared over expressions rather than columns; the snapshot cannot describe it, so Push neither creates nor compares it — emit pg.CreateIndex for it yourself",
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
