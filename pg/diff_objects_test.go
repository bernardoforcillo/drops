package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestBuildSnapshotCapturesEnums(t *testing.T) {
	status := pg.NewEnum("userStatus", "active", "pending", "banned")
	snap := pg.BuildSnapshot(pg.NewSchema().AddEnum(status))
	if got := snap.Enums["userStatus"]; got == nil || len(got.Values) != 3 {
		t.Errorf("enum not captured: %+v", got)
	}
}

func TestDiffEmitsCreateEnum(t *testing.T) {
	prev := pg.EmptySnapshot()
	cur := pg.BuildSnapshot(pg.NewSchema().AddEnum(pg.NewEnum("status", "a", "b")))
	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.Contains(s, "CREATE TYPE") && strings.Contains(s, "ENUM") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected CREATE TYPE ... ENUM, got: %v", stmts)
	}
}

func TestDiffEmitsDropEnum(t *testing.T) {
	prev := pg.BuildSnapshot(pg.NewSchema().AddEnum(pg.NewEnum("old", "x")))
	cur := pg.EmptySnapshot()
	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.Contains(s, "DROP TYPE") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected DROP TYPE, got: %v", stmts)
	}
}

func TestDiffEnumAddValue(t *testing.T) {
	prev := pg.BuildSnapshot(pg.NewSchema().AddEnum(pg.NewEnum("status", "a", "b")))
	cur := pg.BuildSnapshot(pg.NewSchema().AddEnum(pg.NewEnum("status", "a", "b", "c")))
	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.Contains(s, "ALTER TYPE") && strings.Contains(s, "ADD VALUE") && strings.Contains(s, "'c'") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected ALTER TYPE ADD VALUE 'c', got: %v", stmts)
	}
}

func TestBuildSnapshotCapturesSequence(t *testing.T) {
	seq := pg.NewSequence("myseq")
	snap := pg.BuildSnapshot(pg.NewSchema().AddSequence(seq))
	if got := snap.Sequences["myseq"]; got == nil {
		t.Error("sequence not captured")
	}
}

func TestDiffEmitsCreateSequence(t *testing.T) {
	prev := pg.EmptySnapshot()
	start := int64(100)
	seq := pg.NewSequence("myseq", pg.SequenceOptions{Start: &start})
	cur := pg.BuildSnapshot(pg.NewSchema().AddSequence(seq))
	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.Contains(s, "CREATE SEQUENCE") && strings.Contains(s, "START WITH 100") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected CREATE SEQUENCE ... START WITH 100, got: %v", stmts)
	}
}

func TestDiffEmitsDropSequence(t *testing.T) {
	prev := pg.BuildSnapshot(pg.NewSchema().AddSequence(pg.NewSequence("old")))
	cur := pg.EmptySnapshot()
	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.Contains(s, "DROP SEQUENCE") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected DROP SEQUENCE, got: %v", stmts)
	}
}

func TestBuildSnapshotCapturesView(t *testing.T) {
	v := pg.NewView("activeUsers", "SELECT * FROM users WHERE active = true")
	snap := pg.BuildSnapshot(pg.NewSchema().AddView(v))
	if got := snap.Views["activeUsers"]; got == nil || got.Definition == "" {
		t.Errorf("view not captured: %+v", got)
	}
}

func TestDiffEmitsCreateView(t *testing.T) {
	prev := pg.EmptySnapshot()
	v := pg.NewView("activeUsers", "SELECT * FROM users WHERE active = true")
	cur := pg.BuildSnapshot(pg.NewSchema().AddView(v))
	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.HasPrefix(s, `CREATE VIEW "activeUsers"`) {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected CREATE VIEW, got: %v", stmts)
	}
}

func TestDiffViewDefinitionChangeReplaces(t *testing.T) {
	prev := pg.BuildSnapshot(pg.NewSchema().AddView(pg.NewView("v", "SELECT 1")))
	cur := pg.BuildSnapshot(pg.NewSchema().AddView(pg.NewView("v", "SELECT 2")))
	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.HasPrefix(s, "CREATE OR REPLACE VIEW") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected CREATE OR REPLACE VIEW, got: %v", stmts)
	}
}

