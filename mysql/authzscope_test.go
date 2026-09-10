package mysql_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/mysql"
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
// pg fixed the same guard by composing it through the builder, and this
// package has the same ContextFilter machinery, so both halves are
// observable here: the junction's DefaultFilters reach the subquery,
// and the guard itself arrives as a context filter rather than as
// something the Entity methods inject.

// The subject these tests put on ctx. It differs from every other such
// constant in the package's tests, so a predicate that bound the wrong
// value cannot pass by coincidence.
const azsSubject = int64(91)

func azsCtx() context.Context {
	return mysql.WithSubject(context.Background(), azsSubject)
}

// azsJunction builds a membership table with the automatic predicate a
// real one has: a soft-delete DefaultFilter, so a revoked membership
// stops authorising.
func azsJunction(name string) (tbl *mysql.Table, org, user *mysql.Column) {
	t := mysql.NewTable(name)
	mysql.Add(t, mysql.BigInt("id").PrimaryKey())
	userCol := mysql.Add(t, mysql.BigInt("userId").NotNull())
	orgCol := mysql.Add(t, mysql.BigInt("organizationId").NotNull())
	mysql.SoftDelete(t)
	return t, orgCol.Column, userCol.Column
}

// azsGuarded builds the guarded resource table. It carries no automatic
// predicate of its own, so every filter that appears in the rendered SQL
// can only have come from the junction table the guard names.
func azsGuarded(name string) (tbl *mysql.Table, owner *mysql.Column) {
	t := mysql.NewTable(name)
	mysql.Add(t, mysql.BigInt("id").PrimaryKey())
	owner = mysql.Add(t, mysql.BigInt("organizationId").NotNull()).Column
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

	pred, err := mysql.MembershipGuard{
		Junction:         members,
		JunctionSubject:  user,
		JunctionResource: org,
		ResourceOwner:    owner,
	}.Predicate(azsCtx())
	if err != nil {
		t.Fatalf("Predicate: %v", err)
	}

	want := "(`azs_invoices`.`organizationId` IN (" +
		"SELECT `azs_members`.`organizationId` FROM `azs_members` " +
		"WHERE (`azs_members`.`deletedAt` IS NULL) " +
		"AND (`azs_members`.`userId` = ?)))"

	got, args := mysql.ToSQL(pred)
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
	ent := mysql.NewEntity[azsInvoice](guarded).
		AuthorizeWith(mysql.MembershipGuard{
			Junction:         members,
			JunctionSubject:  user,
			JunctionResource: org,
			ResourceOwner:    owner,
		})

	drv := &frDriver{}
	db := mysql.New(drv)
	// The recording driver answers with no rows, so Get reports
	// ErrNoRows; the statement it sent on the way is what is under test.
	if _, err := ent.Get(db, azsCtx(), int64(5)); !errors.Is(err, drops.ErrNoRows) {
		t.Fatalf("Get: %v", err)
	}

	want := "SELECT `azsq_invoices`.`id`, `azsq_invoices`.`organizationId` " +
		"FROM `azsq_invoices` WHERE (`azsq_invoices`.`id` = ?) " +
		"AND (`azsq_invoices`.`organizationId` IN (" +
		"SELECT `azsq_members`.`organizationId` FROM `azsq_members` " +
		"WHERE (`azsq_members`.`deletedAt` IS NULL) " +
		"AND (`azsq_members`.`userId` = ?)))"

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
	mysql.SoftDelete(guarded)
	ent := mysql.NewEntity[azsInvoice](guarded).
		AuthorizeWith(mysql.MembershipGuard{
			Junction:         members,
			JunctionSubject:  user,
			JunctionResource: org,
			ResourceOwner:    owner,
		})

	drv := &frDriver{}
	db := mysql.New(drv)
	if _, err := ent.Query(db).Unscoped().All(azsCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	got := drv.queries[0]
	if strings.Contains(got, "`azsu_invoices`.`deletedAt` IS NULL") {
		t.Errorf("Unscoped did not drop the outer table's own filter:\n%s", got)
	}
	if !strings.Contains(got, "(`azsu_members`.`deletedAt` IS NULL)") {
		t.Errorf("Unscoped reached into the membership subquery:\n%s", got)
	}
}

// A ctx with no subject still fails closed and builds no statement at
// all — the subject is read before anything is composed.
func TestMembershipGuardWithoutSubjectStillFailsClosed(t *testing.T) {
	members, org, user := azsJunction("azsn_members")
	_, owner := azsGuarded("azsn_invoices")

	var pred drops.Expression
	pred, err := mysql.MembershipGuard{
		Junction:         members,
		JunctionSubject:  user,
		JunctionResource: org,
		ResourceOwner:    owner,
	}.Predicate(context.Background())
	if err != mysql.ErrSubjectMissing {
		t.Errorf("err = %v, want %v", err, mysql.ErrSubjectMissing)
	}
	if pred != nil {
		t.Errorf("pred = %v, want nil", pred)
	}
}
