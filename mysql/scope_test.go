package mysql_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/mysql"
)

// The tenant axis, declared on the TABLE and resolved by the EXECUTORS.
//
// Every test here asserts on RENDERED SQL and ARGS rather than on rows
// that came back, because a round trip cannot see this defect: a
// fixture with one tenant in it answers a leaking query and a scoped
// query identically, and the whole failure mode is a statement that
// looks right in a review and reads somebody else's rows in production.
//
// The shapes are the ones drops/pg found over five rounds, asked again
// of this dialect, where several of the answers differ because the SQL
// does — a RIGHT JOIN, an aliased DELETE that has to name its alias
// twice, an upsert with no conflict target and no WHERE clause.

// scopeTenant is the tenant these tests put on ctx. It is distinctive
// so a predicate that bound the wrong value cannot pass by coincidence.
const scopeTenant = int64(77)

func scopeCtx() context.Context {
	return mysql.WithTenant(context.Background(), scopeTenant)
}

// scopedTable builds a table carrying a tenant axis and nothing else,
// so every predicate in the rendered SQL can only have come from it.
func scopedTable(name string) (tbl *mysql.Table, id, tenant *mysql.Column) {
	t := mysql.NewTable(name)
	idCol := mysql.Add(t, mysql.BigInt("id").PrimaryKey())
	tenantCol := mysql.Add(t, mysql.BigInt("tenantId").NotNull())
	mysql.Add(t, mysql.Text("title"))
	t.ContextFilter(mysql.TenantFilter(tenantCol))
	t.ScopeWritesByTenant(tenantCol)
	return t, idCol.Column, tenantCol.Column
}

// plainTable builds a table with no automatic predicate at all, to sit
// on the other side of a join or to hold the outer statement.
func plainTable(name string) (tbl *mysql.Table, id *mysql.Col[int64]) {
	t := mysql.NewTable(name)
	idCol := mysql.Add(t, mysql.BigSerial("id").PrimaryKey())
	mysql.Add(t, mysql.BigInt("postId"))
	return t, idCol
}

// ctxRenderable is every builder in the package: the interface exists
// so these tests ask each of them the same question through one helper.
type ctxRenderable interface {
	ToSQLCtx(ctx context.Context) (string, []any, error)
}

// mustCtxSQL renders for ctx and fails the test on a refusal.
func mustCtxSQL(t *testing.T, s ctxRenderable) (string, []any) {
	t.Helper()
	sql, args, err := s.ToSQLCtx(scopeCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	return sql, args
}

// wantText compares rendered SQL exactly. Exactly, because the defect
// this file is about is a predicate that is present and powerless, and
// "contains tenantId" cannot tell that apart from a predicate in the
// wrong clause.
func wantText(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func wantArgs(t *testing.T, got []any, want ...any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
}

// ----------------------------------------------------------------------
// The root query, and the executors that resolve it.
// ----------------------------------------------------------------------

// A bare db.Select().From(scoped) is the spelling the readme shows
// first, and the one an Entity-injected predicate never reached.
func TestSelectFromScopedTableCarriesTheAxis(t *testing.T) {
	posts, id, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().From(posts).Where(mysql.Eq(id, int64(9))))
	wantText(t, sql, "SELECT * FROM `posts` WHERE (`posts`.`id` = ?) AND (`posts`.`tenantId` = ?)")
	wantArgs(t, args, int64(9), scopeTenant)
}

// The executors are what apply the axis, so what goes on the wire has
// to carry it — not just what ToSQLCtx renders.
func TestExecutorsSendTheScopedStatement(t *testing.T) {
	posts, id, _ := scopedTable("posts")
	drv := dropstest.New()
	db := mysql.New(drv)
	ctx := scopeCtx()

	if _, err := db.Select().From(posts).Rows(ctx); err != nil {
		t.Fatalf("Rows: %v", err)
	}
	if _, err := db.Update(posts).Set(mysql.Bind(posts.Col("title"), "x")).Exec(ctx); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := db.Delete(posts).Where(mysql.Eq(id, int64(1))).Exec(ctx); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	for _, st := range drv.Statements() {
		if !strings.Contains(st.SQL, "`tenantId` = ?") {
			t.Errorf("statement went out unscoped: %s", st.SQL)
		}
	}
}

// ToSQL can no longer show the whole statement, and ToSQLCtx is its
// ctx-aware twin. A test that asserted on ToSQL would be asserting on
// half a statement.
func TestToSQLOmitsTheContextFilterAndToSQLCtxDoesNot(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	if sql, _ := db.Select().From(posts).ToSQL(); strings.Contains(sql, "tenantId") {
		t.Errorf("ToSQL rendered a context filter: %s", sql)
	}
	sql, _ := mustCtxSQL(t, db.Select().From(posts))
	if !strings.Contains(sql, "tenantId") {
		t.Errorf("ToSQLCtx lost the context filter: %s", sql)
	}
}

// A ctx with no tenant is ErrTenantMissing and no statement at all.
// Fail-closed is the whole point; a filter that cannot decide what a
// request may see must not be answered with an unfiltered query.
func TestEveryStatementFailsClosedWithoutATenant(t *testing.T) {
	posts, id, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())
	bare := context.Background()

	stmts := map[string]ctxRenderable{
		"select": db.Select().From(posts),
		"update": db.Update(posts).Set(mysql.Bind(posts.Col("title"), "x")),
		"delete": db.Delete(posts).Where(mysql.Eq(id, int64(1))),
		"insert": db.Insert(posts).Row(mysql.Bind(posts.Col("title"), "x")),
	}
	for name, st := range stmts {
		sql, _, err := st.ToSQLCtx(bare)
		if !errors.Is(err, mysql.ErrTenantMissing) {
			t.Errorf("%s: err = %v, want ErrTenantMissing", name, err)
		}
		if sql != "" {
			t.Errorf("%s rendered %q despite refusing", name, sql)
		}
	}
}

