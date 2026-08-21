package sqlite_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/sqlite"
)

// The tenant axis, declared on the TABLE and resolved by the EXECUTORS.
//
// Every test here asserts on RENDERED SQL and ARGS rather than on rows
// that came back, because a round trip cannot see this defect: a
// fixture with one tenant in it answers a leaking query and a scoped
// query identically, and the whole failure mode is a statement that
// looks right in a review and reads somebody else's rows in production.
//
// The shapes are the ones drops/pg found the hard way over five rounds,
// asked again of this dialect, where several of the answers differ
// because the SQL does.

// scopeTenant is the tenant these tests put on ctx. It is distinctive
// so a predicate that bound the wrong value cannot pass by coincidence.
const scopeTenant = int64(77)

func scopeCtx() context.Context {
	return sqlite.WithTenant(context.Background(), scopeTenant)
}

// scopedTable builds a table carrying a tenant axis and nothing else,
// so every predicate in the rendered SQL can only have come from it.
func scopedTable(name string) (tbl *sqlite.Table, id, tenant *sqlite.Column) {
	t := sqlite.NewTable(name)
	idCol := sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	tenantCol := sqlite.Add(t, sqlite.BigInt("tenantId").NotNull())
	sqlite.Add(t, sqlite.Text("title"))
	t.ContextFilter(sqlite.TenantFilter(tenantCol))
	return t, idCol.Column, tenantCol.Column
}

// plainTable builds a table with no automatic predicate at all, to sit
// on the other side of a join or to hold the outer statement. The
// handle comes back typed because binding a value into an INSERT goes
// through (*Col[T]).Val.
func plainTable(name string) (tbl *sqlite.Table, id *sqlite.Col[int64]) {
	t := sqlite.NewTable(name)
	idCol := sqlite.Add(t, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(t, sqlite.BigInt("postId"))
	return t, idCol
}

// ctxRenderable is every builder in the package: the interface exists
// so these tests ask each of them the same question through one helper.
type ctxRenderable interface {
	ToSQLCtx(ctx context.Context) (string, []any, error)
}

func renderCtx(t *testing.T, s ctxRenderable, ctx context.Context) (string, []any) {
	t.Helper()
	sql, args, err := s.ToSQLCtx(ctx)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	return sql, args
}

func sameArgs(got, want []any) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func checkSQL(t *testing.T, gotSQL, wantSQL string, gotArgs, wantArgs []any) {
	t.Helper()
	if gotSQL != wantSQL {
		t.Errorf("sql = %v, want %v", gotSQL, wantSQL)
	}
	if !sameArgs(gotArgs, wantArgs) {
		t.Errorf("args = %v, want %v", gotArgs, wantArgs)
	}
}

// A bare SELECT built straight from the DB — no Entity anywhere in the
// call — carries the axis. This is the spelling the readme shows first
// and the one an entity-injected predicate never reached.
func TestBareSelectCarriesTheTableAxis(t *testing.T) {
	posts, id, _ := scopedTable("sc_posts")
	db := sqlite.New(nil)

	sql, args := renderCtx(t, db.Select().From(posts).Where(sqlite.Eq(id, int64(3))), scopeCtx())
	checkSQL(t, sql,
		`SELECT * FROM "sc_posts" WHERE ("sc_posts"."id" = ?) AND ("sc_posts"."tenantId" = ?)`,
		args, []any{int64(3), scopeTenant})
}

// The filter fails closed: no tenant on ctx is an error and no
// statement at all, rather than a query that quietly spans customers.
func TestSelectWithoutTenantRefusesAndSendsNothing(t *testing.T) {
	posts, _, _ := scopedTable("scn_posts")
	drv := dropstest.New()
	db := sqlite.New(drv)

	var out []struct{}
	err := db.Select().From(posts).All(context.Background(), &out)
	if !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
	// The refusal names the table and column that stopped the query;
	// nothing exported asks a table which filters it carries, so this
	// is the whole diagnostic a caller gets.
	if !strings.Contains(err.Error(), "scn_posts.tenantId") {
		t.Errorf("err = %v, want it to name scn_posts.tenantId", err)
	}
}

// ToSQL can no longer show the whole statement, and says so by omitting
// the context filter; ToSQLCtx is its ctx-aware twin. Pinning both is
// what stops a test asserting on ToSQL and reading a scoped statement
// where the executor would send a different one.
func TestToSQLOmitsContextFiltersAndToSQLCtxCarriesThem(t *testing.T) {
	posts, _, _ := scopedTable("sct_posts")
	db := sqlite.New(nil)
	sel := db.Select().From(posts)

	if got, args := sel.ToSQL(); got != `SELECT * FROM "sct_posts"` || len(args) != 0 {
		t.Errorf("ToSQL = %v %v, want the unscoped render", got, args)
	}
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql, `SELECT * FROM "sct_posts" WHERE ("sct_posts"."tenantId" = ?)`,
		args, []any{scopeTenant})
}

