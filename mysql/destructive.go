package mysql

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// The data-loss gate.
//
// Push refuses a statement that destroys data unless somebody said that
// exact change could be made. Not a flag — a NAMED change: "you may drop
// users.email", not "you may destroy things". The distinction is the
// whole mechanism. A blanket permission is set once, in a script, by
// somebody reasoning about the change in front of them that day, and it
// then stands over every change the schema makes afterwards; the flag
// that authorised dropping a column nobody wanted authorises dropping
// the column somebody did.
//
// So consent is per change and it goes stale loudly: an Allow entry that
// authorises nothing this push found is reported as a notice rather than
// discarded, because a call site that reads exactly like one whose DROP
// is still permitted, and is not, is the more dangerous of the two.
//
// # Rows decide, not statements
//
// A DROP COLUMN on an empty table destroys nothing, and the development
// loop is full of them: add a column, change your mind, push again. So
// the gate asks the database how many rows are actually there and lets
// the change through when the answer is none — TABLE_ROWS first,
// because it is free, and an emptiness probe when the server has no
// figure at all or publishes zero.
//
// Zero is probed here and taken at its word in drops/pg, and the
// difference is InnoDB rather than a preference: TABLE_ROWS is sampled
// from the index and the sample reports 0 for a small table that is not
// empty. Trusting it would let a DROP COLUMN through against rows.

// DestructiveOp names a kind of change that loses data.
//
// The three are what Diff can emit that no rollback undoes. A DROP INDEX
// is not here: rebuilding it costs time and loses nothing.
type DestructiveOp string

const (
	// OpDropTable is DROP TABLE. Object is empty: the table IS the
	// object, and a consent naming one is a consent that can never
	// match — see [Push] and the stale-consent notice.
	OpDropTable DestructiveOp = "drop-table"
	// OpDropColumn is ALTER TABLE ... DROP COLUMN.
	OpDropColumn DestructiveOp = "drop-column"
	// OpRetypeColumn is ALTER TABLE ... MODIFY COLUMN with a new type,
	// which rewrites every value through a conversion that may not
	// round-trip — and which MySQL will happily truncate rather than
	// refuse.
	OpRetypeColumn DestructiveOp = "retype-column"
)

// Destructive is one change that loses data — the same value in both
// directions. Push reports what it found in [PushResult.DataLoss], and a
// caller authorises one by handing the same Op, Table and Object back in
// [PushOptions.Allow].
//
// Only those three are matched. Rows, SQL and Suggestion are what Push
// tells you about the change; requiring a caller to reproduce a row
// count would make a consent expire on the next INSERT.
type Destructive struct {
	// Op is the kind of change.
	Op DestructiveOp
	// Table is the table losing data, unqualified.
	Table string
	// Object is the column, and empty for a table drop.
	Object string

	// Rows is what the table holds: the server's TABLE_ROWS estimate,
	// and -1 for a table it published no usable figure for and the
	// probe found rows in. It is a count of the table's rows, not of
	// the values the change destroys — which for a column drop is the
	// same number.
	Rows int64
	// SQL is the statement that would have run.
	SQL string
	// Suggestion is the consent that would let it run.
	Suggestion string
}

// consentKey is the triple a consent matches on.
func (d Destructive) consentKey() [3]string {
	return [3]string{string(d.Op), d.Table, d.Object}
}

// ErrDestructivePush is returned when a push would destroy data that no
// [PushOptions.Allow] entry authorises. Nothing was applied — not the
// destructive statements and not the harmless ones beside them, because
// a partially applied schema change is the one outcome worse than a
// refused one.
//
// [PushResult.DataLoss] on the returned result carries what was refused,
// with the statement and the consent that would permit it.
var ErrDestructivePush = errors.New("drops/mysql: push would destroy data no consent authorises")

// DestructivePushError is that refusal with the findings attached, for a
// caller that wants them from the error rather than from the result.
type DestructivePushError struct {
	// Changes is everything the push would have destroyed, in the order
	// the diff produced it.
	Changes []Destructive
}

func (e *DestructivePushError) Error() string {
	parts := make([]string, 0, len(e.Changes))
	for _, c := range e.Changes {
		where := c.Table
		if c.Object != "" {
			where += "." + c.Object
		}
		parts = append(parts, fmt.Sprintf("%s on %s (%s)", c.Op, where, rowsPhrase(c.Rows)))
	}
	return fmt.Sprintf("%v: %s", ErrDestructivePush, strings.Join(parts, "; "))
}

