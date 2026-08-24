package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ErrSchemaRequired is returned by Push when schema is nil.
var ErrSchemaRequired = errors.New("drops/mysql: Push requires a non-nil schema")

// ErrForeignDatabase is returned by Push when PushOptions.Database
// names a database other than the one the connection is pointed at.
// See PushOptions.Database.
var ErrForeignDatabase = errors.New("drops/mysql: Push can only target the connection's own database")

// PushOptions tunes how Push applies schema changes.
type PushOptions struct {
	// Database names the database to push into. Empty means the
	// connection's current one, which is the only thing it can usefully
	// be: Diff renders every statement unqualified, so the DDL lands
	// wherever the connection points whatever this says. Naming a
	// second database would have Push read one and write another —
	// re-emitting the same CREATE TABLE for ever, into a database that
	// already has it under a different name — so Push checks the two
	// agree and refuses with ErrForeignDatabase when they do not.
	// [IntrospectOptions.Database] is the read-only form, and has no
	// such restriction.
	Database string

	// Safe adds IF [NOT] EXISTS where MySQL has it, which is CREATE
	// TABLE and DROP TABLE and nothing else. See DiffOptions.Safe.
	Safe bool

	// DryRun returns the statements that would be applied without
	// executing them.
	//
	// Unlike drops/pg's, this really is read-only as far as the
	// application's tables are concerned. Push still asks the server to
	// respell the declared CHECK expressions before it can tell whether
	// they changed, but it does that on a throwaway table of its own
	// rather than on yours — see probeCheckExpressions.
	DryRun bool

	// DropUnmanagedIndexes lets Push drop an index that exists in the
	// database and appears in no table of the Go schema. Off by
	// default; see Push's doc comment.
	DropUnmanagedIndexes bool

	// SplitAlters emits one ALTER TABLE per column change rather than
	// batching a table's changes. See DiffOptions.SplitAlters.
	SplitAlters bool

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
//
// It is returned even when Push returns an error, which is the whole
// point of AppliedCount: MySQL cannot roll a migration back, so
// "it failed" is not a complete answer and "it failed after four
// statements, on the fifth" is.
type PushResult struct {
	// Statements is the ordered SQL diff between the live database and
	// the supplied Go schema.
	Statements []string
	// Applied is true when every statement was executed successfully.
	Applied bool
	// AppliedCount is how many statements committed. On success it
	// equals len(Statements); on failure it is the index of the
	// statement that failed, and every statement before it has already
	// taken effect and cannot be undone by Push.
	AppliedCount int
	// Failed is the statement that failed, empty when none did.
	Failed string
	// Notices are the differences Push saw and did not act on. An empty
	// Statements list with a non-empty Notices list means the database
	// and the schema disagree about something Push declined to change.
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
	// "index-method-ignored", "check-not-normalised",
	// "checks-not-enforced", "table-options".
	Rule string
	// Table is the table the notice concerns.
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
//   - Reads the current state of the database via Introspect.
//   - Builds a target snapshot from schema.
//   - Narrows the live side to the tables the schema declares; see
//     ownedBy.
//   - Settles the rename questions the change raises, or refuses,
//     before anything below touches the server. See "A change that
//     could be a rename".
//   - Asks the server to respell the declared CHECK expressions, so
//     both sides of the diff are written in the server's own dialect.
//   - Diffs the two and, unless DryRun, applies the statements one at a
//     time in order.
//
// # A failed push leaves the schema half-changed
//
// This is the difference that matters most against drops/pg, and it is
// not something drops can paper over. MySQL has no transactional DDL:
// every CREATE, ALTER and DROP commits implicitly, before and after
// itself, and a ROLLBACK afterwards does nothing. drops/pg wraps a push
// in one transaction and a failure leaves the database exactly as it
// was; here, a push of six statements that fails on the fourth leaves
// the first three applied, for ever.
//
// So the contract is:
//
//   - the result is returned alongside the error, never instead of it;
//   - PushResult.AppliedCount says how many statements committed and
//     PushResult.Failed says which one did not;
//   - the fix is to correct the cause and push again. Push recomputes
//     the diff from the live schema every time, so the statements that
//     already applied are simply not in the next diff. That is what
//     makes re-running safe, rather than DiffOptions.Safe, which MySQL
//     barely supports.
//
// One ALTER TABLE is atomic in itself, so a batched statement that
// fails leaves none of its own actions applied. The migration around it
// is what is not atomic.
//
// # A change that could be a rename
//
// A column the database has and the schema does not, paired with one
// the schema has and the database does not, is either a rename or a
// drop-and-add, and the two differ by the whole contents of the
// column. Push cannot tell them apart any more than Diff can, so it
// asks the same question GenerateMigration asks and returns
// *RenameAmbiguityError rather than guessing — before any statement
// runs. There is no transaction around a push here, so the DROP COLUMN
// the guess would have emitted is a commit nobody can take back.
//
// Answer it in the schema with (*Col[T]).RenamedFrom or
// (*Table).RenamedFrom, which is durable and travels to every database
// the schema is pushed to, or for one run with PushOptions.Renames.
//
// # An index the schema never declared
//
// Push does not drop one. Diff does, and is right to: it compares two
// declarations, so an index missing from the newer one was removed.
// Push's "previous" side is a database, where an index missing from the
// Go schema was very likely never declared there — added by a DBA under
// load, or by another tool. Dropping it is a rebuild on a table big
// enough to have needed it. Set DropUnmanagedIndexes to take the other
// side of that trade; the withheld statements are reported as notices
// either way.
//
// # What Push cannot see
//
// Introspect reads back most, not all, of what a schema can hold, and
// Diff only compares what both sides carry. Push will therefore not
// notice a change to:
//
//   - a table's ENGINE, CHARSET or COLLATE. Changing any of them
//     rewrites every row, and the schema layer leaves them unset far
//     more often than it sets them; a mismatch is reported as a
//     "table-options" notice instead;
//   - a multi-column FOREIGN KEY, which the schema layer cannot declare
//     and Introspect skips;
//   - a functional index (MySQL 8.0.13+), whose key parts have no
//     column name behind them. It is reported as an
//     "unrepresentable-index" notice;
//   - a declared USING method the engine discarded — InnoDB takes
//     USING HASH and builds a B-tree — reported as an
//     "index-method-ignored" notice;
//   - anything at all about CHECK constraints on MySQL 5.7, which
//     parses and discards them — reported as "checks-not-enforced";
//   - a CHECK expression the server refused to respell, reported as
//     "check-not-normalised" and left alone rather than churned.
//
// Push is convenient for development but keeps no migration history.
// For production, prefer GenerateMigration so changes are reviewable,
// reproducible and — since a half-applied migration is a real
// possibility here — reversible by a human who can read them.
func Push(ctx context.Context, db *DB, schema *Schema, opts ...PushOptions) (*PushResult, error) {
	if schema == nil {
		return nil, ErrSchemaRequired
	}
	var opt PushOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	server, err := ServerVersion(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("drops/mysql: server version: %w", err)
	}
	if opt.Database != "" {
		session, err := currentDatabase(ctx, db)
		if err != nil {
			return nil, err
		}
		if !strings.EqualFold(session, opt.Database) {
			return nil, fmt.Errorf("%w: the connection is on %q and the statements are rendered unqualified, so a push naming %q would read one database and write the other",
				ErrForeignDatabase, session, opt.Database)
		}
	}
	live, err := Introspect(ctx, db, IntrospectOptions{Database: opt.Database})
	if err != nil {
		return nil, fmt.Errorf("drops/mysql: introspect: %w", err)
	}
	desired := BuildSnapshot(schema)
	answers := mergeDecisions(DeclaredRenames(schema), opt.Renames)
	current := ownedBy(live, desired, renamedAwayTables(live, answers)...)

	// Before the probe, not after it: a push that is going to refuse
	// should not first create and drop a table on the server to answer
	// a question it is about to throw away. Renames are settled from
	// column names and types, which the probe does not touch.
	renames, err := resolvePushRenames(current, desired, answers)
	if err != nil {
		return nil, err
	}

	notices, err := probeCheckExpressions(ctx, db, opt.Database, server, current, desired)
	if err != nil {
		return nil, fmt.Errorf("drops/mysql: normalise declared expressions: %w", err)
	}
	notices = append(notices, tableOptionNotices(current, desired)...)
	notices = append(notices, unrepresentableIndexNotices(current)...)
	notices = append(notices, indexMethodNotices(current, desired)...)

	stmts := Diff(current, desired, DiffOptions{
		Safe:        opt.Safe,
		Server:      server,
		SplitAlters: opt.SplitAlters,
		Renames:     renames,
	})
	if !opt.DropUnmanagedIndexes {
		var withheld []SchemaNotice
		stmts, withheld = withholdUnmanagedIndexDrops(stmts, current, desired)
		notices = append(notices, withheld...)
	}
	sortNotices(notices)

	res := &PushResult{Statements: stmts, Notices: notices}
	if len(stmts) == 0 || opt.DryRun {
		return res, nil
	}
	for _, s := range stmts {
		if _, err := db.Exec(ctx, s); err != nil {
			res.Failed = s
			return res, fmt.Errorf(
				"drops/mysql: push stopped after %d of %d statements, applying %q: %w",
				res.AppliedCount, len(stmts), excerptSQL(s), err)
		}
		res.AppliedCount++
	}
	res.Applied = true
	return res, nil
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
// exists to close — and here the door has no transaction behind it: a
// DROP COLUMN commits itself, so a wrong guess is not something a
// failed push rolls back.
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
	"    mysql.Add(Users, mysql.Varchar(\"emailAddress\", 255).RenamedFrom(\"email\"))\n" +
	"    mysql.NewTable(\"people\").RenamedFrom(\"users\")\n" +
	"stated there it answers every database the schema is pushed to, and it goes inert once the rename has happened.\n" +
	"For one run instead, or to say the column really is being dropped, use PushOptions.Renames: a\n" +
	"RenameDecision naming the pair renames it, one naming only the object that is going declines."

// currentDatabase returns the database the connection is pointed at,
// which is empty when the DSN named none — in which case unqualified
// DDL has nowhere to land and the server says so.
func currentDatabase(ctx context.Context, db *DB) (string, error) {
	rows, err := db.Query(ctx, "SELECT DATABASE()")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", fmt.Errorf("drops/mysql: DATABASE() returned no row")
	}
	var name sql.NullString
	if err := rows.Scan(&name); err != nil {
		return "", err
	}
	return name.String, rows.Err()
}

// ownedBy narrows a live introspection to the tables the Schema
// declares.
//
// A MySQL database is not a namespace a schema gets to itself: it is
// what the DSN names, so the application's tables sit beside another
// service's, a migration-history table, and whatever a previous tool
// left. To Diff, every one of those looks like a table that used to
// exist and no longer should, and it emits a DROP for it — which here
// deletes someone else's data with no transaction to undo it.
//
// drops/pg takes the opposite line, because a PostgreSQL schema can be
// given to one application and Push's Schema option points at exactly
// one. drops/sqlite takes this one, for the same reason as here.
//
// The cost is that dropping a table means writing the DROP into a
// migration rather than deleting the Go declaration and pushing, which
// is the reviewable path anyway.
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

// excerptSQL trims a statement to a short single-line form for error
// messages.
func excerptSQL(s string) string {
	const max = 80
	out := strings.Join(strings.Fields(s), " ")
	if len(out) > max {
		out = out[:max] + "…"
	}
	return out
}

// ----------------------------------------------------------------------
// Expression normalisation
// ----------------------------------------------------------------------

// probeCheckExpressions rewrites the declared CHECK expressions into
// the spelling the server itself would report, so Diff can compare them
// as text.
//
// This is the one thing only a server can do. Neither server echoes a
// CHECK back; each prints its own parse tree. `age >= 0` comes back
// "`age` >= 0" on MariaDB and "(`age` >= 0)" on MySQL, `a AND b` comes
// back with a lowercase "and", and IN lists lose their spaces. No
// amount of string tidying in Go reproduces that, and guessing produces
// a diff that either churns for ever or misses a real change.
//
// So the declared expression is handed to the server on a table drops
// creates for the purpose, read back through the same catalogue view
// Introspect reads, and the table dropped. drops/pg does this against
// the real table, using ADD CONSTRAINT … NOT VALID inside a transaction
// it rolls back, and pays an ACCESS EXCLUSIVE lock on every table
// carrying a CHECK for the length of the probe. Neither half of that
// trick is available here — MariaDB does not accept NOT ENFORCED at
// all, and there is no transaction to roll back — but an empty table of
// our own costs nothing to validate against and locks nothing of yours.
//
// A probe table is named _drops_probe_<random> and is dropped before
// the function returns, including on error. A process killed mid-probe
// leaves one behind; it holds no rows and is safe to drop by hand.
//
// Only expressions already present on both sides are probed: a
// constraint the database does not have yet is created from the
// declared spelling, and the server stores its own on the way in. A
// probe that fails leaves the desired side carrying the database's
// value, so the push does not churn, and returns a notice saying the
// expression went unchecked.
func probeCheckExpressions(ctx context.Context, db *DB, database string, server ServerInfo, current, desired *Snapshot) ([]SchemaNotice, error) {
	var notices []SchemaNotice
	if !server.SupportsCheckConstraints() {
		for _, key := range sortedKeys(desired.Tables) {
			dt := desired.Tables[key]
			for _, name := range sortedKeys(dt.CheckConstraints) {
				notices = append(notices, SchemaNotice{
					Rule:   "checks-not-enforced",
					Table:  dt.Name,
					Object: name,
					Message: fmt.Sprintf(
						"%s parses CHECK constraints and discards them, so %q on %q is neither stored nor enforced; drops declares it anyway and never sees it again",
						server, name, dt.Name),
				})
			}
			// Nothing will ever be read back, so the desired side has
			// to stop asking for it or every push emits the same ADD.
			dt.CheckConstraints = map[string]*CheckSnapshot{}
		}
		return notices, nil
	}

	for _, key := range sortedKeys(desired.Tables) {
		dt := desired.Tables[key]
		ct, ok := current.Tables[key]
		if !ok {
			continue
		}
		var names []string
		for _, name := range sortedKeys(dt.CheckConstraints) {
			if _, live := ct.CheckConstraints[name]; live {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		n, err := probeTableChecks(ctx, db, database, server, dt, ct, names)
		if err != nil {
			return nil, err
		}
		notices = append(notices, n...)
	}
	return notices, nil
}

// probeTableChecks respells one table's CHECK expressions, in place, on
// a throwaway copy of its column list.
func probeTableChecks(ctx context.Context, db *DB, database string, server ServerInfo, desired, current *TableSnapshot, names []string) (notices []SchemaNotice, err error) {
	probe := probeTableName()
	if err := createProbeTable(ctx, db, probe, desired); err != nil {
		return nil, err
	}
	defer func() {
		if _, derr := db.Exec(ctx, "DROP TABLE IF EXISTS "+quoteIdent(probe)); derr != nil && err == nil {
			err = fmt.Errorf("dropping probe table %q: %w", probe, derr)
		}
	}()

	// Probe constraints are named after the probe table so the answers
	// can be matched back to the declarations they came from. The
	// declared names cannot be reused, and neither can a bare "p0":
	// MySQL scopes a CHECK constraint name to the whole schema, so the
	// real table is already using the first and a second push running
	// at the same moment would collide over the second.
	applied := map[string]string{}
	for i, name := range names {
		dc := desired.CheckConstraints[name]
		alias := fmt.Sprintf("%s_p%d", probe, i)
		stmt := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s)",
			quoteIdent(probe), quoteIdent(alias), dc.Value)
		if _, perr := db.Exec(ctx, stmt); perr != nil {
			if !refusedExpression(perr) {
				return nil, fmt.Errorf("probing %q on %q: %w", name, desired.Name, perr)
			}
			notices = append(notices, SchemaNotice{
				Rule:   "check-not-normalised",
				Table:  desired.Name,
				Object: name,
				Message: fmt.Sprintf(
					"the server would not parse the declared expression %s (%v), so it was compared as the database already has it and any change to it will not be applied",
					dc.Value, perr),
			})
			dc.Value = current.CheckConstraints[name].Value
			continue
		}
		applied[alias] = name
	}
	if len(applied) == 0 {
		return notices, nil
	}

	read := EmptySnapshot()
	read.Tables[probe] = newTableSnapshot(probe)
	if err := readIntrospectChecks(ctx, db, database, read, server); err != nil {
		return nil, err
	}
	for alias, name := range applied {
		cs, ok := read.Tables[probe].CheckConstraints[alias]
		if !ok {
			return nil, fmt.Errorf("probe constraint %q on %q was not reported back by the server", alias, probe)
		}
		desired.CheckConstraints[name].Value = cs.Value
	}
	return notices, nil
}

// createProbeTable creates an empty table with the desired table's
// column names and types, and nothing else: no key, no default, no
// AUTO_INCREMENT. The expressions being probed refer to columns, so the
// columns have to exist and to have the right types; every other
// property of the table is irrelevant to how the server renders an
// expression over it, and each one left out is one fewer reason for the
// CREATE to fail.
func createProbeTable(ctx context.Context, db *DB, name string, t *TableSnapshot) error {
	cols := make([]string, 0, len(t.Columns))
	for _, k := range sortedKeys(t.Columns) {
		cols = append(cols, "\t"+quoteIdent(k)+" "+t.Columns[k].Type+" NULL")
	}
	if len(cols) == 0 {
		return fmt.Errorf("drops/mysql: table %q declares no columns", t.Name)
	}
	stmt := fmt.Sprintf("CREATE TABLE %s (\n%s\n)", quoteIdent(name), strings.Join(cols, ",\n"))
	if _, err := db.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("creating probe table for %q: %w", t.Name, err)
	}
	return nil
}

func probeTableName() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "_drops_probe_0"
	}
	return "_drops_probe_" + hex.EncodeToString(b[:])
}

