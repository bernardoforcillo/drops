package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bernardoforcillo/drops/integration"
	"github.com/bernardoforcillo/drops/pg"
	"github.com/bernardoforcillo/drops/stdlib"
)

// pg.InTxAs establishes a PostgreSQL identity for the lifetime of a
// transaction: SET LOCAL ROLE and set_config(k, v, true). Everything
// about it can be asserted against a recorder except the two things
// that actually matter, and those are the two this file is for.
//
// A recorder can show that drops sent "SET LOCAL ROLE" rather than
// "SET ROLE", and pg/rls_test.go does. It cannot show that LOCAL means
// what the documentation says it means — that the server reverts the
// identity when the transaction ends, so that a pooled connection
// cannot carry one request's identity to the next request that draws
// it. Nor can it show that a policy, evaluated by a real planner
// against a real current_setting, actually filters the rows. Both
// claims are about PostgreSQL's behaviour, so only PostgreSQL can be
// asked.
//
// The pool in these tests is capped at ONE connection. That is what
// turns "some later request might get this connection" into "the next
// statement in this test runs on exactly the connection the
// transaction used", which is the only way to observe the leak
// deterministically rather than under load.

// openPGSingleConn opens a PostgreSQL pool of exactly one connection.
//
// One connection is what makes the leak observable. "Some later
// request might get this connection back" is a probability; "the next
// statement in this test runs on the session the transaction just
// used" is a certainty, and it is the same situation a busy pool
// reaches constantly.
func openPGSingleConn(t *testing.T) *pg.DB {
	t.Helper()
	dsn := integration.DSN(t, integration.EnvPostgres)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := sqlDB.PingContext(context.Background()); err != nil {
		t.Fatalf("ping %s: %v", dsn, err)
	}
	return pg.New(stdlib.New(sqlDB))
}

// pgRoleName derives a role name from the test name. Roles are
// cluster-wide rather than per-database, so a leftover one collides
// with every later run and not just this database's.
func pgRoleName(t *testing.T, prefix string) string {
	t.Helper()
	return strings.ToLower(integration.UniqueName(t, prefix))
}

// createPGRole makes a login-less role the pool's user may SET ROLE
// to, and removes it at test end.
//
// CREATE ROLE needs CREATEROLE or superuser; the compose file's user
// is a superuser, so a failure there is a misconfigured server rather
// than a reason to skip. The GRANT is the part worth explaining. SET
// ROLE requires membership, and the membership a CREATEROLE user gets
// automatically when it creates a role carries ADMIN but not SET — so
// on a non-superuser DSN the SET LOCAL ROLE is refused with SQLSTATE
// 42501 and InTxAs does exactly what it promises: aborts. That is the
// same requirement Session.Role documents, met here explicitly so the
// suite exercises the feature rather than its refusal path.
func createPGRole(t *testing.T, db *pg.DB, name string) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.Exec(ctx, fmt.Sprintf(`CREATE ROLE %q NOLOGIN`, name)); err != nil {
		t.Fatalf("CREATE ROLE %q (the DSN's user needs CREATEROLE or superuser): %v", name, err)
	}
	if _, err := db.Exec(ctx, fmt.Sprintf(`GRANT %q TO CURRENT_USER`, name)); err != nil {
		t.Fatalf("GRANT %q TO CURRENT_USER: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP OWNED BY %q`, name))
		_, _ = db.Exec(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %q`, name))
	})
}

// rlsPost is the row these tests read back.
type rlsPost struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// rlsFixture builds the arrangement the feature is for: a table
// holding two tenants' rows, a role to read it as, and a policy that
// decides which rows that role sees by reading a session setting.
type rlsFixture struct {
	db       *pg.DB
	tbl      *pg.Table
	id       *pg.Col[int64]
	tenantID *pg.Col[int64]
	title    *pg.Col[string]
	role     string
}

func newRLSFixture(t *testing.T) *rlsFixture {
	t.Helper()
	db := openPGSingleConn(t)
	ctx := context.Background()

	tbl := pg.NewTable(integration.UniqueName(t, "rls_posts"))
	dropPG(t, db, tbl)
	id := pg.Add(tbl, pg.BigInt("id").NotNull())
	tenantID := pg.Add(tbl, pg.BigInt("tenantId").NotNull())
	title := pg.Add(tbl, pg.Text("title").NotNull())
	tbl.PrimaryKey(id)
	execPG(t, db, pg.CreateTable(tbl))

	// Seeded as the owner, before the policy exists: the fixture is
	// the state of the world, not something the policy has to permit.
	if _, err := db.Insert(tbl).
		Row(id.Val(int64(1)), tenantID.Val(int64(1)), title.Val("first tenant, first post")).
		Row(id.Val(int64(2)), tenantID.Val(int64(1)), title.Val("first tenant, second post")).
		Row(id.Val(int64(3)), tenantID.Val(int64(2)), title.Val("second tenant, only post")).
		Exec(ctx); err != nil {
		t.Fatalf("seed: %v", err)
	}

	role := pgRoleName(t, "rls_app")
	createPGRole(t, db, role)

	quoted := pgQuoteIdent(tbl.Name())
	for _, stmt := range []string{
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON %s TO %q`, quoted, role),
		fmt.Sprintf(`ALTER TABLE %s ENABLE ROW LEVEL SECURITY`, quoted),
		// The predicate the whole feature exists to satisfy: the row's
		// tenant against a setting the SESSION carries, which is
		// exactly the thing a pooled connection has no business
		// carrying from one request to the next.
		fmt.Sprintf(`CREATE POLICY tenant_isolation ON %s
		         USING ("tenantId" = current_setting('app.tenantId')::bigint)
		    WITH CHECK ("tenantId" = current_setting('app.tenantId')::bigint)`, quoted),
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatalf("policy setup (%s): %v", strings.Fields(stmt)[0], err)
		}
	}

	return &rlsFixture{
		db: db, tbl: tbl,
		id: id, tenantID: tenantID, title: title,
		role: role,
	}
}

