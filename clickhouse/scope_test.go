package clickhouse_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/clickhouse"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/vector"
)

// The read side of the tenant axis, asserted on RENDERED SQL and BOUND
// ARGUMENTS throughout.
//
// Never on a round-trip. A round-trip against a fixture holding one
// tenant's rows passes whether or not the statement carries a
// predicate, which is precisely the review that let every defect in
// this round survive: the rows came back, the test was green, and the
// statement spanned every customer.

var (
	scHits    = clickhouse.NewTable("hits")
	scHitID   = clickhouse.Add(scHits, clickhouse.UInt64("id"))
	scHitTen  = clickhouse.Add(scHits, clickhouse.String("tenantId"))
	scHitPath = clickhouse.Add(scHits, clickhouse.String("path"))
	scHitTS   = clickhouse.Add(scHits, clickhouse.DateTime("ts", "UTC"))

	// scSessions is the second scoped table, for joins.
	scSessions   = clickhouse.NewTable("sessions")
	scSessionID  = clickhouse.Add(scSessions, clickhouse.UInt64("id"))
	scSessionTen = clickhouse.Add(scSessions, clickhouse.String("tenantId"))

	// scPublic carries no axis at all: the control for every assertion
	// that a scoped table renders something an unscoped one does not.
	scPublic   = clickhouse.NewTable("public")
	scPublicID = clickhouse.Add(scPublic, clickhouse.UInt64("id"))
)

func init() {
	scHits.Engine(clickhouse.MergeTree()).OrderBy(scHitTen, scHitTS).
		ContextFilter(clickhouse.TenantFilter(scHitTen)).
		ScopeWritesByTenant(scHitTen)
	scSessions.Engine(clickhouse.MergeTree()).OrderBy(scSessionTen).
		ContextFilter(clickhouse.TenantFilter(scSessionTen))
	scPublic.Engine(clickhouse.MergeTree()).OrderBy(scPublicID)
}

func tenantCtx(tenant string) context.Context {
	return clickhouse.WithTenant(context.Background(), tenant)
}

// mustSQL renders b for ctx and fails the test if it refuses.
func mustSQL(t *testing.T, b *clickhouse.SelectBuilder, ctx context.Context) (string, []any) {
	t.Helper()
	sql, args, err := b.ToSQLCtx(ctx)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	return sql, args
}

func checkSQL(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("\n  got:  %s\n  want: %s", got, want)
	}
}

func checkArgs(t *testing.T, got []any, want ...any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

// ----------------------------------------------------------------------
// The axis reaches the statements nobody built with an entity
// ----------------------------------------------------------------------

// The spelling the readme shows first. Before the axis moved onto the
// table there was nothing here at all — no predicate and no refusal.
func TestBareSelectCarriesTheAxis(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t, db.Select(scHitID).From(scHits), tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" WHERE ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "acme")
}

// The caller's own predicates come first and the guard follows, so a
// query log reads intent then scoping.
func TestSelectOrdersCallerPredicatesBeforeTheGuard(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).Where(clickhouse.Eq(scHitPath, "/")),
		tenantCtx("acme"))
	checkSQL(t, sql,
		`SELECT "hits"."id" FROM "hits" WHERE ("hits"."path" = ?) AND ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "/", "acme")
}

// Fail closed. A ctx with no tenant is an error and no statement, not a
// query that quietly spans every customer.
func TestSelectRefusesWithoutTenant(t *testing.T) {
	drv := dropstest.New()
	db := clickhouse.New(drv)
	if _, err := db.Select(scHitID).From(scHits).Rows(context.Background()); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Fatalf("Rows: got %v, want ErrTenantMissing", err)
	}
	// The point of failing closed is that nothing reaches the wire.
	if n := len(drv.Statements()); n != 0 {
		t.Errorf("statements sent despite refusal: %v", drv.SQL())
	}
	// The refusal names the table and column that stopped the query,
	// which is the only diagnostic a caller gets.
	_, _, err := db.Select(scHitID).From(scHits).ToSQLCtx(context.Background())
	if !strings.Contains(err.Error(), "hits.tenantId") {
		t.Errorf("refusal does not name the axis: %v", err)
	}
}

// Unscoped is the documented way out, and it is visible at the call
// site, which is the whole reason it is a method rather than a ctx flag.
func TestUnscopedSpansTenants(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t, db.Select(scHitID).From(scHits).Unscoped(), context.Background())
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits"`)
	checkArgs(t, args)
}

// ----------------------------------------------------------------------
// ToSQL can no longer show the whole statement; ToSQLCtx can
// ----------------------------------------------------------------------

