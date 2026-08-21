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