// The refusal names the table and column that stopped the query: in a
// schema where four tables are scoped, "ctx has no tenant" alone says
// the same thing whichever one refused.
func TestTenantMissingNamesTheTableThatRefused(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	_, _, err := db.Select().From(posts).ToSQLCtx(context.Background())
	if err == nil || !strings.Contains(err.Error(), "posts.tenantId") {
		t.Errorf("err = %v, want it to name posts.tenantId", err)
	}
}

// Unscoped is the escape hatch, and it clears the context filters as
// well as the default ones: a half-scoped statement is the worse of the
// two answers.
func TestUnscopedDropsTheAxisAndNeedsNoTenant(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _, err := db.Select().From(posts).Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("Unscoped still refused: %v", err)
	}
	wantText(t, sql, "SELECT * FROM `posts`")
}

// ----------------------------------------------------------------------
// Item 2: the executors copy, so rendering twice does not accrete.
// ----------------------------------------------------------------------

// A builder rendered twice must not carry the predicate twice, and a
// builder held across requests must not answer the second request with
// the first one's tenant. Both failures are silent: the rows come back
// right and only the argument list shows it.
func TestRenderingTwiceDoesNotAccreteOrPinTheTenant(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())
	sel := db.Select().From(posts)

	first, firstArgs := mustCtxSQL(t, sel)
	second, secondArgs := mustCtxSQL(t, sel)
	wantText(t, second, first)
	if len(secondArgs) != 1 || len(firstArgs) != 1 {
		t.Fatalf("args accreted: %v then %v", firstArgs, secondArgs)
	}

	other := mysql.WithTenant(context.Background(), int64(1234))
	_, args, err := sel.ToSQLCtx(other)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	wantArgs(t, args, int64(1234))
}

// The same, for the two statements where a repeated predicate would be
// a repeated write rather than a repeated read.
func TestUpdateAndDeleteAreReusable(t *testing.T) {
	posts, id, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	upd := db.Update(posts).Set(mysql.Bind(posts.Col("title"), "x"))
	u1, _ := mustCtxSQL(t, upd)
	u2, _ := mustCtxSQL(t, upd)
	wantText(t, u2, u1)

	del := db.Delete(posts).Where(mysql.Eq(id, int64(3)))
	d1, _ := mustCtxSQL(t, del)
	d2, _ := mustCtxSQL(t, del)
	wantText(t, d2, d1)
}

