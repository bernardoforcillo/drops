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