func TestDiffMaterializedViewChangeDropsAndRecreates(t *testing.T) {
	prev := pg.BuildSnapshot(pg.NewSchema().AddView(pg.NewMaterializedView("mv", "SELECT 1")))
	cur := pg.BuildSnapshot(pg.NewSchema().AddView(pg.NewMaterializedView("mv", "SELECT 2")))
	stmts := pg.Diff(prev, cur)
	sawDrop := false
	sawCreate := false
	for _, s := range stmts {
		if strings.HasPrefix(s, "DROP MATERIALIZED VIEW") {
			sawDrop = true
		}
		if strings.HasPrefix(s, "CREATE MATERIALIZED VIEW") {
			sawCreate = true
		}
	}
	if !sawDrop || !sawCreate {
		t.Errorf("expected drop + recreate for materialised view, got: %v", stmts)
	}
}

func TestBuildSnapshotCapturesRLSPolicy(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	users.EnableRLS()
	users.AddPolicy(pg.NewPolicy("usersOwnRowsOnly").
		For("SELECT").
		To("appUser").
		Using("id = current_setting('app.user_id')::int"))

	snap := pg.BuildSnapshot(pg.NewSchema(users))
	ts := snap.Tables["public.users"]
	if !ts.IsRLSEnabled {
		t.Error("RLS not captured")
	}
	pol := ts.Policies["usersOwnRowsOnly"]
	if pol == nil {
		t.Fatal("policy not captured")
	}
	if pol.For != "SELECT" || len(pol.To) != 1 {
		t.Errorf("policy fields: %+v", pol)
	}
}

func TestDiffEmitsEnableRLSAndCreatePolicy(t *testing.T) {
	usersPrev := pg.NewTable("users")
	pg.Add(usersPrev, pg.BigSerial("id").PrimaryKey())
	prev := pg.BuildSnapshot(pg.NewSchema(usersPrev))

	usersCur := pg.NewTable("users")
	pg.Add(usersCur, pg.BigSerial("id").PrimaryKey())
	usersCur.EnableRLS()
	usersCur.AddPolicy(pg.NewPolicy("p").Using("true"))
	cur := pg.BuildSnapshot(pg.NewSchema(usersCur))

	stmts := pg.Diff(prev, cur)
	sawEnable := false
	sawCreate := false
	for _, s := range stmts {
		if strings.Contains(s, "ENABLE ROW LEVEL SECURITY") {
			sawEnable = true
		}
		if strings.Contains(s, "CREATE POLICY") {
			sawCreate = true
		}
	}
	if !sawEnable {
		t.Errorf("expected ENABLE ROW LEVEL SECURITY, got: %v", stmts)
	}
	if !sawCreate {
		t.Errorf("expected CREATE POLICY, got: %v", stmts)
	}
}

func TestSnapshotJSONRoundTripWithObjects(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	users.EnableRLS()
	users.AddPolicy(pg.NewPolicy("p").For("SELECT").Using("true"))
	schema := pg.NewSchema(users).
		AddEnum(pg.NewEnum("status", "a", "b")).
		AddSequence(pg.NewSequence("seq")).
		AddView(pg.NewView("v", "SELECT 1"))

	snap := pg.BuildSnapshot(schema)
	body, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pg.UnmarshalSnapshot(body)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Enums["status"] == nil {
		t.Error("enum lost on round-trip")
	}
	if parsed.Sequences["seq"] == nil {
		t.Error("sequence lost on round-trip")
	}
	if parsed.Views["v"] == nil {
		t.Error("view lost on round-trip")
	}
	ts := parsed.Tables["public.users"]
	if !ts.IsRLSEnabled {
		t.Error("RLS flag lost on round-trip")
	}
	if ts.Policies["p"] == nil {
		t.Error("policy lost on round-trip")
	}
}
