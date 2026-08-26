package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/bernardoforcillo/drops"
	"github.com/bernardoforcillo/drops/dropstest"
	"github.com/bernardoforcillo/drops/pg"
)

// The limits the package doc states, executed.
//
// doc.go now has a "Where the automatic scoping stops" section, and a
// documented limit that is wrong is worse than no documentation at all:
// a reviewer trusts it, and every one of these paragraphs is read by
// somebody deciding whether a query needs a predicate written by hand.
// So each claim is pinned here against rendered SQL and args. The
// contract is two-way — a limit that stops being true fails this file,
// which is the notice that the paragraph describing it has to go.
//
// Some of the section's claims were already pinned when it was written
// and are not restated here: view bodies by TestViewBodiesAreTheCallersToScope,
// the FULL JOIN refusal by TestFullJoinOnScopedTableIsRefused and
// TestFullJoinWithScopedFromTableIsRefused, and Unscoped's statement
// locality by TestUnscopedStatementBodyKeepsItsOwnScoping,
// TestUnscopedReachesJoinedTables and TestSetOpRightOperandKeepsItsOwnStatement.

// The tables here are reachTable's: both kinds of automatic predicate,
// a render-time soft-delete DefaultFilter and a tenant ContextFilter
// resolved by the executor.

// A drops.Raw body has nothing inside it to walk, and a drops.ExprFunc
// is a closure the renderer calls with no ctx. Both are the caller's to
// scope, and the failure mode a reader has to recognise is that neither
// says so at run time: the statement renders, is sent, and returns rows
// from every tenant.
func TestRawAndClosureBodiesAreTheCallersToScope(t *testing.T) {
	posts := reachTable("lim_raw_posts")
	other := reachTable("lim_raw_other")
	db := pg.New(nil)

	// A CTE body written as a raw fragment: rendered as given, on a ctx
	// carrying no tenant at all, without a refusal.
	rawCTE := func() *pg.SelectBuilder {
		return db.Select().
			With(pg.CTEDef("moved", drops.Raw(`SELECT * FROM "lim_raw_posts"`))).
			FromExpr(drops.Raw(`"moved"`))
	}
	wantRaw := `WITH "moved" AS (SELECT * FROM "lim_raw_posts") SELECT * FROM "moved"`
	got, args, err := rawCTE().ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != wantRaw {
		t.Errorf("got = %v, want %v", got, wantRaw)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}

	// A builder captured inside a closure: its DefaultFilters render,
	// because rendering is what happens to it, and its tenant predicate
	// does not — while the enclosing statement carries its own.
	inner := db.Select(posts.Col("id")).From(posts)
	closure := drops.ExprFunc(func(b *drops.Builder) {
		b.WriteString("EXISTS (")
		b.Append(inner)
		b.WriteByte(')')
	})
	wantClosure := `SELECT * FROM "lim_raw_other" ` +
		`WHERE ("lim_raw_other"."deletedAt" IS NULL) ` +
		`AND EXISTS (SELECT "lim_raw_posts"."id" FROM "lim_raw_posts" ` +
		`WHERE ("lim_raw_posts"."deletedAt" IS NULL)) ` +
		`AND ("lim_raw_other"."tenantId" = $1)`
	got, args, err = db.Select().From(other).Where(closure).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != wantClosure {
		t.Errorf("got = %v, want %v", got, wantClosure)
	}
	if !sameArgs(args, []any{reachTenant}) {
		t.Errorf("args = %v, want %v", args, []any{reachTenant})
	}
}