func TestToSQLOmitsContextFiltersAndToSQLCtxDoesNot(t *testing.T) {
	db := clickhouse.New(nil)
	q := db.Select(scHitID).From(scHits)
	blind, blindArgs := q.ToSQL()
	checkSQL(t, blind, `SELECT "hits"."id" FROM "hits"`)
	if len(blindArgs) != 0 {
		t.Errorf("ToSQL bound %v", blindArgs)
	}
	sql, args := mustSQL(t, q, tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" WHERE ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "acme")
}

// A builder is a value. Rendering it twice must not accrete predicates,
// and a builder held across requests must not answer the second request
// with the first one's tenant.
func TestBuilderIsReusableAcrossRequests(t *testing.T) {
	db := clickhouse.New(nil)
	q := db.Select(scHitID).From(scHits)

	first, firstArgs := mustSQL(t, q, tenantCtx("acme"))
	second, secondArgs := mustSQL(t, q, tenantCtx("globex"))

	want := `SELECT "hits"."id" FROM "hits" WHERE ("hits"."tenantId" = ?)`
	checkSQL(t, first, want)
	checkSQL(t, second, want)
	checkArgs(t, firstArgs, "acme")
	checkArgs(t, secondArgs, "globex")

	// And the builder itself is untouched: its blind rendering is what
	// it always was.
	blind, _ := q.ToSQL()
	checkSQL(t, blind, `SELECT "hits"."id" FROM "hits"`)
}

// Executing the same builder twice binds the tenant once each time.
// Accreting would come back with the right rows and a growing argument
// list, which is why this asserts on the recorded statements.
func TestExecutingTwiceDoesNotAccretePredicates(t *testing.T) {
	drv := dropstest.New().AlwaysRows([]string{"id"})
	db := clickhouse.New(drv)
	q := db.Select(scHitID).From(scHits)
	for _, tenant := range []string{"acme", "globex"} {
		rows, err := q.Rows(tenantCtx(tenant))
		if err != nil {
			t.Fatalf("Rows: %v", err)
		}
		rows.Close()
	}
	for i, st := range drv.Statements() {
		if n := strings.Count(st.SQL, "tenantId"); n != 1 {
			t.Errorf("statement %d carries %d tenant predicates: %s", i, n, st.SQL)
		}
		if len(st.Args) != 1 {
			t.Errorf("statement %d bound %v", i, st.Args)
		}
	}
	if got := drv.Statements()[1].Args[0]; got != "globex" {
		t.Errorf("second execution bound %v, want globex", got)
	}
}

// ----------------------------------------------------------------------
// Joins: the clause depends on the kind
// ----------------------------------------------------------------------

func TestInnerJoinPutsBothGuardsInWhere(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).
			Join(scSessions, clickhouse.Eq(scHitID, scSessionID)),
		tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" INNER JOIN "sessions" ON ("hits"."id" = "sessions"."id")`+
		` WHERE ("hits"."tenantId" = ?) AND ("sessions"."tenantId" = ?)`)
	checkArgs(t, args, "acme", "acme")
}

// ALL INNER JOIN is the same join said explicitly.
func TestAllJoinPutsTheGuardInWhere(t *testing.T) {
	db := clickhouse.New(nil)
	sql, _ := mustSQL(t,
		db.Select(scHitID).From(scHits).
			AllJoin(scSessions, clickhouse.Eq(scHitID, scSessionID)),
		tenantCtx("acme"))
	if !strings.Contains(sql, `ON ("hits"."id" = "sessions"."id") WHERE`) {
		t.Errorf("ALL INNER JOIN grew an ON-clause guard: %s", sql)
	}
	if !strings.Contains(sql, `AND ("sessions"."tenantId" = ?)`) {
		t.Errorf("joined table lost its guard: %s", sql)
	}
}

// A predicate on the nullable side of a LEFT JOIN is false for every
// unmatched row, so in the WHERE clause the guard would delete exactly
// the parent rows the LEFT JOIN exists to keep.
func TestLeftJoinPutsTheJoinedGuardInTheOnClause(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).
			LeftJoin(scSessions, clickhouse.Eq(scHitID, scSessionID)),
		tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" LEFT JOIN "sessions"`+
		` ON ("hits"."id" = "sessions"."id") AND ("sessions"."tenantId" = ?)`+
		` WHERE ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "acme", "acme")
}

// ANY keeps one arbitrary match per left row and picks it while the
// join runs. A guard applied afterwards deletes the pairs whose pick
// happened to belong to somebody else — rows the caller is entitled to,
// dropped non-deterministically. This is the ClickHouse-only half of
// the placement question.
func TestAnyJoinPutsTheJoinedGuardInTheOnClause(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).
			AnyJoin(scSessions, clickhouse.Eq(scHitID, scSessionID)),
		tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" ANY INNER JOIN "sessions"`+
		` ON ("hits"."id" = "sessions"."id") AND ("sessions"."tenantId" = ?)`+
		` WHERE ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "acme", "acme")
}

// RIGHT JOIN inverts both placements: the joined table is preserved, so
// its guard goes back to the WHERE clause, and the FROM table has
// become the nullable side, so its guard moves into the ON clause.
func TestRightJoinInvertsBothPlacements(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).
			RightJoin(scSessions, clickhouse.Eq(scHitID, scSessionID)),
		tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" RIGHT JOIN "sessions"`+
		` ON ("hits"."id" = "sessions"."id") AND ("hits"."tenantId" = ?)`+
		` WHERE ("sessions"."tenantId" = ?)`)
	checkArgs(t, args, "acme", "acme")
}