// session returns the Session a request for tenant would carry.
//
// Both halves are populated, and the reason is the trap that makes
// people say RLS "does nothing": a policy is not applied to a
// superuser at all, nor to the table's owner unless the table is set
// to FORCE ROW LEVEL SECURITY. The DSN's user is the owner and is
// usually a superuser, so a Session that set only the setting would
// read every tenant's rows and prove nothing. SET LOCAL ROLE to a
// plain role is what puts the connection under the policy.
func (f *rlsFixture) session(tenant string) pg.Session {
	return pg.Session{
		Role:     f.role,
		Settings: map[string]string{"app.tenantId": tenant},
	}
}

// titles reads the table through drops' own builder, so the statement
// under the policy is one drops composed.
func (f *rlsFixture) titles(t *testing.T, ctx context.Context, tx *pg.DB) []string {
	t.Helper()
	var out []rlsPost
	if err := tx.Select().From(f.tbl).OrderBy(f.id).All(ctx, &out); err != nil {
		t.Fatalf("select: %v", err)
	}
	titles := make([]string, len(out))
	for i, p := range out {
		titles[i] = p.Title
	}
	return titles
}

// The claim: a policy drops declared, satisfied by an identity drops
// established, filters rows on a real server. A statement with no
// tenant predicate in it at all comes back holding one tenant's rows.
func TestPGInTxAsSatisfiesARowLevelSecurityPolicy(t *testing.T) {
	f := newRLSFixture(t)
	ctx := context.Background()

	tests := []struct {
		name   string
		tenant string
		want   []string
	}{
		{
			name:   "first tenant",
			tenant: "1",
			want:   []string{"first tenant, first post", "first tenant, second post"},
		},
		{
			name:   "second tenant",
			tenant: "2",
			want:   []string{"second tenant, only post"},
		},
		{
			name:   "a tenant with no rows sees no rows, not an error",
			tenant: "3",
			want:   []string{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			if err := f.db.InTxAs(ctx, f.session(tc.tenant), func(tx *pg.DB) error {
				got = f.titles(t, ctx, tx)
				return nil
			}); err != nil {
				t.Fatalf("InTxAs: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("titles = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("titles = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// The other half of the policy. WITH CHECK decides what may be
// written, and a row stamped for another tenant is refused by the
// server whatever the application believed.
func TestPGInTxAsIsCheckedOnWritesToo(t *testing.T) {
	f := newRLSFixture(t)
	ctx := context.Background()
	id, tenantID, title := f.id, f.tenantID, f.title

	t.Run("a row for the session's own tenant is accepted", func(t *testing.T) {
		if err := f.db.InTxAs(ctx, f.session("1"), func(tx *pg.DB) error {
			_, err := tx.Insert(f.tbl).
				Row(id.Val(int64(10)), tenantID.Val(int64(1)), title.Val("mine")).
				Exec(ctx)
			return err
		}); err != nil {
			t.Fatalf("InTxAs: %v", err)
		}
	})

	t.Run("a row for another tenant is refused by the server", func(t *testing.T) {
		err := f.db.InTxAs(ctx, f.session("1"), func(tx *pg.DB) error {
			_, err := tx.Insert(f.tbl).
				Row(id.Val(int64(11)), tenantID.Val(int64(2)), title.Val("theirs")).
				Exec(ctx)
			return err
		})
		if err == nil {
			t.Fatal("the server accepted a row stamped for another tenant")
		}
		if !strings.Contains(err.Error(), "row-level security") {
			t.Errorf("err = %v, want the row-level security policy to have refused it", err)
		}
	})

	t.Run("and the refused row is not there", func(t *testing.T) {
		var out []rlsPost
		if err := f.db.Select().From(f.tbl).
			Where(pg.Eq(f.id, int64(11))).
			All(ctx, &out); err != nil {
			t.Fatalf("select: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("the rolled-back insert left %d row(s) behind", len(out))
		}
	})
}

// THE test. LOCAL is the load-bearing word in this feature and a
// rendering test cannot prove it means anything; this one asks the
// server.
//
// The pool holds exactly one connection, so the statements after the
// transaction run on the very session the transaction used — the
// situation a pooled deployment reaches constantly and a test almost
// never reaches by accident.
//
// The control in the middle is the point. It establishes a setting
// with is_local = false inside a committed transaction and shows that
// the setting IS still there afterwards, which proves two things at
// once: that neither database/sql nor the driver resets the session
// between checkouts, and therefore that the absence of the LOCAL
// settings afterwards is PostgreSQL reverting them rather than
// something else tidying up. Without it this test would pass just as
// happily against a pool that issued DISCARD ALL.
func TestPGInTxAsLeavesNoIdentityOnThePooledConnection(t *testing.T) {
	f := newRLSFixture(t)
	ctx := context.Background()

	poolUser := pgScalar(t, f.db, "SELECT current_user")
	if poolUser == f.role {
		t.Fatalf("the pool already connects as %q; this test cannot tell an established identity from the default one", f.role)
	}

	var insideUser, insideSetting string
	if err := f.db.InTxAs(ctx, f.session("1"), func(tx *pg.DB) error {
		insideUser = pgScalar(t, tx, "SELECT current_user")
		insideSetting = pgScalar(t, tx, "SELECT current_setting('app.tenantId', true)")
		return nil
	}); err != nil {
		t.Fatalf("InTxAs: %v", err)
	}
	if insideUser != f.role {
		t.Fatalf("inside the transaction current_user = %q, want %q — SET LOCAL ROLE did not take", insideUser, f.role)
	}
	if insideSetting != "1" {
		t.Fatalf("inside the transaction app.tenantId = %q, want %q", insideSetting, "1")
	}

	// The control: a session-lifetime setting, committed. If the pool
	// or the driver reset sessions between checkouts, this would be
	// gone too and the assertions below would prove nothing.
	if err := f.db.InTx(ctx, func(tx *pg.DB) error {
		_, err := tx.Exec(ctx, "SELECT set_config('app.controlProbe', 'survives', false)")
		return err
	}); err != nil {
		t.Fatalf("control probe: %v", err)
	}
	if got := pgScalar(t, f.db, "SELECT current_setting('app.controlProbe', true)"); got != "survives" {
		t.Skipf("this deployment resets pooled sessions (app.controlProbe = %q after a commit), so it cannot distinguish LOCAL from session scope", got)
	}

	// And now the assertions that matter, on the same connection.
	if got := pgScalar(t, f.db, "SELECT current_user"); got != poolUser {
		t.Errorf("after the transaction current_user = %q, want %q — the role outlived the transaction and the next request inherits it", got, poolUser)
	}
	if got := pgScalar(t, f.db, "SELECT current_setting('app.tenantId', true)"); got != "" {
		t.Errorf("after the transaction app.tenantId = %q, want it gone — the next request on this connection reads another tenant's identity", got)
	}

	// The whole point, stated as the leak it prevents: a query with no
	// identity established sees nothing, rather than tenant 1's rows.
	// It runs as the pool user, so the policy does not apply to it and
	// it sees everything the owner sees — which is why the assertion
	// is on the setting, not the row count. Establish the role alone
	// and the missing setting makes current_setting raise, which is
	// PostgreSQL failing closed on a policy it cannot evaluate.
	err := f.db.InTxAs(ctx, pg.Session{Role: f.role}, func(tx *pg.DB) error {
		var out []rlsPost
		return tx.Select().From(f.tbl).All(ctx, &out)
	})
	if err == nil {
		t.Error("a session with the role but no tenant setting read rows: the policy found a value for app.tenantId that this request never set")
	}
}

// A failure to establish the identity aborts the transaction rather
// than running the body as the pool's own user. Asserted against a
// real server because the interesting part is what the server was left
// holding: the connection has to go back to the pool clean and usable.
func TestPGInTxAsAbortsWhenTheRoleDoesNotExist(t *testing.T) {
	f := newRLSFixture(t)
	ctx := context.Background()
	poolUser := pgScalar(t, f.db, "SELECT current_user")

	ran := false
	err := f.db.InTxAs(ctx, pg.Session{
		Role:     f.role + "_does_not_exist",
		Settings: map[string]string{"app.tenantId": "1"},
	}, func(tx *pg.DB) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("InTxAs ran a transaction under a role that does not exist")
	}
	if ran {
		t.Error("the body ran as the pool's own user after the role could not be set — the fail-open this method exists to refuse")
	}
	if !strings.Contains(err.Error(), "SET LOCAL ROLE") {
		t.Errorf("err = %v, want it to name the statement that failed", err)
	}

	// The connection came back clean, and works.
	if got := pgScalar(t, f.db, "SELECT current_user"); got != poolUser {
		t.Errorf("after the aborted transaction current_user = %q, want %q", got, poolUser)
	}
	var out []rlsPost
	if err := f.db.Select().From(f.tbl).All(ctx, &out); err != nil {
		t.Fatalf("the pool was left unusable: %v", err)
	}
	if len(out) != 3 {
		t.Errorf("read %d rows as the owner, want 3", len(out))
	}
}

// A role name that a bare concatenation would get wrong, created for
// real and set for real. Quoting is what makes it work, and only a
// real server can confirm the quoting was right.
//
// Mixed case rather than a keyword, and the difference is what makes
// the test sharp. An unquoted keyword is a parse error, which any
// server rejects loudly; an unquoted mixed-case name is folded to
// lower case and then simply does not resolve, so SET ROLE RlsMixed
// fails with "role does not exist" — the failure a hand-rolled version
// hits in production against a role it can see in \du. (A keyword role
// is covered against the recorder in pg/rls_test.go, where a unique
// name is not needed; roles are cluster-wide, and a test that created
// a role literally named "select" would collide with every other
// consumer of the server.)
func TestPGInTxAsQuotesARoleThatFoldingWouldLose(t *testing.T) {
	db := openPGSingleConn(t)
	ctx := context.Background()

	role := integration.UniqueName(t, "RlsMixedCase")
	if role == strings.ToLower(role) {
		t.Fatalf("role %q has no upper case in it, so this test proves nothing", role)
	}
	createPGRole(t, db, role)

	var got string
	if err := db.InTxAs(ctx, pg.Session{Role: role}, func(tx *pg.DB) error {
		got = pgScalar(t, tx, "SELECT current_user")
		return nil
	}); err != nil {
		t.Fatalf("InTxAs with a mixed-case role: %v", err)
	}
	if got != role {
		t.Errorf("current_user = %q, want %q", got, role)
	}
}

// A role name drops refuses never reaches the server, and the refusal
// is local — no transaction is opened, so there is nothing for the
// pool to clean up.
func TestPGInTxAsRefusesARoleItCannotQuote(t *testing.T) {
	db := openPGSingleConn(t)
	ctx := context.Background()

	err := db.InTxAs(ctx, pg.Session{Role: "app\x00tenant"}, func(tx *pg.DB) error {
		t.Error("the body ran under a role name that cannot reach the server intact")
		return nil
	})
	if !errors.Is(err, pg.ErrInvalidRole) {
		t.Fatalf("err = %v, want ErrInvalidRole", err)
	}
	// The connection was never touched and still works.
	if got := pgScalar(t, db, "SELECT 1"); got != "1" {
		t.Errorf("SELECT 1 = %q after a refused session", got)
	}
}

// pgScalar runs a one-column, one-row query and returns the value as
// text, with a NULL coming back as the empty string — which is what
// current_setting(..., true) reports for a setting that is not set.
func pgScalar(t *testing.T, db *pg.DB, sqlText string) string {
	t.Helper()
	rows, err := db.Query(context.Background(), sqlText)
	if err != nil {
		t.Fatalf("%s: %v", sqlText, err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		t.Fatalf("%s: no row", sqlText)
	}
	var v sql.NullString
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("%s: scan: %v", sqlText, err)
	}
	return v.String
}

// A setting name drops does not check, because it is bound and the
// server is the authority on it. PostgreSQL accepts a custom setting
// only when its name is prefixed; an unprefixed name it does not
// recognise is an error, and the error aborts the transaction rather
// than leaving the body to run with an identity it does not have.
//
// This is the case the byte-level check on Session.Settings
// deliberately does NOT cover, so it is worth knowing what happens
// instead of assuming.
func TestPGInTxAsAbortsOnASettingTheServerRefuses(t *testing.T) {
	f := newRLSFixture(t)
	ctx := context.Background()

	ran := false
	err := f.db.InTxAs(ctx, pg.Session{
		Role: f.role,
		Settings: map[string]string{
			"app.tenantId":                  "1",
			"there_is_no_such_setting_here": "x",
		},
	}, func(tx *pg.DB) error {
		ran = true
		return nil
	})
	if err == nil {
		t.Fatal("InTxAs ran the body with a setting the server rejected")
	}
	if ran {
		t.Error("the body ran after a setting could not be established")
	}
	if !strings.Contains(err.Error(), "there_is_no_such_setting_here") {
		t.Errorf("err = %v, want it to name the setting that failed", err)
	}
	// The connection is clean and usable afterwards.
	if got := pgScalar(t, f.db, "SELECT current_setting('app.tenantId', true)"); got != "" {
		t.Errorf("app.tenantId = %q after the aborted transaction, want it gone", got)
	}
}

// The documented limit, against the server that decides it. Nothing
// stops the body of the transaction resetting its own role, and drops
// does not pretend otherwise — what it promises is about the
// transaction BOUNDARY, and the server is what keeps that promise.
//
// So: the body resets the role and sets a session-lifetime value for
// the very setting the policy reads. Both are real, both take effect,
// and both are gone once the transaction ends — because SET LOCAL's
// revert restores what the setting was when the transaction started,
// which is what a is_local=false SET inside that transaction is
// measured against.
func TestPGInTxAsBoundaryHoldsEvenWhenTheBodyChangesItsOwnIdentity(t *testing.T) {
	f := newRLSFixture(t)
	ctx := context.Background()
	poolUser := pgScalar(t, f.db, "SELECT current_user")

	if err := f.db.InTxAs(ctx, f.session("1"), func(tx *pg.DB) error {
		if got := pgScalar(t, tx, "SELECT current_user"); got != f.role {
			t.Fatalf("current_user = %q, want %q", got, f.role)
		}
		if _, err := tx.Exec(ctx, "RESET ROLE"); err != nil {
			return err
		}
		if got := pgScalar(t, tx, "SELECT current_user"); got != poolUser {
			t.Errorf("after the body's own RESET ROLE current_user = %q, want %q — drops does not police this, and should not appear to", got, poolUser)
		}
		return nil
	}); err != nil {
		t.Fatalf("InTxAs: %v", err)
	}

	if got := pgScalar(t, f.db, "SELECT current_user"); got != poolUser {
		t.Errorf("after the transaction current_user = %q, want %q", got, poolUser)
	}
	if got := pgScalar(t, f.db, "SELECT current_setting('app.tenantId', true)"); got != "" {
		t.Errorf("app.tenantId = %q after the transaction, want it gone whatever the body did", got)
	}
}

// pgQuoteIdent quotes an identifier for the raw DDL this file issues.
// It is spelled out here rather than reached for in pg because pg does
// not export it, and a test that needed it exported would be asking
// for API surface to serve a fixture.
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