// refusedExpression reports whether err is the server refusing the
// expression, as opposed to the statement never reaching a server that
// could have an opinion about it.
//
// The line is drawn at whether the server answered at all, and not at
// the SQLSTATE, because SQLSTATE does not separate the two here. A
// syntax error arrives as 42000 and an unknown column as 42S22, but
// almost everything a server says specifically about a CHECK arrives
// as the catch-all HY000: MariaDB 10.11 rejects a function it will not
// allow in a CHECK with error 1901 HY000, and MySQL's
// check-constraint errors sit in the same class. Reading only the 42,
// 22 and 0A classes therefore misses the whole family of rejections
// this notice exists for, and fails the push on them instead.
//
// What makes "the server answered" enough is where the probe runs. It
// is one ALTER TABLE against a table drops created a moment earlier,
// under a name nothing else knows, holding no rows — so the reasons a
// real ALTER fails without saying anything about its expression are
// gone: no other session can be holding a lock on it, there are no
// rows to violate the constraint, and an account that could not do DDL
// would have failed at the CREATE. A failure that never reached the
// server — a dropped connection, a cancelled context — carries no
// server error, and that is the case that still fails the push.
//
// The cost of being wrong in this direction is bounded: the check is
// compared as the database already holds it, so a change to it does
// not ship, and the notice quotes the server's own error rather than
// blaming the expression on drops's say-so.
func refusedExpression(err error) bool {
	return serverErrorNumber(err) != 0 || serverSQLState(err) != ""
}

