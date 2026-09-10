package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The data-loss gate, and the two halves SQLite adds to it.
//
// Push refuses a change that destroys data unless somebody said that
// exact change could be made. Not a flag — a NAMED change: "you may
// drop users.email", not "you may destroy things". The distinction is
// the whole mechanism. A blanket permission is set once, in a script,
// by somebody reasoning about the change in front of them that day, and
// it then stands over every change the schema makes afterwards; the
// flag that authorised dropping a column nobody wanted authorises
// dropping the column somebody did.
//
// So consent is per change and it goes stale loudly: an Allow entry
// that authorises nothing this push found is reported as a notice
// rather than discarded, because a call site that reads exactly like
// one whose drop is still permitted, and is not, is the more dangerous
// of the two.
//
// # Rows decide, not statements
//
// A dropped column on an empty table destroys nothing, and the
// development loop is full of them: add a column, change your mind,
// push again. A gate that stops for those teaches the reflex of
// setting AllowDestructive, after which it is no longer protecting the
// case it exists for. So the gate asks how many rows are actually
// there and lets the change through when the answer is none.
//
// SQLite counts exactly, where drops/pg reads a planner estimate and
// drops/mysql reads TABLE_ROWS. That is not a preference: SQLite
// publishes no row statistics at all — sqlite_stat1 exists only after
// an ANALYZE somebody ran, and says nothing about a table it does not
// cover. What SQLite has instead is the property that makes the exact
// count affordable. The database is a local file, and the change being
// gated is a table REBUILD: a CREATE, an INSERT ... SELECT over every
// row, a DROP and a RENAME. Counting the rows is strictly cheaper than
// the operation it is deciding about, so there is nothing to trade.
//
// # Why the gate matters more here than anywhere else
//
// On PostgreSQL a destructive change is a DROP COLUMN — a statement
// that says what it does, in a migration a reviewer can read. On
// SQLite ALTER TABLE cannot do it, so the same change is a rebuild,
// and a rebuild that loses half the table is spelled exactly like one
// that widens it. The column that is going is simply absent from the
// INSERT's column list. There is no statement to classify and no line
// in the diff to catch the eye, which is why the fact has to be read
// out of the two snapshots and gated here.

// ErrDestructivePush is returned when a push would destroy data that no
// consent authorises. Nothing was applied — not the destructive
// statements and not the harmless ones beside them, because on SQLite
// they are the same four statements, and a rebuild that stopped halfway
// is the one outcome worse than a refused one.
//
// [DestructivePushError] carries the findings. It unwraps to this, so
// errors.Is reaches it either way.
var ErrDestructivePush = errors.New("drops/sqlite: push would destroy data no consent authorises")

// consentKey is the triple a consent matches on.
//
// Rule, Table and Object — never Message, Rows or Suggestion. Those are
// what Push tells you ABOUT the change, and requiring a caller to
// reproduce a row count would make a consent expire on the next INSERT.
func (c DestructiveChange) consentKey() [3]string {
	return [3]string{c.Rule, c.Table, c.Object}
}

// countedRules are the changes whose cost is the rows.
//
// The three mirror drops/pg's OpDropTable, OpDropColumn and
// OpRetypeColumn, and they are the only ones an empty table acquits.
// The other rules this package raises are deliberately not here:
//
//   - alter-column-set-not-null, add-unique-constraint and
//     add-check-constraint are already put to the rows by
//     confirmAgainstTheRows, which discards the ones the data acquits
//     before they ever reach this stage. An empty table answers zero to
//     all three there, so counting them again would be asking twice.
//   - rebuild-drops-index and rebuild-stale-trigger lose a schema
//     object, not rows. An index the rebuild does not put back is just
//     as missing from an empty table as from a full one, so emptiness
//     is not an acquittal for them and they are refused regardless of
//     the count.
var countedRules = map[string]bool{
	"drop-table":        true,
	"drop-column":       true,
	"alter-column-type": true,
}