// Unwrap makes errors.Is(err, ErrDestructivePush) true.
func (e *DestructivePushError) Unwrap() error { return ErrDestructivePush }

// rowsPhrase says what the row count means, since -1 is not a count.
func rowsPhrase(rows int64) string {
	if rows < 0 {
		return "never analysed, and not empty"
	}
	return fmt.Sprintf("~%d rows", rows)
}

// The statements Diff emits for the three operations. Reading the plan
// rather than re-deriving it from the snapshots is deliberate: what
// matters is what would RUN, and a column the diff treats as renamed —
// or a table ownedBy held back — never reaches this list at all.
var (
	reDropTablePlan = regexp.MustCompile("(?is)^\\s*DROP\\s+TABLE\\s+(?:IF\\s+EXISTS\\s+)?`([^`]+)`")
	// The column actions are matched ANYWHERE in the statement rather
	// than at its head, because MySQL batches: one ALTER TABLE carries
	// the ADD COLUMN and the DROP COLUMN together, and a rule that read
	// only the first action would either withhold the addition or let
	// the drop through beside it.
	reAlterTablePlan = regexp.MustCompile("(?is)^\\s*ALTER\\s+TABLE\\s+`([^`]+)`")
	reDropColAction  = regexp.MustCompile("(?is)\\bDROP\\s+COLUMN\\s+`([^`]+)`")
	reRetypeAction   = regexp.MustCompile("(?is)\\bMODIFY\\s+COLUMN\\s+`([^`]+)`")
)

// destructiveChanges reads the plan and returns what in it loses data,
// in plan order and without row counts.
func destructiveChanges(stmts []string) []Destructive {
	var out []Destructive
	for _, s := range stmts {
		if m := reDropTablePlan.FindStringSubmatch(s); m != nil {
			out = append(out, Destructive{Op: OpDropTable, Table: m[1], SQL: s})
			continue
		}
		alter := reAlterTablePlan.FindStringSubmatch(s)
		if alter == nil {
			continue
		}
		table := alter[1]
		for _, m := range reDropColAction.FindAllStringSubmatch(s, -1) {
			out = append(out, Destructive{Op: OpDropColumn, Table: table, Object: m[1], SQL: s})
		}
		for _, m := range reRetypeAction.FindAllStringSubmatch(s, -1) {
			out = append(out, Destructive{Op: OpRetypeColumn, Table: table, Object: m[1], SQL: s})
		}
	}
	for i := range out {
		out[i].Suggestion = out[i].suggestion()
	}
	return out
}

// suggestion is the consent that would authorise this change, spelled
// so it can be pasted.
func (d Destructive) suggestion() string {
	object := ""
	if d.Object != "" {
		object = fmt.Sprintf(", Object: %q", d.Object)
	}
	verb := "permit it"
	switch d.Op {
	case OpDropTable:
		verb = "keep the table"
	case OpDropColumn:
		verb = "keep the column"
	case OpRetypeColumn:
		verb = "convert the values yourself in a migration"
	}
	return fmt.Sprintf(
		"pass PushOptions{Allow: []Destructive{{Op: %s, Table: %q%s}}} to %s, or declare it in the Go schema to %s",
		opConstName(d.Op), d.Table, object, "permit this one change", verb)
}

// opConstName spells an op as the constant a caller would type.
func opConstName(op DestructiveOp) string {
	switch op {
	case OpDropTable:
		return "mysql.OpDropTable"
	case OpDropColumn:
		return "mysql.OpDropColumn"
	case OpRetypeColumn:
		return "mysql.OpRetypeColumn"
	}
	return fmt.Sprintf("%q", string(op))
}

