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

	// DropUnmanagedTables lets Push drop a table that exists in the
	// database but appears in no table of the Go schema. It is off by
	// default, for the same reason as DropUnmanagedIndexes and with
	// more at stake: Push's "previous" side is a database, and a table
	// the Go schema does not name most likely was never drops's to
	// begin with — another service's, a vendor's, an extension's, a
	// migration history table, something a DBA left. The withheld DROPs
	// are reported as "unmanaged-table" notices whether the push is a
	// DryRun or a real one, ready to run by hand.
	//
	// The restriction is Push's alone. It does not apply to Diff or to
	// GenerateMigration, where both sides are declarations and a table
	// missing from the newer one really was removed.
	//
	// Turning it on does not by itself authorise the drop of a table
	// with rows in it; see Allow.
	DropUnmanagedTables bool

	// Allow names the destructive changes this push is permitted to
	// make to a table that is not empty. Anything destructive that is
	// not named here, and not against an empty table, is withheld:
	// Push applies nothing, returns ErrDestructivePush, and reports
	// what it withheld in PushResult.DataLoss.
	//
	// Naming the object rather than passing a global --force is the
	// whole point. A flag says "destroy whatever today's diff happens
	// to contain", which is a decision made before anyone knew what
	// that was; a Destructive names one table and one column, so
	// yesterday's consent cannot authorise today's unrelated DROP, and
	// the consent is a Go value visible in the diff of the pull request
	// that grants it.
	Allow []Destructive
}

// DestructiveOp names a kind of change that destroys data.
type DestructiveOp string

// The names carry an Op prefix because pg.DropTable is already the DDL
// builder that renders a DROP TABLE statement, and one of the two
// spellings has to give way to the other.
const (
	// OpDropTable is a DROP TABLE: every row goes.
	OpDropTable DestructiveOp = "drop-table"
	// OpDropColumn is an ALTER TABLE ... DROP COLUMN: one value per
	// row goes.
	OpDropColumn DestructiveOp = "drop-column"
	// OpRetypeColumn is an ALTER TABLE ... SET DATA TYPE. It is here
	// because PostgreSQL casts every existing value on the way, and a
	// cast can truncate (varchar(20) to varchar(10)), round (numeric to
	// integer) or fail halfway through the rewrite.
	OpRetypeColumn DestructiveOp = "retype-column"
)

// Destructive names one change Push is authorised to make. Pass them
// in PushOptions.Allow.
type Destructive struct {
	// Op is the kind of change being authorised.
	Op DestructiveOp
	// Table is the unqualified table name, as the database has it.
	Table string
	// Object is the column name; empty for OpDropTable.
	Object string
}

// DataLoss is a destructive change Push found, declined to make, and
// is telling you about.
//
// Rows is an ESTIMATE, read from pg_class.reltuples. Distinguishing
// "empty" from "not empty" is all the rule needs — an empty table has
// nothing to lose, so dropping it needs no permission — and a COUNT(*)
// per candidate table is a sequential scan bolted onto the operation
// you most want to finish quickly.
//
// The estimate is -1 on a table PostgreSQL has never analysed, which is
// every table created since the last autovacuum ran: exactly the tables
// a development push creates and reshapes minutes later. Refusing those
// would make the rule fire hardest where there is nothing to protect,
// so an unknown estimate is settled with SELECT EXISTS (SELECT 1 FROM
// t LIMIT 1) — one row read, on the only tables whose count nobody
// knows, and only for a change nobody has authorised. Rows stays -1
// afterwards, because "some" is all that probe answers.
type DataLoss struct {
	// Op is the kind of change.
	Op DestructiveOp
	// Table is the table it is against.
	Table string
	// Object is the column, empty for a table-level change.
	Object string
	// Rows is the planner's row-count estimate for Table, or -1 when
	// the table has never been analysed.
	Rows int64
	// SQL is the statement Push withheld.
	SQL string
	// Suggestion is the Destructive value that would authorise it.
	Suggestion string
}

