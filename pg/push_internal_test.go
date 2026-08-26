package pg

import (
	"strings"
	"testing"
)

// ownedBy is what stands between a push and somebody else's data, and
// the live side of a push carries four kinds of object rather than one.
// This test is written against Diff's output rather than against the
// narrowed snapshot's maps, because the thing being prevented is a
// statement: the maps are an implementation detail, DROP TYPE is not.
//
// Introspect does not read enums, sequences or views yet, so the shared
// database below is one no Introspect can currently produce. That is
// the point — the fixture is the database drops will see the day the
// gap Introspect's doc comment promises to close is closed, and the
// assertion is that closing it cannot quietly turn Push back into the
// thing it was written to stop being.
func TestOwnedByLeavesEveryKindOfForeignObjectAlone(t *testing.T) {
	live := EmptySnapshot()
	live.Tables["public.users"] = &TableSnapshot{Name: "users", Schema: "public"}
	live.Tables["public.audit_log"] = &TableSnapshot{Name: "audit_log", Schema: "public"}
	live.Enums["mood"] = &EnumSnapshot{Name: "mood", Values: []string{"ok"}}
	live.Sequences["billing_seq"] = &SequenceSnapshot{Name: "billing_seq"}
	// The PostGIS pair, which is what ownedBy's own doc comment names
	// as the reason it exists — and which is a pair of views.
	live.Views["geometry_columns"] = &ViewSnapshot{Name: "geometry_columns", Definition: "SELECT 1"}
	live.Views["geography_columns"] = &ViewSnapshot{Name: "geography_columns", Definition: "SELECT 1"}

	declared := EmptySnapshot()
	declared.Tables["public.users"] = &TableSnapshot{Name: "users", Schema: "public"}

	narrowed, held := ownedBy(live, declared)
	if stmts := Diff(narrowed, declared); len(stmts) != 0 {
		t.Fatalf("Diff over the narrowed live side = %v, want no statements: none of it is drops's to drop", stmts)
	}

	got := map[string]string{}
	for _, n := range unmanagedNotices(live, held, false) {
		key := n.Table
		if key == "" {
			key = n.Object
		}
		got[key] = n.Rule + " " + n.SQL
	}
	want := map[string]string{
		"audit_log":         `unmanaged-table DROP TABLE "audit_log" CASCADE;`,
		"mood":              `unmanaged-enum DROP TYPE "mood";`,
		"billing_seq":       `unmanaged-sequence DROP SEQUENCE "billing_seq";`,
		"geometry_columns":  `unmanaged-view DROP VIEW "geometry_columns";`,
		"geography_columns": `unmanaged-view DROP VIEW "geography_columns";`,
	}
	for name, wantNotice := range want {
		if got[name] != wantNotice {
			t.Errorf("notice for %q: got = %v, want %v", name, got[name], wantNotice)
		}
	}
	if len(got) != len(want) {
		t.Errorf("notices = %v, want exactly %d, one per withheld object", got, len(want))
	}
	if _, ok := narrowed.Tables["public.users"]; !ok {
		t.Error("the declared table was narrowed away too")
	}
}

// A push that really is meant to own the whole schema still gets the
// whole live side, or DropUnmanagedTables would silently keep half of
// what it promises.
func TestOwnedByIsNotConsultedWhenTheSchemaOwnsEverything(t *testing.T) {
	live := EmptySnapshot()
	live.Views["reports"] = &ViewSnapshot{Name: "reports", Definition: "SELECT 1"}
	declared := EmptySnapshot()

	stmts := Diff(live, declared)
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "DROP VIEW") {
		t.Fatalf("Diff = %v, want the DROP VIEW: Diff compares two declarations and is right to emit it", stmts)
	}
}