// The FROM table's guard goes into the FIRST right join's ON clause:
// that is where the FROM table enters the join tree, so every later
// join sees the restricted relation.
func TestFromGuardGoesIntoTheFirstRightJoin(t *testing.T) {
	db := clickhouse.New(nil)
	sql, _ := mustSQL(t,
		db.Select(scHitID).From(scHits).
			RightJoin(scPublic, clickhouse.Eq(scHitID, scPublicID)).
			RightJoin(scSessions, clickhouse.Eq(scHitID, scSessionID)),
		tenantCtx("acme"))
	first := strings.Index(sql, `RIGHT JOIN "public"`)
	second := strings.Index(sql, `RIGHT JOIN "sessions"`)
	guard := strings.Index(sql, `AND ("hits"."tenantId" = ?)`)
	if !(first < guard && guard < second) {
		t.Errorf("FROM guard is not in the first RIGHT JOIN's ON clause: %s", sql)
	}
}

// A FULL JOIN preserves both sides, so the ON clause leaks one side's
// unmatched rows and the WHERE clause drops the other's. There is no
// third place, so the statement is refused rather than answered wrongly
// — from either direction.
func TestFullJoinOfAScopedTableIsRefused(t *testing.T) {
	db := clickhouse.New(nil)
	_, _, err := db.Select(scPublicID).From(scPublic).
		FullJoin(scSessions, clickhouse.Eq(scPublicID, scSessionID)).
		ToSQLCtx(tenantCtx("acme"))
	if !errors.Is(err, clickhouse.ErrFullJoinScoped) {
		t.Fatalf("joined side: got %v, want ErrFullJoinScoped", err)
	}
	_, _, err = db.Select(scHitID).From(scHits).
		FullJoin(scPublic, clickhouse.Eq(scHitID, scPublicID)).
		ToSQLCtx(tenantCtx("acme"))
	if !errors.Is(err, clickhouse.ErrFullJoinScoped) {
		t.Fatalf("FROM side: got %v, want ErrFullJoinScoped", err)
	}
	// Unscoped is the way through, and it is visible in the source.
	if _, _, err := db.Select(scHitID).From(scHits).
		FullJoin(scSessions, clickhouse.Eq(scHitID, scSessionID)).
		Unscoped().ToSQLCtx(context.Background()); err != nil {
		t.Fatalf("Unscoped FULL JOIN: %v", err)
	}
}

// ASOF keeps the CLOSEST match per left row, so the WHERE clause has
// ANY's problem; and its ON clause is a grammar — equalities then one
// trailing inequality — so the ON-clause answer cannot be appended.
// Both clauses are wrong, so the statement is refused.
func TestAsofJoinOfAScopedTableIsRefused(t *testing.T) {
	db := clickhouse.New(nil)
	_, _, err := db.Select(scPublicID).From(scPublic).
		AsofJoin(scSessions, clickhouse.And(
			clickhouse.Eq(scPublicID, scSessionID),
			clickhouse.Lte(scPublicID, scSessionID))).
		ToSQLCtx(tenantCtx("acme"))
	if !errors.Is(err, clickhouse.ErrAsofJoinScoped) {
		t.Fatalf("got %v, want ErrAsofJoinScoped", err)
	}
	// An ASOF join of a table with no axis is untouched, and so is one
	// the caller has unscoped.
	if _, _, err := db.Select(scPublicID).From(scPublic).
		AsofJoin(scPublic, drops.Raw("1")).ToSQLCtx(context.Background()); err != nil {
		t.Fatalf("unscoped ASOF join: %v", err)
	}
}

// A joined table's DefaultFilters travel by the same rule as its
// context filters — a soft-deleting table LEFT JOINed into a query must
// not answer with its deleted rows, and must not turn the join inner.
func TestJoinedTableCarriesItsDefaultFilters(t *testing.T) {
	db := clickhouse.New(nil)
	soft := clickhouse.NewTable("soft")
	softID := clickhouse.Add(soft, clickhouse.UInt64("id"))
	softDel := clickhouse.Add(soft, clickhouse.DateTime("deletedAt", "").Nullable())
	soft.DefaultFilter(clickhouse.IsNull(softDel))

	sql, _ := mustSQL(t,
		db.Select(scPublicID).From(scPublic).LeftJoin(soft, clickhouse.Eq(scPublicID, softID)),
		context.Background())
	checkSQL(t, sql, `SELECT "public"."id" FROM "public" LEFT JOIN "soft"`+
		` ON ("public"."id" = "soft"."id") AND ("soft"."deletedAt" IS NULL)`)
}