// ----------------------------------------------------------------------
// Item 7: joined tables carry their own filters, into the clause the
// join kind allows. MySQL has three kinds and no FULL JOIN.
// ----------------------------------------------------------------------

// An INNER JOIN emits only matched pairs, so the joined table's
// predicate reads the same in either clause and goes in the WHERE with
// the others.
func TestInnerJoinCarriesTheJoinedTablesAxisInWhere(t *testing.T) {
	comments, _ := plainTable("comments")
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().From(comments).
		Join(posts, mysql.Eq(comments.Col("postId"), postID)))
	wantText(t, sql, "SELECT * FROM `comments` INNER JOIN `posts` "+
		"ON (`comments`.`postId` = `posts`.`id`) WHERE (`posts`.`tenantId` = ?)")
	wantArgs(t, args, scopeTenant)
}

// A LEFT JOIN preserves the FROM table, so its joined side's guard
// belongs in the ON clause: in the WHERE clause it is false for every
// unmatched row and quietly turns the join into an INNER one.
func TestLeftJoinCarriesTheJoinedTablesAxisInTheOnClause(t *testing.T) {
	comments, _ := plainTable("comments")
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().From(comments).
		LeftJoin(posts, mysql.Eq(comments.Col("postId"), postID)))
	wantText(t, sql, "SELECT * FROM `comments` LEFT JOIN `posts` "+
		"ON ((`comments`.`postId` = `posts`.`id`) AND (`posts`.`tenantId` = ?))")
	wantArgs(t, args, scopeTenant)
}

// A RIGHT JOIN inverts the reasoning twice over. The joined table is
// the preserved side, so its guard goes back in the WHERE clause — in
// the ON clause an unmatched row of it survives with a NULL left side,
// predicate and all, which is a leak rather than an over-filter. And
// the FROM table has become the nullable side, so ITS guard has to move
// into the first RIGHT JOIN's ON clause.
func TestRightJoinInvertsBothPlacements(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	tags, tagID, _ := scopedTable("tags")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().From(posts).
		RightJoin(tags, mysql.Eq(postID, tagID)))
	wantText(t, sql, "SELECT * FROM `posts` RIGHT JOIN `tags` "+
		"ON ((`posts`.`id` = `tags`.`id`) AND (`posts`.`tenantId` = ?)) "+
		"WHERE (`tags`.`tenantId` = ?)")
	wantArgs(t, args, scopeTenant, scopeTenant)
}

// The FROM table's predicates go into the FIRST right join's ON clause:
// that is where the FROM table enters the join tree, so every later
// join sees the restricted relation. In a later ON clause the same
// predicate would be false for rows an earlier outer join had already
// NULL-extended.
func TestFromTablePredicateLandsInTheFirstRightJoin(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	tags, tagID, _ := scopedTable("tags")
	extra, _ := plainTable("extra")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Select().From(posts).
		RightJoin(tags, mysql.Eq(postID, tagID)).
		RightJoin(extra, mysql.Eq(postID, extra.Col("postId"))))
	first := strings.Index(sql, "RIGHT JOIN `tags`")
	second := strings.Index(sql, "RIGHT JOIN `extra`")
	at := strings.Index(sql, "`posts`.`tenantId` = ?")
	if at < first || at > second {
		t.Errorf("the FROM table's guard is not in the first RIGHT JOIN's ON clause: %s", sql)
	}
}

// Unscoped is statement-wide: a flag that unscoped the FROM table while
// a joined one kept its axis would answer with a silently narrowed
// slice of the rows that were asked for.
func TestUnscopedReachesTheJoinedTablesToo(t *testing.T) {
	comments, _ := plainTable("comments")
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, _, err := db.Select().From(comments).
		Join(posts, mysql.Eq(comments.Col("postId"), postID)).
		Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("Unscoped still refused: %v", err)
	}
	if strings.Contains(sql, "tenantId") {
		t.Errorf("Unscoped left the joined table's axis behind: %s", sql)
	}
}

