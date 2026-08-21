package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrSchemaRequired is returned by [Push] when schema is nil.
var ErrSchemaRequired = errors.New("drops/clickhouse: Push requires a non-nil schema")

// ErrMixedDatabases is returned by [Push] when the schema's tables do
// not agree on a database and no [PushOptions.Database] settles it.
// One push reads one database; see [PushOptions.Database].
var ErrMixedDatabases = errors.New("drops/clickhouse: Push needs one database, and the schema names several")

// PushOptions tunes how [Push] applies schema changes.
type PushOptions struct {
	// Database names the database to read and write. Empty means the
	// one the schema's tables name — every table declared with
	// [NewDatabaseTable] and the same database, or every table
	// declared with [NewTable] and none, in which case the
	// connection's own database is used.
	//
	// A schema that mixes the two is [ErrMixedDatabases]. It is not a
	// limitation drops could paper over: an unqualified statement
	// lands wherever the connection points, so a schema that is half
	// qualified describes a shape that depends on the DSN, and there
	// is no single database to read back and compare against.
	Database string

	// DryRun returns the statements that would be applied without
	// executing them. It is genuinely read-only: Push reads two system
	// tables and nothing else before it decides.
	DryRun bool

	// AllowRefused runs the statements even though the plan also
	// carries refusals.
	//
	// The default is all-or-nothing, because a plan drops does not
	// understand in full is a plan whose result nobody has described.
	// The exception this exists for is real though: a sorting key that
	// has moved is not going to be fixed by this push whatever
	// happens, and it should not also block the ADD COLUMN that stops
	// an insert failing. Push still returns [ErrRefused] afterwards,
	// so a partial push cannot be mistaken for a finished one.
	AllowRefused bool
}

// PushResult is the outcome of a [Push] call.
//
// It is returned even when Push returns an error, which is the whole
// point of AppliedCount: ClickHouse cannot roll a schema change back,
// so "it failed" is not a complete answer and "it failed after four
// statements, on the fifth" is.
type PushResult struct {
	// Statements is the ordered SQL between the live database and the
	// supplied schema.
	Statements []string

	// Applied is true when every statement was executed successfully.
	Applied bool

	// AppliedCount is how many statements the server accepted. On
	// success it equals len(Statements); on failure it is the index of
	// the statement that failed, and every statement before it has
	// taken effect and will not be undone by Push.
	//
	// A statement the server accepted is not necessarily a change that
	// has finished: a MODIFY COLUMN queues a mutation and returns.
	// See system.mutations, and [Analyze].
	AppliedCount int

	// Failed is the statement that failed, empty when none did.
	Failed string

	// Refusals are the changes ClickHouse will not make. A push that
	// returns none of these and no error has brought the database into
	// line; a push that returns some has not, whatever else it did.
	Refusals []Refusal

	// Notices are the differences Push saw and did not act on, because
	// acting would have meant re-emitting the same statement on every
	// push. Each carries the statement it withheld.
	Notices []Notice

	// Warnings grade the statements Push is about to run, or ran. They
	// are filled in whether or not the statements execute, so a DryRun
	// is a full review.
	Warnings []SafetyWarning
}

// Push introspects the live database, diffs it against the supplied Go
// schema, and applies the changes — drops's equivalent of drizzle-kit's
// `push`, for a database where rather a lot of changes have no
// statement behind them.
//
// Behaviour:
//   - refuses a schema with a table that names no engine, with
//     [ErrEngineRequired] and before it reads anything;
//   - reads the current state with [Introspect], scoped to one
//     database;
//   - builds the target snapshot with [BuildSnapshot];
//   - narrows the live side to the tables the schema declares, so a
//     table drops was never told about is left alone;
//   - diffs the two into a [Plan];
//   - unless DryRun, runs the plan's statements one at a time, in
//     order.
//
// # A failed push leaves the schema half-changed
//
// ClickHouse has no transactional DDL. Every CREATE, ALTER and DROP
// takes effect on its own, and a push of six statements that fails on
// the fourth leaves the first three applied for good. So the contract
// is the same one drops/mysql states: the result comes back alongside
// the error rather than instead of it, [PushResult.AppliedCount] says
// how many statements landed, and the fix is to correct the cause and
// push again — Push recomputes the diff from the live database every
// time, so the statements that already applied are simply not in the
// next one. That, rather than IF EXISTS, is what makes re-running
// safe; Push turns [DiffOptions.Safe] on as well so that a re-run does
// not trip over the statement it half-finished.
//
// # A statement that returned is not a change that finished
//
// This is the difference from every other dialect drops speaks. A
// MODIFY COLUMN that changes a type queues a mutation and returns
// immediately; the rewriting happens afterwards, in the background,
// and can still fail. Push reports the statement as applied because
// the server accepted it, which is all any client can know.
// system.mutations is where the rest of the story is, and
// [PushResult.Warnings] names the statements that put a row there.
//
// # What Push will not do
//
// It emits no statement for a change ClickHouse cannot make: an
// engine, a partition key, a primary key, a sorting key, or a type
// change or drop of a column that takes part in one of those. Those
// come back as [PushResult.Refusals], and by default they stop the
// push entirely. Set AllowRefused to run the rest anyway.
//
// It also withholds a handful of statements that would otherwise be
// re-emitted for ever, because introspection cannot read back what the
// declaration says — a column's TTL, a table TTL the server has
// re-spelled, a sampling key. Each is a [Notice] carrying the withheld
// SQL.
//
// Push keeps no migration history. For production prefer
// [TreeMigrator], where the statements are reviewed before they run —
// which for a store with no rollback is worth more than it is
// anywhere else.
func Push(ctx context.Context, db *DB, schema *Schema, opts ...PushOptions) (*PushResult, error) {
	if schema == nil {
		return nil, ErrSchemaRequired
	}
	if db == nil {
		return nil, fmt.Errorf("drops/clickhouse: Push needs a db")
	}
	var opt PushOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	database, err := pushDatabase(schema, opt.Database)
	if err != nil {
		return nil, err
	}
	if err := checkEngines(schema); err != nil {
		return nil, err
	}

	desired := BuildSnapshot(schema)
	live, err := Introspect(ctx, db, IntrospectOptions{
		Database: database,
		Tables:   declaredTableNames(schema),
	})
	if err != nil {
		return nil, fmt.Errorf("drops/clickhouse: introspect: %w", err)
	}

	plan := Diff(ownedBy(live, desired), desired, DiffOptions{Safe: true})
	res := &PushResult{
		Statements: plan.Statements,
		Refusals:   plan.Refusals,
		Notices:    plan.Notices,
		Warnings:   Analyze(plan),
	}
	if len(plan.Refusals) > 0 && !opt.AllowRefused {
		return res, plan.Refused()
	}
	if opt.DryRun || len(plan.Statements) == 0 {
		return res, plan.Refused()
	}

	for _, stmt := range plan.Statements {
		if _, err := db.Exec(ctx, stmt); err != nil {
			res.Failed = stmt
			return res, fmt.Errorf("drops/clickhouse: applying %q: %w", excerptSQL(stmt), err)
		}
		res.AppliedCount++
	}
	res.Applied = true
	return res, plan.Refused()
}