// ----------------------------------------------------------------------
// PREWHERE
// ----------------------------------------------------------------------

// Nothing drops adds automatically is written into PREWHERE, because
// PREWHERE is evaluated before FINAL: a guard there is tested against
// every stored version of a row rather than the one FINAL keeps.
func TestGuardsAreNeverWrittenIntoPrewhere(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).Final().
			Prewhere(clickhouse.Gte(scHitTS, "2020-01-01")),
		tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" FINAL`+
		` PREWHERE ("hits"."ts" >= ?) WHERE ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "2020-01-01", "acme")
	if strings.Contains(sql[:strings.Index(sql, "WHERE")], "tenantId") {
		t.Errorf("the guard reached PREWHERE: %s", sql)
	}
}

// A PREWHERE conjunct stands in FRONT of the clause the guards are
// written into, so a lone one that opens a comment would swallow them.
// It is bracketed on its own shape rather than on how many there are —
// which turns a silent swallow into a statement the server refuses.
func TestLonePrewhereConjunctIsBracketedWhenItCanEscape(t *testing.T) {
	db := clickhouse.New(nil)
	sql, _ := mustSQL(t,
		db.Select(scHitID).From(scHits).Prewhere(drops.Raw("path = '/' --")),
		tenantCtx("acme"))
	if !strings.Contains(sql, `PREWHERE (path = '/' --)`) {
		t.Errorf("escaping PREWHERE conjunct was left bare: %s", sql)
	}
	// An ordinary lone PREWHERE predicate is untouched.
	sql, _ = mustSQL(t,
		db.Select(scHitID).From(scHits).Prewhere(clickhouse.Eq(scHitPath, "/")),
		tenantCtx("acme"))
	if !strings.Contains(sql, `PREWHERE ("hits"."path" = ?) WHERE`) {
		t.Errorf("ordinary PREWHERE predicate was bracketed: %s", sql)
	}
}

// A subquery written into PREWHERE is a statement like any other.
func TestPrewhereSubqueryIsResolved(t *testing.T) {
	db := clickhouse.New(nil)
	sub := db.Select(scSessionID).From(scSessions)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).Prewhere(clickhouse.InSub(scHitID, sub)),
		tenantCtx("acme"))
	if !strings.Contains(sql, `FROM "sessions" WHERE ("sessions"."tenantId" = ?)`) {
		t.Errorf("PREWHERE subquery went out unscoped: %s", sql)
	}
	checkArgs(t, args, "acme", "acme")
}

// ----------------------------------------------------------------------
// Aliases
// ----------------------------------------------------------------------

// A predicate built from the declared handles names the un-aliased
// relation, which ClickHouse answers with UNKNOWN_IDENTIFIER. It is
// restated under the alias instead.
func TestAliasedScopedTableRendersItsGuardUnderTheAlias(t *testing.T) {
	db := clickhouse.New(nil)
	h := scHits.As("h")
	sql, args := mustSQL(t, db.Select(h.Col("id")).From(h), tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "h"."id" FROM "hits" AS "h" WHERE ("h"."tenantId" = ?)`)
	checkArgs(t, args, "acme")
	if strings.Contains(sql, `"hits"."tenantId"`) {
		t.Errorf("statement names a relation it has no FROM entry for: %s", sql)
	}
}

// A self-join is two relations and each has to be restricted, once per
// side, each under its own qualifier.
func TestSelfJoinOfAScopedTableGuardsBothSides(t *testing.T) {
	db := clickhouse.New(nil)
	h := scHits.As("h")
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).Join(h, clickhouse.Eq(scHitID, h.Col("id"))),
		tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" INNER JOIN "hits" AS "h"`+
		` ON ("hits"."id" = "h"."id") WHERE ("hits"."tenantId" = ?) AND ("h"."tenantId" = ?)`)
	checkArgs(t, args, "acme", "acme")
}

