package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// What MembershipGuard renders, and what it refuses.
//
// The guard hand-wrote its junction subquery — "<owner> IN (SELECT <res>
// FROM <junction> WHERE <subj> = $1)" — into a drops.ExprFunc, naming the
// junction table as a string. Two things followed from that, and the
// second is what let it survive five rounds of enforcement.
//
// The subquery carried none of the junction table's own automatic
// predicates. A membership table is exactly the kind that has them: a
// tenant column on a shared schema, a soft-delete column so a revoked
// membership is kept for audit rather than deleted. Neither reached the
// subquery, so a soft-deleted membership still authorised, and a
// membership belonging to another tenant authorised as well. On a guard
// that is not a widened read, it is a widened permission.
//
// And being a closure, nothing could fix that from the outside. The round
// before this one taught the resolver to walk the predicates a table's
// filters answer with, precisely so a policy written as "the rows this
// subject may see are the ones some other scoped table names" gets that
// other table scoped. The walk reached this guard's predicate and stopped
// at the closure. So the guard was the one predicate in the package that
// the fix for this very failure mode could not reach.
//
// The structural half — the check that fails the next producer written in
// that shape, on the day it is written — is
// TestNoAutomaticPredicateIsOpaqueToTheResolver in
// authzscope_internal_test.go.

// The tenant and subject these tests put on ctx. They differ from each
// other and from every other such constant in the package's tests, so a
// predicate that bound the wrong one cannot pass by coincidence.
const (
	azTenant  = int64(83)
	azSubject = int64(84)
)

func azCtx() context.Context {
	return pg.WithSubject(pg.WithTenant(context.Background(), azTenant), azSubject)
}

// azJunction builds a membership table with both kinds of automatic
// predicate a real one has: a soft-delete DefaultFilter, so a revoked
// membership stops authorising, and a tenant ContextFilter, so one
// customer's membership rows cannot authorise another customer's.
func azJunction(name string) (tbl *pg.Table, org, user *pg.Column) {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	userCol := pg.Add(t, pg.BigInt("userId").NotNull())
	orgCol := pg.Add(t, pg.BigInt("organizationId").NotNull())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	pg.ApplyMixins(t, &pg.SoftDeleteMixin{})
	t.ContextFilter(pg.TenantFilter(tenant))
	return t, orgCol.Column, userCol.Column
}

// azGuarded builds the guarded resource table and registers a
// MembershipGuard over junction on it — which is what Entity.AuthorizeWith
// does, minus the entity.
//
// The guarded table carries no automatic predicate of its own, so every
// filter that appears in the rendered SQL can only have come from the
// junction table the guard names.
func azGuarded(name string, junction *pg.Table, subject, resource *pg.Column) (tbl *pg.Table, owner *pg.Column) {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	owner = pg.Add(t, pg.BigInt("organizationId").NotNull()).Column
	t.ContextFilter(pg.MembershipGuard{
		Junction:         junction,
		JunctionSubject:  subject,
		JunctionResource: resource,
		ResourceOwner:    owner,
	}.Predicate)
	return t, owner
}