// A join whose ON clause holds a statement is a statement like any
// other, and the resolver walks into it.
func TestJoinOnConditionIsResolved(t *testing.T) {
	comments, _ := plainTable("comments")
	posts, postID, _ := scopedTable("posts")
	other, otherID := plainTable("other")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Select().From(comments).
		Join(other, mysql.Exists(db.Select(postID).From(posts))).
		Where(mysql.Eq(otherID, int64(1))))
	if !strings.Contains(sql, "EXISTS (SELECT `posts`.`id` FROM `posts` WHERE (`posts`.`tenantId` = ?))") {
		t.Errorf("a join's ON condition went out unscoped: %s", sql)
	}
}

// ----------------------------------------------------------------------
// Item 8: CTE bodies and subquery operands, resolved recursively.
// ----------------------------------------------------------------------

func TestCTEBodyIsScoped(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	cte := mysql.CTEDef("recent", db.Select(postID).From(posts))
	sql, args := mustCtxSQL(t, db.Select().With(cte).FromExpr(cte.Ref()))
	wantText(t, sql, "WITH `recent` AS (SELECT `posts`.`id` FROM `posts` "+
		"WHERE (`posts`.`tenantId` = ?)) SELECT * FROM `recent`")
	wantArgs(t, args, scopeTenant)
}

// A CTE is a value a caller can attach to a second query, so the
// resolved body must not be written back into it — that would pin one
// request's tenant into every later statement that names it.
func TestCTEIsNotMutatedByResolution(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	cte := mysql.CTEDef("recent", db.Select(postID).From(posts))
	first, _ := mustCtxSQL(t, db.Select().With(cte).FromExpr(cte.Ref()))
	_, args, err := db.Select().With(cte).FromExpr(cte.Ref()).
		ToSQLCtx(mysql.WithTenant(context.Background(), int64(5)))
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	wantArgs(t, args, int64(5))
	if strings.Count(first, "tenantId") != 1 {
		t.Errorf("the CTE body accreted predicates: %s", first)
	}
}

// A derived table is the ordinary way to write a per-tenant aggregate,
// and AsSubquery used to swallow the statement in a closure.
func TestDerivedTableIsScoped(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().FromExpr(db.Select(postID).From(posts).AsSubquery("p")))
	wantText(t, sql, "SELECT * FROM (SELECT `posts`.`id` FROM `posts` "+
		"WHERE (`posts`.`tenantId` = ?)) AS `p`")
	wantArgs(t, args, scopeTenant)
}

// Every list the renderer reads has to be one the resolver walks. A
// list it renders and never walks is a place a caller can write a
// SELECT that goes out unscoped.
func TestEveryClauseOfASelectIsWalked(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	plain, plainID := plainTable("plain")
	db := mysql.New(dropstest.New())
	sub := func() *mysql.SelectBuilder { return db.Select(postID).From(posts) }

	cases := map[string]*mysql.SelectBuilder{
		"columns":  db.Select(mysql.Subquery(sub())).From(plain),
		"where":    db.Select().From(plain).Where(mysql.InSub(plainID, sub())),
		"groupBy":  db.Select().From(plain).GroupBy(mysql.Subquery(sub())),
		"having":   db.Select().From(plain).Having(mysql.InSub(plainID, sub())),
		"orderBy":  db.Select().From(plain).OrderBy(mysql.Subquery(sub())),
		"fromExpr": db.Select().From(plain).FromExpr(sub().AsSubquery("s")),
	}
	for name, sel := range cases {
		sql, args := mustCtxSQL(t, sel)
		if !strings.Contains(sql, "`posts`.`tenantId` = ?") {
			t.Errorf("%s: the nested statement went out unscoped: %s", name, sql)
		}
		if len(args) != 1 || args[0] != scopeTenant {
			t.Errorf("%s: args = %v", name, args)
		}
	}
}

// The same lists, on a ctx with no tenant: a nested statement that
// cannot be scoped refuses the whole statement rather than rendering.
func TestANestedStatementRefusesTheWholeQuery(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	plain, plainID := plainTable("plain")
	db := mysql.New(dropstest.New())

	sel := db.Select().From(plain).Where(mysql.InSub(plainID, db.Select(postID).From(posts)))
	if _, _, err := sel.ToSQLCtx(context.Background()); !errors.Is(err, mysql.ErrTenantMissing) {
		t.Errorf("err = %v, want ErrTenantMissing", err)
	}
}