// An alias SHARES its table's scope rather than snapshotting it, so an
// alias taken at package scope — before init runs — still carries the
// axis a later registration adds. Go initialises package variables
// before init, so this is the ordinary spelling, not a corner.
func TestAliasTakenBeforeRegistrationStillCarriesTheAxis(t *testing.T) {
	db := clickhouse.New(nil)
	late := clickhouse.NewTable("late")
	lateID := clickhouse.Add(late, clickhouse.UInt64("id"))
	lateTen := clickhouse.Add(late, clickhouse.String("tenantId"))

	alias := late.As("l") // taken first, exactly as a package var would be
	late.ContextFilter(clickhouse.TenantFilter(lateTen))

	sql, args := mustSQL(t, db.Select(alias.Col("id")).From(alias), tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "l"."id" FROM "late" AS "l" WHERE ("l"."tenantId" = ?)`)
	checkArgs(t, args, "acme")
	_ = lateID
}

// The same sharing for a DefaultFilter and for an InsertHook.
func TestAliasSharesDefaultFiltersRegisteredLater(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("late2")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	del := clickhouse.Add(tbl, clickhouse.DateTime("deletedAt", "").Nullable())
	alias := tbl.As("a")
	tbl.DefaultFilter(clickhouse.IsNull(del))

	sql, _ := mustSQL(t, db.Select(alias.Col("id")).From(alias), context.Background())
	checkSQL(t, sql, `SELECT "a"."id" FROM "late2" AS "a" WHERE ("a"."deletedAt" IS NULL)`)
	_ = id
}

// A database-qualified table is its own relation: the rename is keyed
// on the qualified name, so two tables of one name in two databases do
// not restate each other's predicates.
func TestDatabaseQualifiedAliasRendersUnderTheAlias(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewDatabaseTable("analytics", "hits")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.ContextFilter(clickhouse.TenantFilter(ten))

	sql, _ := mustSQL(t, db.Select(id).From(tbl), tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "analytics"."hits"."id" FROM "analytics"."hits"`+
		` WHERE ("analytics"."hits"."tenantId" = ?)`)

	a := tbl.As("q")
	sql, _ = mustSQL(t, db.Select(a.Col("id")).From(a), tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "q"."id" FROM "analytics"."hits" AS "q" WHERE ("q"."tenantId" = ?)`)
}

// ----------------------------------------------------------------------
// Statements written inside expressions
// ----------------------------------------------------------------------

// Every operand position a caller can hand a statement to. Before the
// operands came out of their closures each of these read every tenant's
// rows through a statement that carried its own tenant predicate and
// looked scoped.
func TestStatementsInsideOperandsAreScoped(t *testing.T) {
	db := clickhouse.New(nil)
	sub := func() *clickhouse.SelectBuilder { return db.Select(scSessionID).From(scSessions) }

	cases := []struct {
		name  string
		build func() *clickhouse.SelectBuilder
	}{
		{"In", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.In(scPublicID, sub()))
		}},
		{"InSub", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.InSub(scPublicID, sub()))
		}},
		{"NotInSub", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.NotInSub(scPublicID, sub()))
		}},
		{"Exists", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.Exists(sub()))
		}},
		{"NotExists", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.NotExists(sub()))
		}},
		// The pairing pg spent four rounds on: NotExists(sub) was
		// scoped and the identical Not(Exists(sub)) was not.
		{"Not-of-Exists", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.Not(clickhouse.Exists(sub())))
		}},
		{"And-Or-Not-Exists", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.And(
				clickhouse.Or(clickhouse.Not(clickhouse.Exists(sub())), clickhouse.Eq(scPublicID, 1)),
				clickhouse.Eq(scPublicID, 2)))
		}},
		{"Eq-right", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.Eq(scPublicID, clickhouse.Subquery(sub())))
		}},
		{"Between-bound", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).
				Where(clickhouse.Between(scPublicID, 0, clickhouse.Subquery(sub())))
		}},
		{"function-arg", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).
				Where(clickhouse.Eq(scPublicID, clickhouse.Coalesce(clickhouse.Subquery(sub()), 0)))
		}},
		{"cast-operand", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).
				Where(clickhouse.Eq(scPublicID, clickhouse.CastAs(clickhouse.Subquery(sub()), "UInt64")))
		}},
		{"case-branch", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.Eq(scPublicID,
				clickhouse.Case().When(clickhouse.Eq(scPublicID, 1), clickhouse.Subquery(sub())).Else(0).End()))
		}},
		{"case-condition", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).Where(clickhouse.Eq(scPublicID,
				clickhouse.Case().When(clickhouse.Exists(sub()), 1).Else(0).End()))
		}},
		{"projection", func() *clickhouse.SelectBuilder {
			return db.Select(clickhouse.As(clickhouse.Subquery(sub()), "n")).From(scPublic)
		}},
		{"order-by", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).OrderBy(clickhouse.Subquery(sub()))
		}},
		{"group-by", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).GroupBy(clickhouse.Subquery(sub()))
		}},
		{"having", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).GroupBy(scPublicID).
				Having(clickhouse.Gt(clickhouse.CountAll(), clickhouse.Subquery(sub())))
		}},
		{"sample-by", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).SampleBy(clickhouse.Subquery(sub()))
		}},
		{"join-on", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).
				Join(scPublic, clickhouse.InSub(scPublicID, sub()))
		}},
		{"window-partition", func() *clickhouse.SelectBuilder {
			return db.Select(clickhouse.Over(clickhouse.CountAll(),
				clickhouse.WindowSpec().PartitionBy(clickhouse.Subquery(sub())))).From(scPublic)
		}},
		{"cte-body", func() *clickhouse.SelectBuilder {
			return db.Select(scPublicID).From(scPublic).
				With(clickhouse.CTEDef("recent", sub()))
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sql, args := mustSQL(t, tc.build(), tenantCtx("acme"))
			if !strings.Contains(sql, `FROM "sessions" WHERE ("sessions"."tenantId" = ?)`) {
				t.Errorf("nested statement went out unscoped: %s", sql)
			}
			found := false
			for _, a := range args {
				if a == "acme" {
					found = true
				}
			}
			if !found {
				t.Errorf("tenant not bound: %v", args)
			}
			// And the refusal propagates: a nested statement that
			// cannot be scoped refuses the whole thing.
			if _, _, err := tc.build().ToSQLCtx(context.Background()); !errors.Is(err, clickhouse.ErrTenantMissing) {
				t.Errorf("no tenant on ctx: got %v, want ErrTenantMissing", err)
			}
		})
	}
}

// Unscoped describes THIS statement's authority, not the authority of
// the statements written inside it — which is also how to unscope one
// relation of a query and no other.
func TestUnscopedDoesNotReachIntoASubquery(t *testing.T) {
	db := clickhouse.New(nil)
	sub := db.Select(scSessionID).From(scSessions)
	q := db.Select(scHitID).From(scHits).Unscoped().Where(clickhouse.InSub(scHitID, sub))
	sql, args := mustSQL(t, q, tenantCtx("acme"))
	if strings.Contains(sql, `"hits"."tenantId"`) {
		t.Errorf("Unscoped did not drop the FROM table's guard: %s", sql)
	}
	if !strings.Contains(sql, `"sessions"."tenantId" = ?`) {
		t.Errorf("Unscoped reached into the subquery: %s", sql)
	}
	checkArgs(t, args, "acme")
}

// A CTE body drops did not build has nothing to resolve and renders
// what it always did — the honest statement of the boundary.
func TestRawCTEBodyIsLeftAlone(t *testing.T) {
	db := clickhouse.New(nil)
	sql, _ := mustSQL(t,
		db.Select(scPublicID).From(scPublic).
			With(clickhouse.CTEDef("raw", drops.Raw("SELECT 1 AS one"))),
		context.Background())
	checkSQL(t, sql, `WITH "raw" AS (SELECT 1 AS one) SELECT "public"."id" FROM "public"`)
}

// A default filter is as capable of holding a statement as a context
// filter is, and the renderer is as blind to it.
func TestDefaultFilterHoldingAStatementIsWalked(t *testing.T) {
	db := clickhouse.New(nil)
	guarded := clickhouse.NewTable("guarded")
	gid := clickhouse.Add(guarded, clickhouse.UInt64("id"))
	guarded.DefaultFilter(clickhouse.NotInSub(gid, db.Select(scSessionID).From(scSessions)))

	sql, args := mustSQL(t, db.Select(gid).From(guarded), tenantCtx("acme"))
	if !strings.Contains(sql, `FROM "sessions" WHERE ("sessions"."tenantId" = ?)`) {
		t.Errorf("default filter's statement went out unscoped: %s", sql)
	}
	checkArgs(t, args, "acme")
	if _, _, err := db.Select(gid).From(guarded).ToSQLCtx(context.Background()); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Errorf("got %v, want ErrTenantMissing", err)
	}
}

// A filter that reads its own table does not terminate. It is refused,
// with the cycle spelled out and the way out named.
func TestFilterCycleIsRefused(t *testing.T) {
	db := clickhouse.New(nil)
	notes := clickhouse.NewTable("notes")
	noteID := clickhouse.Add(notes, clickhouse.UInt64("id"))
	notes.ContextFilter(func(ctx context.Context) (drops.Expression, error) {
		return clickhouse.InSub(noteID, db.Select(noteID).From(notes)), nil
	})
	_, _, err := db.Select(noteID).From(notes).ToSQLCtx(context.Background())
	if !errors.Is(err, clickhouse.ErrContextFilterCycle) {
		t.Fatalf("got %v, want ErrContextFilterCycle", err)
	}
	if !strings.Contains(err.Error(), "notes -> notes") {
		t.Errorf("cycle is not spelled out: %v", err)
	}
}

// ----------------------------------------------------------------------
// Executors
// ----------------------------------------------------------------------

// Count is a read of the table like any other. Rendering its body blind
// would answer "how many rows are there" for every tenant while the
// query that lists them answers for one.
func TestCountIsScoped(t *testing.T) {
	drv := dropstest.New().AlwaysRows([]string{"c"}, []any{int64(3)})
	db := clickhouse.New(drv)
	if _, err := db.Select(scHitID).From(scHits).Count(tenantCtx("acme")); err != nil {
		t.Fatalf("Count: %v", err)
	}
	if !strings.Contains(drv.LastSQL(), `"hits"."tenantId" = ?`) {
		t.Errorf("Count went out unscoped: %s", drv.LastSQL())
	}
	drv.Reset()
	if _, err := db.Select(scHitID).From(scHits).Count(context.Background()); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Errorf("Count with no tenant: got %v, want ErrTenantMissing", err)
	}
	if len(drv.Statements()) != 0 {
		t.Errorf("Count sent a statement despite refusing: %v", drv.SQL())
	}
}

// ExecExpr took a ctx and handed it only to the driver, so a builder
// passed to it went out with none of its tables' filters.
func TestExecExprResolvesForItsCtx(t *testing.T) {
	drv := dropstest.New()
	db := clickhouse.New(drv)
	if _, err := db.ExecExpr(tenantCtx("acme"), db.Select(scHitID).From(scHits)); err != nil {
		t.Fatalf("ExecExpr: %v", err)
	}
	if !strings.Contains(drv.LastSQL(), `"hits"."tenantId" = ?`) {
		t.Errorf("ExecExpr went out unscoped: %s", drv.LastSQL())
	}
	drv.Reset()
	if _, err := db.ExecExpr(context.Background(), db.Select(scHitID).From(scHits)); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Errorf("got %v, want ErrTenantMissing", err)
	}
	if len(drv.Statements()) != 0 {
		t.Errorf("ExecExpr sent a statement despite refusing: %v", drv.SQL())
	}
	// A DDL helper has nothing to resolve and renders what it always
	// did, on any ctx.
	drv.Reset()
	ddl := clickhouse.NewTable("ddl")
	col := clickhouse.Add(ddl, clickhouse.UInt64("id"))
	ddl.Engine(clickhouse.MergeTree()).OrderBy(col)
	if _, err := db.ExecExpr(context.Background(), clickhouse.CreateTable(ddl)); err != nil {
		t.Fatalf("ExecExpr(CreateTable): %v", err)
	}
	if !strings.HasPrefix(drv.LastSQL(), `CREATE TABLE "ddl"`) {
		t.Errorf("DDL was rewritten: %s", drv.LastSQL())
	}
}

// The entity query goes through the same executors, so it gets the same
// axis without an entity-side injection.
func TestEntityQueryIsScoped(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args, err := scHitEntity.Query(db).Where(clickhouse.Eq(scHitPath, "/")).ToSQLCtx(tenantCtx("acme"))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	checkSQL(t, sql, `SELECT * FROM "hits" WHERE ("hits"."path" = ?) AND ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "/", "acme")
	if _, _, err := scHitEntity.Query(db).ToSQLCtx(context.Background()); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Errorf("got %v, want ErrTenantMissing", err)
	}
}

