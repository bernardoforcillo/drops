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

// A covering index has to reach the CREATE INDEX. Rendering it without
// the INCLUDE list built a plain index, and since the snapshot did not
// record the list either, nothing downstream could tell.
func TestDiffRendersIncludeColumns(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	email := pg.Add(users, pg.Text("email").NotNull())
	name := pg.Add(users, pg.Text("name").NotNull())
	users.AddIndex(pg.NewIndex("usersCoverIdx", users, email).Include(name.Column))

	stmts := pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(pg.NewSchema(users)))
	var create string
	for _, s := range stmts {
		if strings.HasPrefix(s, "CREATE INDEX") {
			create = s
		}
	}
	want := `CREATE INDEX "usersCoverIdx" ON "users" ("email") INCLUDE ("name");`
	if create != want {
		t.Errorf("CREATE INDEX = %q, want %q", create, want)
	}
}

// An index over expressions rather than columns leaves the snapshot
// with nothing between the parentheses. Emitting `ON "users" ()` is a
// syntax error that takes the whole migration down; Diff skips it and
// Push reports it as a notice instead.
func TestDiffSkipsAnIndexItCannotRender(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(users, pg.Text("name").NotNull())
	users.AddIndex(pg.NewIndex("usersLowerNameIdx", users, pg.Lower(name)))

	for _, s := range pg.Diff(pg.EmptySnapshot(), pg.BuildSnapshot(pg.NewSchema(users))) {
		if strings.Contains(s, "CREATE INDEX") {
			t.Errorf("Diff emitted an unrenderable index: %q", s)
		}
	}
}

// A CHECK constraint used to be matched on its name alone, so
// tightening the expression under an unchanged name produced nothing.
func TestDiffRewritesAChangedCheck(t *testing.T) {
	build := func(expr string) *pg.Snapshot {
		users := pg.NewTable("users")
		pg.Add(users, pg.BigSerial("id").PrimaryKey())
		pg.Add(users, pg.Integer("age").NotNull())
		users.AddCheck("usersAgeSane", expr)
		return pg.BuildSnapshot(pg.NewSchema(users))
	}
	stmts := pg.Diff(build(`"age" >= 0`), build(`"age" >= 18`))
	want := []string{
		`ALTER TABLE "users" DROP CONSTRAINT "usersAgeSane";`,
		`ALTER TABLE "users" ADD CONSTRAINT "usersAgeSane" CHECK ("age" >= 18);`,
	}
	if len(stmts) != len(want) {
		t.Fatalf("statements = %v, want %v", stmts, want)
	}
	for i := range want {
		if stmts[i] != want[i] {
			t.Errorf("statement %d = %q, want %q", i, stmts[i], want[i])
		}
	}
	if got := pg.Diff(build(`"age" >= 0`), build(`"age" >= 0`)); len(got) != 0 {
		t.Errorf("an unchanged CHECK produced %v", got)
	}
}

// A composite PRIMARY KEY is matched by its columns, not its name.
// PostgreSQL names one "members_pkey" and drops names the same key
// "membersOrgIdUserIdPk"; matching on the name had a push against a
// server-named key drop and recreate it every time.
func TestDiffMatchesCompositePKByColumns(t *testing.T) {
	serverNamed := &pg.Snapshot{Tables: map[string]*pg.TableSnapshot{
		"public.members": {
			Name: "members",
			CompositePrimaryKeys: map[string]*pg.CompositePKSnapshot{
				"members_pkey": {Name: "members_pkey", Columns: []string{"orgId", "userId"}},
			},
		},
	}}

	members := pg.NewTable("members")
	org := pg.Add(members, pg.BigInt("orgId").NotNull())
	user := pg.Add(members, pg.BigInt("userId").NotNull())
	members.PrimaryKey(org, user)
	declared := pg.BuildSnapshot(pg.NewSchema(members))
	// Diff the constraints only; the server-named side carries no
	// columns, so ignore what the column pass has to say about them.
	var pkStmts []string
	for _, s := range pg.Diff(serverNamed, declared) {
		if strings.Contains(s, "PRIMARY KEY") || strings.Contains(s, "DROP CONSTRAINT") {
			pkStmts = append(pkStmts, s)
		}
	}
	if len(pkStmts) != 0 {
		t.Errorf("the same key under a different name produced %v", pkStmts)
	}
}

// One expression element makes the whole index unrepresentable.
//
// An index on (name, lower(email)) used to reach the snapshot as an
// index on (name): Diff created that, Introspect read it back, and the
// two agreed for ever after that the wrong index was the right one.
// Nothing reported it, because the notice only fired when every
// element was an expression.
func TestDiffSkipsAnIndexWithOneExpressionElement(t *testing.T) {
	users := pg.NewTable("users")
	pg.Add(users, pg.BigSerial("id").PrimaryKey())
	email := pg.Add(users, pg.Text("email").NotNull())
	name := pg.Add(users, pg.Text("name").NotNull())
	users.AddIndex(pg.NewIndex("usersMixedIdx", users, name, pg.Lower(email)))

	snap := pg.BuildSnapshot(pg.NewSchema(users))
	if cols := snap.Tables["public.users"].Indexes["usersMixedIdx"].Columns; len(cols) != 0 {
		t.Errorf("the snapshot kept %v of an index it cannot describe", cols)
	}
	for _, s := range pg.Diff(pg.EmptySnapshot(), snap) {
		if strings.Contains(s, "CREATE INDEX") {
			t.Errorf("Diff emitted a truncated index: %q", s)
		}
	}
}
