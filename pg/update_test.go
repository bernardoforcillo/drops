package pg_test

import (
	"context"
	"errors"
	"strings"
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

// ----------------------------------------------------------------------
// A statement written inside the UPDATE is a statement and is scoped
// like one
// ----------------------------------------------------------------------

// UpdateBuilder.resolveCtx used to resolve the context filters of the
// tables the statement names — the target and the FROM tables — and
// stop there. Every other statement the caller wrote inside it was
// reached only through WriteSQL, which has no ctx, so the body rendered
// its DefaultFilters and none of its ContextFilters:
//
//	UPDATE "users" SET "role" = $1
//	WHERE EXISTS (SELECT "posts"."id" FROM "posts") AND ("users"."tenantId" = $2)
//
// — a statement that looks scoped, carries its own tenant predicate,
// and lets every tenant's posts decide which of this tenant's rows are
// written. On an UPDATE a cross-tenant read is not a widened result
// set, it is the input to a write.
//
// The cases below are not a list of shapes to support. They are one
// probe per clause an operand can live in, because the resolution is a
// walk over the operand tree (see resolveExpr): the shapes nobody
// reported — an operand three levels down, an array operator, a JSON
// operator — come out of the same recursion, which is what
// scopetree_test.go asserts for the SELECT builder.
func TestUpdateSubqueryOperandsCarryTheTenantAxis(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) {
		pg.Add(t, pg.Text("name"))
		pg.Add(t, pg.BigInt("ownerId"))
		pg.Add(t, pg.Boolean("active"))
	})
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	name := wbind[string](accounts.Col("name"))
	owner := wbind[int64](accounts.Col("ownerId"))
	active := wbind[bool](accounts.Col("active"))

	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }
	// The inner statement with its own tenant predicate. The placeholder
	// number depends on where in the outer statement it renders, so it is
	// built per case.
	subSQL := func(n string) string {
		return `SELECT "posts"."id" FROM "posts" ` +
			`WHERE ("posts"."deletedAt" IS NULL) AND ("posts"."tenantId" = ` + n + `)`
	}

	tests := []struct {
		name string
		upd  func() *pg.UpdateBuilder
		want string
		args []any
	}{
		{
			name: "EXISTS in the WHERE clause",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).Where(pg.Exists(sub()))
			},
			want: `UPDATE "accounts" SET "name" = $1 WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ` +
				`EXISTS (` + subSQL(`$2`) + `) AND ` +
				`("accounts"."tenantId" = $3)`,
			args: []any{"x", wtenant, wtenant},
		},
		{
			name: "IN a subquery in the WHERE clause",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).
					Where(pg.In(accounts.Col("id"), sub()))
			},
			want: `UPDATE "accounts" SET "name" = $1 WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ` +
				`("accounts"."id" IN (` + subSQL(`$2`) + `)) AND ` +
				`("accounts"."tenantId" = $3)`,
			args: []any{"x", wtenant, wtenant},
		},
		{
			// Three levels down, through combinators nobody named. The
			// walk does not know how deep it is.
			name: "NOT over OR over EXISTS in the WHERE clause",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).
					Where(pg.Not(pg.Or(pg.Exists(sub()), pg.Eq(accounts.Col("id"), int64(5)))))
			},
			want: `UPDATE "accounts" SET "name" = $1 WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ` +
				`(NOT (EXISTS (` + subSQL(`$2`) + `) OR ("accounts"."id" = $3))) AND ` +
				`("accounts"."tenantId" = $4)`,
			args: []any{"x", wtenant, int64(5), wtenant},
		},
		{
			// The assigned value is an operand position like any other,
			// and the one that decides what gets written rather than
			// which rows do.
			name: "a scalar subquery as the assigned value",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(owner.Expr(pg.Subquery(sub())))
			},
			want: `UPDATE "accounts" SET "ownerId" = (` + subSQL(`$1`) + `) WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ("accounts"."tenantId" = $2)`,
			args: []any{wtenant, wtenant},
		},
		{
			// An assigned value nested two levels under operators, to
			// show the SET list is walked rather than pattern-matched
			// for a *SelectBuilder sitting directly in it.
			name: "a subquery under operators as the assigned value",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(active.Expr(pg.Not(pg.Exists(sub()))))
			},
			want: `UPDATE "accounts" SET "active" = (NOT EXISTS (` + subSQL(`$1`) + `)) WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ("accounts"."tenantId" = $2)`,
			args: []any{wtenant, wtenant},
		},
		{
			// Two shapes nobody reported, to show the walk is not a list:
			// an array operator's right-hand side and a BETWEEN bound
			// are operand positions of the same node type, so they cost
			// nothing to cover and would each have needed a case of
			// their own under a design that matched on shapes.
			name: "a subquery as an array operator's operand",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).
					Where(pg.ArrayOverlaps(accounts.Col("id"), pg.Subquery(sub())))
			},
			want: `UPDATE "accounts" SET "name" = $1 WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ` +
				`("accounts"."id" && (` + subSQL(`$2`) + `)) AND ` +
				`("accounts"."tenantId" = $3)`,
			args: []any{"x", wtenant, wtenant},
		},
		{
			name: "a subquery as a BETWEEN bound",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).
					Where(pg.Between(accounts.Col("id"), pg.Subquery(sub()), int64(99)))
			},
			want: `UPDATE "accounts" SET "name" = $1 WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ` +
				`("accounts"."id" BETWEEN (` + subSQL(`$2`) + `) AND $3) AND ` +
				`("accounts"."tenantId" = $4)`,
			args: []any{"x", wtenant, int64(99), wtenant},
		},
		{
			// RETURNING is read back to the caller, so a subquery in it
			// is a cross-tenant read out of a write statement.
			name: "a scalar subquery in the RETURNING clause",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).
					Returning(accounts.Col("id"), pg.Subquery(sub()))
			},
			want: `UPDATE "accounts" SET "name" = $1 WHERE ` +
				`("accounts"."deletedAt" IS NULL) AND ("accounts"."tenantId" = $2) ` +
				`RETURNING "accounts"."id", (` + subSQL(`$3`) + `)`,
			args: []any{"x", wtenant, wtenant},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkCtx(t, wctx(), tc.upd(), tc.want, tc.args...)
		})
	}
}

