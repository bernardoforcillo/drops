package sqlite_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/sqlite"
)

// DestructiveChanges reads the diff rather than the SQL. The rules it
// shares with Push are exercised against a live engine in the
// integration suite, because what they claim — that the copy converts
// this value, that the engine rejects that one — is the engine's
// behaviour and not drops'. What is left over here is the part that is
// pure: which findings the two snapshots produce, and one rule Push
// itself can never reach.

func snapshotOf(tables ...*sqlite.Table) *sqlite.Snapshot {
	return sqlite.BuildSnapshot(sqlite.NewSchema(tables...))
}

func rules(changes []sqlite.DestructiveChange) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Rule+" "+c.Object)
	}
	return out
}

// A whole table going is a drop-table, and Push never sees one: it
// diffs against the tables the schema declares, so a table the schema
// stopped naming is somebody else's and is left alone. The rule is
// here for the callers that diff two snapshots directly, and it is
// tested here because no live push can reach it.
func TestDestructiveChangesReportsADroppedTable(t *testing.T) {
	users := sqlite.NewTable("users")
	sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())

	got := sqlite.DestructiveChanges(snapshotOf(users), snapshotOf())
	if len(got) != 1 || got[0].Rule != "drop-table" || got[0].Table != "users" {
		t.Fatalf("got %v, want one drop-table on users", rules(got))
	}
	if !strings.Contains(got[0].Message, "every row") {
		t.Errorf("the message does not say what goes: %s", got[0].Message)
	}
}

// A schema that has not changed destroys nothing. Obvious, and worth
// pinning: a guard that fired on an empty diff would make every push
// need the permission.
func TestDestructiveChangesIsSilentOnAnUnchangedSchema(t *testing.T) {
	users := sqlite.NewTable("users")
	sqlite.Add(users, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(users, sqlite.Text("email"))

	if got := sqlite.DestructiveChanges(snapshotOf(users), snapshotOf(users)); len(got) != 0 {
		t.Fatalf("got %v, want nothing", rules(got))
	}
}

// The findings come out in a fixed order for a fixed pair of snapshots,
// because a refusal a caller prints should not shuffle between runs.
func TestDestructiveChangesIsDeterministic(t *testing.T) {
	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("alpha"))
	sqlite.Add(before, sqlite.Text("beta"))
	sqlite.Add(before, sqlite.Text("gamma"))

	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())

	want := []string{"drop-column alpha", "drop-column beta", "drop-column gamma"}
	for i := 0; i < 5; i++ {
		got := rules(sqlite.DestructiveChanges(snapshotOf(before), snapshotOf(after)))
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("run %d: got %v, want %v", i, got, want)
		}
	}
}

// A stated rename is not a drop, and the analyser has to be told the
// same renames Diff is told for the two to agree on what the migration
// means.
func TestDestructiveChangesFollowsAStatedRename(t *testing.T) {
	before := sqlite.NewTable("users")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("email"))

	after := sqlite.NewTable("users")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("emailAddress"))

	prev, cur := snapshotOf(before), snapshotOf(after)
	if got := sqlite.DestructiveChanges(prev, cur); len(got) != 1 || got[0].Rule != "drop-column" {
		t.Fatalf("unstated: got %v, want one drop-column", rules(got))
	}
	stated := sqlite.DiffOptions{Renames: []sqlite.Rename{
		{Kind: sqlite.RenameColumn, Table: "users", From: "email", To: "emailAddress"},
	}}
	if got := sqlite.DestructiveChanges(prev, cur, stated); len(got) != 0 {
		t.Fatalf("stated: got %v, want nothing", rules(got))
	}
}

