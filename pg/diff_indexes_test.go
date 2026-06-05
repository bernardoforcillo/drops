package pg_test

import (
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

func TestBuildSnapshotCapturesIndexes(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	email := pg.Add(users, pg.Text("email").NotNull())
	users.AddIndex(pg.NewIndex("usersEmailIdx", users, email))
	users.AddIndex(pg.NewIndex("usersEmailUniqIdx", users, email).Unique())

	snap := pg.BuildSnapshot(pg.NewSchema(users))
	ts := snap.Tables["public.users"]
	if ts == nil {
		t.Fatal("users table missing from snapshot")
	}
	if got := ts.Indexes["usersEmailIdx"]; got == nil {
		t.Error("usersEmailIdx not captured")
	}
	if got := ts.Indexes["usersEmailUniqIdx"]; got == nil || !got.IsUnique {
		t.Errorf("unique index not captured: %+v", got)
	}
}

func TestDiffEmitsCreateIndex(t *testing.T) {
	prev := pg.EmptySnapshot()

	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	email := pg.Add(users, pg.Text("email").NotNull())
	users.AddIndex(pg.NewIndex("usersEmailIdx", users, email))
	cur := pg.BuildSnapshot(pg.NewSchema(users))

	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.HasPrefix(s, "CREATE INDEX") || strings.HasPrefix(s, "CREATE UNIQUE INDEX") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("Diff should emit CREATE INDEX, got: %v", stmts)
	}
}

func TestDiffEmitsDropIndex(t *testing.T) {
	prevUsers := pg.NewTable("users")
	pg.Add(prevUsers, pg.BigSerial("id").PrimaryKey())
	prevEmail := pg.Add(prevUsers, pg.Text("email").NotNull())
	prevUsers.AddIndex(pg.NewIndex("usersEmailIdx", prevUsers, prevEmail))
	prev := pg.BuildSnapshot(pg.NewSchema(prevUsers))

	curUsers := pg.NewTable("users")
	pg.Add(curUsers, pg.BigSerial("id").PrimaryKey())
	pg.Add(curUsers, pg.Text("email").NotNull())
	// no index
	cur := pg.BuildSnapshot(pg.NewSchema(curUsers))

	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.HasPrefix(s, "DROP INDEX") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("Diff should emit DROP INDEX, got: %v", stmts)
	}
}

func TestDiffIndexChangeReplacesIt(t *testing.T) {
	// Same name, different uniqueness — should emit DROP + CREATE.
	prevUsers := pg.NewTable("users")
	pg.Add(prevUsers, pg.BigSerial("id").PrimaryKey())
	prevEmail := pg.Add(prevUsers, pg.Text("email").NotNull())
	prevUsers.AddIndex(pg.NewIndex("usersEmailIdx", prevUsers, prevEmail))
	prev := pg.BuildSnapshot(pg.NewSchema(prevUsers))

	curUsers := pg.NewTable("users")
	pg.Add(curUsers, pg.BigSerial("id").PrimaryKey())
	curEmail := pg.Add(curUsers, pg.Text("email").NotNull())
	curUsers.AddIndex(pg.NewIndex("usersEmailIdx", curUsers, curEmail).Unique())
	cur := pg.BuildSnapshot(pg.NewSchema(curUsers))

	stmts := pg.Diff(prev, cur)
	sawDrop := false
	sawCreate := false
	for _, s := range stmts {
		if strings.HasPrefix(s, "DROP INDEX") {
			sawDrop = true
		}
		if strings.HasPrefix(s, "CREATE UNIQUE INDEX") {
			sawCreate = true
		}
	}
	if !sawDrop || !sawCreate {
		t.Errorf("changed index should drop+recreate, got: %v", stmts)
	}
}

func TestBuildSnapshotCapturesCompositePK(t *testing.T) {
	t1 := pg.NewTable("memberships")
	uid := pg.Add(t1, pg.BigInt("userId").NotNull())
	rid := pg.Add(t1, pg.BigInt("roleId").NotNull())
	t1.PrimaryKey(uid, rid)
	snap := pg.BuildSnapshot(pg.NewSchema(t1))
	ts := snap.Tables["public.memberships"]
	if len(ts.CompositePrimaryKeys) != 1 {
		t.Fatalf("expected 1 composite PK, got %d", len(ts.CompositePrimaryKeys))
	}
	for _, pk := range ts.CompositePrimaryKeys {
		if len(pk.Columns) != 2 {
			t.Errorf("PK should span 2 cols: %+v", pk)
		}
	}
}