// Rendering a builder twice must not accrete predicates, and a builder
// held across requests must not pin one request's tenant into the next.
// Both are one property: the executors resolve onto a COPY.
func TestABuilderIsReusableAcrossRequests(t *testing.T) {
	posts, _, _ := scopedTable("scr_posts")
	db := sqlite.New(nil)
	sel := db.Select().From(posts)

	want := `SELECT * FROM "scr_posts" WHERE ("scr_posts"."tenantId" = ?)`
	for _, tenant := range []int64{scopeTenant, 91, scopeTenant} {
		sql, args := renderCtx(t, sel, sqlite.WithTenant(context.Background(), tenant))
		checkSQL(t, sql, want, args, []any{tenant})
	}
}

// An UPDATE carries the axis, so a write cannot reach another tenant's
// row by guessing an id.
func TestUpdateCarriesTheTableAxis(t *testing.T) {
	posts, id, _ := scopedTable("scu_posts")
	title := posts.Col("title")
	db := sqlite.New(nil)

	upd := db.Update(posts).
		SetExpr(title, drops.Raw("'x'")).
		Where(sqlite.Eq(id, int64(4)))
	sql, args := renderCtx(t, upd, scopeCtx())
	checkSQL(t, sql,
		`UPDATE "scu_posts" SET "title" = 'x' WHERE ("scu_posts"."id" = ?) AND ("scu_posts"."tenantId" = ?)`,
		args, []any{int64(4), scopeTenant})

	for i := 0; i < 2; i++ {
		sql, args := renderCtx(t, upd, scopeCtx())
		if strings.Count(sql, "tenantId") != 1 {
			t.Fatalf("render %d bound the tenant %d times: %s", i, strings.Count(sql, "tenantId"), sql)
		}
		if len(args) != 2 {
			t.Fatalf("render %d args = %v, want 2", i, args)
		}
	}
}