// The two tightening rules, as far as the snapshots can settle them:
// which candidates come out, under which names. Whether the rows
// actually contradict them is the live half, and it is in the
// integration suite — this is the part that would still be true with no
// database in the room.
func TestDestructiveChangesReportsNewConstraintsAsCandidates(t *testing.T) {
	before := sqlite.NewTable("seats")
	sqlite.Add(before, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(before, sqlite.Text("code"))
	sqlite.Add(before, sqlite.Text("row"))
	sqlite.Add(before, sqlite.BigInt("number"))

	after := sqlite.NewTable("seats")
	sqlite.Add(after, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(after, sqlite.Text("code").Unique())
	row := sqlite.Add(after, sqlite.Text("row"))
	number := sqlite.Add(after, sqlite.BigInt("number"))
	after.AddUnique("seats_row_number", row, number)
	after.AddCheck("seats_number_positive", `"number" > 0`)

	got := sqlite.DestructiveChanges(snapshotOf(before), snapshotOf(after))
	want := []string{
		"add-unique-constraint code",
		"add-unique-constraint seats_row_number",
		"add-check-constraint seats_number_positive",
	}
	if strings.Join(rules(got), ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v", rules(got), want)
	}
	// The suggestion is the query that finds the rows in the way, so a
	// reader can settle it themselves rather than only being told no.
	if !strings.Contains(got[1].Suggestion, `GROUP BY "row", "number"`) {
		t.Errorf("the composite suggestion does not name both columns: %s", got[1].Suggestion)
	}
	if !strings.Contains(got[2].Suggestion, `WHERE NOT ("number" > 0)`) {
		t.Errorf("the check suggestion does not carry the expression: %s", got[2].Suggestion)
	}
}

// A constraint that was already there is not a new one, and a CHECK
// re-declared with the same expression is the same CHECK. Otherwise
// every push of an unchanged schema would carry a candidate and the
// probes would be run for nothing.
func TestDestructiveChangesIgnoresConstraintsThatWereAlreadyThere(t *testing.T) {
	tbl := func() *sqlite.Table {
		t := sqlite.NewTable("seats")
		sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
		sqlite.Add(t, sqlite.Text("code").Unique())
		row := sqlite.Add(t, sqlite.Text("row"))
		number := sqlite.Add(t, sqlite.BigInt("number"))
		t.AddUnique("seats_row_number", row, number)
		t.AddCheck("seats_number_positive", `"number" > 0`)
		return t
	}
	if got := sqlite.DestructiveChanges(snapshotOf(tbl()), snapshotOf(tbl())); len(got) != 0 {
		t.Fatalf("got %v, want nothing", rules(got))
	}
}

// A CHECK whose expression changed under an unchanged name is a new
// CHECK: the rows that satisfied the old one say nothing about the new
// one.
func TestDestructiveChangesReportsARewrittenCheck(t *testing.T) {
	mk := func(expr string) *sqlite.Table {
		t := sqlite.NewTable("readings")
		sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
		sqlite.Add(t, sqlite.BigInt("celsius"))
		t.AddCheck("readings_sane", expr)
		return t
	}
	got := sqlite.DestructiveChanges(snapshotOf(mk(`"celsius" >= -273`)), snapshotOf(mk(`"celsius" >= 0`)))
	if len(got) != 1 || got[0].Rule != "add-check-constraint" {
		t.Fatalf("got %v, want one add-check-constraint", rules(got))
	}
}

// A UNIQUE constraint is compared by the columns it spans, not by its
// name.
//
// SQLite stores one as an anonymous index and reports it back under a
// generated name, so a snapshot read out of a database never carries
// the name the schema gave it — which is why Diff compares the column
// tuples (see sameUniqueKeys) and why this has to as well. Reading the
// two names as two constraints would call a constraint that has been
// enforced all along a new one, on a snapshot pair a caller of
// DestructiveChanges gets simply by introspecting.
func TestDestructiveChangesMatchesAUniqueByItsColumnsNotItsName(t *testing.T) {
	introspected := &sqlite.Snapshot{Tables: map[string]*sqlite.TableSnapshot{
		"seats": {
			Name: "seats",
			Columns: map[string]*sqlite.ColumnSnapshot{
				"id":     {Name: "id", Type: "INTEGER", PrimaryKey: true, NotNull: true},
				"row":    {Name: "row", Type: "TEXT"},
				"number": {Name: "number", Type: "INTEGER"},
			},
			UniqueConstraints: map[string]*sqlite.UniqueSnapshot{
				"sqlite_autoindex_seats_1": {Name: "sqlite_autoindex_seats_1", Columns: []string{"row", "number"}},
			},
		},
	}}

	declared := sqlite.NewTable("seats")
	sqlite.Add(declared, sqlite.BigInt("id").PrimaryKey())
	row := sqlite.Add(declared, sqlite.Text("row"))
	number := sqlite.Add(declared, sqlite.BigInt("number"))
	declared.AddUnique("seats_row_number", row, number)

	if got := sqlite.DestructiveChanges(introspected, snapshotOf(declared)); len(got) != 0 {
		t.Fatalf("got %v, want nothing: the constraint is the one already there under the name SQLite gave it", rules(got))
	}
}