func TestDiffEmitsAddCompositePK(t *testing.T) {
	prev := pg.EmptySnapshot()

	t1 := pg.NewTable("memberships")
	uid := pg.Add(t1, pg.BigInt("userId").NotNull())
	rid := pg.Add(t1, pg.BigInt("roleId").NotNull())
	t1.PrimaryKey(uid, rid)
	cur := pg.BuildSnapshot(pg.NewSchema(t1))

	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.Contains(s, "ADD CONSTRAINT") && strings.Contains(s, "PRIMARY KEY") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("Diff should emit ADD CONSTRAINT ... PRIMARY KEY, got: %v", stmts)
	}
}

func TestBuildSnapshotCapturesCompositeUnique(t *testing.T) {
	t1 := pg.NewTable("users")
	pg.Add(t1, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(t1, pg.BigInt("tenantId").NotNull())
	name := pg.Add(t1, pg.Text("name").NotNull())
	t1.AddUnique("usersTenantNameUnique", tenant, name)
	snap := pg.BuildSnapshot(pg.NewSchema(t1))
	uq := snap.Tables["public.users"].UniqueConstraints["usersTenantNameUnique"]
	if uq == nil || len(uq.Columns) != 2 {
		t.Errorf("composite unique not captured: %+v", uq)
	}
}

func TestBuildSnapshotCapturesCheckConstraint(t *testing.T) {
	t1 := pg.NewTable("users")
	pg.Add(t1, pg.BigSerial("id").PrimaryKey())
	pg.Add(t1, pg.Integer("age").NotNull())
	t1.AddCheck("usersAgeNonNegative", "age >= 0")
	snap := pg.BuildSnapshot(pg.NewSchema(t1))
	c := snap.Tables["public.users"].CheckConstraints["usersAgeNonNegative"]
	if c == nil || c.Value != "age >= 0" {
		t.Errorf("check constraint not captured: %+v", c)
	}
}

func TestDiffEmitsAddCheckConstraint(t *testing.T) {
	prev := pg.EmptySnapshot()

	t1 := pg.NewTable("users")
	pg.Add(t1, pg.BigSerial("id").PrimaryKey())
	pg.Add(t1, pg.Integer("age").NotNull())
	t1.AddCheck("usersAgeNonNegative", "age >= 0")
	cur := pg.BuildSnapshot(pg.NewSchema(t1))

	stmts := pg.Diff(prev, cur)
	saw := false
	for _, s := range stmts {
		if strings.Contains(s, "ADD CONSTRAINT") && strings.Contains(s, "CHECK (age >= 0)") {
			saw = true
		}
	}
	if !saw {
		t.Errorf("Diff should emit ADD CONSTRAINT ... CHECK, got: %v", stmts)
	}
}

func TestSnapshotJSONRoundTripWithNewFeatures(t *testing.T) {
	t1 := pg.NewTable("memberships")
	uid := pg.Add(t1, pg.BigInt("userId").NotNull())
	rid := pg.Add(t1, pg.BigInt("roleId").NotNull())
	t1.PrimaryKey(uid, rid)
	t1.AddIndex(pg.NewIndex("membershipsUserIdx", t1, uid))
	t1.AddCheck("membershipsRoleValid", "roleId > 0")

	snap := pg.BuildSnapshot(pg.NewSchema(t1))
	body, err := snap.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := pg.UnmarshalSnapshot(body)
	if err != nil {
		t.Fatal(err)
	}
	ts := parsed.Tables["public.memberships"]
	if ts == nil {
		t.Fatal("memberships table missing after round-trip")
	}
	if len(ts.CompositePrimaryKeys) != 1 {
		t.Errorf("composite PK lost after round-trip")
	}
	if len(ts.Indexes) != 1 {
		t.Errorf("index lost after round-trip")
	}
	if len(ts.CheckConstraints) != 1 {
		t.Errorf("check constraint lost after round-trip")
	}
}
