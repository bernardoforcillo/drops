package clickhouse_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
)

// What a real server said, and what it did not.
//
// The rendering asserted below was checked against a real ClickHouse
// 26.7.2.1 query parser — the engine embedded in the chdb python
// package, driven in clickhouse-local mode — by running EXPLAIN AST
// over each emitted statement. EXPLAIN AST parses and stops, so a
// passing probe proves the grammar accepts the string and nothing
// more. Three results from that probe are load-bearing here and are
// cited again at the tests that encode them:
//
//   - FOR INSERT is a syntax error: "Expected one of: ALL, SELECT,
//     end of query". A ClickHouse row policy has no write half.
//   - TO must follow USING. PostgreSQL's clause order — the order
//     drops/pg emits — is a syntax error here.
//   - WITH CHECK is accepted by the parser but has nowhere to go:
//     system.row_policies carries only select_filter, and the token
//     appears nowhere in ClickHouse's documentation.
//
// One semantic claim was executed rather than parsed, using the
// users.xml <filter> form of the same row-policy machinery (SQL-created
// policies cannot be exercised that way because clickhouse-local
// exposes no writeable access storage and rejects CREATE ROW POLICY
// with code 514). Under a policy of tenant = 'acme' on a three-row
// table, SELECT returned two rows, an INSERT of a fourth row belonging
// to another tenant succeeded with no error, and system.parts then
// reported four physical rows against a SELECT that still returned
// two. That is the fact the doc comments in rowpolicy.go rest on.
//
// Everything else about ClickHouse row policies in those doc comments —
// the two access_control_improvements defaults, how permissive and
// restrictive policies combine, what happens on a distributed table —
// rests on ClickHouse's own documentation and changelog, not on a
// server this test suite reached. The tests below assert rendering.

var (
	rpDocs     = clickhouse.NewDatabaseTable("analytics", "docs")
	rpTenant   = clickhouse.Add(rpDocs, clickhouse.String("tenantId").LowCardinality())
	rpDocsBody = clickhouse.Add(rpDocs, clickhouse.String("body"))
	rpOwner    = clickhouse.Add(rpDocs, clickhouse.UInt64("ownerId"))
)

var _ = rpDocsBody

func init() {
	rpDocs.Engine(clickhouse.MergeTree()).OrderBy(rpTenant)
}

func policySQL(e drops.Expression) string {
	sql, _ := clickhouse.ToSQL(e)
	return sql
}

