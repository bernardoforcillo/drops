package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// DELETE has two renderings — the statement itself, and the UPDATE a
// DeleteHook rewrites it into for a soft-deleted table — and a scoping
// defect can hide in either one. Both are asserted here, on rendered
// SQL, for the reason update_test.go gives: a missing predicate on a
// USING table is invisible to a round trip against a single-tenant
// fixture, and a predicate qualified with the un-aliased relation only
// fails against a real server.

// whard builds a table that carries both kinds of automatic predicate
// but no DeleteHook, so a DELETE against it renders as a DELETE. The
// soft-delete fixture (wscoped, in update_test.go) cannot show that,
// because its hook replaces the statement before the DELETE renderer
// ever runs.
func whard(name string, cols func(*pg.Table)) *pg.Table {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	archived := pg.Add(t, pg.Timestamp("archivedAt", true))
	if cols != nil {
		cols(t)
	}
	t.DefaultFilter(pg.IsNull(archived))
	t.ContextFilter(pg.TenantFilter(tenant))
	return t
}

// ----------------------------------------------------------------------
// OPEN-1 — the target table under an alias
// ----------------------------------------------------------------------

// DELETE FROM "notes" AS "n" introduces exactly one relation and it is
// "n". A default filter rendering "notes"."archivedAt" names a relation
// with no FROM entry: PostgreSQL raises 42P01 and the row is never
// deleted, so the statement fails rather than over-deleting — but it
// fails on the one table shape that must never lose its scoping.
func TestDeleteAliasQualifiesAutomaticFilters(t *testing.T) {
	db := pg.New(nil)
	notes := whard("notes", nil)
	n := notes.As("n")

	checkCtx(t, wctx(),
		db.Delete(n).Where(pg.Eq(n.Col("id"), 1)),
		`DELETE FROM "notes" AS "n" WHERE `+
			`("n"."archivedAt" IS NULL) AND ("n"."id" = $1) AND ("n"."tenantId" = $2)`,
		1, wtenant,
	)
}

// The soft-delete rewrite goes out as an UPDATE, so it inherits the
// UPDATE renderer's alias handling. Asserting it separately is what
// keeps the two renderings from drifting apart.
func TestDeleteAliasSoftDeleteRewriteQualifiesAutomaticFilters(t *testing.T) {
	db := pg.New(nil)
	notes := wscoped("notes", nil)
	n := notes.As("n")

	checkCtx(t, wctx(),
		db.Delete(n).Where(pg.Eq(n.Col("id"), 1)),
		`UPDATE "notes" AS "n" SET "deletedAt" = now() WHERE `+
			`("n"."deletedAt" IS NULL) AND ("n"."id" = $1) AND ("n"."tenantId" = $2)`,
		1, wtenant,
	)
}

// ----------------------------------------------------------------------
// OPEN-2 — the USING tables
// ----------------------------------------------------------------------

// DELETE ... USING puts its join condition in the WHERE clause, so an
// unfiltered USING table lets another tenant's rows choose which of
// yours are removed.
func TestDeleteUsingCarriesJoinedTableScope(t *testing.T) {
	db := pg.New(nil)
	accounts := whard("accounts", nil)
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	checkCtx(t, wctx(),
		db.Delete(accounts).
			Using(posts).
			Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))),
		`DELETE FROM "accounts" USING "posts" WHERE `+
			`("accounts"."archivedAt" IS NULL) AND `+
			`("posts"."archivedAt" IS NULL) AND `+
			`("accounts"."id" = "posts"."accountId") AND `+
			`("accounts"."tenantId" = $1) AND `+
			`("posts"."tenantId" = $2)`,
		wtenant, wtenant,
	)
}

// A USING table reached under an alias needs the same relation rename
// the target table needs.
func TestDeleteUsingAliasQualifiesJoinedTableScope(t *testing.T) {
	db := pg.New(nil)
	accounts := whard("accounts", nil)
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	p := posts.As("p")

	checkCtx(t, wctx(),
		db.Delete(accounts).
			Using(p).
			Where(pg.Eq(accounts.Col("id"), p.Col("accountId"))),
		`DELETE FROM "accounts" USING "posts" AS "p" WHERE `+
			`("accounts"."archivedAt" IS NULL) AND `+
			`("p"."archivedAt" IS NULL) AND `+
			`("accounts"."id" = "p"."accountId") AND `+
			`("accounts"."tenantId" = $1) AND `+
			`("p"."tenantId" = $2)`,
		wtenant, wtenant,
	)
}