// A DELETE carries it too, and refuses without a tenant — the one
// statement in the package nothing walks back.
func TestDeleteCarriesTheTableAxisAndRefusesWithoutOne(t *testing.T) {
	posts, id, _ := scopedTable("scd_posts")
	drv := dropstest.New()
	db := sqlite.New(drv)

	del := db.Delete(posts).Where(sqlite.Eq(id, int64(9)))
	sql, args := renderCtx(t, del, scopeCtx())
	checkSQL(t, sql,
		`DELETE FROM "scd_posts" WHERE ("scd_posts"."id" = ?) AND ("scd_posts"."tenantId" = ?)`,
		args, []any{int64(9), scopeTenant})

	if _, err := db.Delete(posts).Exec(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// An UPDATE's SET list is an operand position a statement can be
// written into, and there a cross-tenant READ decides what gets
// written.
func TestUpdateSetListIsResolved(t *testing.T) {
	posts, _, _ := scopedTable("scs_posts")
	plain, plainID := plainTable("scs_plain")
	db := sqlite.New(nil)

	upd := db.Update(plain).
		SetExpr(plain.Col("postId"), sqlite.Subquery(db.Select(posts.Col("id")).From(posts))).
		Where(sqlite.Eq(plainID, int64(1)))
	sql, args := renderCtx(t, upd, scopeCtx())
	checkSQL(t, sql,
		`UPDATE "scs_plain" SET "postId" = (SELECT "scs_posts"."id" FROM "scs_posts" `+
			`WHERE ("scs_posts"."tenantId" = ?)) WHERE ("scs_plain"."id" = ?)`,
		args, []any{scopeTenant, int64(1)})

	if _, _, err := upd.ToSQLCtx(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
}

// A joined table carries its own filters. For an INNER JOIN they belong
// in the WHERE clause, beside the FROM table's own.
func TestInnerJoinCarriesTheJoinedTablesAxisInWhere(t *testing.T) {
	posts, postID, _ := scopedTable("scj_posts")
	plain, plainID := plainTable("scj_plain")
	db := sqlite.New(nil)

	sel := db.Select(plainID).From(plain).
		Join(posts, sqlite.Eq(plain.Col("postId"), postID))
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`SELECT "scj_plain"."id" FROM "scj_plain" JOIN "scj_posts" `+
			`ON ("scj_plain"."postId" = "scj_posts"."id") `+
			`WHERE ("scj_posts"."tenantId" = ?)`,
		args, []any{scopeTenant})
}

// A LEFT JOIN's are not: the joined side is the nullable one, and a
// predicate on a nullable side is false for every unmatched row, so a
// tenant guard in the WHERE clause would drop exactly the parent rows
// the LEFT JOIN exists to keep and silently turn it into an INNER JOIN.
func TestLeftJoinCarriesTheJoinedTablesAxisInTheOnClause(t *testing.T) {
	posts, postID, _ := scopedTable("scl_posts")
	plain, plainID := plainTable("scl_plain")
	db := sqlite.New(nil)

	sel := db.Select(plainID).From(plain).
		LeftJoin(posts, sqlite.Eq(plain.Col("postId"), postID))
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`SELECT "scl_plain"."id" FROM "scl_plain" LEFT JOIN "scl_posts" `+
			`ON (("scl_plain"."postId" = "scl_posts"."id") AND ("scl_posts"."tenantId" = ?))`,
		args, []any{scopeTenant})
	if strings.Contains(sql, "WHERE") {
		t.Errorf("the guard landed in a WHERE clause, which degenerates the join:\n%s", sql)
	}
}

// A joined table's DEFAULT filters follow the same placement rule. They
// used not to be written at all: joining a soft-deleted table read its
// hidden rows however carefully the FROM table was scoped.
func TestJoinedTableCarriesItsDefaultFilters(t *testing.T) {
	child := sqlite.NewTable("scdf_child")
	childID := sqlite.Add(child, sqlite.BigInt("id").PrimaryKey())
	sqlite.SoftDelete(child)
	plain, plainID := plainTable("scdf_plain")
	db := sqlite.New(nil)

	inner, _ := db.Select(plainID).From(plain).
		Join(child, sqlite.Eq(plain.Col("postId"), childID)).ToSQL()
	if !strings.Contains(inner, `WHERE ("scdf_child"."deletedAt" IS NULL)`) {
		t.Errorf("INNER join dropped the joined table's default filter:\n%s", inner)
	}
	outer, _ := db.Select(plainID).From(plain).
		LeftJoin(child, sqlite.Eq(plain.Col("postId"), childID)).ToSQL()
	if !strings.Contains(outer, `AND ("scdf_child"."deletedAt" IS NULL))`) {
		t.Errorf("LEFT join did not put the joined table's default filter in the ON clause:\n%s", outer)
	}
}

// A CTE body is a statement of its own and is resolved as one.
// WITH … AS (<statement over a scoped table>) is the shape a reporting
// query is written in, and an unscoped body reaches every tenant's rows
// however carefully the outer SELECT is scoped.
func TestCTEBodyIsResolved(t *testing.T) {
	posts, postID, _ := scopedTable("scc_posts")
	plain, plainID := plainTable("scc_plain")
	db := sqlite.New(nil)

	cte := sqlite.CTEDef("recent", db.Select(postID).From(posts))
	sel := db.Select(plainID).From(plain).With(cte)
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`WITH "recent" AS (SELECT "scc_posts"."id" FROM "scc_posts" `+
			`WHERE ("scc_posts"."tenantId" = ?)) `+
			`SELECT "scc_plain"."id" FROM "scc_plain"`,
		args, []any{scopeTenant})

	if _, _, err := sel.ToSQLCtx(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
}

// A data-modifying CTE body is resolved on the same terms. This is the
// shape that made WITH moved AS (DELETE FROM scoped RETURNING …) a
// cross-tenant WRITE in the dialect that found it: a resolver naming
// only the SELECT builder walks straight past it.
func TestADataModifyingCTEBodyIsResolved(t *testing.T) {
	posts, postID, _ := scopedTable("scm_posts")
	plain, plainID := plainTable("scm_plain")
	db := sqlite.New(nil)

	cte := sqlite.CTEDef("moved", db.Delete(posts).Where(sqlite.Eq(postID, int64(3))))
	sel := db.Select(plainID).From(plain).With(cte)
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`WITH "moved" AS (DELETE FROM "scm_posts" WHERE ("scm_posts"."id" = ?) `+
			`AND ("scm_posts"."tenantId" = ?)) `+
			`SELECT "scm_plain"."id" FROM "scm_plain"`,
		args, []any{int64(3), scopeTenant})

	if _, _, err := sel.ToSQLCtx(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
}

// A subquery operand is resolved wherever it is written, and the depth
// it is written at is not a boundary: Not(Exists(sub)) is scoped
// exactly as NotExists(sub) is. That equivalence is the whole of what
// holding operands in a node buys.
func TestSubqueryOperandsAreResolvedAtEveryDepth(t *testing.T) {
	posts, postID, _ := scopedTable("scq_posts")
	plain, plainID := plainTable("scq_plain")
	db := sqlite.New(nil)

	sub := func() *sqlite.SelectBuilder { return db.Select(postID).From(posts) }
	body := `SELECT "scq_posts"."id" FROM "scq_posts" WHERE ("scq_posts"."tenantId" = ?)`

	tests := []struct {
		name string
		pred func() drops.Expression
		want string
	}{
		{"NotExists", func() drops.Expression { return sqlite.NotExists(sub()) },
			`NOT EXISTS (` + body + `)`},
		{"Not of Exists", func() drops.Expression { return sqlite.Not(sqlite.Exists(sub())) },
			`(NOT EXISTS (` + body + `))`},
		{"And of Or of Not of Exists", func() drops.Expression {
			return sqlite.And(sqlite.Or(sqlite.Not(sqlite.Exists(sub()))), sqlite.Eq(plainID, int64(1)))
		}, `(((NOT EXISTS (` + body + `))) AND ("scq_plain"."id" = ?))`},
		{"In of a subquery", func() drops.Expression { return sqlite.In(plainID, sub()) },
			`("scq_plain"."id" IN (` + body + `))`},
		{"Eq of a scalar subquery", func() drops.Expression {
			return sqlite.Eq(plainID, sqlite.Subquery(sub()))
		}, `("scq_plain"."id" = (` + body + `))`},
		{"a function argument", func() drops.Expression {
			return sqlite.Eq(plainID, sqlite.Coalesce(sqlite.Subquery(sub()), 0))
		}, `("scq_plain"."id" = coalesce((` + body + `), ?))`},
		{"a CASE branch", func() drops.Expression {
			return sqlite.Case().When(sqlite.Exists(sub()), 1).Else(0).End()
		}, `CASE WHEN EXISTS (` + body + `) THEN ? ELSE ? END`},
		{"a BETWEEN bound", func() drops.Expression {
			return sqlite.Between(plainID, sqlite.Subquery(sub()), 10)
		}, `("scq_plain"."id" BETWEEN (` + body + `) AND ?)`},
		{"a window partition key", func() drops.Expression {
			return sqlite.Over(sqlite.RowNumber(),
				sqlite.WindowSpec().PartitionBy(sqlite.Subquery(sub())))
		}, `row_number() OVER (PARTITION BY (` + body + `))`},
		{"a CAST operand", func() drops.Expression {
			return sqlite.CastAs(sqlite.Subquery(sub()), "INTEGER")
		}, `CAST((` + body + `) AS INTEGER)`},
		{"a FILTER predicate", func() drops.Expression {
			return sqlite.Filter(sqlite.CountAll(), sqlite.Exists(sub()))
		}, `count(*) FILTER (WHERE EXISTS (` + body + `))`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sel := db.Select(tc.pred()).From(plain)
			sql, _ := renderCtx(t, sel, scopeCtx())
			if !strings.Contains(sql, tc.want) {
				t.Errorf("got = %v,\nwant it to contain %v", sql, tc.want)
			}
			// And the same shape refuses on a ctx with no tenant,
			// rather than rendering the inner statement unscoped.
			if _, _, err := db.Select(tc.pred()).From(plain).ToSQLCtx(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
				t.Errorf("bare ctx: err = %v, want %v", err, sqlite.ErrTenantMissing)
			}
		})
	}
}