func TestCreateRowPolicyRendering(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "minimal, one role",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("tenant_acme").
					On(rpDocs).
					UsingEq(rpTenant, "acme").
					To("app"))),
			want: `CREATE ROW POLICY "tenant_acme" ON "analytics"."docs" FOR SELECT USING ("tenantId" = 'acme') TO "app"`,
		},
		{
			name: "restrictive, several roles",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("tenant_acme").
					On(rpDocs).
					Restrictive().
					UsingEq(rpTenant, "acme").
					To("app", "bi"))),
			want: `CREATE ROW POLICY "tenant_acme" ON "analytics"."docs" FOR SELECT USING ("tenantId" = 'acme') AS RESTRICTIVE TO "app", "bi"`,
		},
		{
			name: "TO ALL",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("p").On(rpDocs).Using("1").ToAll())),
			want: `CREATE ROW POLICY "p" ON "analytics"."docs" FOR SELECT USING (1) TO ALL`,
		},
		{
			name: "TO ALL EXCEPT",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("p").On(rpDocs).Using("1").ToAllExcept("admin", "root"))),
			want: `CREATE ROW POLICY "p" ON "analytics"."docs" FOR SELECT USING (1) TO ALL EXCEPT "admin", "root"`,
		},
		{
			name: "no TO clause at all",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("p").On(rpDocs).Using("1"))),
			want: `CREATE ROW POLICY "p" ON "analytics"."docs" FOR SELECT USING (1)`,
		},
		{
			name: "IF NOT EXISTS",
			got: policySQL(clickhouse.CreateRowPolicyIfNotExists(
				clickhouse.NewRowPolicy("p").On(rpDocs).Using("1").ToAll())),
			want: `CREATE ROW POLICY IF NOT EXISTS "p" ON "analytics"."docs" FOR SELECT USING (1) TO ALL`,
		},
		{
			name: "OR REPLACE",
			got: policySQL(clickhouse.CreateOrReplaceRowPolicy(
				clickhouse.NewRowPolicy("p").On(rpDocs).Using("1").ToAll())),
			want: `CREATE ROW POLICY OR REPLACE "p" ON "analytics"."docs" FOR SELECT USING (1) TO ALL`,
		},
		{
			name: "ON CLUSTER sits between the name and the target",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("p").On(rpDocs).OnCluster("prod").Using("1").ToAll())),
			want: `CREATE ROW POLICY "p" ON CLUSTER "prod" ON "analytics"."docs" FOR SELECT USING (1) TO ALL`,
		},
		{
			name: "table in the default database",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("p").On(events).Using("1").ToAll())),
			want: `CREATE ROW POLICY "p" ON "events" FOR SELECT USING (1) TO ALL`,
		},
		{
			name: "every table in a database",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("p_all").OnAllTablesIn("analytics").Using("1").ToAll())),
			want: `CREATE ROW POLICY "p_all" ON "analytics".* FOR SELECT USING (1) TO ALL`,
		},
		{
			name: "unsigned literal is not quoted",
			got: policySQL(clickhouse.CreateRowPolicy(
				clickhouse.NewRowPolicy("p").On(rpDocs).UsingEq(rpOwner, uint64(42)).ToAll())),
			want: `CREATE ROW POLICY "p" ON "analytics"."docs" FOR SELECT USING ("ownerId" = 42) TO ALL`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("sql mismatch\n  got:  %s\n  want: %s", tc.got, tc.want)
			}
		})
	}
}

func TestDropRowPolicyRendering(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "plain",
			got:  policySQL(clickhouse.DropRowPolicy(clickhouse.NewRowPolicy("p").On(rpDocs))),
			want: `DROP ROW POLICY "p" ON "analytics"."docs"`,
		},
		{
			name: "if exists",
			got:  policySQL(clickhouse.DropRowPolicyIfExists(clickhouse.NewRowPolicy("p").On(rpDocs))),
			want: `DROP ROW POLICY IF EXISTS "p" ON "analytics"."docs"`,
		},
		{
			// DROP puts ON CLUSTER after the target where CREATE puts it
			// before. The asymmetry is ClickHouse's, not ours; both
			// orders were confirmed against the 26.7.2.1 parser and the
			// swapped ones are syntax errors.
			name: "on cluster trails the target",
			got: policySQL(clickhouse.DropRowPolicyIfExists(
				clickhouse.NewRowPolicy("p").On(rpDocs).OnCluster("prod"))),
			want: `DROP ROW POLICY IF EXISTS "p" ON "analytics"."docs" ON CLUSTER "prod"`,
		},
		{
			name: "database wide",
			got:  policySQL(clickhouse.DropRowPolicy(clickhouse.NewRowPolicy("p").OnAllTablesIn("analytics"))),
			want: `DROP ROW POLICY "p" ON "analytics".*`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("sql mismatch\n  got:  %s\n  want: %s", tc.got, tc.want)
			}
		})
	}
}