// ----------------------------------------------------------------------
// Item 12: the writes.
// ----------------------------------------------------------------------

func TestUpdateCarriesTheAxisAfterTheCallersPredicate(t *testing.T) {
	posts, id, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Update(posts).
		Set(mysql.Bind(posts.Col("title"), "x")).
		Where(mysql.Eq(id, int64(4))))
	wantText(t, sql, "UPDATE `posts` SET `title` = ? WHERE (`posts`.`id` = ?) AND (`posts`.`tenantId` = ?)")
	wantArgs(t, args, "x", int64(4), scopeTenant)
}

func TestDeleteCarriesTheAxis(t *testing.T) {
	posts, id, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Delete(posts).Where(mysql.Eq(id, int64(4))))
	wantText(t, sql, "DELETE FROM `posts` WHERE (`posts`.`id` = ?) AND (`posts`.`tenantId` = ?)")
	wantArgs(t, args, int64(4), scopeTenant)
}

// A SET whose value is a statement decides what gets written rather
// than which rows do, and it used to render through WriteSQL — reading
// every tenant's rows to compute a value stored in this tenant's.
func TestUpdateSetExpressionIsScoped(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	plain, _ := plainTable("plain")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Update(plain).
		SetExpr(plain.Col("postId"), mysql.Subquery(db.Select(postID).From(posts).Limit(1))))
	if !strings.Contains(sql, "`posts`.`tenantId` = ?") {
		t.Errorf("the SET subquery went out unscoped: %s", sql)
	}
}

// ----------------------------------------------------------------------
// Item 9: an aliased statement, and this dialect's aliased DELETE.
// ----------------------------------------------------------------------

// A filter is built from the declared column handles, so under an alias
// it names a relation the statement has no FROM entry for — error 1054,
// a query that cannot run rather than a widened result.
func TestAliasedSelectRendersTheAxisUnderTheAlias(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	p := posts.As("p")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().From(p))
	wantText(t, sql, "SELECT * FROM `posts` AS `p` WHERE (`p`.`tenantId` = ?)")
	wantArgs(t, args, scopeTenant)
}

// An aliased DELETE has to be written in the multi-table form — MariaDB
// answers 1064 to DELETE FROM t AS a — which names the alias twice and
// leaves no un-aliased spelling to fall back on.
func TestAliasedDeleteRendersTheAxisUnderTheAlias(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	p := posts.As("p")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Delete(p))
	wantText(t, sql, "DELETE `p` FROM `posts` AS `p` WHERE (`p`.`tenantId` = ?)")
	wantArgs(t, args, scopeTenant)
}

func TestAliasedUpdateRendersTheAxisUnderTheAlias(t *testing.T) {
	posts, _, _ := scopedTable("posts")
	p := posts.As("p")
	db := mysql.New(dropstest.New())

	sql, _ := mustCtxSQL(t, db.Update(p).Set(mysql.Bind(p.Col("title"), "x")))
	wantText(t, sql, "UPDATE `posts` AS `p` SET `title` = ? WHERE (`p`.`tenantId` = ?)")
}

// Go initialises package-level variables before it runs init, so an
// alias declared beside its table is taken before any init that
// registers the axis. Sharing the scope is what stops that alias from
// rendering a DELETE with no predicate at all.
func TestAnAliasTakenBeforeTheAxisStillCarriesIt(t *testing.T) {
	posts := mysql.NewTable("posts")
	mysql.Add(posts, mysql.BigInt("id").PrimaryKey())
	tenant := mysql.Add(posts, mysql.BigInt("tenantId"))
	early := posts.As("p") // taken first, as a package-level var would be

	posts.ContextFilter(mysql.TenantFilter(tenant))

	db := mysql.New(dropstest.New())
	sql, args := mustCtxSQL(t, db.Delete(early))
	wantText(t, sql, "DELETE `p` FROM `posts` AS `p` WHERE (`p`.`tenantId` = ?)")
	wantArgs(t, args, scopeTenant)
}

