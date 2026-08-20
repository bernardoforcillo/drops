package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops/pg"
)

// The promise in tenant.go is that "every statement that reads or
// writes it takes the tenant from ctx and carries the predicate". The
// assertions below are on rendered SQL rather than on rows come back,
// because both failures they cover are invisible to a round trip: an
// UPDATE whose FROM table is unfiltered still returns the right rows
// for a fixture holding one tenant, and an aliased UPDATE whose
// predicate names the un-aliased relation only fails against a real
// server, with 42P01.
//
// UPDATE and DELETE are the two shapes where the consequence of a
// missing predicate is a write rather than a read, which is why they
// have a file of their own.

// ----------------------------------------------------------------------
// Fixture and helpers
// ----------------------------------------------------------------------

// wscoped builds a table carrying both kinds of automatic predicate a
// write statement has to honour: a DefaultFilter (the soft-delete
// guard, resolved when the statement renders) and a ContextFilter (the
// tenant axis, resolved from a ctx by the executor). A table with only
// one of them cannot tell the two code paths apart.
func wscoped(name string, cols func(*pg.Table)) *pg.Table {
	t := pg.NewTable(name)
	pg.Add(t, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(t, pg.BigInt("tenantId").NotNull())
	if cols != nil {
		cols(t)
	}
	pg.ApplyMixins(t, &pg.SoftDeleteMixin{})
	t.ContextFilter(pg.TenantFilter(tenant))
	return t
}

// wbind re-wraps a type-erased column handle as a typed one, the way a
// caller outside the package reaches a value binding for a column it
// got from Table.Col.
func wbind[T any](c *pg.Column) *pg.Col[T] { return &pg.Col[T]{Column: c} }

// wtenant is the tenant every test in this file and delete_test.go puts
// on its ctx.
const wtenant = int64(7)

func wctx() context.Context { return pg.WithTenant(context.Background(), wtenant) }

type ctxSQLable interface {
	ToSQLCtx(ctx context.Context) (string, []any, error)
}

// checkCtx asserts on the statement a given ctx would send — the whole
// statement, context filters included. Asserting on ToSQL instead would
// pass with the tenant axis missing, since ToSQL is documented not to
// carry it.
func checkCtx(t *testing.T, ctx context.Context, q ctxSQLable, wantSQL string, wantArgs ...any) {
	t.Helper()
	gotSQL, gotArgs, err := q.ToSQLCtx(ctx)
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if gotSQL != wantSQL {
		t.Errorf("sql mismatch\n  got  = %v\n  want = %v", gotSQL, wantSQL)
	}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("args mismatch\n  got  = %v\n  want = %v", gotArgs, wantArgs)
	}
	for i := range wantArgs {
		if gotArgs[i] != wantArgs[i] {
			t.Errorf("args mismatch\n  got  = %v\n  want = %v", gotArgs, wantArgs)
			return
		}
	}
}

// ----------------------------------------------------------------------
// OPEN-1 — the target table under an alias
// ----------------------------------------------------------------------

// An aliased UPDATE has one FROM entry and it is the alias. A default
// filter that renders "notes"."deletedAt" against UPDATE "notes" AS "n"
// names a relation the statement never introduces, and PostgreSQL
// answers 42P01 — the statement cannot run at all.
func TestUpdateAliasQualifiesAutomaticFilters(t *testing.T) {
	db := pg.New(nil)
	notes := wscoped("notes", func(t *pg.Table) { pg.Add(t, pg.Text("body")) })
	n := notes.As("n")
	body := wbind[string](n.Col("body"))

	checkCtx(t, wctx(),
		db.Update(n).Set(body.Val("hi")),
		`UPDATE "notes" AS "n" SET "body" = $1 `+
			`WHERE ("n"."deletedAt" IS NULL) AND ("n"."tenantId" = $2)`,
		"hi", wtenant,
	)
}

// ----------------------------------------------------------------------
// OPEN-2 — the FROM tables
// ----------------------------------------------------------------------

// UPDATE ... FROM puts its join condition in the WHERE clause, so an
// unfiltered FROM table does not merely widen a result — it lets
// another tenant's rows decide which of yours get written.
func TestUpdateFromCarriesJoinedTableScope(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) { pg.Add(t, pg.Text("name")) })
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	name := wbind[string](accounts.Col("name"))

	checkCtx(t, wctx(),
		db.Update(accounts).
			Set(name.Val("x")).
			From(posts).
			Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))),
		`UPDATE "accounts" SET "name" = $1 FROM "posts" WHERE `+
			`("accounts"."deletedAt" IS NULL) AND `+
			`("posts"."deletedAt" IS NULL) AND `+
			`("accounts"."id" = "posts"."accountId") AND `+
			`("accounts"."tenantId" = $2) AND `+
			`("posts"."tenantId" = $3)`,
		"x", wtenant, wtenant,
	)
}