// reDriverErrorNumber matches the error number in the message
// go-sql-driver/mysql renders — "Error 1901 (HY000): Function or …".
var reDriverErrorNumber = regexp.MustCompile(`^Error (\d+) \(`)

// serverErrorNumber digs the MySQL error number out of a driver error,
// by the same route as serverSQLState and for the same reason: drops
// has no dependencies, so the driver's error type cannot be named and
// exposes the number as a struct field rather than through a method.
func serverErrorNumber(err error) int {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if n := errorNumberByReflection(e); n != 0 {
			return n
		}
		if m := reDriverErrorNumber.FindStringSubmatch(e.Error()); m != nil {
			n, cerr := strconv.Atoi(m[1])
			if cerr == nil {
				return n
			}
		}
	}
	return 0
}

func errorNumberByReflection(err error) int {
	v := reflect.ValueOf(err)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return 0
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return 0
	}
	f := v.FieldByName("Number")
	if !f.IsValid() {
		return 0
	}
	switch f.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(f.Uint())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(f.Int())
	}
	return 0
}

// reDriverSQLState matches the SQLSTATE in the message
// go-sql-driver/mysql renders — "Error 1054 (42S22): Unknown column".
var reDriverSQLState = regexp.MustCompile(`^Error \d+ \(([0-9A-Za-z]{5})\)`)