// A predicate holding a statement is a value a caller may build once
// and use in two requests. Resolution copies, so the second request
// resolves against its own ctx.
func TestAPredicateIsReusableAcrossRequests(t *testing.T) {
	posts, postID, _ := scopedTable("scp_posts")
	plain, plainID := plainTable("scp_plain")
	db := sqlite.New(nil)

	pred := sqlite.In(plainID, db.Select(postID).From(posts))
	sel := db.Select(plainID).From(plain).Where(pred)
	want := `SELECT "scp_plain"."id" FROM "scp_plain" WHERE ("scp_plain"."id" IN (` +
		`SELECT "scp_posts"."id" FROM "scp_posts" WHERE ("scp_posts"."tenantId" = ?)))`

	for _, tenant := range []int64{scopeTenant, 12, scopeTenant} {
		sql, args := renderCtx(t, sel, sqlite.WithTenant(context.Background(), tenant))
		checkSQL(t, sql, want, args, []any{tenant})
	}
}

// An aliased table's predicates must render under the alias, or the
// statement names a relation it has no FROM entry for and SQLite
// answers "no such column". A tenant-scoped table would otherwise be
// the one kind that could not be queried under an alias at all.
func TestAnAliasedTableRendersItsFiltersUnderTheAlias(t *testing.T) {
	posts, _, _ := scopedTable("sca_posts")
	p := posts.As("p")
	db := sqlite.New(nil)

	sql, args := renderCtx(t, db.Select().From(p), scopeCtx())
	checkSQL(t, sql, `SELECT * FROM "sca_posts" AS "p" WHERE ("p"."tenantId" = ?)`,
		args, []any{scopeTenant})
}