// checkEngines refuses a schema whose tables do not all name an
// engine, before Push has read anything.
//
// A table with no engine renders its ENGINE clause as the marker
// [CreateTable] emits — a Go comment, which the server answers with a
// syntax error. [CreateTableErr] exists for "migration tooling that
// wants a definite error rather than SQL that references a sentinel
// string", and Push is that tooling: it would otherwise introspect,
// diff, and send a statement that could never have run.
//
// The check is over the whole schema rather than only the tables the
// server is missing, because a declaration with no engine is
// incomplete however the live database happens to look: [BuildSnapshot]
// records an empty engine, and [Diff] reads an empty engine as "the
// declaration does not say", so such a table is also one whose engine
// drift nothing will ever report.
func checkEngines(schema *Schema) error {
	for _, t := range schema.Tables() {
		if t.engine == nil {
			return fmt.Errorf("%w: table %s", ErrEngineRequired, QualifiedName(t.database, t.name))
		}
	}
	return nil
}

// pushDatabase works out which database this push is about.
//
// The schema is the statement of intent: a table declared with
// [NewDatabaseTable] renders its statements qualified and one declared
// with [NewTable] does not, so the two cannot be pushed together and a
// database read from anywhere else would be reading a different table
// from the one the statements write.
func pushDatabase(schema *Schema, override string) (string, error) {
	seen := map[string]bool{}
	for _, t := range schema.Tables() {
		seen[t.Database()] = true
	}
	if len(seen) > 1 {
		names := make([]string, 0, len(seen))
		for db := range seen {
			if db == "" {
				db = "(the connection's own)"
			}
			names = append(names, db)
		}
		return "", fmt.Errorf("%w: %s", ErrMixedDatabases, strings.Join(names, ", "))
	}
	for db := range seen {
		if db != "" {
			if override != "" && override != db {
				return "", fmt.Errorf(
					"%w: PushOptions.Database is %q and the schema declares %q",
					ErrMixedDatabases, override, db)
			}
			return db, nil
		}
	}
	return override, nil
}

// declaredTableNames lists the tables the schema names, so the
// introspection reads those and not the whole database. A database
// shared with another service — and in ClickHouse it usually is, since
// system tables live beside yours — is not drops's to enumerate.
func declaredTableNames(schema *Schema) []string {
	out := make([]string, 0, len(schema.Tables()))
	for _, t := range schema.Tables() {
		out = append(out, t.Name())
	}
	return out
}

// ownedBy narrows a live introspection to the tables the schema
// declares.
//
// [Diff] compares two declarations, so a table in the first and not in
// the second is a table that was removed, and it emits a DROP. Push's
// "previous" side is a server, where a table missing from the Go
// schema was very likely never declared there at all — a view someone
// built for a dashboard, another service's data, a Kafka engine table
// feeding a materialized view. The schema is the only statement of
// ownership drops has, so a table it never names is left alone.
//
// The cost is that removing a table means writing the DROP yourself
// rather than deleting the Go declaration and pushing, which for an
// analytics store nobody can restore from a transaction log is the
// trade worth making.
func ownedBy(live, declared *Snapshot) *Snapshot {
	out := &Snapshot{
		ID:      live.ID,
		PrevID:  live.PrevID,
		Version: live.Version,
		Dialect: live.Dialect,
		Tables:  make(map[string]*TableSnapshot, len(live.Tables)),
	}
	for key, ts := range live.Tables {
		if _, ok := declared.Tables[key]; ok {
			out.Tables[key] = ts
		}
	}
	return out
}

// excerptSQL trims a statement to a short single-line form for an
// error message.
func excerptSQL(s string) string {
	const max = 80
	out := strings.Join(strings.Fields(s), " ")
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}