// serverSQLState digs the SQLSTATE out of a driver error.
//
// drops has no dependencies, so it cannot name *mysql.MySQLError, and
// unlike pgx that type exposes its SQLSTATE as a struct field rather
// than through a method — there is no interface to assert. Reflection
// reads the field by name from whatever error the driver returned, and
// the message is parsed as a fallback for drivers that format one
// without carrying it separately.
func serverSQLState(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if s := sqlStateByReflection(e); s != "" {
			return s
		}
		if m := reDriverSQLState.FindStringSubmatch(e.Error()); m != nil {
			return m[1]
		}
	}
	return ""
}

func sqlStateByReflection(err error) string {
	v := reflect.ValueOf(err)
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return ""
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return ""
	}
	f := v.FieldByName("SQLState")
	if !f.IsValid() {
		return ""
	}
	switch f.Kind() {
	case reflect.String:
		return f.String()
	case reflect.Array, reflect.Slice:
		if f.Type().Elem().Kind() != reflect.Uint8 || f.Len() != 5 {
			return ""
		}
		out := make([]byte, 5)
		for i := range out {
			out[i] = byte(f.Index(i).Uint())
		}
		return string(out)
	}
	return ""
}

// ----------------------------------------------------------------------
// Notices
// ----------------------------------------------------------------------