// A FROM table reached under an alias needs the same rename the target
// table needs, and for the same reason.
func TestUpdateFromAliasQualifiesJoinedTableScope(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) { pg.Add(t, pg.Text("name")) })
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	p := posts.As("p")
	name := wbind[string](accounts.Col("name"))

	checkCtx(t, wctx(),
		db.Update(accounts).
			Set(name.Val("x")).
			From(p).
			Where(pg.Eq(accounts.Col("id"), p.Col("accountId"))),
		`UPDATE "accounts" SET "name" = $1 FROM "posts" AS "p" WHERE `+
			`("accounts"."deletedAt" IS NULL) AND `+
			`("p"."deletedAt" IS NULL) AND `+
			`("accounts"."id" = "p"."accountId") AND `+
			`("accounts"."tenantId" = $2) AND `+
			`("p"."tenantId" = $3)`,
		"x", wtenant, wtenant,
	)
}

// A FROM table that is tenant-scoped and a ctx with no tenant is the
// fail-closed case, and it has to fail closed for the joined table too:
// the alternative is an UPDATE that writes rows chosen by every
// tenant's posts.
func TestUpdateFromRefusesWithoutTenant(t *testing.T) {
	db := pg.New(nil)
	accounts := pg.NewTable("accounts")
	pg.Add(accounts, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(accounts, pg.Text("name"))
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })

	_, _, err := db.Update(accounts).
		Set(name.Val("x")).
		From(posts).
		Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))).
		ToSQLCtx(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Errorf("got = %v, want %v", err, pg.ErrTenantMissing)
	}
}

// Unscoped is statement-wide. A flag that dropped the target table's
// scoping while a FROM table kept its tenant axis would answer an
// administrative UPDATE by writing a silently narrowed set of rows.
func TestUpdateUnscopedClearsFromTableScope(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) { pg.Add(t, pg.Text("name")) })
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	name := wbind[string](accounts.Col("name"))

	// No tenant on ctx either: an unscoped statement must not consult
	// the FROM table's context filters at all, so it must not refuse.
	checkCtx(t, context.Background(),
		db.Update(accounts).
			Set(name.Val("x")).
			From(posts).
			Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))).
			Unscoped(),
		`UPDATE "accounts" SET "name" = $1 FROM "posts" WHERE `+
			`("accounts"."id" = "posts"."accountId")`,
		"x",
	)
}

// Two FROM tables are two relations, each restricted on its own terms.
func TestUpdateFromMultipleTablesEachCarryScope(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) { pg.Add(t, pg.Text("name")) })
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	tags := wscoped("tags", func(t *pg.Table) { pg.Add(t, pg.BigInt("postId")) })
	name := wbind[string](accounts.Col("name"))

	checkCtx(t, wctx(),
		db.Update(accounts).
			Set(name.Val("x")).
			From(posts, tags).
			Where(pg.Eq(accounts.Col("id"), posts.Col("accountId"))),
		`UPDATE "accounts" SET "name" = $1 FROM "posts", "tags" WHERE `+
			`("accounts"."deletedAt" IS NULL) AND `+
			`("posts"."deletedAt" IS NULL) AND `+
			`("tags"."deletedAt" IS NULL) AND `+
			`("accounts"."id" = "posts"."accountId") AND `+
			`("accounts"."tenantId" = $2) AND `+
			`("posts"."tenantId" = $3) AND `+
			`("tags"."tenantId" = $4)`,
		"x", wtenant, wtenant, wtenant,
	)
}

// Rendering the same builder twice must send the same statement twice.
// Resolving into u.wheres rather than into a copy would give the second
// execution two tenant predicates and the third three — with the same
// value bound each time, so the rows still come back right and nothing
// fails until an argument limit does.
func TestUpdateFromResolveIsRepeatable(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) { pg.Add(t, pg.Text("name")) })
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	name := wbind[string](accounts.Col("name"))

	q := db.Update(accounts).Set(name.Val("x")).From(posts)
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

// ToSQL is documented to render what the builder knows without a ctx.
// It still has to carry the default filters of every relation the
// statement names, including the FROM tables — those need no ctx.
func TestUpdateToSQLCarriesFromTableDefaultFilters(t *testing.T) {
	db := pg.New(nil)
	accounts := pg.NewTable("accounts")
	pg.Add(accounts, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(accounts, pg.Text("name"))
	pg.ApplyMixins(accounts, &pg.SoftDeleteMixin{})
	posts := pg.NewTable("posts")
	pg.Add(posts, pg.BigInt("accountId"))
	pg.ApplyMixins(posts, &pg.SoftDeleteMixin{})

	check(t, db.Update(accounts).Set(name.Val("x")).From(posts),
		`UPDATE "accounts" SET "name" = $1 FROM "posts" WHERE `+
			`("accounts"."deletedAt" IS NULL) AND ("posts"."deletedAt" IS NULL)`,
		"x",
	)
}