// An alias SHARES the table's scope rather than snapshotting it, so a
// filter registered after As was called still reaches the alias. Go
// initialises package-level vars before init, so the snapshot version
// lost the axis in exactly the spelling this package invites.
func TestAnAliasTakenBeforeTheFilterStillCarriesIt(t *testing.T) {
	posts := sqlite.NewTable("scb_posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(posts, sqlite.BigInt("tenantId").NotNull())

	// The alias is taken FIRST, as a package-level var would be.
	p := posts.As("p")
	posts.ContextFilter(sqlite.TenantFilter(tenant))

	db := sqlite.New(nil)
	sql, args := renderCtx(t, db.Delete(posts), scopeCtx())
	checkSQL(t, sql, `DELETE FROM "scb_posts" WHERE ("scb_posts"."tenantId" = ?)`,
		args, []any{scopeTenant})

	sql, args = renderCtx(t, db.Select().From(p), scopeCtx())
	checkSQL(t, sql, `SELECT * FROM "scb_posts" AS "p" WHERE ("p"."tenantId" = ?)`,
		args, []any{scopeTenant})
}

// A default filter registered after As is shared too — the same bug,
// reachable in this dialect before any of the context-filter work,
// where the loss is a soft-delete guard rather than a tenant axis.
func TestAnAliasTakenBeforeSoftDeleteStillHidesDeletedRows(t *testing.T) {
	posts := sqlite.NewTable("scv_posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	p := posts.As("p")
	sqlite.SoftDelete(posts)

	db := sqlite.New(nil)
	sql, _ := db.Select().From(p).ToSQL()
	if !strings.Contains(sql, `WHERE ("p"."deletedAt" IS NULL)`) {
		t.Errorf("alias lost the soft-delete guard:\n%s", sql)
	}
}

// A filter whose predicate selects from the table it filters does not
// terminate. It is refused, and the refusal names the cycle.
func TestASelfReferentialFilterIsRefused(t *testing.T) {
	notes := sqlite.NewTable("scy_notes")
	id := sqlite.Add(notes, sqlite.BigInt("id").PrimaryKey())
	db := sqlite.New(nil)
	notes.ContextFilter(func(context.Context) (drops.Expression, error) {
		return sqlite.In(id, db.Select(id).From(notes)), nil
	})

	_, _, err := db.Select().From(notes).ToSQLCtx(context.Background())
	if !errors.Is(err, sqlite.ErrContextFilterCycle) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrContextFilterCycle)
	}
	if !strings.Contains(err.Error(), "scy_notes -> scy_notes") {
		t.Errorf("err = %v, want it to name the cycle", err)
	}
}