// A subquery that cannot be resolved aborts the whole UPDATE, in every
// clause it can be written in.
//
// Refusing is the only safe answer: the alternative is a write whose
// row set was chosen by a read that saw every tenant. It matches what
// the target table already did — TestUpdateFromRefusesWithoutTenant —
// and what a bare SELECT through the same subquery does.
func TestUpdateSubqueryRefusesWithoutTenant(t *testing.T) {
	db := pg.New(nil)
	// The target table is unscoped, so only the inner statement can
	// refuse and the test cannot pass by accident.
	accounts := pg.NewTable("accounts")
	pg.Add(accounts, pg.BigSerial("id").PrimaryKey())
	name := pg.Add(accounts, pg.Text("name"))
	owner := pg.Add(accounts, pg.BigInt("ownerId"))
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }

	tests := []struct {
		name string
		upd  func() *pg.UpdateBuilder
	}{
		{
			name: "WHERE",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).Where(pg.Exists(sub()))
			},
		},
		{
			name: "SET",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).
					Set(wbind[int64](owner.Column).Expr(pg.Subquery(sub())))
			},
		},
		{
			name: "RETURNING",
			upd: func() *pg.UpdateBuilder {
				return db.Update(accounts).Set(name.Val("x")).
					Returning(pg.Subquery(sub()))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sql, _, err := tc.upd().ToSQLCtx(context.Background())
			if !errors.Is(err, pg.ErrTenantMissing) {
				t.Errorf("got = %v (sql %q), want %v", err, sql, pg.ErrTenantMissing)
			}
			if sql != "" {
				t.Errorf("got sql = %q, want none", sql)
			}
		})
	}
}