// The rewrite has to carry the USING tables too, and as an UPDATE ...
// FROM: the join condition it inherits from the DELETE's WHERE names
// the USING relation, so an UPDATE without the FROM clause is 42P01,
// and one whose FROM clause is unscoped is the cross-tenant write this
// round is closing.
func TestDeleteUsingSoftDeleteRewriteCarriesUsingTables(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", nil)
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	checkCtx(t, wctx(),
		db.Delete(accounts).
			Using(posts).
			Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))),
		`UPDATE "accounts" SET "deletedAt" = now() FROM "posts" WHERE `+
			`("accounts"."deletedAt" IS NULL) AND `+
			`("posts"."deletedAt" IS NULL) AND `+
			`("accounts"."id" = "posts"."accountId") AND `+
			`("accounts"."tenantId" = $1) AND `+
			`("posts"."tenantId" = $2)`,
		wtenant, wtenant,
	)
}

// The fail-closed case, for the joined table. A DELETE that cannot say
// which tenant's posts it is joining must not run.
func TestDeleteUsingRefusesWithoutTenant(t *testing.T) {
	db := pg.New(nil)
	accounts := pg.NewTable("accounts")
	pg.Add(accounts, pg.BigSerial("id").PrimaryKey())
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	_, _, err := db.Delete(accounts).
		Using(posts).
		Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))).
		ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("got = %v, want %v", err, pg.ErrTenantMissing)
	}
}

// Unscoped is the widest opt-out drops has and it is statement-wide.
// Leaving a USING table's tenant axis in place would make an
// administrative DELETE remove a silently narrowed set of rows — worse
// than either honest answer.
func TestDeleteUnscopedClearsUsingTableScope(t *testing.T) {
	db := pg.New(nil)
	accounts := whard("accounts", nil)
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	checkCtx(t, context.Background(),
		db.Delete(accounts).
			Using(posts).
			Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))).
			Unscoped(),
		`DELETE FROM "accounts" USING "posts" WHERE `+
			`("accounts"."id" = "posts"."accountId")`,
	)
}

// Two USING tables are two relations, each restricted on its own terms.
func TestDeleteUsingMultipleTablesEachCarryScope(t *testing.T) {
	db := pg.New(nil)
	accounts := whard("accounts", nil)
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	tags := whard("tags", func(t *pg.Table) { pg.Add(t, pg.BigInt("postId")) })

	checkCtx(t, wctx(),
		db.Delete(accounts).
			Using(posts, tags).
			Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))),
		`DELETE FROM "accounts" USING "posts", "tags" WHERE `+
			`("accounts"."archivedAt" IS NULL) AND `+
			`("posts"."archivedAt" IS NULL) AND `+
			`("tags"."archivedAt" IS NULL) AND `+
			`("accounts"."id" = "posts"."accountId") AND `+
			`("accounts"."tenantId" = $1) AND `+
			`("posts"."tenantId" = $2) AND `+
			`("tags"."tenantId" = $3)`,
		wtenant, wtenant, wtenant,
	)
}