// A statement embedded in a DefaultFilter is walked for the executing
// ctx; the ToSQL path has no ctx and renders the unresolved list. Both
// halves are the claim: the first is what the previous round bought,
// the second is why ToSQL output must not be read as what a request
// sends.
func TestDefaultFilterStatementIsWalkedOnlyWhereThereIsACtx(t *testing.T) {
	blocked := reachTable("lim_blocked")
	notes := pg.NewTable("lim_notes")
	pg.Add(notes, pg.BigSerial("id").PrimaryKey())
	author := pg.Add(notes, pg.BigInt("authorId").NotNull())
	db := pg.New(nil)
	notes.DefaultFilter(pg.NotIn(author.Column,
		pg.Subquery(db.Select(blocked.Col("id")).From(blocked))))

	wantCtx := `SELECT * FROM "lim_notes" WHERE ("lim_notes"."authorId" NOT IN (` +
		`(SELECT "lim_blocked"."id" FROM "lim_blocked" ` +
		`WHERE ("lim_blocked"."deletedAt" IS NULL) ` +
		`AND ("lim_blocked"."tenantId" = $1))))`
	got, args, err := db.Select().From(notes).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != wantCtx {
		t.Errorf("got = %v, want %v", got, wantCtx)
	}
	if !sameArgs(args, []any{reachTenant}) {
		t.Errorf("args = %v, want %v", args, []any{reachTenant})
	}

	wantPlain := `SELECT * FROM "lim_notes" WHERE ("lim_notes"."authorId" NOT IN (` +
		`(SELECT "lim_blocked"."id" FROM "lim_blocked" ` +
		`WHERE ("lim_blocked"."deletedAt" IS NULL))))`
	got, args = db.Select().From(notes).ToSQL()
	if got != wantPlain {
		t.Errorf("got = %v, want %v", got, wantPlain)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

// ToSQL renders an incomplete statement for a table carrying context
// filters: the tenant predicate is absent because there is no ctx to
// build it from. It is for inspection; ToSQLCtx is what a request would
// send. Pinned because the difference is invisible in the output — the
// statement is valid SQL either way, and the one that is missing a
// predicate is the one that returns more rows.
func TestToSQLRendersTheIncompleteStatement(t *testing.T) {
	posts := reachTable("lim_tosql_posts")
	db := pg.New(nil)

	wantPlain := `SELECT * FROM "lim_tosql_posts" WHERE ("lim_tosql_posts"."deletedAt" IS NULL)`
	got, args := db.Select().From(posts).ToSQL()
	if got != wantPlain {
		t.Errorf("got = %v, want %v", got, wantPlain)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}

	wantCtx := wantPlain + ` AND ("lim_tosql_posts"."tenantId" = $1)`
	got, args, err := db.Select().From(posts).ToSQLCtx(reachCtx())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != wantCtx {
		t.Errorf("got = %v, want %v", got, wantCtx)
	}
	if !sameArgs(args, []any{reachTenant}) {
		t.Errorf("args = %v, want %v", args, []any{reachTenant})
	}
}

// A DeleteHook runs inside WriteSQL, which has no ctx, so the statement
// it hands back is rendered ctx-free. DeleteHookFunc is exported, so an
// application can install one; the hook below composes an UPDATE over a
// second, tenant-scoped table and it goes out with no tenant predicate
// and no refusal, on a ctx that carries no tenant at all.
//
// SoftDeleteMixin's own rewrite is not in that position — it is built
// from the DeleteBuilder's already-resolved WHERE and RETURNING clauses,
// which is what TestStatementBodiesOfEveryBuilderAreResolved reads back
// as a scoped UPDATE — and the difference between the two is the whole
// content of the paragraph this pins.
func TestDeleteHookReplacementIsRenderedWithoutCtx(t *testing.T) {
	scoped := pg.NewTable("lim_hooked")
	pg.Add(scoped, pg.BigSerial("id").PrimaryKey())
	tenant := pg.Add(scoped, pg.BigInt("tenantId").NotNull())
	body := pg.Add(scoped, pg.Text("body"))
	scoped.ContextFilter(pg.TenantFilter(tenant))

	target := pg.NewTable("lim_hook_target")
	targetID := pg.Add(target, pg.BigSerial("id").PrimaryKey())
	target.OnDelete(pg.DeleteHookFunc(func(d *pg.DeleteBuilder) drops.Expression {
		return d.DB().Update(scoped).Set(body.Expr(drops.Raw("'x'")))
	}))

	db := pg.New(nil)
	want := `UPDATE "lim_hooked" SET "body" = 'x'`
	got, args, err := db.Delete(target).
		Where(pg.Eq(targetID.Column, 3)).
		ToSQLCtx(context.Background())
	if err != nil {
		t.Fatalf("ToSQLCtx: %v", err)
	}
	if got != want {
		t.Errorf("got = %v, want %v", got, want)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
}

type limParent struct {
	ID   int64      `drop:"id"`
	Kids []limChild `dropRel:"kids"`
}

type limChild struct {
	ID       int64 `drop:"id"`
	ParentID int64 `drop:"parentId"`
	TenantID int64 `drop:"tenantId"`
}

// Eager-load refusal is not atomic. The parent query is issued first and
// the edge's refusal arrives after it, so the caller gets an error and
// no rows while the parent read has already reached the server. That is
// not a leak — nothing is returned — but it is a statement a reader of
// the audit log will find, and a caller who needs the pair to be all or
// nothing wraps the call in a transaction.
func TestEagerLoadRefusalIsNotAtomic(t *testing.T) {
	parent := pg.NewTable("lim_parent")
	parentID := pg.Add(parent, pg.BigSerial("id").PrimaryKey())
	child := pg.NewTable("lim_child")
	pg.Add(child, pg.BigSerial("id").PrimaryKey())
	childFK := pg.Add(child, pg.BigInt("parentId").NotNull())
	childTenant := pg.Add(child, pg.BigInt("tenantId").NotNull())
	child.ContextFilter(pg.TenantFilter(childTenant))
	pg.NewRelations(parent).HasMany("kids", child, parentID, childFK)

	drv := dropstest.New().Rows([]string{"id"}, []any{int64(1)})
	db := pg.New(drv)
	ent := pg.NewEntity[limParent](parent)

	// The parent table is unscoped, so only the edge can refuse — and it
	// refuses on a ctx with no tenant.
	_, err := ent.Query(db).With("kids").All(context.Background())
	if !errors.Is(err, pg.ErrTenantMissing) {
		t.Fatalf("err = %v, want %v", err, pg.ErrTenantMissing)
	}
	sent := drv.SQL()
	want := []string{`SELECT * FROM "lim_parent"`}
	if len(sent) != len(want) || sent[0] != want[0] {
		t.Errorf("statements sent = %v, want %v", sent, want)
	}
}