// Unscoped opts the statement out of its own scoping, not out of the
// scoping of the statements written inside it: a subquery is a
// statement of its own and keeps its own tenant axis, which is also how
// to widen one relation of a write and no other. [SelectBuilder]
// behaves the same way, and a write must not be looser than a read.
func TestUpdateUnscopedKeepsSubqueryScope(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) { pg.Add(t, pg.Text("name")) })
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	name := wbind[string](accounts.Col("name"))

	checkCtx(t, wctx(),
		db.Update(accounts).Set(name.Val("x")).
			Where(pg.Exists(db.Select(posts.Col("id")).From(posts))).
			Unscoped(),
		`UPDATE "accounts" SET "name" = $1 WHERE EXISTS (`+
			`SELECT "posts"."id" FROM "posts" `+
			`WHERE ("posts"."deletedAt" IS NULL) AND ("posts"."tenantId" = $2))`,
		"x", wtenant,
	)
}

// Rendering the same builder twice must send the same statement twice,
// subqueries included. Writing a resolved body back into the builder's
// own WHERE or SET list would pin the first request's tenant into every
// later execution — the same value for a second request's ctx, so the
// rows come back plausible and nothing fails.
func TestUpdateSubqueryResolveIsRepeatable(t *testing.T) {
	db := pg.New(nil)
	accounts := wscoped("accounts", func(t *pg.Table) {
		pg.Add(t, pg.Text("name"))
		pg.Add(t, pg.BigInt("ownerId"))
	})
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	owner := wbind[int64](accounts.Col("ownerId"))
	sub := func() *pg.SelectBuilder { return db.Select(posts.Col("id")).From(posts) }

	q := db.Update(accounts).
		Set(owner.Expr(pg.Subquery(sub()))).
		Where(pg.Exists(sub())).
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

	// A second request, with a second tenant: the builder must resolve
	// against the ctx it is executed with, not the one it first saw.
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
}

// An UpdateHook assigns through the same SET list, and is the operand
// position no call site shows.
//
// UpdateHookCtx.SetExpr takes any expression and the hook is registered
// on the table, so it reaches every UPDATE written against it — while
// the hooks ran inside WriteSQL, whatever they assigned was reachable
// only through a renderer that has no ctx, so a hook computing a value
// from a scoped table read every tenant's rows on every write. Running
// them during resolution instead puts what they add in front of the
// same walk as the caller's own assignments.
func TestUpdateHookAssignedSubqueryCarriesTheTenantAxis(t *testing.T) {
	db := pg.New(nil)
	posts := wscoped("posts", func(t *pg.Table) { pg.Add(t, pg.BigInt("accountId")) })
	accounts := wscoped("accounts", func(t *pg.Table) {
		pg.Add(t, pg.Text("name"))
		pg.Add(t, pg.BigInt("ownerId"))
	})
	owner := accounts.Col("ownerId")
	accounts.OnUpdate(pg.UpdateHookFunc(func(hc *pg.UpdateHookCtx) {
		hc.SetExpr(owner, pg.Subquery(db.Select(posts.Col("id")).From(posts)))
	}))
	name := wbind[string](accounts.Col("name"))

	checkCtx(t, wctx(),
		db.Update(accounts).Set(name.Val("x")),
		`UPDATE "accounts" SET "name" = $1, "ownerId" = (`+
			`SELECT "posts"."id" FROM "posts" `+
			`WHERE ("posts"."deletedAt" IS NULL) AND ("posts"."tenantId" = $2)) WHERE `+
			`("accounts"."deletedAt" IS NULL) AND ("accounts"."tenantId" = $3)`,
		"x", wtenant, wtenant,
	)

	// The hooks contribute exactly once. Running them during resolution
	// and again while the resolved copy renders would assign the column
	// twice, which PostgreSQL answers with 42601.
	first, _, err := db.Update(accounts).Set(name.Val("x")).ToSQLCtx(wctx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got := strings.Count(first, `"ownerId" = `); got != 1 {
		t.Errorf("got %d assignments of \"ownerId\", want 1: %v", got, first)
	}
}