// Rendering the same builder twice must send the same statement twice —
// the copy-on-resolve property, asserted for the USING tables.
func TestDeleteUsingResolveIsRepeatable(t *testing.T) {
	db := pg.New(nil)
	accounts := whard("accounts", nil)
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	q := db.Delete(accounts).Using(posts)
	first, _, err := q.ToSQLCtx(wctx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	second, _, err := q.ToSQLCtx(wctx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if first != second {
		t.Errorf("got = %v, want %v", second, first)
	}
}

// ToSQL carries every default filter the statement's relations declare,
// USING tables included — those need no ctx.
func TestDeleteToSQLCarriesUsingTableDefaultFilters(t *testing.T) {
	db := pg.New(nil)
	accounts := pg.NewTable("accounts")
	pg.Add(accounts, pg.BigSerial("id").PrimaryKey())
	accArch := pg.Add(accounts, pg.Timestamp("archivedAt", true))
	accounts.DefaultFilter(pg.IsNull(accArch))
	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigInt("accountId"))
	postArch := pg.Add(posts, pg.Timestamp("archivedAt", true))
	posts.DefaultFilter(pg.IsNull(postArch))

	check(t, db.Delete(accounts).Using(posts),
		`DELETE FROM "accounts" USING "posts" WHERE `+
			`("accounts"."archivedAt" IS NULL) AND ("posts"."archivedAt" IS NULL)`,
	)
}

// ----------------------------------------------------------------------
// A statement written inside the DELETE is a statement and is scoped
// like one
// ----------------------------------------------------------------------

// DeleteBuilder.resolveCtx used to resolve the context filters of the
// target table and of the USING tables and stop there, so a statement
// the caller wrote inside the DELETE was reached only through WriteSQL,
// which has no ctx:
//
//	DELETE FROM "users"
//	WHERE "users"."id" NOT IN (SELECT "posts"."authorId" FROM "posts")
//	AND ("users"."tenantId" = $1)
//
// — every tenant's posts deciding which of this tenant's rows survive,
// out of a statement that carries a tenant predicate of its own. A
// DELETE is the write nothing walks back, and the sub-SELECT is the
// idiom for "delete the rows nothing references any more", so this is
// the shape most likely to be written and least likely to be noticed.
//
// The two renderings are asserted separately — the DELETE itself and
// the UPDATE a soft-delete DeleteHook rewrites it into — for the reason
// TestDeleteUsingSoftDeleteRewriteCarriesUsingTables gives: the rewrite
// is built from d.Wheres() and d.ReturningClauses(), so it inherits
// whatever the resolution did or did not put there, and round 3 showed
// that is exactly where a fix gets left behind.
func TestDeleteSubqueryOperandsCarryTheTenantAxis(t *testing.T) {
	db := pg.New(nil)
	accounts := whard("accounts", nil)
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("accountId")).From(posts) }
	subSQL := func(n string) string {
		return `SELECT "posts"."accountId" FROM "posts" ` +
			`WHERE ("posts"."archivedAt" IS NULL) AND ("posts"."tenantId" = ` + n + `)`
	}

	tests := []struct {
		name string
		del  func() *pg.DeleteBuilder
		want string
		args []any
	}{
		{
			name: "NOT IN a subquery in the WHERE clause",
			del: func() *pg.DeleteBuilder {
				return db.Delete(accounts).Where(pg.NotIn(accounts.Col("id"), sub()))
			},
			want: `DELETE FROM "accounts" WHERE ` +
				`("accounts"."archivedAt" IS NULL) AND ` +
				`("accounts"."id" NOT IN (` + subSQL(`$1`) + `)) AND ` +
				`("accounts"."tenantId" = $2)`,
			args: []any{wtenant, wtenant},
		},
		{
			name: "EXISTS in the WHERE clause",
			del: func() *pg.DeleteBuilder {
				return db.Delete(accounts).Where(pg.Exists(sub()))
			},
			want: `DELETE FROM "accounts" WHERE ` +
				`("accounts"."archivedAt" IS NULL) AND ` +
				`EXISTS (` + subSQL(`$1`) + `) AND ` +
				`("accounts"."tenantId" = $2)`,
			args: []any{wtenant, wtenant},
		},
		{
			// Three levels down, through combinators nobody named.
			name: "AND over NOT over EXISTS in the WHERE clause",
			del: func() *pg.DeleteBuilder {
				return db.Delete(accounts).Where(
					pg.And(pg.Not(pg.Exists(sub())), pg.Eq(accounts.Col("id"), int64(5))))
			},
			want: `DELETE FROM "accounts" WHERE ` +
				`("accounts"."archivedAt" IS NULL) AND ` +
				`((NOT EXISTS (` + subSQL(`$1`) + `)) AND ("accounts"."id" = $2)) AND ` +
				`("accounts"."tenantId" = $3)`,
			args: []any{wtenant, int64(5), wtenant},
		},
		{
			name: "a scalar subquery in the RETURNING clause",
			del: func() *pg.DeleteBuilder {
				return db.Delete(accounts).Returning(accounts.Col("id"), pg.Subquery(sub()))
			},
			want: `DELETE FROM "accounts" WHERE ` +
				`("accounts"."archivedAt" IS NULL) AND ("accounts"."tenantId" = $1) ` +
				`RETURNING "accounts"."id", (` + subSQL(`$2`) + `)`,
			args: []any{wtenant, wtenant},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkCtx(t, wctx(), tc.del(), tc.want, tc.args...)
		})
	}
}

// The soft-delete rewrite carries the resolution too.
//
// The hook builds its UPDATE out of d.Wheres() and d.ReturningClauses()
// while the DELETE renders, which is after resolveCtx has run — so the
// predicates it copies are the resolved ones, and a subquery inside one
// carries its own tenant axis into the statement that actually mutates
// rows. Resolving at render time instead of into the copy's WHERE list
// would leave this path unscoped while the plain DELETE looked fixed.
func TestDeleteSoftDeleteRewriteCarriesSubqueryScope(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", nil)
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("accountId")).From(posts) }
	subSQL := func(n string) string {
		return `SELECT "posts"."accountId" FROM "posts" ` +
			`WHERE ("posts"."deletedAt" IS NULL) AND ("posts"."tenantId" = ` + n + `)`
	}

	checkCtx(t, wctx(),
		db.Delete(accounts).
			Where(pg.NotIn(accounts.Col("id"), sub())).
			Returning(pg.Subquery(sub())),
		`UPDATE "accounts" SET "deletedAt" = now() WHERE `+
			`("accounts"."deletedAt" IS NULL) AND `+
			`("accounts"."id" NOT IN (`+subSQL(`$1`)+`)) AND `+
			`("accounts"."tenantId" = $2) `+
			`RETURNING (`+subSQL(`$3`)+`)`,
		wtenant, wtenant, wtenant,
	)
}