// withRowCounts fills in Rows and drops the changes that destroy
// nothing.
//
// A change against a table that holds no rows is not a change anybody
// needs to authorise: there is no data to lose. Dropping it here rather
// than reporting it and letting the caller judge is deliberate — the
// finding a caller waves through is the finding they stop reading.
//
// A count that will not run is never an acquittal. The change is kept
// with Rows = -1 and says it could not be counted, which is the same
// answer drops/pg gives for a table its planner had no figure for: the
// number is unknown, the loss is not.
func withRowCounts(ctx context.Context, db *DB, changes []DestructiveChange, renames []Rename) []DestructiveChange {
	out := make([]DestructiveChange, 0, len(changes))
	for _, c := range changes {
		if !countedRules[c.Rule] {
			out = append(out, c)
			continue
		}
		table, _ := liveNames(renames, c.Table, c.Object)
		n, err := countRows(ctx, db, "SELECT count(*) FROM "+quoteIdent(table))
		switch {
		case err != nil:
			c.Rows = -1
			c.Message += " The rows could not be counted (" + err.Error() +
				"), so how much this destroys is unknown."
		case n == 0:
			// Nothing to lose, so nothing to authorise.
			continue
		default:
			c.Rows = n
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rowsPhrase says what the count means, since -1 is not a count and 0
// never reaches a reader — a change against an empty table was dropped
// upstream rather than reported as costing nothing.
func rowsPhrase(rows int64) string {
	switch {
	case rows < 0:
		return "row count unknown"
	case rows == 1:
		return "1 row"
	default:
		return fmt.Sprintf("%d rows", rows)
	}
}

// splitByConsent divides the findings into the ones a consent
// authorises and the ones nothing does, and reports the consents that
// matched nothing.
//
// Consents are counted rather than merely matched, so two identical
// entries authorise two identical changes and one authorises one. That
// case does not arise from a diff today — a column is dropped once —
// but the alternative is a rule that silently means "any number of",
// which is not what a reader of the call site would take it to mean.
func splitByConsent(changes, allow []DestructiveChange, blanket bool) (refused, consented, stale []DestructiveChange) {
	if blanket {
		// AllowDestructive is the older, coarser answer and it still
		// means what it always did. The per-change entries are not
		// consulted, so none of them can go stale under it: a caller
		// who set both has already said yes to everything, and
		// reporting their entries as unused would be telling them off
		// for belt and braces.
		return nil, changes, nil
	}
	granted := map[[3]string]int{}
	for _, a := range allow {
		granted[a.consentKey()]++
	}
	used := map[[3]string]int{}
	for _, c := range changes {
		k := c.consentKey()
		if used[k] < granted[k] {
			used[k]++
			consented = append(consented, c)
			continue
		}
		refused = append(refused, c)
	}
	for _, a := range allow {
		k := a.consentKey()
		if used[k] > 0 {
			used[k]--
			continue
		}
		stale = append(stale, a)
	}
	return refused, consented, stale
}

// staleConsentNotices reports every consent that authorised nothing.
//
// Silence here was the failure this answers: a stale Allow entry looks
// exactly like a live one at the call site, so the day the change it
// named comes back — a column re-added and re-dropped, a table
// recreated — it authorises a drop nobody reconsidered.
func staleConsentNotices(stale []DestructiveChange) []SchemaNotice {
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
				s.Rule, where, s.Rule),
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

// SchemaNotice is something Push saw and did not act on.
//
// It is the same shape as drops/pg's and drops/mysql's, and carries the
// same rules where the meaning is the same, so a caller that renders
// one renders all three.
type SchemaNotice struct {
	// Rule is a stable identifier for the kind of notice.
	Rule string
	// Table is the table the notice concerns.
	Table string
	// Object is the column, index or trigger, where one applies.
	Object string
	// Message says what was seen and what was not done about it.
	Message string
}

// String renders the notice as "rule: message".
func (n SchemaNotice) String() string { return n.Rule + ": " + n.Message }

// consentSuggestion renders the Go literal that would authorise a
// change, for a message the reader can paste.
func consentSuggestion(c DestructiveChange) string {
	var b strings.Builder
	b.WriteString("sqlite.PushOptions{Allow: []sqlite.DestructiveChange{{Rule: ")
	b.WriteString(strconv.Quote(c.Rule))
	b.WriteString(", Table: ")
	b.WriteString(strconv.Quote(c.Table))
	if c.Object != "" {
		b.WriteString(", Object: ")
		b.WriteString(strconv.Quote(c.Object))
	}
	b.WriteString("}}}")
	return b.String()
}