// The DEFAULT filter list is shared on the same terms as the context
// filters, and for the same reason at one remove: the loss is a
// soft-delete guard rather than a tenant axis, so the alias reads rows
// the application has already deleted instead of rows another tenant
// owns. Both are the alias disagreeing with its table about what the
// table is.
//
// This is the divergence the cross-dialect verifier found. drops/pg,
// drops/sqlite and drops/clickhouse shared both lists; this dialect
// shared one and snapshotted the other, and nobody had written down
// which answer was the rule.
func TestAnAliasTakenBeforeTheDefaultFilterStillCarriesIt(t *testing.T) {
	tbl := mysql.NewTable("dfa_users")
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false))
	early := tbl.As("u") // taken first, as a package-level var would be

	tbl.DefaultFilter(deleted.IsNull())

	db := mysql.New(dropstest.New())
	sql, _ := db.Select().From(early).ToSQL()
	wantText(t, sql, "SELECT * FROM `dfa_users` AS `u` WHERE (`u`.`deletedAt` IS NULL)")
}

// Sharing runs in the other direction too, and saying so is half the
// rule: a filter registered ON an alias is registered on the table, and
// so on every other alias of it. That is what "the same table" means.
func TestADefaultFilterRegisteredOnAnAliasReachesTheTable(t *testing.T) {
	tbl := mysql.NewTable("dfb_users")
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false))

	tbl.As("u").DefaultFilter(deleted.IsNull())

	db := mysql.New(dropstest.New())
	sql, _ := db.Select().From(tbl).ToSQL()
	wantText(t, sql, "SELECT * FROM `dfb_users` WHERE (`dfb_users`.`deletedAt` IS NULL)")
}

// A DefaultFilter is rendered under the same rename, which is a repair
// rather than a side effect: before it, an aliased query against a
// soft-deleting table rendered `users`.`deletedAt` against
// FROM `users` AS `u` and MySQL answered 1054 — a query that could not
// run at all.
func TestAliasedSelectRendersDefaultFiltersUnderTheAlias(t *testing.T) {
	tbl := mysql.NewTable("users")
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false))
	tbl.DefaultFilter(deleted.IsNull())
	db := mysql.New(dropstest.New())

	sql, _ := db.Select().From(tbl.As("u")).ToSQL()
	wantText(t, sql, "SELECT * FROM `users` AS `u` WHERE (`u`.`deletedAt` IS NULL)")

	base, _ := db.Select().From(tbl).ToSQL()
	wantText(t, base, "SELECT * FROM `users` WHERE (`users`.`deletedAt` IS NULL)")
}

// A self-join names one relation twice, and each side has to be
// restricted on its own.
func TestSelfJoinScopesBothSides(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	p := posts.As("p")
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().From(posts).Join(p, mysql.Eq(postID, p.Col("id"))))
	wantText(t, sql, "SELECT * FROM `posts` INNER JOIN `posts` AS `p` "+
		"ON (`posts`.`id` = `p`.`id`) WHERE (`posts`.`tenantId` = ?) AND (`p`.`tenantId` = ?)")
	wantArgs(t, args, scopeTenant, scopeTenant)
}

// A joined table's DefaultFilters are carried too, and they were not
// before: a soft-deleting table joined into a query answered with its
// deleted rows, which is the same defect as the tenant one with a
// milder consequence.
func TestJoinedTableCarriesItsDefaultFilters(t *testing.T) {
	comments, _ := plainTable("comments")
	posts := mysql.NewTable("posts")
	postID := mysql.Add(posts, mysql.BigInt("id").PrimaryKey())
	deleted := mysql.Add(posts, mysql.Timestamp("deletedAt", false))
	posts.DefaultFilter(deleted.IsNull())
	db := mysql.New(dropstest.New())

	sql, _ := db.Select().From(comments).
		Join(posts, mysql.Eq(comments.Col("postId"), postID)).ToSQL()
	wantText(t, sql, "SELECT * FROM `comments` INNER JOIN `posts` "+
		"ON (`comments`.`postId` = `posts`.`id`) WHERE (`posts`.`deletedAt` IS NULL)")
}