// The axis is a property of the table, so a store built over a scoped
// table gets it without knowing the feature exists. This is the third
// entry point in the package that composes a SELECT — after
// db.Select and Entity.Query — and it is the one that would have been
// missed by a predicate injected at either of the others.
func TestVectorStoreSearchIsScoped(t *testing.T) {
	drv := &vecDriver{}
	db := clickhouse.New(drv)

	tbl := clickhouse.NewTable("vecscope")
	id := clickhouse.Add(tbl, clickhouse.Int64("id"))
	emb := clickhouse.Add(tbl, clickhouse.Custom[[]float32]("embedding", "Array(Float32)"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ten).
		ContextFilter(clickhouse.TenantFilter(ten))

	store := clickhouse.NewVectorStore(db, tbl, id, emb)
	q := vector.Query{Vector: []float32{1, 0}, TopK: 3, Metric: vector.Cosine}
	if _, err := store.Search(tenantCtx("acme"), q); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(drv.queries) != 1 || !strings.Contains(drv.queries[0], `"vecscope"."tenantId" = ?`) {
		t.Errorf("vector search went out unscoped: %v", drv.queries)
	}

	if _, err := store.Search(context.Background(), q); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Errorf("got %v, want ErrTenantMissing", err)
	}
	if len(drv.queries) != 1 {
		t.Errorf("statement sent despite refusal: %v", drv.queries)
	}
}