// TO after USING is not a stylistic choice. The 26.7.2.1 parser
// rejects the PostgreSQL order — CREATE ROW POLICY "p" ON "t" AS
// RESTRICTIVE FOR SELECT TO "a" USING x — with "Syntax error: failed
// at position 75 (TO) … Expected one of: token, Comma, USING, WITH
// CHECK, end of query". A reader porting a drops/pg declaration across
// will write the pg order, so the emitter has to be pinned to the
// order that parses, and pinned by a test that says why.
func TestRowPolicyClauseOrderPutsToAfterUsing(t *testing.T) {
	sql := policySQL(clickhouse.CreateRowPolicy(
		clickhouse.NewRowPolicy("p").On(rpDocs).Restrictive().Using("1").To("app")))
	using, to := strings.Index(sql, "USING"), strings.Index(sql, " TO ")
	if using < 0 || to < 0 {
		t.Fatalf("expected both USING and TO in: %s", sql)
	}
	if to < using {
		t.Errorf("TO precedes USING, which ClickHouse rejects:\n  %s", sql)
	}
	if as := strings.Index(sql, "AS RESTRICTIVE"); as < using {
		t.Errorf("AS must follow USING in the order drops emits:\n  %s", sql)
	}
}

// A ClickHouse row policy covers SELECT and nothing else: FOR INSERT
// is a syntax error, and there is no WITH CHECK to store — the
// parser tolerates the token but system.row_policies has only
// select_filter to put it in. So the emitter always says FOR SELECT
// out loud and never emits a check clause. The failure this prevents
// is a reader carrying drops/pg's Policy.WithCheck across and
// believing writes are constrained: under a tenant policy an INSERT
// naming another tenant succeeds, verified on the embedded engine.
func TestCreateRowPolicyNeverEmitsAWriteHalf(t *testing.T) {
	for _, p := range []*clickhouse.RowPolicy{
		clickhouse.NewRowPolicy("p").On(rpDocs).UsingEq(rpTenant, "acme").To("app"),
		clickhouse.NewRowPolicy("p").On(rpDocs).Restrictive().Using("1").ToAll(),
		clickhouse.NewRowPolicy("p").OnAllTablesIn("analytics").Using("1").ToAll(),
	} {
		sql := policySQL(clickhouse.CreateRowPolicy(p))
		if strings.Contains(sql, "WITH CHECK") {
			t.Errorf("emitted a WITH CHECK clause ClickHouse cannot store: %s", sql)
		}
		if strings.Contains(sql, "FOR INSERT") || strings.Contains(sql, "FOR ALL") {
			t.Errorf("emitted a command ClickHouse's parser rejects: %s", sql)
		}
		if !strings.Contains(sql, "FOR SELECT") {
			t.Errorf("expected an explicit FOR SELECT in: %s", sql)
		}
	}
}

// UsingEq exists so the common condition — a tenant column equal to a
// tenant key — cannot be built by string concatenation. The policy
// text is stored server-side and evaluated on every later SELECT, so
// a quote smuggled into it is not one bad query, it is a permanently
// widened filter that no application code can take back.
func TestUsingEqEscapesLiterals(t *testing.T) {
	got := policySQL(clickhouse.CreateRowPolicy(
		clickhouse.NewRowPolicy("p").On(rpDocs).UsingEq(rpTenant, `a' OR 1 --`).ToAll()))
	want := `CREATE ROW POLICY "p" ON "analytics"."docs" FOR SELECT USING ("tenantId" = 'a'' OR 1 --') TO ALL`
	if got != want {
		t.Errorf("sql mismatch\n  got:  %s\n  want: %s", got, want)
	}

	got = policySQL(clickhouse.CreateRowPolicy(
		clickhouse.NewRowPolicy("p").On(rpDocs).UsingEq(rpTenant, `back\slash`).ToAll()))
	want = `CREATE ROW POLICY "p" ON "analytics"."docs" FOR SELECT USING ("tenantId" = 'back\\slash') TO ALL`
	if got != want {
		t.Errorf("backslash not escaped\n  got:  %s\n  want: %s", got, want)
	}
}