// A DefaultFilter is declaration-time, but the predicate it answers
// with is the same kind of thing a ContextFilter answers with, and a
// statement embedded in one is as invisible to the renderer as a
// statement embedded in the other.
func TestADefaultFilterHoldingAStatementIsResolved(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	blocked, blockedID := plainTable("blocked")
	blocked.DefaultFilter(mysql.NotInSub(blockedID, mysql.New(dropstest.New()).
		Select(postID).From(posts)))
	db := mysql.New(dropstest.New())

	sql, args := mustCtxSQL(t, db.Select().From(blocked))
	if !strings.Contains(sql, "`posts`.`tenantId` = ?") {
		t.Errorf("the default filter's own statement went out unscoped: %s", sql)
	}
	wantArgs(t, args, scopeTenant)
}

// DB.ExecExpr is the one executor on *DB that takes an expression, and
// it used to take a ctx and throw it away — so a statement handed to it
// went out with no context filter and refused nothing.
func TestExecExprRendersForItsContext(t *testing.T) {
	posts, id, _ := scopedTable("posts")
	drv := dropstest.New()
	db := mysql.New(drv)

	if _, err := db.ExecExpr(scopeCtx(), db.Delete(posts).Where(mysql.Eq(id, int64(2)))); err != nil {
		t.Fatalf("ExecExpr: %v", err)
	}
	if got := drv.LastSQL(); !strings.Contains(got, "`posts`.`tenantId` = ?") {
		t.Errorf("ExecExpr sent an unscoped statement: %s", got)
	}
	if _, err := db.ExecExpr(context.Background(), db.Delete(posts)); !errors.Is(err, mysql.ErrTenantMissing) {
		t.Errorf("err = %v, want ErrTenantMissing", err)
	}
	if len(drv.Statements()) != 1 {
		t.Errorf("the refused statement still went to the driver: %d", len(drv.Statements()))
	}
	// A DDL helper has nothing to resolve and renders exactly as it did.
	if _, err := db.ExecExpr(context.Background(), mysql.DropTable(posts)); err != nil {
		t.Fatalf("ExecExpr(DDL): %v", err)
	}
	if got := drv.LastSQL(); got != "DROP TABLE `posts`" {
		t.Errorf("DDL rendering changed: %s", got)
	}
}

// ----------------------------------------------------------------------
// The filter list itself: cycles, refusals, and concurrent registration.
// ----------------------------------------------------------------------

// A filter whose predicate selects from the table it filters has no
// fixed point to converge on: left alone it is a goroutine that never
// returns.
func TestAFilterThatSelectsFromItsOwnTableIsRefused(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())
	posts.ContextFilter(func(context.Context) (drops.Expression, error) {
		return mysql.InSub(postID, db.Select(postID).From(posts)), nil
	})

	_, _, err := db.Select().From(posts).ToSQLCtx(scopeCtx())
	if !errors.Is(err, mysql.ErrContextFilterCycle) {
		t.Fatalf("err = %v, want ErrContextFilterCycle", err)
	}
	if !strings.Contains(err.Error(), "posts -> posts") {
		t.Errorf("the error does not name the cycle: %v", err)
	}
}

// The same filter with Unscoped on the inner statement terminates, and
// that is the shape the author meant: the inner read selects the rows
// the filter is about to restrict.
func TestAFilterCanConsultItsOwnTableWhenUnscoped(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	db := mysql.New(dropstest.New())
	posts.ContextFilter(func(context.Context) (drops.Expression, error) {
		return mysql.InSub(postID, db.Select(postID).From(posts).Unscoped()), nil
	})

	sql, _ := mustCtxSQL(t, db.Select().From(posts))
	if !strings.Contains(sql, "IN (SELECT `posts`.`id` FROM `posts`)") {
		t.Errorf("got %s", sql)
	}
}