// A subquery that cannot be resolved aborts the whole DELETE — both
// renderings of it. Refusing is the only safe answer: the alternative
// is a delete whose row set was chosen by a read that saw every tenant.
func TestDeleteSubqueryRefusesWithoutTenant(t *testing.T) {
	db := pg.New(nil)
	// Unscoped targets, so only the inner statement can refuse and
	// neither case can pass by accident.
	hard := pg.NewTable("accounts")
	pg.Add(hard, pg.BigSerial("id").PrimaryKey())
	soft := pg.NewTable("softAccounts")
	pg.Add(soft, pg.BigSerial("id").PrimaryKey())
	pg.ApplyMixins(soft, &pg.SoftDeleteMixin{})
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("accountId")).From(posts) }

	tests := []struct {
		name string
		del  func() *pg.DeleteBuilder
	}{
		{
			name: "WHERE",
			del: func() *pg.DeleteBuilder {
				return db.Delete(hard).Where(pg.NotIn(hard.Col("id"), sub()))
			},
		},
		{
			name: "RETURNING",
			del: func() *pg.DeleteBuilder {
				return db.Delete(hard).Returning(pg.Subquery(sub()))
			},
		},
		{
			// The rewrite must not be the way round the refusal.
			name: "soft-delete rewrite",
			del: func() *pg.DeleteBuilder {
				return db.Delete(soft).Where(pg.NotIn(soft.Col("id"), sub()))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := tc.del().ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Errorf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want none", sql)
			}
		})
	}
}

// Unscoped opts the DELETE out of its own scoping — its DeleteHooks
// included, so this renders as a hard DELETE — and not out of the
// scoping of the statement written inside it.
func TestDeleteUnscopedKeepsSubqueryScope(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", nil)
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	checkCtx(t, wctx(),
		db.Delete(accounts).
			Where(pg.NotIn(accounts.Col("id"), db.Select(posts.Col("accountId")).From(posts))).
			Unscoped(),
		`DELETE FROM "accounts" WHERE ("accounts"."id" NOT IN (`+
			`SELECT "posts"."accountId" FROM "posts" `+
			`WHERE ("posts"."deletedAt" IS NULL) AND ("posts"."tenantId" = $1)))`,
		wtenant,
	)
}

// Rendering the same builder twice must send the same statement twice,
// subqueries included — asserted on both renderings, since the rewrite
// builds a fresh UpdateBuilder out of the DELETE's predicates every
// time it runs and a resolution written back into the DELETE would
// compound there first.
func TestDeleteSubqueryResolveIsRepeatable(t *testing.T) {
	db := pg.New(nil)
	posts := whard("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("accountId")).From(posts) }

	for _, tc := range []struct {
		name  string
		table *pg.Table
	}{
		{name: "DELETE", table: whard("accounts", nil)},
		{name: "soft-delete rewrite", table: wscoped("accounts", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := db.Delete(tc.table).
				Where(pg.NotIn(tc.table.Col("id"), sub())).
				Returning(pg.Subquery(sub()))

			firstSQL, firstArgs, err := q.ToSQLCtx(wctx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			secondSQL, secondArgs, err := q.ToSQLCtx(wctx())
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if firstSQL != secondSQL {
				t.Errorf("sql mismatch\n  got  = %v\n  want = %v", secondSQL, firstSQL)
			}
			if len(firstArgs) != len(secondArgs) {
				t.Fatalf("args mismatch\n  got  = %v\n  want = %v", secondArgs, firstArgs)
			}

			// A second request, with a second tenant: every bound value
			// is that tenant's, inner statements included.
			otherSQL, otherArgs, err := q.ToSQLCtx(pg.WithTenant(context.Background(), int64(9)))
			if err != nil {
				t.Fatalf("ToSQLCtx: %v", err)
			}
			if otherSQL != firstSQL {
				t.Errorf("sql mismatch\n  got  = %v\n  want = %v", otherSQL, firstSQL)
			}
			for i, a := range otherArgs {
				if a != any(int64(9)) {
					t.Errorf("args[%d] = %v, want %v (all of %v)", i, a, int64(9), otherArgs)
				}
			}
		})
	}
}