// The condition names a column of the table the policy is attached to,
// so it must render bare. "docs"."tenantId" inside USING would name a
// relation the policy's scope does not have, exactly the failure
// Builder.SetBareIdents exists for in the DDL renderers.
func TestUsingEqRendersBareColumn(t *testing.T) {
	got := policySQL(clickhouse.CreateRowPolicy(
		clickhouse.NewRowPolicy("p").On(rpDocs).UsingEq(rpTenant, "acme").ToAll()))
	if strings.Contains(got, `"docs"."tenantId"`) {
		t.Errorf("qualified the condition column: %s", got)
	}
	if !strings.Contains(got, `("tenantId" = 'acme')`) {
		t.Errorf("expected a bare column reference in: %s", got)
	}
}

func TestRowPolicyIncompleteDeclarations(t *testing.T) {
	cases := []struct {
		name   string
		policy *clickhouse.RowPolicy
		want   error
	}{
		{
			name:   "no target",
			policy: clickhouse.NewRowPolicy("p").Using("1").ToAll(),
			want:   clickhouse.ErrRowPolicyTargetRequired,
		},
		{
			name:   "no condition",
			policy: clickhouse.NewRowPolicy("p").On(rpDocs).ToAll(),
			want:   clickhouse.ErrRowPolicyConditionRequired,
		},
		{
			name:   "unsupported literal",
			policy: clickhouse.NewRowPolicy("p").On(rpDocs).UsingEq(rpTenant, struct{}{}).ToAll(),
			want:   clickhouse.ErrRowPolicyUnsupportedLiteral,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := clickhouse.CreateRowPolicyErr(tc.policy); !errors.Is(err, tc.want) {
				t.Errorf("CreateRowPolicyErr = %v, want %v", err, tc.want)
			}
			if err := tc.policy.Err(); !errors.Is(err, tc.want) {
				t.Errorf("Err() = %v, want %v", err, tc.want)
			}
		})
	}
}

// An incomplete policy must not render as a statement a server would
// accept. A CREATE ROW POLICY missing its USING clause is valid SQL
// with a meaning drops cannot state, so the renderer breaks the
// statement on purpose — the same trick CreateTable plays when the
// engine is missing, and for the sharper reason that this object is
// an access control decision.
func TestIncompleteRowPolicyRendersUnrunnableSQL(t *testing.T) {
	for _, p := range []*clickhouse.RowPolicy{
		clickhouse.NewRowPolicy("p").Using("1").ToAll(),
		clickhouse.NewRowPolicy("p").On(rpDocs).ToAll(),
	} {
		sql := policySQL(clickhouse.CreateRowPolicy(p))
		if !strings.Contains(sql, "/* drops/clickhouse:") {
			t.Errorf("expected a loud marker in: %s", sql)
		}
	}
}

func TestNewRowPolicyValidatesNames(t *testing.T) {
	cases := []struct {
		name string
		call func()
	}{
		{"empty policy name", func() { clickhouse.NewRowPolicy("") }},
		{"NUL in policy name", func() { clickhouse.NewRowPolicy("a\x00b") }},
		{"empty role", func() { clickhouse.NewRowPolicy("p").To("") }},
		{"empty cluster", func() { clickhouse.NewRowPolicy("p").OnCluster("") }},
		{"empty database", func() { clickhouse.NewRowPolicy("p").OnAllTablesIn("") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected a panic")
				}
				err, ok := r.(error)
				if !ok || !errors.Is(err, clickhouse.ErrInvalidIdentifier) {
					t.Fatalf("panic = %v, want ErrInvalidIdentifier", r)
				}
			}()
			tc.call()
		})
	}
}