// A filter answers with a predicate, and a predicate is a place a
// statement can be written. The guard meant to decide what a request
// may see would otherwise be itself the unscoped read.
func TestAFiltersOwnSubqueryIsScoped(t *testing.T) {
	posts, postID, _ := scopedTable("posts")
	memberships, memberID, _ := scopedTable("memberships")
	db := mysql.New(dropstest.New())
	posts.ContextFilter(func(context.Context) (drops.Expression, error) {
		return mysql.InSub(postID, db.Select(memberID).From(memberships)), nil
	})

	sql, _ := mustCtxSQL(t, db.Select().From(posts))
	if !strings.Contains(sql, "`memberships`.`tenantId` = ?") {
		t.Errorf("the filter's own subquery went out unscoped: %s", sql)
	}
}

// Registering a filter while queries are in flight is a pattern the
// keyed registration invites, so it must not be a data race. Run with
// -race, this is the test that says so.
func TestFilterRegistrationIsSafeWhileQueriesRun(t *testing.T) {
	posts, _, tenant := scopedTable("posts")
	db := mysql.New(dropstest.New())
	ctx := scopeCtx()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, _, err := db.Select().From(posts).ToSQLCtx(ctx); err != nil {
					t.Errorf("ToSQLCtx: %v", err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 50; j++ {
			posts.ScopeWritesByTenant(tenant)
		}
	}()
	wg.Wait()
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
// delegated straight to the builder — and its doc sentence promised the
// narrow one regardless. That sentence is the one place in the port
// that promised more safety than the code delivered.
func TestEntityQueryUnscopedKeepsTheTenantAndDropsTheDefaultFilter(t *testing.T) {
	tbl := mysql.NewTable("equ_posts")
	mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	tenant := mysql.Add(tbl, mysql.BigInt("tenantId").NotNull())
	mysql.Add(tbl, mysql.Text("title"))
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false))
	tbl.DefaultFilter(deleted.IsNull())
	ent := mysql.NewEntity[equPost](tbl, mysql.AllowUnmappedColumns("deletedAt")).
		ScopeByTenant(tenant)

	drv := dropstest.New().AlwaysRows([]string{"id", "tenantId", "title"})
	db := mysql.New(drv)

	if _, err := ent.Query(db).All(scopeCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	if got := drv.LastSQL(); !strings.Contains(got, "`deletedAt` IS NULL") {
		t.Fatalf("the scoped query lost its default filter:\n%s", got)
	}

	drv.Reset()
	if _, err := ent.Query(db).Unscoped().All(scopeCtx()); err != nil {
		t.Fatalf("All: %v", err)
	}
	got := drv.LastSQL()
	if strings.Contains(got, "`deletedAt` IS NULL") {
		t.Errorf("Unscoped did not drop the default filter:\n%s", got)
	}
	if !strings.Contains(got, "(`equ_posts`.`tenantId` = ?)") {
		t.Errorf("Unscoped dropped the tenant axis:\n%s", got)
	}

	// And it still refuses on a ctx with no tenant, rather than widening
	// to every tenant because the caller asked for hidden rows.
	drv.Reset()
	if _, err := ent.Query(db).Unscoped().All(context.Background()); !errors.Is(err, mysql.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, mysql.ErrTenantMissing)
	}
	if len(drv.SQL()) != 0 {
		t.Errorf("statements = %v, want none", drv.SQL())
	}
}

// equPost is the row type the case above scans.
type equPost struct {
	ID       int64  `drop:"id"`
	TenantID int64  `drop:"tenantId"`
	Title    string `drop:"title"`
}

// The raw builder keeps the wide meaning: a caller who says Unscoped
// there is describing this statement's authority, and gets a statement
// with no automatic predicate of either kind and no refusal.
func TestSelectBuilderUnscopedStaysStatementWide(t *testing.T) {
	tbl := mysql.NewTable("sbu_posts")
	id := mysql.Add(tbl, mysql.BigInt("id").PrimaryKey())
	tenant := mysql.Add(tbl, mysql.BigInt("tenantId").NotNull())
	deleted := mysql.Add(tbl, mysql.Timestamp("deletedAt", false))
	tbl.DefaultFilter(deleted.IsNull())
	tbl.ContextFilter(mysql.TenantFilter(tenant))

	db := mysql.New(dropstest.New())
	sql, args, err := db.Select(id).From(tbl).Unscoped().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	wantText(t, sql, "SELECT `sbu_posts`.`id` FROM `sbu_posts`")
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}