// ErrDestructivePush is returned when a push would destroy data in a
// table that is not empty and PushOptions.Allow does not name the
// change. Nothing is applied: the whole diff is withheld, not just the
// destructive part of it, because a half-applied schema is worse than
// an unapplied one and the caller is about to re-run this anyway.
//
// Unlike every other failure in Push, the PushResult is returned
// alongside the error rather than instead of it — the statements it
// planned and the DataLoss list are the answer to the question the
// error raises.
var ErrDestructivePush = errors.New("drops/pg: push withheld destructive statements; see PushResult.DataLoss")

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
	// DataLoss lists the destructive changes that stopped this push,
	// in the order the diff put them. It is non-empty exactly when
	// Push returned ErrDestructivePush; each entry carries the
	// PushOptions.Allow value that would let it through.
	DataLoss []DataLoss
}

// SchemaNotice is a difference Push can see but will not act on.
//
// A migration tool that silently skips a change is worse than one that
// says which changes it cannot make, so every such decision surfaces
// here rather than being dropped on the floor.
type SchemaNotice struct {
	// Rule is a stable identifier for the kind of notice —
	// "unmanaged-table", "unmanaged-index", "unrepresentable-index",
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
//     See "What the probe costs" below: this happens on a DryRun too.
//   - Narrows the live side to the tables the Go schema declares,
//     unless DropUnmanagedTables says otherwise; see ownedBy.
//   - Diffs the two using DiffOptions{Safe: opts.Safe}.
//   - Refuses the whole push when the diff would destroy data in a
//     table that is not empty and PushOptions.Allow does not name the
//     change; see ErrDestructivePush.
//   - If DryRun, returns the statements unexecuted.
//   - Otherwise applies them inside a single transaction; any failure
//     rolls back the whole push. CREATE INDEX CONCURRENTLY is the one
//     exception — PostgreSQL refuses it inside a transaction block
//     (SQLSTATE 25001) — so those statements run afterwards, once the
//     rest has committed.
//
// # What the probe costs
//
// Respelling a declared expression means handing it to the server as a
// CHECK constraint added NOT VALID and reading back what the deparser
// makes of it. NOT VALID skips the table scan, and the whole thing is
// rolled back, but ALTER TABLE ADD CONSTRAINT still takes an ACCESS
// EXCLUSIVE lock on the table, and PostgreSQL holds a lock until the
// transaction ends — not until the savepoint rolls back. So a push
// against a schema with CHECK constraints or partial indexes briefly
// locks every table carrying one, all at once, and blocks readers of
// those tables for the length of the probe. It happens on a DryRun as
// well, because there is no way to preview a change to an expression
// without first learning how the server spells it.
//
// Set lock_timeout on the connection if a push must never wait behind
// a long reader. A probe that fails for an operational reason — a lock
// timeout, a deadlock, a cancelled statement — fails the whole push
// rather than being reported as an unparseable expression, so a
// change is never quietly left unapplied.
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
// # A table the schema never declared
//
// The same argument, with the whole table's worth of data behind it.
// Push against a shared database — a Supabase or Neon project, an RDS
// instance several services point at, anything with PostGIS in it —
// used to emit DROP TABLE ... CASCADE for every table the Go schema did
// not declare, which is the fastest way for a tool to be banned from an
// organisation for good. It no longer does: the live side is narrowed
// to the declared tables before the diff, and the drops it would have
// emitted come back as "unmanaged-table" notices. DropUnmanagedTables
// takes the other side of the trade.
//
// # Destroying data on purpose
//
// A change that destroys data — DROP TABLE, DROP COLUMN, a SET DATA
// TYPE whose cast rewrites every value — goes through unremarked when
// the table is empty, because there is nothing to lose, which is what
// makes the development loop of pushing a table and reshaping it a
// minute later unaffected. Against a table with rows in it, Push
// applies nothing at all and returns ErrDestructivePush with a DataLoss entry per statement it withheld;
// naming each one in PushOptions.Allow is what lets it through. See
// DataLoss for what "has rows" means and what it costs to ask.
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

	var notices []SchemaNotice
	if !opt.DropUnmanagedTables {
		var withheld []string
		live := current
		current, withheld = ownedBy(live, desired)
		notices = append(notices, unmanagedTableNotices(live, withheld, opt.Safe)...)
	}

	exprNotices, err := renormaliseExpressions(ctx, db, current, desired)
	if err != nil {
		return nil, fmt.Errorf("drops/pg: normalise declared expressions: %w", err)
	}
	notices = append(notices, exprNotices...)
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
	loss, err := withheldDataLoss(ctx, db, schemaName, stmts, current, desired, opt)
	if err != nil {
		return nil, err
	}
	if len(loss) > 0 {
		return &PushResult{Statements: stmts, Applied: false, Notices: notices, DataLoss: loss},
			fmt.Errorf("%w: %d of %d statements would destroy data, starting with %q",
				ErrDestructivePush, len(loss), len(stmts), excerptSQL(loss[0].SQL))
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
// Ownership
// ----------------------------------------------------------------------

// ownedBy narrows a live introspection to the tables the Schema
// declares, and returns the keys of the ones it held back so Push can
// report them.
//
// A PostgreSQL schema is often not a namespace one application has to
// itself: a Supabase or Neon project puts extensions, auth tables and
// storage tables beside yours, an RDS instance serves several services,
// PostGIS leaves spatial_ref_sys behind, and another migration tool
// keeps its history table somewhere. To Diff, every one of those looks
// like a table that used to exist and should no longer, and it emits
// DROP TABLE ... CASCADE for it — so a push against a database drops
// did not create alone deletes other people's data, cascading through
// their foreign keys on the way out.
//
// A list of vendor names to skip cannot work; it has to grow for ever,
// one paper cut at a time, and it says nothing about the table another
// team added last week. The Go Schema is the only statement of
// ownership drops has, and it is one the compiler already checks, so a
// table it never names is left alone. drops/sqlite and drops/mysql
// draw the same line for the same reason.
//
// The cost is that dropping a table means writing the DROP into a
// migration rather than deleting the Go declaration and pushing —
// which is the reviewable path anyway — or setting
// PushOptions.DropUnmanagedTables, which puts the live side back
// whole.
func ownedBy(live, declared *Snapshot) (*Snapshot, []string) {
	out := *live
	out.Tables = make(map[string]*TableSnapshot, len(live.Tables))
	var withheld []string
	for _, key := range sortedKeys(live.Tables) {
		if _, ok := declared.Tables[key]; ok {
			out.Tables[key] = live.Tables[key]
			continue
		}
		withheld = append(withheld, key)
	}
	return &out, withheld
}

// unmanagedTableNotices reports one withheld DROP TABLE per table
// ownedBy held back, carrying the statement Diff would have emitted so
// a caller who wants it can run it by hand.
func unmanagedTableNotices(live *Snapshot, keys []string, safe bool) []SchemaNotice {
	out := make([]SchemaNotice, 0, len(keys))
	for _, key := range keys {
		t := live.Tables[key]
		out = append(out, SchemaNotice{
			Rule:  "unmanaged-table",
			Table: t.Name,
			Message: fmt.Sprintf(
				"table %q exists in the database and is declared by no table in the Go schema; Push left it alone — set PushOptions.DropUnmanagedTables if it really is drops's to drop",
				t.Name),
			SQL: dropTableSQL(t, safe),
		})
	}
	return out
}

// ----------------------------------------------------------------------
// Destructive changes
// ----------------------------------------------------------------------

// withheldDataLoss returns one DataLoss per statement in stmts that
// destroys data in a table that is not empty and that PushOptions.Allow
// does not authorise.
//
// The candidates are derived from the same two snapshots Diff was given
// and then matched against the statements by text, exactly as
// withholdUnmanagedIndexDrops does: re-deriving the statement is what
// keeps a rule from firing on a statement that came from somewhere
// else, and text is what makes the match total.
//
// The row estimates cost one query against pg_class, asked once for the
// whole schema and only when a destructive statement is actually in the
// diff — the overwhelmingly common push has none, and pays nothing.
func withheldDataLoss(ctx context.Context, db *DB, schema string, stmts []string, current, desired *Snapshot, opt PushOptions) ([]DataLoss, error) {
	candidates := map[string]DataLoss{}
	add := func(sql string, d DataLoss) {
		d.SQL = sql
		candidates[sql] = d
	}
	for _, key := range sortedKeys(current.Tables) {
		ct := current.Tables[key]
		dt, declared := desired.Tables[key]
		if !declared {
			add(dropTableSQL(ct, opt.Safe), DataLoss{Op: OpDropTable, Table: ct.Name})
			continue
		}
		for _, name := range sortedKeys(ct.Columns) {
			dc, kept := dt.Columns[name]
			if !kept {
				add(dropColumnSQL(dt.Name, name, opt.Safe),
					DataLoss{Op: OpDropColumn, Table: ct.Name, Object: name})
				continue
			}
			if dc.Type != ct.Columns[name].Type {
				add(setColumnTypeSQL(dt.Name, name, dc.Type),
					DataLoss{Op: OpRetypeColumn, Table: ct.Name, Object: name})
			}
		}
	}

	// Consent first: a caller who has already named every destructive
	// change in the diff is owed neither an estimate nor a probe, and
	// the overwhelmingly common push has no destructive change at all.
	var unconsented []DataLoss
	for _, s := range stmts {
		if d, ok := candidates[s]; ok && !allowsDestructive(opt.Allow, d) {
			unconsented = append(unconsented, d)
		}
	}
	if len(unconsented) == 0 {
		return nil, nil
	}

	rows, err := tableRowEstimates(ctx, db, schema)
	if err != nil {
		return nil, fmt.Errorf("drops/pg: estimate rows before a destructive push: %w", err)
	}
	var withheld []DataLoss
	probed := map[string]bool{}
	for _, d := range unconsented {
		est, known := rows[d.Table]
		if !known {
			// Not in pg_class under this schema at all, which this
			// connection has no business assuming is empty.
			est = -1
		}
		if est == 0 {
			continue // nothing to lose
		}
		if est < 0 {
			has, seen := probed[d.Table]
			if !seen {
				has, err = tableHasRows(ctx, db, schema, d.Table)
				if err != nil {
					return nil, fmt.Errorf("drops/pg: deciding whether %q is empty before destroying data in it: %w", d.Table, err)
				}
				probed[d.Table] = has
			}
			if !has {
				continue
			}
		}
		d.Rows = est
		d.Suggestion = fmt.Sprintf("allow with pg.Destructive{Op: pg.%s, Table: %q, Object: %q}",
			destructiveOpConst(d.Op), d.Table, d.Object)
		withheld = append(withheld, d)
	}
	return withheld, nil
}

// allowsDestructive reports whether the caller named this exact change.
func allowsDestructive(allow []Destructive, d DataLoss) bool {
	for _, a := range allow {
		if a.Op == d.Op && a.Table == d.Table && a.Object == d.Object {
			return true
		}
	}
	return false
}

// destructiveOpConst renders the Go identifier for an op, so the
// suggestion can be pasted into the call rather than transcribed.
func destructiveOpConst(op DestructiveOp) string {
	switch op {
	case OpDropTable:
		return "OpDropTable"
	case OpDropColumn:
		return "OpDropColumn"
	case OpRetypeColumn:
		return "OpRetypeColumn"
	}
	return string(op)
}

// tableRowEstimates reads the planner's row-count estimate for every
// table in the schema.
//
// reltuples is what the last ANALYZE or autovacuum left behind, which
// is why this is one cheap catalogue read rather than a COUNT(*) per
// table. PostgreSQL 14 and later store -1 for a table that has never
// been analysed, and this passes that through: the caller has to
// distinguish "no rows" from "nobody has looked", because only the
// first of those is a reason to let a DROP through. See tableHasRows
// for how the second is settled.
func tableRowEstimates(ctx context.Context, db *DB, schema string) (map[string]int64, error) {
	rows, err := db.Query(ctx, `SELECT c.relname, c.reltuples::bigint
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var est int64
		if err := rows.Scan(&name, &est); err != nil {
			return nil, err
		}
		out[name] = est
	}
	return out, rows.Err()
}

// tableHasRows answers the one question the estimate could not: is
// there anything in here at all.
//
// LIMIT 1 stops at the first row, so this reads one page of a table of
// any size — the property COUNT(*) does not have, and the reason a
// probe is affordable here and a count is not. It runs only for a table
// PostgreSQL has never analysed, and only for a destructive change
// nobody has authorised, so the common push never reaches it.
//
// A probe that fails fails the push. Not being able to tell whether a
// table holds data is not a reason to go ahead and destroy it, and it
// is not a reason to report it as data loss either — the caller would
// be granting consent for a table nobody has looked inside.
func tableHasRows(ctx context.Context, db *DB, schema, table string) (bool, error) {
	stmt := fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.%s LIMIT 1)`,
		quoteIdent(schema), quoteIdent(table))
	rows, err := db.Query(ctx, stmt)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return false, err
		}
		return false, errors.New("the emptiness probe returned no row")
	}
	var has bool
	if err := rows.Scan(&has); err != nil {
		return false, err
	}
	return has, rows.Err()
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