// A policy attached to a table takes that table as its target, so the
// declaration reads like drops/pg's Table.AddPolicy. It does not mean
// the policy is part of the table's schema — see the note on
// Table.AddRowPolicy for why nothing carries it into a snapshot.
func TestTableAddRowPolicyBindsTheTarget(t *testing.T) {
	tbl := clickhouse.NewDatabaseTable("analytics", "notes")
	tenant := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant)

	p := clickhouse.NewRowPolicy("notes_acme").UsingEq(tenant, "acme").To("app")
	tbl.AddRowPolicy(p)

	got := tbl.RowPolicies()
	if len(got) != 1 || got[0] != p {
		t.Fatalf("RowPolicies() = %v, want the one declared policy", got)
	}
	want := `CREATE ROW POLICY "notes_acme" ON "analytics"."notes" FOR SELECT USING ("tenantId" = 'acme') TO "app"`
	if sql := policySQL(clickhouse.CreateRowPolicy(p)); sql != want {
		t.Errorf("sql mismatch\n  got:  %s\n  want: %s", sql, want)
	}
}

// An explicit target wins over the table it is attached to: a policy
// declared ON db.* and then hung off one table for readability must
// keep the wider target, or attaching it would silently narrow an
// access rule.
func TestTableAddRowPolicyKeepsAnExplicitTarget(t *testing.T) {
	tbl := clickhouse.NewDatabaseTable("analytics", "notes")
	tenant := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant)

	p := clickhouse.NewRowPolicy("all_acme").OnAllTablesIn("analytics").Using("1").ToAll()
	tbl.AddRowPolicy(p)

	want := `CREATE ROW POLICY "all_acme" ON "analytics".* FOR SELECT USING (1) TO ALL`
	if sql := policySQL(clickhouse.CreateRowPolicy(p)); sql != want {
		t.Errorf("attaching narrowed the target\n  got:  %s\n  want: %s", sql, want)
	}
}

// Aliasing a table is a query-time act; the alias must not inherit a
// declaration-time access object, and appending to the alias must not
// reach back into the original's slice.
func TestTableAliasDoesNotCarryRowPolicies(t *testing.T) {
	tbl := clickhouse.NewDatabaseTable("analytics", "notes2")
	tenant := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant)
	tbl.AddRowPolicy(clickhouse.NewRowPolicy("a").Using("1").ToAll())

	if got := tbl.As("n").RowPolicies(); len(got) != 0 {
		t.Errorf("alias carried %d policies, want 0", len(got))
	}
	if got := tbl.RowPolicies(); len(got) != 1 {
		t.Errorf("aliasing disturbed the original: %d policies, want 1", len(got))
	}
}

func TestRowPolicyAccessors(t *testing.T) {
	p := clickhouse.NewRowPolicy("p").
		On(rpDocs).
		OnCluster("prod").
		Restrictive().
		UsingEq(rpTenant, "acme").
		To("app", "bi")

	if p.Name() != "p" {
		t.Errorf("Name() = %q", p.Name())
	}
	if p.Cluster() != "prod" {
		t.Errorf("Cluster() = %q", p.Cluster())
	}
	if !p.IsRestrictive() {
		t.Error("IsRestrictive() = false")
	}
	if p.TargetDatabase() != "analytics" || p.TargetTable() != "docs" {
		t.Errorf("target = %q.%q", p.TargetDatabase(), p.TargetTable())
	}
	if p.AppliesToAllTables() {
		t.Error("AppliesToAllTables() = true for a table-scoped policy")
	}
	if p.UsingExpr() != `"tenantId" = 'acme'` {
		t.Errorf("UsingExpr() = %q", p.UsingExpr())
	}
	roles := p.Roles()
	if len(roles) != 2 || roles[0] != "app" || roles[1] != "bi" {
		t.Errorf("Roles() = %v", roles)
	}
	roles[0] = "mutated"
	if p.Roles()[0] != "app" {
		t.Error("Roles() handed back the live slice")
	}
	if p.AppliesToAllRoles() {
		t.Error("AppliesToAllRoles() = true after To()")
	}
	if got := clickhouse.NewRowPolicy("q").ToAllExcept("admin").ExceptRoles(); len(got) != 1 || got[0] != "admin" {
		t.Errorf("ExceptRoles() = %v", got)
	}
}