// Unscoped is what a caller says to widen a statement, and it widens
// this statement only: a subquery is a statement of its own and keeps
// its own scoping.
func TestUnscopedDoesNotReachIntoASubquery(t *testing.T) {
	posts, postID, _ := scopedTable("scz_posts")
	plain, plainID := plainTable("scz_plain")
	db := sqlite.New(nil)

	sel := db.Select(plainID).From(plain).
		Where(sqlite.In(plainID, db.Select(postID).From(posts))).
		Unscoped()
	sql, args := renderCtx(t, sel, scopeCtx())
	if !strings.Contains(sql, `WHERE ("scz_posts"."tenantId" = ?)`) {
		t.Errorf("Unscoped reached into the subquery:\n%s", sql)
	}
	if !sameArgs(args, []any{scopeTenant}) {
		t.Errorf("args = %v, want %v", args, []any{scopeTenant})
	}
}

// ExecExpr takes a ctx and must let it reach the render. It is an
// executor, and one that rendered blind would be the way around every
// other one.
func TestExecExprLetsTheCtxReachTheStatement(t *testing.T) {
	posts, id, _ := scopedTable("sce_posts")
	drv := dropstest.New()
	db := sqlite.New(drv)

	if _, err := db.ExecExpr(scopeCtx(), db.Delete(posts).Where(sqlite.Eq(id, int64(2)))); err != nil {
		t.Fatalf("ExecExpr: %v", err)
	}
	want := `DELETE FROM "sce_posts" WHERE ("sce_posts"."id" = ?) AND ("sce_posts"."tenantId" = ?)`
	if got := drv.LastSQL(); got != want {
		t.Errorf("got = %v, want %v", got, want)
	}

	drv.Reset()
	if _, err := db.ExecExpr(context.Background(), db.Delete(posts)); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// An expression with a statement inside it, handed to ExecExpr, is
// walked to that statement too — and an opaque one renders exactly as
// it always did.
func TestExecExprWalksIntoAnExpressionAndLeavesOpaqueOnesAlone(t *testing.T) {
	posts, postID, _ := scopedTable("scx_posts")
	drv := dropstest.New()
	db := sqlite.New(drv)

	if _, err := db.ExecExpr(scopeCtx(), sqlite.Exists(db.Select(postID).From(posts))); err != nil {
		t.Fatalf("ExecExpr: %v", err)
	}
	if !strings.Contains(drv.LastSQL(), `("scx_posts"."tenantId" = ?)`) {
		t.Errorf("the walk did not reach the statement:\n%s", drv.LastSQL())
	}

	drv.Reset()
	if _, err := db.ExecExpr(context.Background(), drops.Raw("PRAGMA foreign_keys = ON")); err != nil {
		t.Fatalf("ExecExpr: %v", err)
	}
	if got := drv.LastSQL(); got != "PRAGMA foreign_keys = ON" {
		t.Errorf("got = %v, want the raw text unchanged", got)
	}
}

// SoftDeleteByID and Restore say Unscoped so they can reach an
// already-hidden row — and Unscoped is statement-wide, so it clears the
// table's context filters with everything else. Left there, an id is
// unique across the whole table rather than per tenant, so these two
// methods hid or restored whichever tenant's row happened to carry that
// id, on a ctx carrying no tenant at all. They put the axis back
// explicitly.
func TestSoftDeleteByIDStaysInsideTheTenant(t *testing.T) {
	posts := sqlite.NewTable("sf_posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(posts, sqlite.BigInt("tenantId").NotNull())
	sd := sqlite.SoftDelete(posts)
	posts.ContextFilter(sqlite.TenantFilter(tenant))
	ent := sqlite.NewEntity[sfPost](posts)

	drv := dropstest.New()
	db := sqlite.New(drv)

	if _, err := ent.SoftDeleteByID(db, scopeCtx(), int64(4), sd); err != nil {
		t.Fatalf("SoftDeleteByID: %v", err)
	}
	last := drv.Last()
	want := `UPDATE "sf_posts" SET "deletedAt" = CURRENT_TIMESTAMP ` +
		`WHERE ("sf_posts"."id" = ?) AND ("sf_posts"."tenantId" = ?)`
	if last.SQL != want {
		t.Errorf("sql = %v, want %v", last.SQL, want)
	}
	if !sameArgs(last.Args, []any{int64(4), scopeTenant}) {
		t.Errorf("args = %v, want %v", last.Args, []any{int64(4), scopeTenant})
	}
	// The soft-delete guard itself is gone, which is what Unscoped was
	// for: without that the method could never touch a hidden row and
	// Restore would be a no-op by construction.
	if strings.Contains(last.SQL, "deletedAt\" IS NULL") {
		t.Errorf("the default filter survived, so a hidden row is unreachable:\n%s", last.SQL)
	}

	drv.Reset()
	if _, err := ent.Restore(db, scopeCtx(), int64(4), sd); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	wantRestore := `UPDATE "sf_posts" SET "deletedAt" = NULL ` +
		`WHERE ("sf_posts"."id" = ?) AND ("sf_posts"."tenantId" = ?)`
	if got := drv.LastSQL(); got != wantRestore {
		t.Errorf("sql = %v, want %v", got, wantRestore)
	}

	// And with no tenant on ctx neither of them sends anything.
	drv.Reset()
	if _, err := ent.SoftDeleteByID(db, context.Background(), int64(4), sd); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

type sfPost struct {
	ID        int64      `drop:"id"`
	TenantID  int64      `drop:"tenantId"`
	DeletedAt *time.Time `drop:"deletedAt"`
}

// A self-join of a scoped table restricts BOTH sides: each is a
// separate relation in the statement, and each has to be restricted.
// The aliased side renders its predicate under the alias, the base side
// under the table name — which is what resolveFilterExprs is for, and
// what makes a scoped table joinable to itself at all.
//
// The join condition is written raw rather than with p.Col("id"), and
// stays that way now only because the assertion below is about the
// automatic predicates. A column handle taken off an alias renders
// under the alias since [Table.As] began rebinding its columns — see
// TestAnAliasColumnQualifiesWithTheAlias for that half.
func TestASelfJoinRestrictsBothSides(t *testing.T) {
	posts, id, _ := scopedTable("sj_posts")
	p := posts.As("p")
	db := sqlite.New(nil)

	sel := db.Select(id).From(posts).
		Join(p, drops.Raw(`"sj_posts"."id" = "p"."id"`))
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`SELECT "sj_posts"."id" FROM "sj_posts" JOIN "sj_posts" AS "p" `+
			`ON "sj_posts"."id" = "p"."id" `+
			`WHERE ("sj_posts"."tenantId" = ?) AND ("p"."tenantId" = ?)`,
		args, []any{scopeTenant, scopeTenant})
}

// An alias is a second handle on ONE table, and the two halves of that
// are the scope it shares and the columns it rebinds. This is the
// second half: a handle reached through the alias qualifies with the
// alias, so both sides of a self-join are addressable at once without
// the caller dropping to drops.Raw.
//
// This dialect used to copy the table shallowly, so p.Col("id")
// rendered under the base table's name — the alias's own columns named
// a relation the statement did not mention where it named one at all,
// and on a self-join both sides of the ON condition collapsed onto one.
func TestAnAliasColumnQualifiesWithTheAlias(t *testing.T) {
	posts, id, _ := scopedTable("acq_posts")
	p := posts.As("p")
	db := sqlite.New(nil)

	sel := db.Select(id).From(posts).Join(p, sqlite.Eq(id, p.Col("id")))
	sql, args := renderCtx(t, sel, scopeCtx())
	checkSQL(t, sql,
		`SELECT "acq_posts"."id" FROM "acq_posts" JOIN "acq_posts" AS "p" `+
			`ON ("acq_posts"."id" = "p"."id") `+
			`WHERE ("acq_posts"."tenantId" = ?) AND ("p"."tenantId" = ?)`,
		args, []any{scopeTenant, scopeTenant})
}

// Rebinding changes how a handle RENDERS and nothing else. Everywhere a
// column is identified rather than rendered — an entity's key columns,
// the tenant axis, a hook's Has — the aliased copy has to collapse back
// onto the column it was copied from, or the alias would look to those
// callers like a second column that happens to share a name.
func TestAnAliasColumnIsStillTheColumnItWasCopiedFrom(t *testing.T) {
	posts := sqlite.NewTable("acr_posts")
	id := sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	sqlite.Add(posts, sqlite.BigInt("tenantId").NotNull())
	p := posts.As("p")

	// The axis is declared over the ALIAS's handle; the INSERT is built
	// from the table. One axis between them, or the INSERT stamps a
	// second column that happens to share a name.
	posts.ScopeWritesByTenant(p.Col("tenantId"))

	db := sqlite.New(nil)
	ins := db.Insert(posts).Values(id.Val(int64(1)))
	sql, args := renderCtx(t, ins, scopeCtx())
	checkSQL(t, sql, `INSERT INTO "acr_posts" ("id", "tenantId") VALUES (?, ?)`,
		args, []any{int64(1), scopeTenant})
}

// Registering a filter while queries against the table are in flight is
// a supported thing to do — an entity built inside a request handler
// re-registers its own filter on every request — so the filter list is
// a shared, locked value rather than a slice each reader walks while a
// writer appends into it.
func TestFiltersMayBeRegisteredWhileQueriesAreInFlight(t *testing.T) {
	posts := sqlite.NewTable("sr_posts")
	sqlite.Add(posts, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(posts, sqlite.BigInt("tenantId").NotNull())
	db := sqlite.New(nil)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				// A ctx with no tenant is a legal answer here: until
				// the filter is registered the statement carries none.
				_, _, _ = db.Select().From(posts).ToSQLCtx(scopeCtx())
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				posts.ContextFilter(sqlite.TenantFilter(tenant))
			}
		}()
	}
	wg.Wait()
}