// The junction table's own filters reach the subquery, which is what
// makes a revoked or wrong-tenant membership stop authorising.
//
// Every predicate in the statement below belongs to "azm_members": the
// guarded table has none of its own. So the soft-delete guard, the tenant
// predicate and the bound tenant are proof that the subquery was resolved
// as the statement it is rather than rendered as the string it was.
func TestMembershipGuardSubqueryCarriesTheJunctionTablesFilters(t *testing.T) {
	members, org, user := azJunction("azm_members")
	guarded, _ := azGuarded("azm_invoices", members, user, org)

	db := pg.New(nil)
	want := `SELECT * FROM "azm_invoices" WHERE ` +
		`("azm_invoices"."organizationId" IN (` +
		`SELECT "azm_members"."organizationId" FROM "azm_members" ` +
		`WHERE ("azm_members"."deletedAt" IS NULL) ` +
		`AND ("azm_members"."userId" = $1) ` +
		`AND ("azm_members"."tenantId" = $2)))`

	got, args, err := db.Select().From(guarded).ToSQLCtx(azCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if !sameArgs(args, []any{azSubject, azTenant}) {
		t.Errorf("args = %v, want %v", args, []any{azSubject, azTenant})
	}
}

// A junction table whose tenant cannot be resolved refuses the whole
// statement rather than answering with an unfiltered membership check.
//
// The subject is on ctx, so the guard itself is satisfied: what is
// missing is the junction table's own axis, which the hand-written
// subquery never asked for and so could never have refused over.
func TestMembershipGuardWithoutTheJunctionsTenantRefuses(t *testing.T) {
	members, org, user := azJunction("azn_members")
	guarded, _ := azGuarded("azn_invoices", members, user, org)

	db := pg.New(nil)
	ctx := pg.WithSubject(context.Background(), azSubject)
	sql, _, err := db.Select().From(guarded).ToSQLCtx(ctx)
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
}

// The guard is a predicate like any other, so every statement kind that
// reaches a table's filters reaches it — and the junction subquery has to
// be scoped in all of them. A write is where the difference is felt: an
// UPDATE or a DELETE authorised by a stale membership row changes rows
// the request was never allowed to see.
func TestMembershipGuardIsScopedOnEveryStatementKind(t *testing.T) {
	members, org, user := azJunction("azk_members")
	guarded, _ := azGuarded("azk_invoices", members, user, org)
	total := pg.Add(guarded, pg.BigInt("total").NotNull())

	db := pg.New(nil)
	// The membership check with the junction table's own filters
	// resolved, as it reads once the placeholders of the enclosing
	// statement are accounted for.
	sub := func(subjPH, tenantPH string) string {
		return `("azk_invoices"."organizationId" IN (` +
			`SELECT "azk_members"."organizationId" FROM "azk_members" ` +
			`WHERE ("azk_members"."deletedAt" IS NULL) ` +
			`AND ("azk_members"."userId" = ` + subjPH + `) ` +
			`AND ("azk_members"."tenantId" = ` + tenantPH + `)))`
	}
	plain := pg.NewTable("azk_plain")
	plainID := pg.Add(plain, pg.BigSerial("id").PrimaryKey())

	tests := []struct {
		name string
		stmt func() ctxRenderer
		want string
		args []any
	}{
		{
			name: "Select",
			stmt: func() ctxRenderer { return db.Select().From(guarded) },
			want: `SELECT * FROM "azk_invoices" WHERE ` + sub("$1", "$2"),
			args: []any{azSubject, azTenant},
		},
		{
			name: "Join",
			stmt: func() ctxRenderer {
				return db.Select(plainID).From(plain).
					Join(guarded, pg.Eq(plainID, guarded.Col("id")))
			},
			want: `SELECT "azk_plain"."id" FROM "azk_plain" ` +
				`INNER JOIN "azk_invoices" ON ("azk_plain"."id" = "azk_invoices"."id") ` +
				`WHERE ` + sub("$1", "$2"),
			args: []any{azSubject, azTenant},
		},
		{
			name: "Update",
			stmt: func() ctxRenderer { return db.Update(guarded).Set(total.Val(int64(9))) },
			want: `UPDATE "azk_invoices" SET "total" = $1 WHERE ` + sub("$2", "$3"),
			args: []any{int64(9), azSubject, azTenant},
		},
		{
			name: "Delete",
			stmt: func() ctxRenderer { return db.Delete(guarded) },
			want: `DELETE FROM "azk_invoices" WHERE ` + sub("$1", "$2"),
			args: []any{azSubject, azTenant},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, args, err := tc.stmt().ToSQLCtx(azCtx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if got != tc.want {
				t.Errorf("got = %v, want %v", got, tc.want)
			}
			if !sameArgs(args, tc.args) {
				t.Errorf("args = %v, want %v", args, tc.args)
			}
		})
	}
}

// A junction table with no automatic predicates of its own renders the
// membership check and nothing else: the guard adds scoping where there
// is scoping to add, and adds nothing where there is none.
func TestMembershipGuardOverAPlainJunctionAddsNothing(t *testing.T) {
	members := pg.NewTable("azp_members")
	user := pg.Add(members, pg.BigInt("userId").NotNull()).Column
	org := pg.Add(members, pg.BigInt("organizationId").NotNull()).Column
	guarded, _ := azGuarded("azp_invoices", members, user, org)

	db := pg.New(nil)
	want := `SELECT * FROM "azp_invoices" WHERE ` +
		`("azp_invoices"."organizationId" IN (` +
		`SELECT "azp_members"."organizationId" FROM "azp_members" ` +
		`WHERE ("azp_members"."userId" = $1)))`
	got, args, err := db.Select().From(guarded).ToSQLCtx(azCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if !sameArgs(args, []any{azSubject}) {
		t.Errorf("args = %v, want %v", args, []any{azSubject})
	}
}

// A guard whose junction table is the guarded table itself is the cycle
// the previous round named: resolving the table's filters asks for a
// statement over the same table, which asks for its filters again. It is
// refused rather than followed, and the refusal names the way out.
//
// The hand-written subquery could not reach this shape at all, which is
// the same fact from the other side: it was never resolved, so it never
// re-entered anything.
func TestMembershipGuardOnItsOwnTableIsRefused(t *testing.T) {
	self := pg.NewTable("azs_people")
	pg.Add(self, pg.BigSerial("id").PrimaryKey())
	user := pg.Add(self, pg.BigInt("userId").NotNull()).Column
	org := pg.Add(self, pg.BigInt("organizationId").NotNull()).Column
	self.ContextFilter(pg.MembershipGuard{
		Junction:         self,
		JunctionSubject:  user,
		JunctionResource: org,
		ResourceOwner:    org,
	}.Predicate)

	db := pg.New(nil)
	sql, _, err := db.Select().From(self).ToSQLCtx(azCtx())
	if !errors.Is(err, pg.ErrContextFilterCycle) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrContextFilterCycle)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
}

// The guard still refuses a ctx with no subject, and refuses it before it
// builds anything — the property the round that introduced it pinned, and
// the one a rewrite is most likely to lose by moving the subject lookup
// below the statement it is for.
func TestMembershipGuardWithoutSubjectStillRefuses(t *testing.T) {
	members, org, user := azJunction("azq_members")
	guarded, owner := azGuarded("azq_invoices", members, user, org)
	g := pg.MembershipGuard{
		Junction:         members,
		JunctionSubject:  user,
		JunctionResource: org,
		ResourceOwner:    owner,
	}

	tenantOnly := pg.WithTenant(context.Background(), azTenant)
	if _, err := g.Predicate(tenantOnly); !errors.Is(err, pg.ErrSubjectMissing) {
		t.Errorf("Predicate err = %v, want %v", err, pg.ErrSubjectMissing)
	}
	db := pg.New(nil)
	sql, _, err := db.Select().From(guarded).ToSQLCtx(tenantOnly)
	if !errors.Is(err, pg.ErrSubjectMissing) {
		t.Fatalf("got = %v (sql %q), want %v", err, sql, pg.ErrSubjectMissing)
	}
	if sql != "" {
		t.Errorf("got sql = %q, want no statement at all", sql)
	}
}

// An incomplete guard is a declaration error rather than a query, and
// stays one: each of the four handles is required, and none of them
// defaults to something that would quietly authorise.
func TestMembershipGuardWithMissingHandlesRefuses(t *testing.T) {
	members, org, user := azJunction("azr_members")
	guarded := pg.NewTable("azr_invoices")
	pg.Add(guarded, pg.BigSerial("id").PrimaryKey())
	owner := pg.Add(guarded, pg.BigInt("organizationId").NotNull()).Column

	tests := []struct {
		name  string
		guard pg.MembershipGuard
	}{
		{"no junction", pg.MembershipGuard{JunctionSubject: user, JunctionResource: org, ResourceOwner: owner}},
		{"no subject column", pg.MembershipGuard{Junction: members, JunctionResource: org, ResourceOwner: owner}},
		{"no resource column", pg.MembershipGuard{Junction: members, JunctionSubject: user, ResourceOwner: owner}},
		{"no owner column", pg.MembershipGuard{Junction: members, JunctionSubject: user, JunctionResource: org}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := tc.guard.Predicate(azCtx())
			if err == nil {
				t.Fatalf("got no error, want one")
			}
			if e != nil {
				t.Errorf("got expression %v, want none", e)
			}
		})
	}
}