// withRowCounts fills in Rows and drops the changes that destroy
// nothing, because the table they touch is empty.
//
// One estimate query for the whole schema, then a probe per table the
// estimate has no opinion about. A table that is genuinely empty is the
// common case in development and the rare one in production, so the
// order matters: the free answer first.
func withRowCounts(ctx context.Context, db *DB, database string, changes []Destructive) ([]Destructive, error) {
	if len(changes) == 0 {
		return nil, nil
	}
	estimates, err := rowEstimates(ctx, db, database)
	if err != nil {
		return nil, err
	}
	probed := map[string]bool{}
	out := make([]Destructive, 0, len(changes))
	for _, c := range changes {
		rows, known := estimates[c.Table]
		if known && rows > 0 {
			c.Rows = rows
			out = append(out, c)
			continue
		}
		// No figure, or a figure of zero. Both go to the probe: on
		// InnoDB the estimate is sampled from the index and reports 0
		// for a small table that is not empty, so taking zero at its
		// word would let a DROP COLUMN through against rows.
		hasRows, ok := probed[c.Table]
		if !ok {
			hasRows, err = tableHasRows(ctx, db, database, c.Table)
			if err != nil {
				return nil, err
			}
			probed[c.Table] = hasRows
		}
		if !hasRows {
			// Nothing to lose, nothing to consent to — this is the
			// change the development loop makes all day, and a gate
			// that stopped for it would be turned off.
			continue
		}
		// Kept as -1: the table holds rows and nothing knows how many.
		// Reporting a made-up number would be worse than reporting
		// that nobody has counted.
		c.Rows = -1
		out = append(out, c)
	}
	return out, nil
}

// rowEstimates reads TABLE_ROWS for every base table in the database.
//
// TABLE_ROWS is NULL for a table the server has no statistics for, and
// a NULL is left OUT of the map rather than stored as zero: the two
// mean opposite things here, and the caller probes for both.
func rowEstimates(ctx context.Context, db *DB, database string) (map[string]int64, error) {
	rows, err := db.Query(ctx, `SELECT TABLE_NAME, TABLE_ROWS
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = COALESCE(NULLIF(?, ''), DATABASE()) AND TABLE_TYPE = 'BASE TABLE'`, database)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var name string
		var est *int64
		if err := rows.Scan(&name, &est); err != nil {
			return nil, err
		}
		if est != nil {
			out[name] = *est
		}
	}
	return out, rows.Err()
}

// tableHasRows answers the one question the estimate could not, and
// answers it in constant time: EXISTS stops at the first row rather
// than counting to a number nobody needs.
func tableHasRows(ctx context.Context, db *DB, database, table string) (bool, error) {
	// Database-qualified when Push was given one, so the probe asks
	// about the table the DDL will land on rather than whichever one
	// the session's current database resolves the bare name to.
	name := quoteIdent(table)
	if database != "" {
		name = quoteIdent(database) + "." + name
	}
	rows, err := db.Query(ctx, `SELECT EXISTS (SELECT 1 FROM `+name+` LIMIT 1)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var has bool
	if err := rows.Scan(&has); err != nil {
		return false, err
	}
	return has, rows.Err()
}

// splitConsent separates the changes nobody authorised from the ones
// somebody did, and reports the consents that authorised nothing.
//
// A consent matches at most one change: two identical entries are one
// answer given twice, and the second is stale like any other that names
// nothing left to name.
func splitConsent(changes, allow []Destructive) (unconsented, consented, stale []Destructive) {
	remaining := map[[3]string]int{}
	for _, c := range changes {
		remaining[c.consentKey()]++
	}
	used := map[[3]string]int{}
	for _, a := range allow {
		k := a.consentKey()
		if used[k] < remaining[k] {
			used[k]++
			continue
		}
		stale = append(stale, a)
	}
	granted := map[[3]string]int{}
	for _, c := range changes {
		k := c.consentKey()
		if granted[k] < used[k] {
			granted[k]++
			consented = append(consented, c)
			continue
		}
		unconsented = append(unconsented, c)
	}
	return unconsented, consented, stale
}

// staleConsentNotices reports every consent that authorised nothing.
//
// Silence here was the failure this answers: a stale Allow entry looks
// exactly like a live one at the call site, so the day the change it
// named comes back — a column re-added and re-dropped, a table
// recreated — it authorises a drop nobody reconsidered.
func staleConsentNotices(stale []Destructive) []SchemaNotice {
	out := make([]SchemaNotice, 0, len(stale))
	for _, s := range stale {
		where := s.Table
		if s.Object != "" {
			where += "." + s.Object
		}
		out = append(out, SchemaNotice{
			Rule:   "stale-consent",
			Table:  s.Table,
			Object: s.Object,
			Message: fmt.Sprintf(
				"the consent for %s on %s authorises nothing this push found: the change has already been applied, the object is gone, or it was written with an Object a %s never carries. Remove it — a consent that matches nothing reads at the call site exactly like one that still does.",
				s.Op, where, s.Op),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table < out[j].Table
		}
		return out[i].Object < out[j].Object
	})
	return out
}
