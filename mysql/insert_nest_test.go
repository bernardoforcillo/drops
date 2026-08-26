package mysql_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// [InsertBuilder.Unscoped] now says in writing that it stops at the
// edge of the statement it was said on. It has always behaved that
// way, but the sentence was in pg's and sqlite's doc and not in this
// one, so the boundary here was something a reviewer had to go and
// assert for themselves rather than read — and an escape hatch whose
// blast radius is undocumented is one that gets assumed wider or
// narrower than it is.
//
// The wider assumption is the dangerous one: a caller who believes
// Unscoped on the INSERT widens the subquery too will read this
// statement as spanning tenants and write it expecting every tenant's
// notes to be considered. It considers one tenant's, and the row it
// writes is computed from a slice of what the author thought.

// nestSchema builds an outer table to insert into and an inner table
// to read from, each carrying a tenant axis and nothing else, so every
// predicate in the rendered SQL can only have come from one of them.
func nestSchema() (outer *mysql.Table, name *mysql.Col[string], inner *mysql.Table, innerID *mysql.Col[int64]) {
	out := mysql.NewTable("gadgets")
	mysql.Add(out, mysql.BigInt("id").PrimaryKey())
	outTenant := mysql.Add(out, mysql.BigInt("tenantId").NotNull())
	outName := mysql.Add(out, mysql.Text("name"))
	out.ContextFilter(mysql.TenantFilter(outTenant.Column))
	out.ScopeWritesByTenant(outTenant.Column)

	in := mysql.NewTable("notes")
	inID := mysql.Add(in, mysql.BigInt("id").PrimaryKey())
	inTenant := mysql.Add(in, mysql.BigInt("tenantId").NotNull())
	in.ContextFilter(mysql.TenantFilter(inTenant.Column))

	return out, outName, in, inID
}

// TestInsertUnscopedLeavesInnerStatementsScoped pins the boundary of
// the escape hatch. The outer INSERT gives up its stamp — there is no
// tenantId in its column list — and the subquery bound as its value
// keeps its own predicate in the same statement.
//
// The SQL is compared exactly rather than searched for a substring,
// because the defect this is about is a predicate that is present and
// powerless, and "contains tenantId" cannot tell that apart from one
// in the wrong clause.
func TestInsertUnscopedLeavesInnerStatementsScoped(t *testing.T) {
	outer, name, inner, innerID := nestSchema()
	db := mysql.New(dropstest.New())

	sub := db.Select(innerID).From(inner).Limit(1)
	sql, args := mustCtxSQL(t, db.Insert(outer).
		Row(name.Expr(mysql.Subquery(sub))).
		Unscoped())

	wantText(t, sql, "INSERT INTO `gadgets` (`name`) VALUES "+
		"((SELECT `notes`.`id` FROM `notes` WHERE (`notes`.`tenantId` = ?) LIMIT ?))")
	wantArgs(t, args, scopeTenant, int64(1))
}

// And the refusal travels with the predicate. An inner statement that
// cannot say which tenant it means stops the whole INSERT, rather than
// the outer Unscoped standing in as an answer for it: the caller gave
// up the axis on the statement they were writing, not on the one they
// were reading from.
func TestInsertUnscopedDoesNotAnswerForAnInnerStatement(t *testing.T) {
	outer, name, inner, innerID := nestSchema()
	drv := dropstest.New()
	db := mysql.New(drv)

	sub := db.Select(innerID).From(inner).Limit(1)
	_, _, err := db.Insert(outer).
		Row(name.Expr(mysql.Subquery(sub))).
		Unscoped().ToSQLCtx(context.Background())

	if !errors.Is(err, mysql.ErrTenantMissing) {
		t.Fatalf("err = %v, want ErrTenantMissing", err)
	}
	if got := len(drv.Statements()); got != 0 {
		t.Errorf("statements sent: got = %v, want 0 (%v)", got, drv.Statements())
	}
}
