package sqlite_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/sqlite"
)

// What MembershipGuard renders once its junction subquery is a statement
// the package composed rather than SQL it wrote out by hand.
//
// The guard used to write "<owner> IN (SELECT <res> FROM <junction> WHERE
// <subj> = ?)" into a drops.ExprFunc, naming the junction table as a
// string. A membership table is exactly the kind that carries a
// DefaultFilter — a soft-delete column, so a revoked membership is kept
// for audit rather than deleted — and a subquery drops had never been
// told was a subquery carried none of it. So a revoked membership row
// went on authorising: everywhere else in the package a widened read
// returns rows the caller should not see, and on a guard it also grants
// a permission the subject does not have.
//
// pg fixed the same guard by composing it through the builder. sqlite has
// no ContextFilter machinery, so only the DefaultFilter half of that is
// observable here — and it is observable today, which is what these tests
// pin.

// The subject these tests put on ctx. It differs from every other such
// constant in the package's tests, so a predicate that bound the wrong
// value cannot pass by coincidence.
const azsSubject = int64(91)

func azsCtx() context.Context {
	return sqlite.WithSubject(context.Background(), azsSubject)
}

// azsJunction builds a membership table with the automatic predicate a
// real one has: a soft-delete DefaultFilter, so a revoked membership
// stops authorising.
func azsJunction(name string) (tbl *sqlite.Table, org, user *sqlite.Column) {
	t := sqlite.NewTable(name)
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	userCol := sqlite.Add(t, sqlite.BigInt("userId").NotNull())
	orgCol := sqlite.Add(t, sqlite.BigInt("organizationId").NotNull())
	sqlite.SoftDelete(t)
	return t, orgCol.Column, userCol.Column
}

// azsGuarded builds the guarded resource table. It carries no automatic
// predicate of its own, so every filter that appears in the rendered SQL
// can only have come from the junction table the guard names.
func azsGuarded(name string) (tbl *sqlite.Table, owner *sqlite.Column) {
	t := sqlite.NewTable(name)
	sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	owner = sqlite.Add(t, sqlite.BigInt("organizationId").NotNull()).Column
	return t, owner
}

type azsInvoice struct {
	ID    int64 `drop:"id"`
	OrgID int64 `drop:"organizationId"`
}

// The junction table's own DefaultFilters reach the subquery, which is
// what makes a revoked membership stop authorising.
func TestMembershipGuardSubqueryCarriesTheJunctionsDefaultFilters(t *testing.T) {
	members, org, user := azsJunction("azs_members")
	_, owner := azsGuarded("azs_invoices")

	pred, err := sqlite.MembershipGuard{
		Junction:         members,
		JunctionSubject:  user,
		JunctionResource: org,
		ResourceOwner:    owner,
	}.Predicate(azsCtx())
	if err != nil {
		t.Fatalf("Predicate: %v", err)
	}

	want := `("azs_invoices"."organizationId" IN (` +
		`SELECT "azs_members"."organizationId" FROM "azs_members" ` +
		`WHERE ("azs_members"."deletedAt" IS NULL) ` +
		`AND ("azs_members"."userId" = ?)))`

	got, args := sqlite.ToSQL(pred)
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if len(args) != 1 || args[0] != any(azsSubject) {
		t.Errorf("args = %v, want %v", args, []any{azsSubject})
	}
}

// The same predicate through the statement an entity actually sends, so
// the fix is pinned where a caller meets it rather than only on the guard
// in isolation.
func TestGuardedEntityQueryCarriesTheJunctionsDefaultFilters(t *testing.T) {
	members, org, user := azsJunction("azsq_members")
	guarded, owner := azsGuarded("azsq_invoices")
	ent := sqlite.NewEntity[azsInvoice](guarded).
		AuthorizeWith(sqlite.MembershipGuard{
			Junction:         members,
			JunctionSubject:  user,
			JunctionResource: org,
			ResourceOwner:    owner,
		})

	drv := &entDriver{}
	db := sqlite.New(drv)
	// The recording driver answers with no rows, so Get reports
	// ErrNoRows; the statement it sent on the way is what is under test.
	if _, err := ent.Get(db, azsCtx(), int64(5)); err != sqlite.ErrNoRows {
		t.Fatalf("Get: %v", err)
	}

	want := `SELECT "azsq_invoices"."id", "azsq_invoices"."organizationId" ` +
		`FROM "azsq_invoices" WHERE ("azsq_invoices"."id" = ?) ` +
		`AND ("azsq_invoices"."organizationId" IN (` +
		`SELECT "azsq_members"."organizationId" FROM "azsq_members" ` +
		`WHERE ("azsq_members"."deletedAt" IS NULL) ` +
		`AND ("azsq_members"."userId" = ?)))`

	if len(drv.queries) != 1 {
		t.Fatalf("queries = %d, want 1: %v", len(drv.queries), drv.queries)
	}
	if got := drv.queries[0]; got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	wantArgs := []any{int64(5), azsSubject}
	if len(drv.args[0]) != len(wantArgs) {
		t.Fatalf("args = %v, want %v", drv.args[0], wantArgs)
	}
	for i, a := range drv.args[0] {
		if a != wantArgs[i] {
			t.Errorf("args = %v, want %v", drv.args[0], wantArgs)
			break
		}
	}
}

// Unscoped is statement-local: widening the query the caller wrote does
// not widen the membership check inside it. A caller reading their own
// soft-deleted invoices must not thereby authorise through memberships
// that were revoked.
func TestUnscopedDoesNotWidenTheMembershipSubquery(t *testing.T) {
	members, org, user := azsJunction("azsu_members")
	guarded, owner := azsGuarded("azsu_invoices")
	sqlite.SoftDelete(guarded)
	ent := sqlite.NewEntity[azsInvoice](guarded).
		AuthorizeWith(sqlite.MembershipGuard{
			Junction:         members,
			JunctionSubject:  user,
			JunctionResource: org,
			ResourceOwner:    owner,
		})

	drv := &entDriver{}
	db := sqlite.New(drv)
	if _, err := ent.Query(db).Unscoped().All(azsCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	got := drv.queries[0]
	if strings.Contains(got, `"azsu_invoices"."deletedAt" IS NULL`) {
		t.Errorf("Unscoped did not drop the outer table's own filter:\n%s", got)
	}
	if !strings.Contains(got, `("azsu_members"."deletedAt" IS NULL)`) {
		t.Errorf("Unscoped reached into the membership subquery:\n%s", got)
	}
}

// A ctx with no subject still fails closed and builds no statement at
// all — the subject is read before anything is composed.
func TestMembershipGuardWithoutSubjectStillFailsClosed(t *testing.T) {
	members, org, user := azsJunction("azsn_members")
	_, owner := azsGuarded("azsn_invoices")

	var pred drops.Expression
	pred, err := sqlite.MembershipGuard{
		Junction:         members,
		JunctionSubject:  user,
		JunctionResource: org,
		ResourceOwner:    owner,
	}.Predicate(context.Background())
	if err != sqlite.ErrSubjectMissing {
		t.Errorf("err = %v, want %v", err, sqlite.ErrSubjectMissing)
	}
	if pred != nil {
		t.Errorf("pred = %v, want nil", pred)
	}
}