// A soft delete rewrites the DELETE into an UPDATE, and the rewrite has
// to carry everything the DELETE carried: the tenant predicate, which
// arrives in the wheres the hook copies, and the DEFAULT filters as
// this execution resolved them, which a freshly built UpdateBuilder
// would otherwise re-derive unwalked.
func TestASoftDeleteRewriteCarriesTheResolvedScope(t *testing.T) {
	blocked, blockedID, _ := scopedTable("hd_blocked")
	notes := sqlite.NewTable("hd_notes")
	noteID := sqlite.Add(notes, sqlite.BigInt("id").PrimaryKey())
	tenant := sqlite.Add(notes, sqlite.BigInt("tenantId").NotNull())
	notes.ContextFilter(sqlite.TenantFilter(tenant))
	db := sqlite.New(nil)
	// A default filter with a statement inside it: the shape that goes
	// out unresolved when the rewrite re-derives the list itself.
	notes.DefaultFilter(sqlite.NotIn(noteID, db.Select(blockedID).From(blocked)))
	sqlite.ApplyMixins(notes, &sqlite.SoftDeleteMixin{})

	del := db.Delete(notes).Where(sqlite.Eq(noteID, int64(2)))
	sql, args := renderCtx(t, del, scopeCtx())
	checkSQL(t, sql,
		`UPDATE "hd_notes" SET "deletedAt" = CURRENT_TIMESTAMP WHERE `+
			`("hd_notes"."id" NOT IN (SELECT "hd_blocked"."id" FROM "hd_blocked" `+
			`WHERE ("hd_blocked"."tenantId" = ?))) `+
			`AND ("hd_notes"."deletedAt" IS NULL) `+
			`AND ("hd_notes"."id" = ?) AND ("hd_notes"."tenantId" = ?)`,
		args, []any{scopeTenant, int64(2), scopeTenant})

	if _, _, err := del.ToSQLCtx(context.Background()); !errors.Is(err, sqlite.ErrTenantMissing) {
		t.Errorf("err = %v, want %v", err, sqlite.ErrTenantMissing)
	}
}