// An ASOF JOIN is refused for the JOINED table's filters, not for the
// FROM table's: an ASOF join drops unmatched left rows like an inner
// one, so restricting the left side in the WHERE clause selects exactly
// the pairs it should.
func TestAsofJoinIsAllowedWhenOnlyTheFromTableIsScoped(t *testing.T) {
	db := clickhouse.New(nil)
	sql, args := mustSQL(t,
		db.Select(scHitID).From(scHits).
			AsofJoin(scPublic, clickhouse.Lte(scHitID, scPublicID)),
		tenantCtx("acme"))
	checkSQL(t, sql, `SELECT "hits"."id" FROM "hits" ASOF JOIN "public"`+
		` ON ("hits"."id" <= "public"."id") WHERE ("hits"."tenantId" = ?)`)
	checkArgs(t, args, "acme")
}

// Registering a filter is a supported thing to do while queries are in
// flight — an entity built inside a request handler does it on every
// request — so the lists it touches are shared with every alias and
// guarded. Run under -race, this is the test that says so.
func TestFiltersMayBeRegisteredWhileQueriesRun(t *testing.T) {
	db := clickhouse.New(nil)
	tbl := clickhouse.NewTable("concurrent")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	ten := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	tbl.Engine(clickhouse.MergeTree()).OrderBy(ten)
	alias := tbl.As("c")

	type row struct {
		ID       uint64 `drop:"id"`
		TenantID string `drop:"tenantId"`
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			clickhouse.NewEntity[row](tbl).ScopeByTenant(ten)
		}
	}()
	for i := 0; i < 200; i++ {
		// Both the base handle and an alias of it, since they share one
		// scope and one lock.
		if _, _, err := db.Select(id).From(tbl).ToSQLCtx(tenantCtx("acme")); err != nil {
			t.Fatalf("base: %v", err)
		}
		if _, _, err := db.Select(alias.Col("id")).From(alias).ToSQLCtx(tenantCtx("acme")); err != nil {
			t.Fatalf("alias: %v", err)
		}
	}
	<-done

	// However many times the entity was rebuilt, the table carries one
	// filter: registration under a key replaces rather than appends.
	sql, args := mustSQL(t, db.Select(id).From(tbl), tenantCtx("acme"))
	if n := strings.Count(sql, "tenantId"); n != 1 {
		t.Errorf("%d tenant predicates after 200 registrations: %s", n, sql)
	}
	checkArgs(t, args, "acme")
}