// withholdUnmanagedIndexDrops removes from stmts every DROP INDEX that
// targets an index the database has, the Go schema never declared, and
// this push can actually leave standing — returning what is left and a
// notice for each one held back.
//
// The statements are matched by text rather than re-derived, because
// the text is what Diff produced from the same two snapshots a moment
// earlier — anything that does not match is a drop Diff emitted for a
// different reason (an index declared on both sides whose shape
// changed) and has to go through.
//
// # Why an index over a departing column is not withheld
//
// Withholding exists because Push cannot tell an index the schema
// stopped declaring from one somebody built by hand, and destroying the
// second by mistake is worse than leaving the first behind. That
// reasoning holds only while the index can survive the migration. An
// index spanning a column this push drops cannot: MySQL takes a
// single-column one away with the column, narrows a multi-column one
// to what remains, and refuses the DROP COLUMN outright when the index
// is UNIQUE and spans more than one column (1072) or is one a foreign
// key needs (1553). See Diff.
//
// So withholding there protects nothing. It either leaves a narrowed
// index the caller never asked for — under a notice claiming the index
// was left alone, which by then is false — or it stops the push on a
// server error with no explanation attached. Both were reachable under
// the default options, which is where most pushes run. The drop goes
// through instead, and the notice says the index was dropped and why,
// so the fact still reaches the caller.
func withholdUnmanagedIndexDrops(stmts []string, current, desired *Snapshot) ([]string, []SchemaNotice) {
	withheld := map[string]SchemaNotice{}
	var forced []SchemaNotice
	for _, key := range sortedKeys(current.Tables) {
		ct := current.Tables[key]
		dt := desired.Tables[key]
		for _, name := range sortedKeys(ct.Indexes) {
			// An index name is scoped to its table in MySQL, so the
			// question is whether this table declares it — not
			// whether any table does, as it would be in PostgreSQL.
			if dt != nil {
				if _, ok := dt.Indexes[name]; ok {
					continue
				}
			}
			sql := dropIndexSQL(ct.Name, name)
			if lost := departingColumns(ct.Indexes[name], ct, dt); len(lost) > 0 {
				forced = append(forced, SchemaNotice{
					Rule:   "unmanaged-index",
					Table:  ct.Name,
					Object: name,
					Message: fmt.Sprintf(
						"index %q on %q is declared by no table in the Go schema, but it keys column %s, which this push drops; MySQL will not leave it as it is, so Push dropped the index rather than leave a narrowed one behind or stop on the column drop",
						name, ct.Name, strings.Join(quoteIdents(lost), ", ")),
					SQL: sql,
				})
				continue
			}
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
		return stmts, forced
	}
	kept := make([]string, 0, len(stmts))
	notices := forced
	for _, s := range stmts {
		if n, ok := withheld[s]; ok {
			notices = append(notices, n)
			continue
		}
		kept = append(kept, s)
	}
	return kept, notices
}

// departingColumns lists, in key order, the index's columns that this
// push drops. A table the schema no longer declares is not one Push
// narrows, so a nil desired side means nothing is departing.
func departingColumns(idx *IndexSnapshot, current, desired *TableSnapshot) []string {
	if idx == nil || desired == nil {
		return nil
	}
	var out []string
	for _, c := range idx.Columns {
		if _, live := current.Columns[c]; !live {
			continue
		}
		if _, kept := desired.Columns[c]; !kept {
			out = append(out, c)
		}
	}
	return out
}

// unrepresentableIndexNotices reports live indexes the snapshot cannot
// describe: at least one key part was an expression rather than a
// column, which MySQL 8.0.13 allows and which leaves the snapshot with
// no columns to compare or to render.
func unrepresentableIndexNotices(current *Snapshot) []SchemaNotice {
	var out []SchemaNotice
	for _, key := range sortedKeys(current.Tables) {
		ct := current.Tables[key]
		for _, name := range sortedKeys(ct.Indexes) {
			if len(ct.Indexes[name].Columns) > 0 {
				continue
			}
			out = append(out, SchemaNotice{
				Rule:   "unrepresentable-index",
				Table:  ct.Name,
				Object: name,
				Message: fmt.Sprintf(
					"index %q on %q has at least one key part that is an expression rather than a column; the snapshot cannot describe it, so Push compares it against nothing — declare it with mysql.NewIndex only if you mean to replace it",
					name, ct.Name),
			})
		}
	}
	return out
}

// indexMethodNotices reports a declared USING method the engine did
// not adopt.
//
// InnoDB accepts USING HASH on a CREATE INDEX and builds a B-tree
// anyway, without a warning, so the request is neither honoured nor
// refused — it simply is not there afterwards. Diff cannot act on it
// (see indexMethodEqual: recreating the index produces the same
// B-tree, so the difference would never close) and silence would leave
// the schema saying one thing and the server doing another, which is
// the case this whole list exists for.
func indexMethodNotices(current, desired *Snapshot) []SchemaNotice {
	var out []SchemaNotice
	for _, key := range sortedKeys(desired.Tables) {
		dt := desired.Tables[key]
		ct, ok := current.Tables[key]
		if !ok {
			continue
		}
		for _, name := range sortedKeys(dt.Indexes) {
			di := dt.Indexes[name]
			ci, live := ct.Indexes[name]
			if !live || di.Method == "" || ci.Method == di.Method {
				continue
			}
			out = append(out, SchemaNotice{
				Rule:   "index-method-ignored",
				Table:  dt.Name,
				Object: name,
				Message: fmt.Sprintf(
					"index %q on %q is declared USING %s and the server built a %s one; the engine accepted the clause and discarded it, so recreating the index would change nothing",
					name, dt.Name, di.Method, orElse(ci.Method, "BTREE")),
			})
		}
	}
	return out
}

// tableOptionNotices reports a declared ENGINE, CHARSET or COLLATE that
// the live table does not have. Push does not change any of them: each
// is a full table copy, and a schema that leaves them unset — which is
// the common case — must not be read as asking for the server's
// defaults to be replaced.
func tableOptionNotices(current, desired *Snapshot) []SchemaNotice {
	var out []SchemaNotice
	for _, key := range sortedKeys(desired.Tables) {
		dt := desired.Tables[key]
		ct, ok := current.Tables[key]
		if !ok {
			continue
		}
		var diffs []string
		if dt.Engine != "" && !strings.EqualFold(dt.Engine, ct.Engine) {
			diffs = append(diffs, fmt.Sprintf("ENGINE is %q, schema says %q", ct.Engine, dt.Engine))
		}
		if dt.Charset != "" && !strings.EqualFold(dt.Charset, ct.Charset) {
			diffs = append(diffs, fmt.Sprintf("CHARSET is %q, schema says %q", ct.Charset, dt.Charset))
		}
		if dt.Collation != "" && !strings.EqualFold(dt.Collation, ct.Collation) {
			diffs = append(diffs, fmt.Sprintf("COLLATE is %q, schema says %q", ct.Collation, dt.Collation))
		}
		if len(diffs) == 0 {
			continue
		}
		out = append(out, SchemaNotice{
			Rule:  "table-options",
			Table: dt.Name,
			Message: fmt.Sprintf("table %q: %s; changing any of them copies every row, so Push left them alone",
				dt.Name, strings.Join(diffs, "; ")),
			SQL: fmt.Sprintf("ALTER TABLE %s ENGINE=%s;", quoteIdent(dt.Name), orElse(dt.Engine, ct.Engine)),
		})
	}
	return out
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