// EntityQuery.Unscoped is narrower than [SelectBuilder.Unscoped], on
// purpose and in all four dialects: it drops the DEFAULT filters and
// keeps the context filters.
//
// The two lists are not the same kind of thing. A default filter is a
// default scope; a context filter is a row-visibility boundary.
// Widening a default scope when the caller asked to widen it costs
// nothing. Dropping the boundary hands this request every tenant's
// rows, or every subject's, and it does so on the one method a caller
// reaches for while thinking about soft-deleted rows rather than about
// tenancy.
//
// So Unscoped has ONE meaning per level: statement-wide on the raw
// builder, where the caller is describing the whole statement's
// authority and a reviewer reads the whole of what was given up, and
// defaults-only on the entity query.
//
// This dialect used to make the other trade — the entity query
// delegated straight to the builder.
func TestEntityQueryUnscopedKeepsTheTenantAndDropsTheDefaultFilter(t *testing.T) {
	tbl := clickhouse.NewTable("equ_hits")
	clickhouse.Add(tbl, clickhouse.UInt64("id"))
	tenant := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	clickhouse.Add(tbl, clickhouse.String("path"))
	deleted := clickhouse.SoftDelete(tbl).DeletedAt
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant).
		DefaultFilter(deleted.IsNull()).
		ContextFilter(clickhouse.TenantFilter(tenant))
	ent := clickhouse.NewEntity[equHit](tbl, clickhouse.AllowUnmappedColumns("deletedAt"))

	drv := dropstest.New().AlwaysRows([]string{"id", "tenantId", "path"})
	db := clickhouse.New(drv)

	if _, err := ent.Query(db).All(tenantCtx("acme")); err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := drv.LastSQL(); !strings.Contains(got, `"deletedAt" IS NULL`) {
		t.Fatalf("the scoped query lost its default filter:\n%s", got)
	}

	drv.Reset()
	if _, err := ent.Query(db).Unscoped().All(tenantCtx("acme")); err != nil {
		t.Fatalf("All: %v", err)
	}
	got := drv.LastSQL()
	if strings.Contains(got, `"deletedAt" IS NULL`) {
		t.Errorf("Unscoped did not drop the default filter:\n%s", got)
	}
	if !strings.Contains(got, `("equ_hits"."tenantId" = ?)`) {
		t.Errorf("Unscoped dropped the tenant axis:\n%s", got)
	}

	// And it still refuses on a ctx with no tenant, rather than widening
	// to every tenant because the caller asked for hidden rows.
	drv.Reset()
	if _, err := ent.Query(db).Unscoped().All(context.Background()); !errors.Is(err, clickhouse.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, clickhouse.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// equHit is the row type the case above scans.
type equHit struct {
	ID       uint64 `drop:"id"`
	TenantID string `drop:"tenantId"`
	Path     string `drop:"path"`
}

// The raw builder keeps the wide meaning: a caller who says Unscoped
// there is describing this statement's authority, and gets a statement
// with no automatic predicate of either kind and no refusal.
func TestSelectBuilderUnscopedStaysStatementWide(t *testing.T) {
	tbl := clickhouse.NewTable("sbu_hits")
	id := clickhouse.Add(tbl, clickhouse.UInt64("id"))
	tenant := clickhouse.Add(tbl, clickhouse.String("tenantId"))
	deleted := clickhouse.SoftDelete(tbl).DeletedAt
	tbl.Engine(clickhouse.MergeTree()).OrderBy(tenant).
		DefaultFilter(deleted.IsNull()).
		ContextFilter(clickhouse.TenantFilter(tenant))

	db := clickhouse.New(dropstest.New())
	sql, args, err := db.Select(id).From(tbl).Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if sql != `SELECT "sbu_hits"."id" FROM "sbu_hits"` {
		t.Errorf("sql = %s", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}
